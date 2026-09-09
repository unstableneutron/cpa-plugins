package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	providerID = "qoder"
	loginURL   = "https://qoder.com/device/selectAccounts"
	pollURL    = "https://openapi.qoder.sh/api/v1/deviceToken/poll"
	userURL    = "https://openapi.qoder.sh/api/v1/userinfo"
	modelURL   = "https://api3.qoder.sh/algo/api/v2/model/list"
	chatURL    = "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
)

type provider struct{}
type registration struct {
	SchemaVersion          uint32 `json:"schema_version"`
	Metadata, Capabilities map[string]any
}
type executorRequest struct {
	Model, Format, SourceFormat string
	Payload, StorageJSON        []byte
	AuthMetadata                map[string]any
	AuthAttributes              map[string]string
	StreamID                    string `json:"stream_id"`
	HostCallbackID              string `json:"host_callback_id"`
}
type tokenStorage struct {
	Token        string                     `json:"token"`
	RefreshToken string                     `json:"refresh_token"`
	UserID       string                     `json:"user_id"`
	Name         string                     `json:"name"`
	Email        string                     `json:"email"`
	ExpireTime   int64                      `json:"expire_time"`
	Type         string                     `json:"type"`
	LastRefresh  string                     `json:"last_refresh"`
	MachineID    string                     `json:"machine_id,omitempty"`
	MachineToken string                     `json:"machine_token,omitempty"`
	MachineType  string                     `json:"machine_type,omitempty"`
	ModelConfigs map[string]json.RawMessage `json:"model_configs,omitempty"`
}
type authData struct {
	Provider, ID, FileName, Label string
	StorageJSON                   []byte
	Metadata                      map[string]any
	Attributes                    map[string]string
	NextRefreshAfter              time.Time
}
type httpResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type hostStreamRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}
type loginState struct {
	Verifier  string `json:"verifier"`
	Nonce     string `json:"nonce"`
	MachineID string `json:"machine_id"`
}

func (provider) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		return registration{nativeabi.SchemaVersion, map[string]any{"Name": "Qoder", "Version": nativeabi.Version, "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}}, map[string]any{"model_provider": true, "auth_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}}}, nil
	case nativeabi.MethodPluginQuiesce, nativeabi.MethodPluginShutdown:
		return struct{}{}, nil
	case "executor.identifier", "auth.identifier":
		return map[string]string{"identifier": providerID}, nil
	case "auth.parse":
		return parseAuth(raw)
	case "auth.login.start":
		return startLogin()
	case "auth.login.poll":
		return pollLogin(raw)
	case "auth.refresh":
		return refresh(raw)
	case "model.static":
		return map[string]any{"Provider": providerID, "Models": []any{}}, nil
	case "model.for_auth":
		return discoverModels(raw)
	case "executor.execute":
		return execute(raw, false)
	case "executor.execute_stream":
		return execute(raw, true)
	case "executor.count_tokens":
		return countTokens(raw)
	case "executor.http_request":
		return rawHTTP(raw)
	default:
		return nil, fail("unknown_method", "unknown method: "+method, 0, "request")
	}
}

