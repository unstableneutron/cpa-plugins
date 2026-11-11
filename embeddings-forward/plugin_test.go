package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

type seenRequest struct {
	path          string
	authorization string
	headers       http.Header
	body          map[string]json.RawMessage
	raw           []byte
}

// fixture is a deterministic local upstream that records every request.
type fixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	seen   []seenRequest
	reply  func(seenRequest) (int, string, string)
}

func newFixture(t *testing.T, reply func(seenRequest) (int, string, string)) *fixture {
	t.Helper()
	f := &fixture{t: t, reply: reply}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := seenRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization"), headers: r.Header.Clone(), raw: raw}
		_ = json.Unmarshal(raw, &req.body)
		f.mu.Lock()
		f.seen = append(f.seen, req)
		f.mu.Unlock()
		status, contentType, body := f.reply(req)
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

type testHandler struct {
	*handler
	t    *testing.T
	mu   sync.Mutex
	logs []string
}

func newTestHandler(t *testing.T, config string) *testHandler {
	t.Helper()
	h := &testHandler{handler: newHandler(), t: t}
	h.do = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		if req.HostCallbackID != "cb-1" {
			t.Errorf("host callback id = %q, want cb-1", req.HostCallbackID)
		}
		httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
		if err != nil {
			return hostHTTPResponse{}, err
		}
		httpReq.Header = req.Headers
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return hostHTTPResponse{}, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return hostHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
	}
	h.logf = func(_, level, message string, fields map[string]any) {
		encoded, _ := json.Marshal(fields)
		h.mu.Lock()
		h.logs = append(h.logs, level+" "+message+" "+string(encoded))
		h.mu.Unlock()
	}
	if _, callErr := h.Call(nativeabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte(config)})); callErr != nil {
		t.Fatalf("register: %v", callErr)
	}
	return h
}

func (h *testHandler) post(path, body string, headers map[string][]string) directResponse {
	h.t.Helper()
	result, callErr := h.Call(methodIngressHandle, mustJSON(ingressRequest{Method: http.MethodPost, Path: path, Headers: headers, Body: []byte(body), HostCallbackID: "cb-1"}))
	if callErr != nil {
		h.t.Fatalf("ingress.handle: %v", callErr)
	}
	resp := result.(ingressResponse)
	if !resp.Handled || resp.Response == nil {
		h.t.Fatalf("%s was not handled", path)
	}
	// Round-trip through JSON exactly like the host does.
	var decoded directResponse
	if err := json.Unmarshal(mustJSON(resp.Response), &decoded); err != nil {
		h.t.Fatal(err)
	}
	return decoded
}

func errorCode(t *testing.T, resp directResponse) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil || body.Error.Type == "" {
		t.Fatalf("response is not a structured error: %d %s", resp.StatusCode, resp.Body)
	}
	return body.Error.Code
}

func stringField(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("not a string: %s", raw)
	}
	return value
}

