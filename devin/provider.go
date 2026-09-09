package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	providerID = "devin"
	chatURL    = "https://server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage"
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
type authData struct {
	Provider, ID, FileName, Label string
	StorageJSON                   []byte
	Metadata                      map[string]any
	Attributes                    map[string]string
}
type storage struct {
	Token string `json:"devin_session_token"`
	Type  string `json:"type,omitempty"`
}
type openAIRequest struct {
	Messages    []message    `json:"messages"`
	Tools       []openAITool `json:"tools"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature *float64     `json:"temperature"`
	TopP        *float64     `json:"top_p"`
}
type message struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID       string                           `json:"id"`
		Function struct{ Name, Arguments string } `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}
type openAITool struct {
	Function struct {
		Name, Description string
		Parameters        json.RawMessage
	} `json:"function"`
}
type devinRequest struct {
	ideName, version, token, attestation, system, cascadeID, model string
	messages                                                       []devinMessage
	tools                                                          []devinTool
	maxTokens                                                      int
	temperature, topP                                              float64
}
type devinMessage struct {
	id, content, toolCallID string
	source                  int
	toolCalls               []toolCall
}
type devinTool struct{ name, description, schema string }
type toolCall struct{ ID, Name, Arguments string }
type usageStats struct{ Input, Output, CacheRead, CacheWrite uint64 }
type responseChunk struct {
	ID, Text, Reasoning string
	Stop                int
	ToolCalls           []toolCall
	Usage               *usageStats
}

func (provider) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		return registration{nativeabi.SchemaVersion, map[string]any{"Name": "Devin", "Version": nativeabi.Version, "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}}, map[string]any{"model_provider": true, "auth_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}}}, nil
	case nativeabi.MethodPluginQuiesce, nativeabi.MethodPluginShutdown:
		return struct{}{}, nil
	case "executor.identifier", "auth.identifier":
		return map[string]string{"identifier": providerID}, nil
	case "auth.parse":
		return parseAuth(raw)
	case "auth.refresh":
		return refresh(raw)
	case "auth.login.start", "auth.login.poll":
		return nil, failure("unsupported", "Devin login is not available; provide a session token auth file", 501, "credential")
	case "model.static":
		return map[string]any{"Provider": providerID, "Models": []any{}}, nil
	case "model.for_auth":
		return staticModels(), nil
	case "executor.execute":
		return execute(raw, false)
	case "executor.execute_stream":
		return execute(raw, true)
	case "executor.count_tokens":
		return nil, failure("unsupported", "Devin has no token-count endpoint", 501, "request")
	case "executor.http_request":
		return rawHTTP(raw)
	default:
		return nil, failure("unknown_method", "unknown method: "+method, 0, "request")
	}
}

func execute(raw []byte, stream bool) (any, *nativeabi.Error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	token, err := credentials(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, failure("invalid_auth", err.Error(), 401, "credential")
	}
	if !chatFormat(req.SourceFormat) || !chatFormat(req.Format) {
		return nil, failure("unsupported_format", "Devin accepts chat-completions payloads", 400, "request")
	}
	upBody, f := buildUpstream(req.Payload, stripModel(req.Model), token)
	if f != nil {
		return nil, f
	}
	headers := http.Header{"Content-Type": {"application/connect+proto"}, "Connect-Protocol-Version": {"1"}, "Authorization": {"Basic " + token + "-" + token}, "User-Agent": {""}, "Accept-Encoding": {"identity"}, "Accept": {"*/*"}}
	if stream {
		if req.StreamID == "" {
			return nil, failure("invalid_request", "stream_id is required", 400, "request")
		}
		var up hostStreamResponse
		if err = runtime.HostCall(nativeabi.MethodHostHTTPDoStream, hostReq("POST", chatURL, headers, upBody, req.HostCallbackID), &up); err != nil {
			return nil, hostFailure(err)
		}
		if up.StatusCode < 200 || up.StatusCode >= 300 {
			_ = closeHost(up.StreamID)
			return nil, failure("upstream_http_error", fmt.Sprintf("Devin returned HTTP %d", up.StatusCode), up.StatusCode, scope(up.StatusCode))
		}
		go forward(req.StreamID, up.StreamID, req.Model)
		return map[string]any{"headers": up.Headers}, nil
	}
	var up httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", chatURL, headers, upBody, req.HostCallbackID), &up); err != nil {
		return nil, hostFailure(err)
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		return nil, failure("upstream_http_error", string(up.Body), up.StatusCode, scope(up.StatusCode))
	}
	chunks, err := parseFrames(up.Body)
	if err != nil {
		return nil, failure("invalid_upstream_response", err.Error(), 502, "request")
	}
	return map[string]any{"Payload": aggregate(chunks, req.Model), "Headers": up.Headers}, nil
}