func execute(raw []byte, stream bool) (any, *nativeabi.Error) {
	var r executorRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	if !chatFormat(r.SourceFormat) || !chatFormat(r.Format) {
		return nil, fail("unsupported_format", "Qoder accepts chat-completions payloads", 400, "request")
	}
	s, e := credentials(r.StorageJSON)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	body, e := requestBody(r.Payload, r.Model, s)
	if e != nil {
		return nil, fail("invalid_request", e.Error(), 400, "request")
	}
	encoded := []byte(encodeBody(body))
	headers, e := cosyHeaders(encoded, chatURL, s)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("X-Model-Key", stripModel(r.Model))
	source := "system"
	var cfg map[string]any
	_ = json.Unmarshal(s.ModelConfigs[stripModel(r.Model)], &cfg)
	if v, _ := cfg["source"].(string); v != "" {
		source = v
	}
	headers.Set("X-Model-Source", source)
	if stream {
		if r.StreamID == "" {
			return nil, fail("invalid_request", "stream_id is required", 400, "request")
		}
		var up hostStreamResponse
		if e = runtime.HostCall(nativeabi.MethodHostHTTPDoStream, hostReq("POST", chatURL, headers, encoded, r.HostCallbackID), &up); e != nil {
			return nil, hostFail(e)
		}
		if up.StatusCode != 200 {
			_ = closeHost(up.StreamID)
			return nil, fail("upstream_http_error", fmt.Sprintf("Qoder returned HTTP %d", up.StatusCode), up.StatusCode, scope(up.StatusCode))
		}
		go forward(r.StreamID, up.StreamID)
		return map[string]any{"headers": up.Headers}, nil
	}
	var up httpResponse
	if e = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", chatURL, headers, encoded, r.HostCallbackID), &up); e != nil {
		return nil, hostFail(e)
	}
	if up.StatusCode != 200 {
		return nil, fail("upstream_http_error", string(up.Body), up.StatusCode, scope(up.StatusCode))
	}
	chunks, f := extractSSE(up.Body)
	if f != nil {
		return nil, f
	}
	return map[string]any{"Payload": aggregate(chunks, r.Model), "Headers": up.Headers}, nil
}

func requestBody(payload []byte, requestedModel string, s tokenStorage) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(payload, &chat); err != nil {
		return nil, err
	}
	messages, _ := chat["messages"].([]any)
	normalized, system := normalizeMessages(messages)
	model := stripModel(first(requestedModel, stringAny(chat["model"])))
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	rawCfg, ok := s.ModelConfigs[model]
	if !ok {
		return nil, fmt.Errorf("qoder model config is unavailable for %q", model)
	}
	var cfg map[string]any
	if json.Unmarshal(rawCfg, &cfg) != nil {
		return nil, fmt.Errorf("qoder model config is invalid")
	}
	cfg["key"] = model
	max := int(number(cfg["max_output_tokens"]))
	if max <= 0 {
		max = 32768
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if n := int(number(chat[key])); n > 0 && n < max {
			max = n
		}
	}
	last := lastUser(normalized)
	tools := chat["tools"]
	if tools == nil {
		tools = []any{}
	}
	record := stable("qoder-record", model, mustJSON(normalized), mustJSON(tools), strconv.Itoa(max))
	reasoning, _ := cfg["is_reasoning"].(bool)
	v := map[string]any{"request_id": randomUUID(), "request_set_id": record, "chat_record_id": record, "session_id": stable("qoder-session", s.UserID, model), "stream": true, "chat_task": "FREE_INPUT", "is_reply": true, "is_retry": false, "source": 1, "version": "3", "session_type": "qodercli", "agent_id": "agent_common", "task_id": "common", "code_language": "", "chat_prompt": "", "image_urls": nil, "aliyun_user_type": "", "system": system, "messages": normalized, "tools": tools, "parameters": map[string]any{"max_tokens": max}, "chat_context": map[string]any{"chatPrompt": "", "imageUrls": nil, "extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": model, "is_reasoning": reasoning}, "originalContent": last}, "features": []any{}, "text": last}, "model_config": cfg, "business": map[string]any{"product": "cli", "version": "1.0.0", "type": "agent", "stage": "start", "id": randomUUID(), "name": truncate(last, 30), "begin_at": time.Now().UnixMilli()}}
	return json.Marshal(v)
}