func TestEmbeddingsRewriteModelPerProviderAndFailOverOnInsufficientCredits(t *testing.T) {
	upstreamBody := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1234567890123,-2e-7]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":2,"total_tokens":2}}`
	explabs := newFixture(t, func(seenRequest) (int, string, string) {
		return http.StatusTooManyRequests, "", `{"error":{"message":"insufficient credits","type":"insufficient_quota"}}`
	})
	tokenrouter := newFixture(t, func(seenRequest) (int, string, string) { return http.StatusOK, "", upstreamBody })
	h := newTestHandler(t, fmt.Sprintf(`
embeddings:
  enabled: true
  providers:
    - name: explabs
      base_url: %s/v1
      api_keys: [explabs-key]
      cooldown_seconds: 60
      models:
        - {name: text-embedding-3-small, alias: openai/text-embedding-3-small}
    - name: tokenrouter
      base_url: %s/v1
      api_keys: [tokenrouter-key]
      models:
        - {name: openai/text-embedding-3-small}
        - {name: text-embedding-3-small}
`, explabs.server.URL, tokenrouter.server.URL))
	now := time.Unix(1000, 0)
	h.keys.now = func() time.Time { return now }

	request := `{"model":"openai/text-embedding-3-small","input":["a","b"],"dimensions":256,"encoding_format":"float"}`
	resp := h.post(routeEmbeddings, request, nil)
	if resp.StatusCode != http.StatusOK || string(resp.Body) != upstreamBody {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	explabsSeen, tokenSeen := explabs.requests(), tokenrouter.requests()
	if len(explabsSeen) != 1 || len(tokenSeen) != 1 {
		t.Fatalf("requests explabs=%d tokenrouter=%d", len(explabsSeen), len(tokenSeen))
	}
	if got := stringField(t, explabsSeen[0].body["model"]); got != "text-embedding-3-small" {
		t.Fatalf("explabs model = %q, want unprefixed name", got)
	}
	if string(explabsSeen[0].body["dimensions"]) != "256" || explabsSeen[0].path != "/v1/embeddings" || explabsSeen[0].authorization != "Bearer explabs-key" {
		t.Fatalf("explabs request = %+v", explabsSeen[0])
	}
	if tokenSeen[0].authorization != "Bearer tokenrouter-key" || string(tokenSeen[0].raw) != request {
		t.Fatalf("tokenrouter request must keep the client body: %s %q", tokenSeen[0].raw, tokenSeen[0].authorization)
	}

	// The cooling provider is ordered last, so the next request skips it.
	h.post(routeEmbeddings, request, nil)
	if len(explabs.requests()) != 1 || len(tokenrouter.requests()) != 2 {
		t.Fatalf("cooling provider was tried first: explabs=%d", len(explabs.requests()))
	}
	// Retry-After (120s) outlasts cooldown_seconds (60s).
	now = now.Add(90 * time.Second)
	h.post(routeEmbeddings, request, nil)
	if len(explabs.requests()) != 1 {
		t.Fatal("Retry-After was not honored")
	}
	now = now.Add(31 * time.Second)
	h.post(routeEmbeddings, `{"model":"text-embedding-3-small","input":"c"}`, nil)
	if got := explabs.requests(); len(got) != 2 || stringField(t, got[1].body["model"]) != "text-embedding-3-small" {
		t.Fatalf("explabs was not retried after cooldown with the unprefixed name: %d", len(got))
	}
}

func TestKeyRotationOn429And402AndProviderFailoverOn5xx(t *testing.T) {
	var mu sync.Mutex
	statusByKey := map[string]int{"Bearer a1": 429, "Bearer a2": 402, "Bearer a3": 200}
	primary := newFixture(t, func(req seenRequest) (int, string, string) {
		mu.Lock()
		defer mu.Unlock()
		return statusByKey[req.authorization], "", `{"results":[{"index":0,"relevance_score":0.9}]}`
	})
	secondary := newFixture(t, func(seenRequest) (int, string, string) {
		return http.StatusOK, "", `{"results":[{"index":0,"relevance_score":0.1}]}`
	})
	h := newTestHandler(t, fmt.Sprintf(`
rerank:
  enabled: true
  providers:
    - {name: openrouter, base_url: %s/api/v1, api_keys: [a1, a2, a3], cooldown_seconds: 30}
    - {name: backup, base_url: %s, api_keys: [b1]}
`, primary.server.URL, secondary.server.URL))
	now := time.Unix(1000, 0)
	h.keys.now = func() time.Time { return now }

	body := `{"model":"cohere/rerank-v3.5","query":"q","documents":["d"]}`
	if resp := h.post(routeRerank, body, nil); resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "0.9") {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	var keys []string
	for _, req := range primary.requests() {
		keys = append(keys, req.authorization)
	}
	if strings.Join(keys, ",") != "Bearer a1,Bearer a2,Bearer a3" || primary.requests()[0].path != "/api/v1/rerank" {
		t.Fatalf("key order = %v", keys)
	}
	// a1 (429) and a2 (402) are cooling, so a3 is tried first.
	h.post(routeRerank, body, nil)
	if got := primary.requests()[3].authorization; got != "Bearer a3" {
		t.Fatalf("cooling keys were tried first: %s", got)
	}

	// A 5xx moves to the next provider instead of burning the remaining keys.
	mu.Lock()
	statusByKey["Bearer a3"] = 503
	mu.Unlock()
	if resp := h.post(routeRerank, body, nil); resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "0.1") {
		t.Fatalf("failover response = %d %s", resp.StatusCode, resp.Body)
	}
	if got := len(primary.requests()); got != 5 {
		t.Fatalf("primary attempts = %d, want 5 (one a3 attempt after the 5xx)", got)
	}
}

func decisionQuestions(n int) (string, []string) {
	keys := make([]string, n)
	var parts []string
	for i := range keys {
		// Non-lexical order catches merges that sort answers.
		keys[i] = fmt.Sprintf("q%03d", (i*37)%n)
		parts = append(parts, fmt.Sprintf(`%q:{"type":"noul","instructions":"Is item %d relevant?"}`, keys[i], i))
	}
	return "{" + strings.Join(parts, ",") + "}", keys
}

