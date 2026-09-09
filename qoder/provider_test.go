package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

var testHost struct {
	sync.Mutex
	call func(string, []byte) (any, *nativeabi.Error)
}

func TestMain(m *testing.M) {
	err := runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
		testHost.Lock()
		call := testHost.call
		testHost.Unlock()
		if call == nil {
			return []byte(`{"ok":false,"error":{"code":"test","message":"unexpected callback"}}`), 1
		}
		v, f := call(method, request)
		var result json.RawMessage
		if v != nil {
			result, _ = json.Marshal(v)
		}
		raw, _ := json.Marshal(nativeabi.Envelope{OK: f == nil, Result: result, Error: f})
		if f != nil {
			return raw, 1
		}
		return raw, 0
	})
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
func setHost(t *testing.T, call func(string, []byte) (any, *nativeabi.Error)) {
	t.Helper()
	testHost.Lock()
	testHost.call = call
	testHost.Unlock()
	t.Cleanup(func() { testHost.Lock(); testHost.call = nil; testHost.Unlock() })
}

func TestBodyEncodingDerivedFixtures(t *testing.T) {
	cases := map[string]string{"": "", "f": "$&$D", "hello world": "YuHp$Hq&J(WPHFru", "{\"x\":1}": "ep$$BMn%mYKI"}
	for in, want := range cases {
		if got := encodeBody([]byte(in)); got != want {
			t.Errorf("encodeBody(%q)=%q want %q", in, got, want)
		}
	}
}
func TestCosyHeadersBindExactEncodedBodyAndPath(t *testing.T) {
	body := []byte("binary\x00body")
	h, err := cosyHeaders(body, chatURL, tokenStorage{Token: "dt-token", UserID: "user", MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Cosy-Bodylength") != "11" || h.Get("Cosy-Sigpath") != "/api/v2/service/pro/sse/agent_chat_generation" || !strings.HasPrefix(h.Get("Authorization"), "Bearer COSY.") {
		t.Fatalf("headers=%v", h)
	}
	if h.Get("Cosy-Bodyhash") != "f2be35a964d22452143c7753effbfe88" {
		t.Fatalf("hash=%s", h.Get("Cosy-Bodyhash"))
	}
}
func TestRequestBodyNormalizesSystemToolsAndModelConfig(t *testing.T) {
	s := tokenStorage{UserID: "u", ModelConfigs: map[string]json.RawMessage{"auto": json.RawMessage(`{"key":"old","is_reasoning":true,"max_output_tokens":1000,"source":"system"}`)}}
	raw, err := requestBody([]byte(`{"model":"ignored","messages":[{"role":"system","content":"rules"},{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"tool","tool_call_id":"c1","content":"answer"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"max_tokens":300}`), "qoder/auto", s)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		t.Fatal("decode")
	}
	if body["system"] != "rules" || body["parameters"].(map[string]any)["max_tokens"] != float64(300) || len(body["messages"].([]any)) != 2 || len(body["tools"].([]any)) != 1 {
		t.Fatalf("body=%s", raw)
	}
	if body["model_config"].(map[string]any)["key"] != "auto" {
		t.Fatalf("model config=%v", body["model_config"])
	}
}

func TestSSEExtractionErrorsToolsUsageAndAggregation(t *testing.T) {
	inner1 := `{"id":"r1","created":7,"choices":[{"delta":{"content":"hi ","tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`
	inner2 := `{"choices":[{"delta":{"content":"there","tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	env1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner1})
	env2, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner2})
	stream := "data: " + string(env1) + "\n\ndata: " + string(env2) + "\n\ndata: [DONE]\n\n"
	chunks, f := extractSSE([]byte(stream))
	if f != nil {
		t.Fatal(f)
	}
	out := aggregate(chunks, "qoder/auto")
	if !strings.Contains(string(out), "hi there") || !strings.Contains(string(out), `{\"q\":\"x\"}`) || !strings.Contains(string(out), `"total_tokens":5`) {
		t.Fatalf("out=%s", out)
	}
	_, f, _ = extractLine([]byte(`data: {"statusCodeValue":429,"body":"quota"}`))
	if f == nil || !f.Retryable || f.Scope != "request" || f.HTTPStatus != 429 {
		t.Fatalf("failure=%+v", f)
	}
}