func forward(pluginID, hostID string) {
	var once sync.Once
	closePlugin := func(f *nativeabi.Error) {
		once.Do(func() {
			_ = runtime.HostCall(nativeabi.MethodHostStreamClose, nativeabi.StreamCloseRequest{StreamID: pluginID, Failure: f}, nil)
		})
	}
	defer func() {
		_ = closeHost(hostID)
		if recover() != nil {
			closePlugin(fail("plugin_panic", "Qoder stream processing failed", 500, "request"))
		}
	}()
	var pending []byte
	for {
		var r hostStreamRead
		if err := runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": hostID}, &r); err != nil {
			closePlugin(hostFail(err))
			return
		}
		if r.Error != "" {
			closePlugin(fail("upstream_stream_error", r.Error, 502, "request"))
			return
		}
		pending = append(pending, r.Payload...)
		for {
			line, rest, ok := nextLine(pending)
			if !ok {
				break
			}
			pending = rest
			payload, f, done := extractLine(line)
			if f != nil {
				closePlugin(f)
				return
			}
			if done {
				closePlugin(nil)
				return
			}
			if len(payload) > 0 {
				if err := runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginID, Payload: payload}, nil); err != nil {
					closePlugin(hostFail(err))
					return
				}
			}
		}
		if r.Done {
			if len(bytes.TrimSpace(pending)) > 0 {
				payload, f, _ := extractLine(pending)
				if f != nil {
					closePlugin(f)
					return
				}
				if len(payload) > 0 {
					if err := runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginID, Payload: payload}, nil); err != nil {
						closePlugin(hostFail(err))
						return
					}
				}
			}
			closePlugin(nil)
			return
		}
	}
}
func nextLine(b []byte) ([]byte, []byte, bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return nil, b, false
	}
	return bytes.TrimSuffix(b[:i], []byte{'\r'}), b[i+1:], true
}
func extractLine(line []byte) ([]byte, *nativeabi.Error, bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil, false
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil, nil, true
	}
	var env struct {
		StatusCodeValue int    `json:"statusCodeValue"`
		Body            string `json:"body"`
	}
	if json.Unmarshal(data, &env) != nil {
		return nil, nil, false
	}
	if env.StatusCodeValue != 0 && env.StatusCodeValue != 200 {
		return nil, fail("upstream_stream_error", first(env.Body, fmt.Sprintf("upstream status %d", env.StatusCodeValue)), env.StatusCodeValue, scope(env.StatusCodeValue)), false
	}
	if env.Body == "[DONE]" {
		return nil, nil, true
	}
	if !json.Valid([]byte(env.Body)) {
		return nil, nil, false
	}
	return []byte(env.Body), nil, false
}
func extractSSE(raw []byte) ([][]byte, *nativeabi.Error) {
	var out [][]byte
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 64<<10), 52_428_800)
	for s.Scan() {
		p, f, done := extractLine(s.Bytes())
		if f != nil {
			return nil, f
		}
		if len(p) > 0 {
			out = append(out, append([]byte(nil), p...))
		}
		if done {
			break
		}
	}
	if err := s.Err(); err != nil {
		return nil, fail("invalid_upstream_response", err.Error(), 502, "request")
	}
	return out, nil
}
func aggregate(chunks [][]byte, model string) []byte {
	var id, content, reasoning, finish string
	var created int64
	tools := map[int]map[string]string{}
	var usage any
	for _, raw := range chunks {
		var c map[string]any
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		if v := stringAny(c["id"]); v != "" {
			id = v
		}
		if n := int64(number(c["created"])); n != 0 {
			created = n
		}
		if c["usage"] != nil {
			usage = c["usage"]
		}
		choices, _ := c["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if v := stringAny(choice["finish_reason"]); v != "" {
			finish = v
		}
		delta, _ := choice["delta"].(map[string]any)
		content += stringAny(delta["content"])
		reasoning += stringAny(delta["reasoning"])
		for _, rawTC := range sliceAny(delta["tool_calls"]) {
			tc, _ := rawTC.(map[string]any)
			idx := int(number(tc["index"]))
			entry := tools[idx]
			if entry == nil {
				entry = map[string]string{}
				tools[idx] = entry
			}
			if v := stringAny(tc["id"]); v != "" {
				entry["id"] = v
			}
			fn, _ := tc["function"].(map[string]any)
			if v := stringAny(fn["name"]); v != "" {
				entry["name"] = v
			}
			entry["arguments"] += stringAny(fn["arguments"])
		}
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning"] = reasoning
	}
	if len(tools) > 0 {
		keys := make([]int, 0, len(tools))
		for k := range tools {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		var calls []any
		for _, k := range keys {
			v := tools[k]
			calls = append(calls, map[string]any{"id": v["id"], "type": "function", "function": map[string]string{"name": v["name"], "arguments": first(v["arguments"], "{}")}})
		}
		msg["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	v := map[string]any{"id": first(id, "qoder-"+randomUUID()), "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": first(finish, "stop")}}}
	if usage != nil {
		v["usage"] = usage
	}
	b, _ := json.Marshal(v)
	return b
}

func startLogin() (any, *nativeabi.Error) {
	verifier, challenge := pkce()
	state := loginState{verifier, randomUUID(), randomUUID()}
	b, _ := json.Marshal(state)
	u := loginURL + "?challenge=" + url.QueryEscape(challenge) + "&challenge_method=S256&machine_id=" + url.QueryEscape(state.MachineID) + "&nonce=" + url.QueryEscape(state.Nonce)
	return map[string]any{"Provider": providerID, "URL": u, "State": string(b), "ExpiresAt": time.Now().Add(3 * time.Minute), "Metadata": map[string]any{}}, nil
}
func pollLogin(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		State          string
		HostCallbackID string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid poll request", 400, "request")
	}
	var state loginState
	if json.Unmarshal([]byte(r.State), &state) != nil || state.Verifier == "" || state.Nonce == "" {
		return nil, fail("invalid_request", "invalid Qoder device state", 400, "request")
	}
	endpoint := pollURL + "?nonce=" + url.QueryEscape(state.Nonce) + "&verifier=" + url.QueryEscape(state.Verifier) + "&challenge_method=S256"
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", endpoint, http.Header{"Accept": {"application/json"}, "User-Agent": {"Go-http-client/2.0"}}, nil, r.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	if resp.StatusCode == 202 || resp.StatusCode == 404 {
		return map[string]any{"Status": "pending", "Message": "waiting for Qoder authorization"}, nil
	}
	if resp.StatusCode != 200 {
		return nil, fail("login_poll_failed", string(resp.Body), resp.StatusCode, "credential")
	}
	var token struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(resp.Body, &token) != nil || token.Token == "" {
		return nil, fail("login_poll_failed", "invalid Qoder token response", 502, "credential")
	}
	s := tokenStorage{Token: token.Token, RefreshToken: token.RefreshToken, UserID: token.UserID, ExpireTime: parseExpiry(token.ExpiresAt, token.ExpiresIn), Type: providerID, LastRefresh: time.Now().Format(time.RFC3339), MachineID: state.MachineID}
	var user httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", userURL, http.Header{"Authorization": {"Bearer " + s.Token}, "Accept": {"application/json"}, "User-Agent": {"Go-http-client/2.0"}}, nil, r.HostCallbackID), &user); err == nil && user.StatusCode == 200 {
		var info struct{ Name, Username, Email string }
		if json.Unmarshal(user.Body, &info) == nil {
			s.Name = first(info.Name, info.Username)
			s.Email = info.Email
		}
	}
	label := first(s.Email, s.UserID, "qoder-user")
	b, _ := json.Marshal(s)
	auth := authData{Provider: providerID, ID: label, FileName: "qoder-" + safe(label) + ".json", Label: label, StorageJSON: b, Metadata: map[string]any{"type": providerID, "email": s.Email, "name": s.Name, "user_id": s.UserID}}
	return map[string]any{"Status": "success", "Auth": auth}, nil
}
func parseAuth(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		Provider, FileName string
		RawJSON            []byte
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid auth parse request", 400, "request")
	}
	if r.Provider != "" && r.Provider != providerID {
		return map[string]any{"Handled": false}, nil
	}
	var s tokenStorage
	if json.Unmarshal(r.RawJSON, &s) != nil || s.Token == "" {
		return map[string]any{"Handled": false}, nil
	}
	s.Type = providerID
	b, _ := json.Marshal(s)
	label := first(s.Email, s.UserID, strings.TrimSuffix(r.FileName, ".json"))
	return map[string]any{"Handled": true, "Auth": authData{Provider: providerID, ID: label, FileName: r.FileName, Label: label, StorageJSON: b, Metadata: map[string]any{"type": providerID}}}, nil
}
func refresh(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		executorRequest
		AuthID, AuthProvider, FileName, Label string
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid refresh request", 400, "request")
	}
	s, e := credentials(r.StorageJSON)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	b, _ := json.Marshal(s)
	return map[string]any{"Auth": authData{Provider: providerID, ID: first(r.AuthID, s.Email, s.UserID), FileName: r.FileName, Label: first(r.Label, s.Email, s.UserID), StorageJSON: b}, "NextRefreshAfter": time.Now().Add(24 * time.Hour)}, nil
}