func buildUpstream(payload []byte, model, token string) ([]byte, *nativeabi.Error) {
	var in openAIRequest
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	if in.Messages == nil {
		return nil, failure("invalid_request", "messages field is required", 400, "request")
	}
	temp, topP := 1.0, .95
	if in.Temperature != nil {
		temp = *in.Temperature
	}
	if in.TopP != nil {
		topP = *in.TopP
	}
	maxTokens := 128000
	if in.MaxTokens > 0 {
		maxTokens = in.MaxTokens
	}
	id := randomID()
	d := devinRequest{ideName: "devin-cli", version: "3000.4.25", token: token, attestation: attestation("cliproxyapi-" + id[:8]), cascadeID: id, model: model, maxTokens: maxTokens, temperature: temp, topP: topP}
	for _, m := range in.Messages {
		content := extractText(m.Content)
		if m.Role == "system" {
			if d.system != "" {
				d.system += "\n"
			}
			d.system += content
			continue
		}
		dm := devinMessage{id: randomID(), content: content, toolCallID: m.ToolCallID}
		switch m.Role {
		case "user":
			dm.source = 1
		case "assistant":
			dm.source = 2
			dm.id = "bot-" + randomID()
		case "tool":
			dm.source = 4
		default:
			continue
		}
		for _, tc := range m.ToolCalls {
			dm.toolCalls = append(dm.toolCalls, toolCall{tc.ID, tc.Function.Name, tc.Function.Arguments})
		}
		d.messages = append(d.messages, dm)
	}
	for _, t := range in.Tools {
		d.tools = append(d.tools, devinTool{t.Function.Name, t.Function.Description, string(t.Function.Parameters)})
	}
	return connectFrame(encodeRequest(d)), nil
}
func extractText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var p []struct{ Type, Text string }
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	var b strings.Builder
	for _, v := range p {
		if v.Type == "text" {
			b.WriteString(v.Text)
		}
	}
	return b.String()
}