func TestDeviceGrantPendingSuccessAndModelDiscovery(t *testing.T) {
	start, f := startLogin()
	if f != nil {
		t.Fatal(f)
	}
	startRaw, _ := json.Marshal(start)
	var sr struct{ State, URL string }
	_ = json.Unmarshal(startRaw, &sr)
	if !strings.Contains(sr.URL, "challenge_method=S256") {
		t.Fatalf("url=%s", sr.URL)
	}
	calls := 0
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		calls++
		var req struct {
			URL            string
			HostCallbackID string `json:"host_callback_id"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.HostCallbackID != "cb" {
			t.Fatalf("callback=%q", req.HostCallbackID)
		}
		switch calls {
		case 1:
			return httpResponse{StatusCode: 200, Body: []byte(`{"token":"dt","refresh_token":"drt","user_id":"uid","expires_in":3600}`)}, nil
		case 2:
			return httpResponse{StatusCode: 200, Body: []byte(`{"name":"Q User","email":"q@example.com"}`)}, nil
		default:
			t.Fatal("too many callbacks")
		}
		return nil, nil
	})
	pollRaw, _ := json.Marshal(map[string]any{"State": sr.State, "host_callback_id": "cb"})
	result, f := pollLogin(pollRaw)
	if f != nil {
		t.Fatal(f)
	}
	encoded, _ := json.Marshal(result)
	var parsed struct{ Auth authData }
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	var saved tokenStorage
	if err := json.Unmarshal(parsed.Auth.StorageJSON, &saved); err != nil {
		t.Fatal(err)
	}
	if parsed.Auth.ID != "q@example.com" || saved.RefreshToken != "drt" {
		t.Fatalf("result=%s", encoded)
	}
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		return httpResponse{StatusCode: 200, Body: []byte(`{"chat":[{"key":"auto","enable":true,"display_name":"Auto","max_input_tokens":123,"is_vl":true},{"key":"off","enable":false}]}`)}, nil
	})
	storageRaw, _ := json.Marshal(tokenStorage{Token: "dt", UserID: "uid", MachineID: "m"})
	modelRaw, _ := json.Marshal(executorRequest{StorageJSON: storageRaw, HostCallbackID: "cb"})
	models, f := discoverModels(modelRaw)
	if f != nil {
		t.Fatal(f)
	}
	encoded, _ = json.Marshal(models)
	var discovered struct {
		AuthUpdate authData
		Models     []map[string]any
	}
	if err := json.Unmarshal(encoded, &discovered); err != nil {
		t.Fatal(err)
	}
	var updated tokenStorage
	if err := json.Unmarshal(discovered.AuthUpdate.StorageJSON, &updated); err != nil {
		t.Fatal(err)
	}
	if len(discovered.Models) != 1 || discovered.Models[0]["ID"] != "qoder/auto" || len(updated.ModelConfigs["auto"]) == 0 {
		t.Fatalf("models=%s", encoded)
	}
}

func TestBinaryEmptyAndLargeHostBodies(t *testing.T) {
	sizes := []int{0, 4, 2 << 20}
	for _, n := range sizes {
		body := make([]byte, n)
		if n > 0 {
			body[0] = 0
			body[n-1] = 255
		}
		setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
			var req struct{ Body []byte }
			_ = json.Unmarshal(raw, &req)
			if len(req.Body) != n {
				t.Fatalf("body len=%d want %d", len(req.Body), n)
			}
			return httpResponse{StatusCode: 201, Body: body}, nil
		})
		s, _ := json.Marshal(tokenStorage{Token: "dt", UserID: "uid", MachineID: "m"})
		raw, _ := json.Marshal(map[string]any{"Method": "POST", "URL": "https://api3.qoder.sh/algo/test", "Headers": http.Header{}, "Body": body, "StorageJSON": s, "host_callback_id": "cb"})
		result, f := rawHTTP(raw)
		if f != nil {
			t.Fatal(f)
		}
		resp := result.(httpResponse)
		if len(resp.Body) != n {
			t.Fatalf("response len=%d", len(resp.Body))
		}
	}
}

func TestForwardSplitSSEClosesExactlyOnce(t *testing.T) {
	inner := `{"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	stream := append([]byte("data: "), envelope...)
	stream = append(stream, []byte("\n\ndata: [DONE]\n\n")...)
	reads, emits, pluginCloses, hostCloses := 0, 0, 0, 0
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		switch method {
		case nativeabi.MethodHostHTTPStreamRead:
			reads++
			if reads == 1 {
				return hostStreamRead{Payload: stream[:9]}, nil
			}
			return hostStreamRead{Payload: stream[9:], Done: true}, nil
		case nativeabi.MethodHostStreamEmit:
			emits++
			return struct{}{}, nil
		case nativeabi.MethodHostStreamClose:
			pluginCloses++
			return struct{}{}, nil
		case nativeabi.MethodHostHTTPStreamClose:
			hostCloses++
			return struct{}{}, nil
		default:
			t.Fatalf("method=%s", method)
		}
		return nil, nil
	})
	forward("plugin", "host")
	if emits != 1 || pluginCloses != 1 || hostCloses != 1 {
		t.Fatalf("emits=%d plugin closes=%d host closes=%d", emits, pluginCloses, hostCloses)
	}
}

func TestExecuteNonStreamUsesEncodedHostRequest(t *testing.T) {
	inner := `{"id":"q1","choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		if method != nativeabi.MethodHostHTTPDo {
			t.Fatalf("method=%s", method)
		}
		var req struct {
			URL            string
			Headers        http.Header
			Body           []byte
			HostCallbackID string `json:"host_callback_id"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.URL != chatURL || req.HostCallbackID != "cb-exec" || json.Valid(req.Body) || req.Headers.Get("Cosy-Sigpath") == "" || req.Headers.Get("X-Model-Key") != "auto" {
			t.Fatalf("request=%+v", req)
		}
		return httpResponse{StatusCode: 200, Body: append(append([]byte("data: "), envelope...), []byte("\n\ndata: [DONE]\n\n")...)}, nil
	})
	s, _ := json.Marshal(tokenStorage{Token: "dt", UserID: "uid", MachineID: "m", ModelConfigs: map[string]json.RawMessage{"auto": json.RawMessage(`{"key":"auto","max_output_tokens":100}`)}})
	raw, _ := json.Marshal(executorRequest{Model: "qoder/auto", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"user","content":"question"}]}`), StorageJSON: s, HostCallbackID: "cb-exec"})
	result, failure := execute(raw, false)
	if failure != nil {
		t.Fatal(failure)
	}
	encoded, _ := json.Marshal(result)
	var response struct{ Payload []byte }
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Payload), `"content":"answer"`) || !strings.Contains(string(response.Payload), `"total_tokens":3`) {
		t.Fatalf("payload=%s", response.Payload)
	}
}