func discoverModels(raw []byte) (any, *nativeabi.Error) {
	var r executorRequest
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid model request", 400, "request")
	}
	s, e := credentials(r.StorageJSON)
	if e != nil {
		// Match the donor's unauthenticated discovery fallback. Qoder has no
		// static catalog, so an auth-less registration contributes no models.
		return map[string]any{"Provider": providerID, "Models": []any{}}, nil
	}
	h, e := cosyHeaders(nil, modelURL, s)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "identity")
	var resp httpResponse
	if e = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", modelURL, h, nil, r.HostCallbackID), &resp); e != nil {
		return nil, hostFail(e)
	}
	if resp.StatusCode != 200 {
		return nil, fail("model_discovery_failed", string(resp.Body), resp.StatusCode, scope(resp.StatusCode))
	}
	var doc struct {
		Chat []map[string]any `json:"chat"`
	}
	if json.Unmarshal(resp.Body, &doc) != nil {
		return nil, fail("invalid_upstream_response", "invalid Qoder model response", 502, "model")
	}
	configs := map[string]json.RawMessage{}
	var models []any
	now := time.Now().Unix()
	for _, entry := range doc.Chat {
		key := stringAny(entry["key"])
		enabled, ok := entry["enable"].(bool)
		if key == "" || ok && !enabled {
			continue
		}
		raw, _ := json.Marshal(entry)
		configs[key] = raw
		m := map[string]any{"ID": "qoder/" + key, "Object": "model", "Created": now, "OwnedBy": "qoder", "Type": "qoder", "DisplayName": first(stringAny(entry["display_name"]), key), "Description": first(stringAny(entry["display_name"]), key) + " via Qoder", "ContextLength": int64(number(entry["max_input_tokens"]))}
		if vl, _ := entry["is_vl"].(bool); vl {
			m["SupportedInputModalities"] = []string{"TEXT", "IMAGE"}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil, fail("invalid_upstream_response", "Qoder returned no enabled models", 502, "model")
	}
	s.ModelConfigs = configs
	b, _ := json.Marshal(s)
	update := authData{Provider: providerID, ID: first(s.Email, s.UserID), Label: first(s.Email, s.UserID), StorageJSON: b}
	return map[string]any{"Provider": providerID, "Models": models, "AuthUpdate": update}, nil
}
func countTokens(raw []byte) (any, *nativeabi.Error) {
	var r executorRequest
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid token request", 400, "request")
	}
	var chat map[string]any
	if json.Unmarshal(r.Payload, &chat) != nil {
		return nil, fail("invalid_request", "invalid chat payload", 400, "request")
	}
	total := 0
	for _, rawMsg := range sliceAny(chat["messages"]) {
		m, _ := rawMsg.(map[string]any)
		total += len(text(m["content"]))
	}
	n := total / 4
	if n < 1 {
		n = 1
	}
	b, _ := json.Marshal(map[string]any{"usage": map[string]int{"prompt_tokens": n, "completion_tokens": 0, "total_tokens": n}})
	return map[string]any{"Payload": b}, nil
}
func rawHTTP(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		executorRequest
		Method, URL string
		Headers     http.Header
		Body        []byte
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fail("invalid_request", "invalid HTTP request", 400, "request")
	}
	s, e := credentials(r.StorageJSON)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	h, e := cosyHeaders(r.Body, r.URL, s)
	if e != nil {
		return nil, fail("invalid_auth", e.Error(), 401, "credential")
	}
	for k, v := range h {
		if len(r.Headers.Values(k)) == 0 {
			r.Headers[k] = v
		}
	}
	var resp httpResponse
	if e = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq(r.Method, r.URL, r.Headers, r.Body, r.HostCallbackID), &resp); e != nil {
		return nil, hostFail(e)
	}
	return resp, nil
}