func forward(pluginID, hostID, model string) {
	var once sync.Once
	closePlugin := func(f *nativeabi.Error) {
		once.Do(func() {
			_ = runtime.HostCall(nativeabi.MethodHostStreamClose, nativeabi.StreamCloseRequest{StreamID: pluginID, Failure: f}, nil)
		})
	}
	defer func() {
		_ = closeHost(hostID)
		if recover() != nil {
			closePlugin(failure("plugin_panic", "Devin stream processing failed", 500, "request"))
		}
	}()
	var buf []byte
	for {
		var r hostStreamRead
		if err := runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": hostID}, &r); err != nil {
			closePlugin(hostFailure(err))
			return
		}
		if r.Error != "" {
			closePlugin(failure("upstream_stream_error", r.Error, 502, "request"))
			return
		}
		buf = append(buf, r.Payload...)
		for {
			flag, payload, rest, ok, err := nextConnectFrame(buf)
			if err != nil {
				closePlugin(failure("invalid_upstream_response", err.Error(), 502, "request"))
				return
			}
			if !ok {
				break
			}
			buf = rest
			if flag&2 != 0 {
				continue
			}
			c, err := parseResponse(payload)
			if err != nil {
				closePlugin(failure("invalid_upstream_response", err.Error(), 502, "request"))
				return
			}
			out := streamChunk(c, model)
			if len(out) > 0 {
				if err = runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginID, Payload: out}, nil); err != nil {
					closePlugin(hostFailure(err))
					return
				}
			}
		}
		if r.Done {
			if len(buf) != 0 {
				closePlugin(failure("invalid_upstream_response", "truncated Connect frame", 502, "request"))
				return
			}
			closePlugin(nil)
			return
		}
	}
}
func parseFrames(raw []byte) ([]responseChunk, error) {
	var out []responseChunk
	for len(raw) > 0 {
		flag, p, rest, ok, err := nextConnectFrame(raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("truncated Connect frame")
		}
		raw = rest
		if flag&2 != 0 {
			continue
		}
		c, err := parseResponse(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
func streamChunk(c responseChunk, model string) []byte {
	if c.ID == "" && c.Text == "" && c.Reasoning == "" && c.Stop == 0 && len(c.ToolCalls) == 0 && c.Usage == nil {
		return nil
	}
	delta := map[string]any{}
	if c.Text != "" {
		delta["content"] = c.Text
	}
	if c.Reasoning != "" {
		delta["reasoning"] = c.Reasoning
	}
	if len(c.ToolCalls) > 0 {
		var calls []any
		for _, tc := range c.ToolCalls {
			calls = append(calls, map[string]any{"index": 0, "id": tc.ID, "type": "function", "function": map[string]string{"name": tc.Name, "arguments": tc.Arguments}})
		}
		delta["tool_calls"] = calls
	}
	var finish any
	if c.Stop != 0 {
		finish = stopReason(c.Stop)
	}
	v := map[string]any{"id": "chatcmpl-" + first(c.ID, "devin-stream"), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if c.Usage != nil {
		v["usage"] = usage(c.Usage)
	}
	b, _ := json.Marshal(v)
	return b
}
func aggregate(chunks []responseChunk, model string) []byte {
	var id, content, reasoning string
	var stop = "stop"
	var calls []toolCall
	var u *usageStats
	for _, c := range chunks {
		if c.ID != "" {
			id = c.ID
		}
		content += c.Text
		reasoning += c.Reasoning
		if c.Stop != 0 {
			stop = stopReason(c.Stop)
		}
		for _, call := range c.ToolCalls {
			if call.ID != "" && call.Name != "" {
				calls = append(calls, call)
			} else if len(calls) > 0 {
				calls[len(calls)-1].Arguments += call.Arguments
			}
		}
		if c.Usage != nil {
			u = c.Usage
		}
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning"] = reasoning
	}
	if len(calls) > 0 {
		var tc []any
		for _, c := range calls {
			tc = append(tc, map[string]any{"id": c.ID, "type": "function", "function": map[string]string{"name": c.Name, "arguments": c.Arguments}})
		}
		msg["tool_calls"] = tc
	}
	v := map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": stop}}, "usage": usage(u)}
	b, _ := json.Marshal(v)
	return b
}
func usage(u *usageStats) map[string]uint64 {
	if u == nil {
		return map[string]uint64{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	return map[string]uint64{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Input + u.Output, "cache_read_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite}
}
func stopReason(v int) string {
	if v == 2 {
		return "stop"
	}
	if v == 1 || v == 3 || v == 5 {
		return "length"
	}
	return "stop"
}

func parseAuth(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		Provider, FileName string
		RawJSON            []byte
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, failure("invalid_request", "invalid auth parse request", 400, "request")
	}
	if r.Provider != "" && r.Provider != providerID {
		return map[string]any{"Handled": false}, nil
	}
	var object map[string]any
	if json.Unmarshal(r.RawJSON, &object) != nil {
		return map[string]any{"Handled": false}, nil
	}
	token, _ := object["devin_session_token"].(string)
	if token == "" {
		token, _ = object["session_token"].(string)
	}
	if token == "" {
		token, _ = object["api_key"].(string)
	}
	if token == "" {
		return map[string]any{"Handled": false}, nil
	}
	s := storage{normalizeToken(token), providerID}
	b, _ := json.Marshal(s)
	id := first(stringValue(object["email"]), strings.TrimSuffix(r.FileName, ".json"), "devin")
	return map[string]any{"Handled": true, "Auth": authData{Provider: providerID, ID: id, FileName: r.FileName, Label: id, StorageJSON: b, Metadata: map[string]any{"type": providerID}}}, nil
}
func refresh(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		executorRequest
		AuthID, AuthProvider, FileName, Label string
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, failure("invalid_request", "invalid refresh request", 400, "request")
	}
	if _, err := credentials(r.StorageJSON, r.AuthMetadata, r.AuthAttributes); err != nil {
		return nil, failure("invalid_auth", err.Error(), 401, "credential")
	}
	var s storage
	_ = json.Unmarshal(r.StorageJSON, &s)
	b, _ := json.Marshal(s)
	return map[string]any{"Auth": authData{Provider: providerID, ID: first(r.AuthID, "devin"), FileName: r.FileName, Label: first(r.Label, r.AuthID, "Devin"), StorageJSON: b}, "NextRefreshAfter": time.Now().Add(24 * time.Hour)}, nil
}
func rawHTTP(raw []byte) (any, *nativeabi.Error) {
	var r struct {
		executorRequest
		Method, URL string
		Headers     http.Header
		Body        []byte
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	token, err := credentials(r.StorageJSON, r.AuthMetadata, r.AuthAttributes)
	if err != nil {
		return nil, failure("invalid_auth", err.Error(), 401, "credential")
	}
	r.Headers.Set("Authorization", "Basic "+token+"-"+token)
	var resp httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq(r.Method, r.URL, r.Headers, r.Body, r.HostCallbackID), &resp); err != nil {
		return nil, hostFailure(err)
	}
	return resp, nil
}
func staticModels() map[string]any { return map[string]any{"Provider": providerID, "Models": []any{}} }
func credentials(raw []byte, meta map[string]any, attrs map[string]string) (string, error) {
	var s storage
	_ = json.Unmarshal(raw, &s)
	token := s.Token
	for _, key := range []string{"devin_session_token", "session_token", "api_key"} {
		if token == "" {
			token = stringValue(meta[key])
		}
		if token == "" {
			token = attrs[key]
		}
	}
	token = normalizeToken(token)
	if token == "" {
		return "", fmt.Errorf("missing Devin session token")
	}
	return token, nil
}
func normalizeToken(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasPrefix(s, "devin-session-token$") {
		s = "devin-session-token$" + s
	}
	return s
}
func stripModel(s string) string { return strings.TrimPrefix(s, providerID+"/") }
func chatFormat(s string) bool   { return s == "" || s == "openai" || s == "chat-completions" }
func attestation(id string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte("devin-fingerprint" + id))
	v := h.Sum64()
	out := fmt.Sprintf("%016x", v)
	for len(out) < 732 {
		h.Reset()
		_, _ = h.Write([]byte(out))
		out += fmt.Sprintf("%016x", h.Sum64())
	}
	return out[:732]
}
func randomID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func hostReq(method, url string, headers http.Header, body []byte, callback string) map[string]any {
	return map[string]any{"method": method, "url": url, "headers": headers, "body": body, "host_callback_id": callback}
}
func closeHost(id string) error {
	return runtime.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}
func failure(code, msg string, status int, scope string) *nativeabi.Error {
	return &nativeabi.Error{Code: code, Message: msg, HTTPStatus: status, Scope: scope, Retryable: status == 408 || status == 429 || status >= 500}
}
func hostFailure(err error) *nativeabi.Error {
	if f, ok := err.(*nativeabi.Error); ok {
		return f
	}
	return failure("host_callback_failed", err.Error(), 502, "request")
}
func scope(status int) string {
	if status == 401 || status == 403 {
		return "credential"
	}
	if status == 404 {
		return "model"
	}
	return "request"
}
func stringValue(v any) string { s, _ := v.(string); return s }
func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