func answerAll(req seenRequest, extra string) string {
	questions, _ := orderedObject(req.body["questions"])
	var answers []string
	for _, q := range questions {
		answers = append(answers, fmt.Sprintf(`%q:{"type":"noul","noul":0.5}`, q.key))
	}
	return fmt.Sprintf(`{"model":"jev-1.14.0","answers":{%s},"usage":{"input_tokens":%d,"output_tokens":1,"cost":0.0001}%s}`, strings.Join(answers, ","), len(questions), extra)
}

func TestDecisionsBatchesByQuestionCapWithBoundedConcurrencyAndMergesUsage(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	twoInFlight := make(chan struct{})
	var once sync.Once
	commandcode := newFixture(t, func(req seenRequest) (int, string, string) {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		if inFlight == 2 {
			once.Do(func() { close(twoInFlight) })
		}
		mu.Unlock()
		select {
		case <-twoInFlight:
		case <-time.After(5 * time.Second):
		}
		// Hold the slot so an unbounded third batch would overlap.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return http.StatusOK, "", answerAll(req, "")
	})
	h := newTestHandler(t, fmt.Sprintf(`
decisions:
  enabled: true
  providers:
    - name: commandcode
      base_url: %s/provider
      api: systemone
      api_keys: [cc-key]
      max_questions: 100
      max_concurrency: 2
      models:
        - {name: typesafe/jev, alias: jev}
`, commandcode.server.URL))

	questions, keys := decisionQuestions(250)
	body := `{"model":"jev","state":{"query":"<q>"},"questions":` + questions + `,"session_id":"s1","trace":{"trace_id":"t"},"provider":{"allow_fallbacks":false},"user":"u"}`
	resp := h.post(routeDecisions, body, map[string][]string{"Typesafe-Danger-Zone-Long-Context": {"true"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	seen := commandcode.requests()
	if len(seen) != 3 {
		t.Fatalf("batches = %d, want 3", len(seen))
	}
	sent := map[string]int{}
	for _, req := range seen {
		batch, _ := orderedObject(req.body["questions"])
		if len(batch) > 100 {
			t.Fatalf("batch has %d questions", len(batch))
		}
		for _, q := range batch {
			sent[q.key]++
		}
		for _, dropped := range []string{"session_id", "trace", "provider", "user"} {
			if _, ok := req.body[dropped]; ok {
				t.Fatalf("System One request kept Decisions-only field %q", dropped)
			}
		}
		if req.path != "/provider/v1/systemone" || stringField(t, req.body["model"]) != "typesafe/jev" || string(req.body["state"]) != `{"query":"<q>"}` {
			t.Fatalf("batch request = %s %s", req.path, req.raw)
		}
		if req.headers.Get("Typesafe-Danger-Zone-Long-Context") != "true" {
			t.Fatal("danger-zone header was not forwarded")
		}
	}
	if len(sent) != 250 {
		t.Fatalf("distinct questions sent = %d", len(sent))
	}
	for key, count := range sent {
		if count != 1 {
			t.Fatalf("question %s sent %d times", key, count)
		}
	}
	if maxInFlight != 2 {
		t.Fatalf("max concurrent batches = %d, want 2", maxInFlight)
	}

	var merged struct {
		Answers json.RawMessage `json:"answers"`
		Model   string          `json:"model"`
		Usage   struct {
			InputTokens  int     `json:"input_tokens"`
			OutputTokens int     `json:"output_tokens"`
			Cost         float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &merged); err != nil {
		t.Fatal(err)
	}
	answers, _ := orderedObject(merged.Answers)
	if len(answers) != 250 {
		t.Fatalf("answers = %d", len(answers))
	}
	for i, answer := range answers {
		if answer.key != keys[i] {
			t.Fatalf("answer %d = %s, want request order %s", i, answer.key, keys[i])
		}
	}
	if merged.Model != "jev-1.14.0" || merged.Usage.InputTokens != 250 || merged.Usage.OutputTokens != 3 || math.Abs(merged.Usage.Cost-0.0003) > 1e-12 {
		t.Fatalf("merged metadata = %+v", merged)
	}
}

func TestDecisionsRefusedQuestionFailsOverOnlyItsBatch(t *testing.T) {
	openrouter := newFixture(t, func(req seenRequest) (int, string, string) {
		if strings.Contains(string(req.body["questions"]), "refused") {
			return http.StatusBadGateway, "text/plain", "error code: 502"
		}
		return http.StatusOK, "", answerAll(req, `,"id":"gen-dec-1","provider":"OpenAI"`)
	})
	commandcode := newFixture(t, func(req seenRequest) (int, string, string) { return http.StatusOK, "", answerAll(req, "") })
	config := fmt.Sprintf(`
decisions:
  enabled: true
  providers:
    - name: openrouter
      base_url: %s/api
      api_keys: [or-key]
      max_questions: 200
      models: [{name: openai/gpt-6-luna-decisions, alias: luna}]
    - name: commandcode
      base_url: %s/provider
      api: systemone
      api_keys: [cc-key]
      max_questions: 100
      models: [{name: typesafe/jev, alias: luna}]
`, openrouter.server.URL, commandcode.server.URL)
	h := newTestHandler(t, config)

	questions, keys := decisionQuestions(300)
	// Question 1 lands in the first 200-question batch.
	questions = strings.Replace(questions, "Is item 1 relevant?", "refused", 1)
	resp := h.post(routeDecisions, `{"model":"luna","state":"s","questions":`+questions+`}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	if got := len(openrouter.requests()); got != 2 {
		t.Fatalf("openrouter batches = %d, want 2", got)
	}
	failedOver := map[string]bool{}
	for _, req := range commandcode.requests() {
		batch, _ := orderedObject(req.body["questions"])
		if len(batch) != 100 {
			t.Fatalf("fallback batch size = %d, want 100", len(batch))
		}
		for _, q := range batch {
			failedOver[q.key] = true
		}
	}
	if len(commandcode.requests()) != 2 || len(failedOver) != 200 || !failedOver[keys[1]] || failedOver[keys[250]] {
		t.Fatalf("only the refused 200-question batch should fail over: %d requests, %d questions", len(commandcode.requests()), len(failedOver))
	}
	var merged struct {
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &merged); err != nil || len(merged.Answers) != 300 || string(merged.Usage["input_tokens"]) != "300" {
		t.Fatalf("merged = %s", resp.Body)
	}

	// Without a fallback, the plain-text 502 becomes a structured error.
	alone := newTestHandler(t, strings.SplitN(config, "    - name: commandcode", 2)[0])
	resp = alone.post(routeSystemOne, `{"model":"luna","state":"s","questions":{"x":{"type":"noul","instructions":"refused"}}}`, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, resp) != "upstream_error" || !strings.Contains(string(resp.Body), "error code: 502") {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestDecisionsBillingErrorFailsOverAndPassesSingleBatchThrough(t *testing.T) {
	openrouter := newFixture(t, func(seenRequest) (int, string, string) {
		return http.StatusPaymentRequired, "", `{"error":{"code":402,"message":"billing_error: TypeSafe credits"}}`
	})
	tokenRouterBody := `{"id":"gen-dec-9","model":"typesafe/jev-1.14-20261014","provider":"TypeSafe","answers":{"billing":{"type":"noul","noul":0.97}},"usage":{"cost":0.00002,"input_tokens":12,"output_tokens":3}}`
	tokenrouter := newFixture(t, func(seenRequest) (int, string, string) { return http.StatusOK, "", tokenRouterBody })
	h := newTestHandler(t, fmt.Sprintf(`
decisions:
  enabled: true
  providers:
    - name: openrouter
      base_url: %s/api
      api_keys: [or-key]
      cooldown_seconds: 600
      models: [{name: typesafe/jev-1.14, alias: jev-latest}]
    - name: tokenrouter
      base_url: %s/api
      api_keys: [tr-key]
      models: [{name: typesafe/jev-1.14, alias: jev-latest}]
`, openrouter.server.URL, tokenrouter.server.URL))
	systemOne := `{"model":"jev-latest","state":"I was charged twice.","questions":{"billing":{"type":"noul","instructions":"Is this about billing?"}}}`
	for i := 0; i < 2; i++ {
		resp := h.post(routeSystemOne, systemOne, nil)
		if resp.StatusCode != http.StatusOK || string(resp.Body) != tokenRouterBody {
			t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
		}
	}
	if got := len(openrouter.requests()); got != 1 {
		t.Fatalf("billing-failed provider attempts = %d, want 1 while cooling", got)
	}
	req := tokenrouter.requests()[0]
	if req.path != "/api/alpha/decisions" || stringField(t, req.body["model"]) != "typesafe/jev-1.14" || string(req.body["state"]) != `"I was charged twice."` {
		t.Fatalf("System One body was not sent to Decisions unchanged: %s %s", req.path, req.raw)
	}
}

func TestDecisionsStringInstructionsAndAssetsPerBatch(t *testing.T) {
	openrouter := newFixture(t, func(req seenRequest) (int, string, string) { return http.StatusOK, "", answerAll(req, "") })
	h := newTestHandler(t, fmt.Sprintf(`
decisions:
  enabled: true
  providers:
    - name: openrouter
      base_url: %s/api
      api_keys: [or-key]
      max_questions: 1
      models: [{name: "respan/span-01-lite:free", string_instructions: true}]
`, openrouter.server.URL))
	body := `{"model":"respan/span-01-lite:free","state":"s","assets":{"img-a":{"format":"png","data_base64":"QQ=="},"img-b":{"format":"png","data_base64":"Qg=="}},` +
		`"questions":{"a":{"type":"noul","instructions":{"ask":"Is img-a red?","note":"<b>"}},"b":{"type":"noul","instructions":"Is img-b blue?"}}}`
	if resp := h.post(routeDecisions, body, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	seen := openrouter.requests()
	if len(seen) != 2 {
		t.Fatalf("requests = %d", len(seen))
	}
	for _, req := range seen {
		questions, _ := orderedObject(req.body["questions"])
		var question struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
		}
		_ = json.Unmarshal(questions[0].value, &question)
		instructions := stringField(t, question.Instructions)
		assets, _ := orderedObject(req.body["assets"])
		switch questions[0].key {
		case "a":
			if instructions != `{"ask":"Is img-a red?","note":"<b>"}` || question.Type != "noul" {
				t.Fatalf("structured instructions = %q", instructions)
			}
			if len(assets) != 1 || assets[0].key != "img-a" {
				t.Fatalf("batch a assets = %v", assets)
			}
		case "b":
			if instructions != "Is img-b blue?" || len(assets) != 1 || assets[0].key != "img-b" {
				t.Fatalf("batch b = %q %v", instructions, assets)
			}
		}
	}
}

func TestStructuredClientAndUpstreamErrors(t *testing.T) {
	rejecting := newFixture(t, func(seenRequest) (int, string, string) {
		return http.StatusBadRequest, "", `{"error":{"message":"bad input","type":"invalid_request_error"}}`
	})
	backup := newFixture(t, func(seenRequest) (int, string, string) { return http.StatusOK, "", `{}` })
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	h := newTestHandler(t, fmt.Sprintf(`
embeddings:
  enabled: true
  providers:
    - {name: gone, base_url: %s, api_keys: [secret-gone-key], models: [{name: m}]}
    - {name: strict, base_url: %s, api_keys: [secret-strict-key], models: [{name: m}]}
    - {name: backup, base_url: %s, api_keys: [secret-backup-key], models: [{name: m}]}
`, closed.URL, rejecting.server.URL, backup.server.URL))

	for name, tc := range map[string]struct {
		body    string
		headers map[string][]string
		status  int
		code    string
	}{
		"invalid json":    {body: `[`, status: 400, code: "invalid_json"},
		"missing model":   {body: `{"input":"x"}`, status: 400, code: "missing_model"},
		"unknown model":   {body: `{"model":"other","input":"x"}`, status: 404, code: "model_not_found"},
		"compressed body": {body: `{}`, headers: map[string][]string{"Content-Encoding": {"zstd"}}, status: 415, code: "unsupported_content_encoding"},
	} {
		resp := h.post(routeEmbeddings, tc.body, tc.headers)
		if resp.StatusCode != tc.status || errorCode(t, resp) != tc.code {
			t.Errorf("%s: response = %d %s", name, resp.StatusCode, resp.Body)
		}
	}

	// The transport failure fails over; the upstream 400 is final and passes through.
	resp := h.post(routeEmbeddings, `{"model":"m","input":"x"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(resp.Body), "bad input") || len(backup.requests()) != 0 {
		t.Fatalf("response = %d %s, backup calls %d", resp.StatusCode, resp.Body, len(backup.requests()))
	}
	h.mu.Lock()
	logs := strings.Join(h.logs, "\n")
	h.mu.Unlock()
	if !strings.Contains(logs, `"provider":"gone"`) {
		t.Fatalf("failover was not logged: %s", logs)
	}
	for _, secret := range []string{"secret-gone-key", "secret-strict-key", "secret-backup-key"} {
		if strings.Contains(logs, secret) || strings.Contains(string(resp.Body), secret) {
			t.Fatalf("secret %s leaked", secret)
		}
	}
}

func TestDecisionsRejectsMissingQuestions(t *testing.T) {
	fixture := newFixture(t, func(seenRequest) (int, string, string) { return 200, "", `{}` })
	h := newTestHandler(t, fmt.Sprintf("decisions:\n  enabled: true\n  providers: [{name: p, base_url: %s, api_keys: [k]}]\n", fixture.server.URL))
	for _, body := range []string{`{"model":"m","state":"s"}`, `{"model":"m","state":"s","questions":{}}`, `{"model":"m","questions":[1]}`} {
		if resp := h.post(routeDecisions, body, nil); resp.StatusCode != 400 || errorCode(t, resp) != "invalid_questions" {
			t.Fatalf("%s: response = %d %s", body, resp.StatusCode, resp.Body)
		}
	}
	if len(fixture.requests()) != 0 {
		t.Fatal("invalid requests reached the upstream")
	}
}

func TestLargeEmbeddingBatchPassesThroughUnchanged(t *testing.T) {
	const inputs, dims = 2048, 256
	var response strings.Builder
	response.WriteString(`{"object":"list","data":[`)
	for i := 0; i < inputs; i++ {
		if i > 0 {
			response.WriteByte(',')
		}
		fmt.Fprintf(&response, `{"object":"embedding","index":%d,"embedding":[`, i)
		for d := 0; d < dims; d++ {
			if d > 0 {
				response.WriteByte(',')
			}
			fmt.Fprintf(&response, "%.17g", float64(i*dims+d)/3e7-0.01)
		}
		response.WriteString("]}")
	}
	response.WriteString(`],"model":"text-embedding-3-small"}`)
	want := response.String()
	upstream := newFixture(t, func(seenRequest) (int, string, string) { return 200, "", want })
	h := newTestHandler(t, fmt.Sprintf("embeddings:\n  enabled: true\n  providers: [{name: p, base_url: %s, api_keys: [k]}]\n", upstream.server.URL))
	input := make([]string, inputs)
	for i := range input {
		input[i] = strings.Repeat("token ", 64) + fmt.Sprint(i)
	}
	request := string(mustJSON(map[string]any{"model": "text-embedding-3-small", "input": input}))
	resp := h.post(routeEmbeddings, request, nil)
	if resp.StatusCode != 200 || string(resp.Body) != want {
		t.Fatalf("response status %d, %d bytes, want %d bytes unchanged", resp.StatusCode, len(resp.Body), len(want))
	}
	if got := upstream.requests()[0].raw; string(got) != request {
		t.Fatalf("request body changed: %d bytes, want %d", len(got), len(request))
	}
}

func TestRegistrationRoutesAndConfigValidation(t *testing.T) {
	h := newTestHandler(t, `
systemone:
  enabled: true
  providers: [{name: commandcode, base_url: "https://api.commandcode.ai/provider", api_keys: [k]}]
rerank:
  enabled: false
`)
	result, _ := h.Call(nativeabi.MethodPluginRegister, mustJSON(lifecycleRequest{ConfigYAML: []byte("systemone:\n  enabled: true\n  providers: [{name: c, base_url: \"https://api.commandcode.ai/provider\", api_keys: [k]}]\n")}))
	if reg := result.(registration); reg.SchemaVersion != 9 || !reg.Capabilities.IngressProxy {
		t.Fatalf("registration = %+v", reg)
	}
	cfg := h.config()
	if len(cfg.Decisions.Providers) != 1 || cfg.Decisions.Providers[0].API != apiSystemOne || cfg.Decisions.Providers[0].url != "https://api.commandcode.ai/provider/v1/systemone" {
		t.Fatalf("legacy systemone module = %+v", cfg.Decisions)
	}
	routes, _ := h.Call(methodIngressRegister, nil)
	var paths []string
	for _, route := range routes.(ingressRegistrationResponse).Routes {
		if route.MaxBodyBytes != maxBodyBytes || len(route.Methods) != 1 || route.Methods[0] != http.MethodPost {
			t.Fatalf("route = %+v", route)
		}
		paths = append(paths, route.Path)
	}
	if strings.Join(paths, ",") != routeSystemOne+","+routeDecisions {
		t.Fatalf("routes = %v, want only enabled modules", paths)
	}
	// Disabled routes are not handled, so the host answers 404.
	if result, _ := h.Call(methodIngressHandle, mustJSON(ingressRequest{Method: "POST", Path: routeRerank, Body: []byte(`{"model":"x"}`)})); result.(ingressResponse).Handled {
		t.Fatal("disabled module was handled")
	}

	for name, config := range map[string]string{
		"no keys":           "embeddings: {enabled: true, providers: [{name: p, base_url: 'https://x'}]}",
		"bad scheme":        "embeddings: {enabled: true, providers: [{name: p, base_url: 'ftp://x', api_keys: [k]}]}",
		"query":             "embeddings: {enabled: true, providers: [{name: p, base_url: 'https://x?a=1', api_keys: [k]}]}",
		"credentials":       "embeddings: {enabled: true, providers: [{name: p, base_url: 'https://u:p@x', api_keys: [k]}]}",
		"strategy":          "embeddings: {enabled: true, providers: [{name: p, base_url: 'https://x', api_keys: [k], strategy: random}]}",
		"batching on embed": "embeddings: {enabled: true, providers: [{name: p, base_url: 'https://x', api_keys: [k], max_questions: 5}]}",
		"api":               "decisions: {enabled: true, providers: [{name: p, base_url: 'https://x', api_keys: [k], api: chat}]}",
		"duplicate":         "decisions: {enabled: true, providers: [{name: p, base_url: 'https://x', api_keys: [k]}, {name: p, base_url: 'https://y', api_keys: [k]}]}",
		"key newline":       "decisions: {enabled: true, providers: [{name: p, base_url: 'https://x', api_keys: [\"secret-value\\n\"]}]}",
		"empty":             "rerank: {enabled: true}",
	} {
		_, callErr := h.Call(nativeabi.MethodPluginReconfigure, mustJSON(lifecycleRequest{ConfigYAML: []byte(config)}))
		if callErr == nil || callErr.Code != "invalid_config" {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(callErr.Message, "secret-value") {
			t.Errorf("%s: config error leaked a key", name)
		}
	}
	if got := h.config(); len(got.Decisions.Providers) != 1 {
		t.Fatal("a rejected reconfigure replaced the working config")
	}
}

func TestModuleRetryRounds(t *testing.T) {
	for _, mod := range []struct {
		name string
		path string
	}{
		{"embeddings", routeEmbeddings}, {"rerank", routeRerank},
		{"decisions", routeDecisions}, {"systemone", routeSystemOne},
	} {
		for _, tc := range []struct {
			name     string
			setting  string
			statuses []int
			calls    int
			status   int
		}{
			{"default unchanged", "", []int{503}, 1, 503},
			{"explicit zero", "retries: 0", []int{503}, 1, 503},
			{"success stops", "retries: 2", []int{200}, 1, 200},
			{"recover 5xx", "retries: 1", []int{503, 200}, 2, 200},
			{"recover transport", "retries: 1", []int{0, 200}, 2, 200},
			{"recover quota", "retries: 1", []int{429, 200}, 2, 200},
			{"recover credits", "retries: 1", []int{402, 200}, 2, 200},
			{"bounded", "retries: 2", []int{503}, 3, 503},
			{"400 final", "retries: 2", []int{400, 200}, 1, 400},
			{"401 final", "retries: 2", []int{401, 200}, 1, 401},
			{"403 final", "retries: 2", []int{403, 200}, 1, 403},
		} {
			t.Run(mod.name+"/"+tc.name, func(t *testing.T) {
				h := newTestHandler(t, fmt.Sprintf("%s:\n  enabled: true\n  %s\n  providers: [{name: p, base_url: 'https://unused.invalid', api_keys: [k]}]\n", mod.name, tc.setting))
				var mu sync.Mutex
				calls := 0
				h.do = func(req hostHTTPRequest) (hostHTTPResponse, error) {
					mu.Lock()
					defer mu.Unlock()
					status := tc.statuses[min(calls, len(tc.statuses)-1)]
					calls++
					if status == 0 {
						return hostHTTPResponse{}, fmt.Errorf("transport failure")
					}
					body := `{"answers":{"q":{"value":"<&>"}}}`
					if status != 200 {
						body = `{"error":{"message":"failed"}}`
					}
					return hostHTTPResponse{StatusCode: status, Body: []byte(body)}, nil
				}
				resp := h.post(mod.path, `{"model":"m","input":"x","query":"q","documents":["d"],"questions":{"q":{"type":"noul","instructions":"choose"}}}`, nil)
				if calls != tc.calls || resp.StatusCode != tc.status {
					t.Fatalf("attempts = %d, status = %d; want %d, %d: %s", calls, resp.StatusCode, tc.calls, tc.status, resp.Body)
				}
				if resp.StatusCode == 200 && string(resp.Body) != `{"answers":{"q":{"value":"<&>"}}}` {
					t.Fatalf("successful response changed: %s", resp.Body)
				}
			})
		}
	}
}

func TestRetryRoundReordersCoolingProvidersAndRotatesKeys(t *testing.T) {
	h := newTestHandler(t, `
embeddings:
  enabled: true
  retries: 1
  providers:
    - {name: primary, base_url: 'https://primary.invalid', api_keys: [a], cooldown_seconds: 60}
    - {name: backup, base_url: 'https://backup.invalid', api_keys: [b1, b2], strategy: round_robin}
`)
	var attempts []string
	h.do = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		key := headerValue(req.Headers, "Authorization")
		attempts = append(attempts, key)
		status := http.StatusServiceUnavailable
		if key == "Bearer a" {
			status = http.StatusTooManyRequests
		} else if key == "Bearer b2" {
			status = http.StatusOK
		}
		return hostHTTPResponse{StatusCode: status, Body: []byte(`{}`)}, nil
	}
	resp := h.post(routeEmbeddings, `{"model":"m","input":"x"}`, nil)
	if resp.StatusCode != 200 || strings.Join(attempts, ",") != "Bearer a,Bearer b1,Bearer b2" {
		t.Fatalf("status = %d, attempts = %v", resp.StatusCode, attempts)
	}
}

func TestDecisionRetriesRetainSuccessfulBatches(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	upstream := newFixture(t, func(req seenRequest) (int, string, string) {
		questions, _ := orderedObject(req.body["questions"])
		if len(questions) != 1 {
			t.Errorf("batch = %s", req.body["questions"])
			return http.StatusBadRequest, "", `{}`
		}
		key := questions[0].key
		mu.Lock()
		counts[key]++
		count := counts[key]
		mu.Unlock()
		assets, _ := orderedObject(req.body["assets"])
		if len(assets) != 1 || assets[0].key != "img-"+key {
			t.Errorf("question %s assets = %s", key, req.body["assets"])
		}
		if key == "b" && count == 1 {
			return http.StatusServiceUnavailable, "", `{"error":{"message":"temporary"}}`
		}
		return http.StatusOK, "", answerAll(req, "")
	})
	h := newTestHandler(t, fmt.Sprintf(`
decisions:
  enabled: true
  retries: 1
  providers:
    - {name: p, base_url: %s, api_keys: [k], max_questions: 1, max_concurrency: 2}
`, upstream.server.URL))
	body := `{"model":"m","state":"s","assets":{"img-a":{"format":"png","data_base64":"QQ=="},"img-b":{"format":"png","data_base64":"Qg=="}},"questions":{"a":{"type":"noul","instructions":"img-a"},"b":{"type":"noul","instructions":"img-b"}}}`
	resp := h.post(routeDecisions, body, nil)
	answers, ok := parseAnswers(resp.Body)
	if resp.StatusCode != 200 || !ok || len(answers) != 2 {
		t.Fatalf("response = %d %s", resp.StatusCode, resp.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["a"] != 1 || counts["b"] != 2 {
		t.Fatalf("attempt counts = %v", counts)
	}
	var merged struct {
		Usage map[string]float64 `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &merged); err != nil || merged.Usage["input_tokens"] != 2 || merged.Usage["output_tokens"] != 2 {
		t.Fatalf("merged usage = %s: %v", resp.Body, err)
	}
}

func TestRetryConfigAndForkMetadata(t *testing.T) {
	for _, name := range []string{"embeddings", "rerank", "decisions", "systemone"} {
		if _, err := parseConfig([]byte(name + ": {retries: -1}")); err == nil {
			t.Errorf("%s: negative retries accepted", name)
		}
	}
	cfg, err := parseConfig([]byte(`
decisions:
  enabled: true
  retries: 1
  providers: [{name: d, base_url: 'https://d.invalid', api_keys: [k]}]
systemone:
  enabled: true
  retries: 2
  providers: [{name: s, base_url: 'https://s.invalid', api_keys: [k]}]
`))
	if err != nil || cfg.Decisions.Retries != 2 || len(cfg.Decisions.Providers) != 2 {
		t.Fatalf("shared retry config = %+v: %v", cfg.Decisions, err)
	}
	original := nativeabi.Version
	defer func() { nativeabi.Version = original }()
	for _, version := range []string{"dev", "0.2.1-test"} {
		nativeabi.Version = version
		h := newHandler()
		result, callErr := h.Call(nativeabi.MethodPluginRegister, mustJSON(lifecycleRequest{}))
		if callErr != nil {
			t.Fatal(callErr)
		}
		reg := result.(registration)
		want := version
		if version == "dev" {
			want = "0.2.1"
		}
		if reg.Metadata.Version != want || reg.Metadata.Name != "Embeddings, rerank and decisions forward" ||
			reg.Metadata.GitHubRepository != "https://github.com/unstableneutron/cpa-plugins" || reg.SchemaVersion != 9 {
			t.Fatalf("fork registration = %+v", reg)
		}
	}
}