func credentials(raw []byte) (tokenStorage, error) {
	var s tokenStorage
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Token == "" || s.UserID == "" {
		return s, fmt.Errorf("Qoder token and user id are required")
	}
	return s, nil
}
func normalizeMessages(in []any) ([]any, string) {
	var out []any
	var systems []string
	for _, v := range in {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if stringAny(m["role"]) == "system" {
			if s := text(m["content"]); s != "" {
				systems = append(systems, s)
			}
			continue
		}
		clone := map[string]any{}
		for k, v := range m {
			clone[k] = v
		}
		clone["content"] = text(m["content"])
		out = append(out, clone)
	}
	return out, strings.Join(systems, "\n\n")
}
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var p []string
	for _, v := range sliceAny(v) {
		m, _ := v.(map[string]any)
		if stringAny(m["type"]) == "text" || m["text"] != nil {
			p = append(p, stringAny(m["text"]))
		}
	}
	return strings.Join(p, "\n")
}
func lastUser(in []any) string {
	for i := len(in) - 1; i >= 0; i-- {
		m, _ := in[i].(map[string]any)
		if stringAny(m["role"]) == "user" {
			return text(m["content"])
		}
	}
	return ""
}
func parseExpiry(s string, seconds int64) int64 {
	if t, e := time.Parse(time.RFC3339, s); e == nil {
		return t.UnixMilli()
	}
	if n, e := strconv.ParseInt(s, 10, 64); e == nil && n > 0 {
		return n
	}
	if seconds <= 0 {
		seconds = 30 * 24 * 3600
	}
	return time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()
}
func stable(parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func stripModel(s string) string { return strings.TrimPrefix(s, "qoder/") }
func chatFormat(s string) bool   { return s == "" || s == "openai" || s == "chat-completions" }
func stringAny(v any) string     { s, _ := v.(string); return s }
func number(v any) float64       { n, _ := v.(float64); return n }
func sliceAny(v any) []any       { s, _ := v.([]any); return s }
func mustJSON(v any) string      { b, _ := json.Marshal(v); return string(b) }
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			return r
		}
		return -1
	}, s)
}
func hostReq(method, url string, h http.Header, body []byte, callback string) map[string]any {
	return map[string]any{"method": method, "url": url, "headers": h, "body": body, "host_callback_id": callback}
}
func closeHost(id string) error {
	return runtime.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}
func fail(code, msg string, status int, scope string) *nativeabi.Error {
	return &nativeabi.Error{Code: code, Message: msg, HTTPStatus: status, Scope: scope, Retryable: status == 408 || status == 429 || status >= 500}
}
func hostFail(e error) *nativeabi.Error {
	if f, ok := e.(*nativeabi.Error); ok {
		return f
	}
	return fail("host_callback_failed", e.Error(), 502, "request")
}
func scope(s int) string {
	if s == 401 || s == 403 {
		return "credential"
	}
	if s == 404 {
		return "model"
	}
	return "request"
}
