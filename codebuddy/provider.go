package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	providerID    = "codebuddy"
	baseURL       = "https://copilot.tencent.com"
	defaultDomain = "www.codebuddy.cn"
	userAgent     = "CLI/2.63.2 CodeBuddy/2.63.2"
)

type provider struct{}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}
type metadata struct {
	Name, Version, Author, GitHubRepository string
	ConfigFields                            []any
}
type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}
type executorRequest struct {
	Model          string
	Payload        []byte
	StorageJSON    []byte
	AuthMetadata   map[string]any
	AuthAttributes map[string]string
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type authData struct {
	Provider, ID, FileName, Label string
	StorageJSON                   []byte
	Metadata                      map[string]any
	NextRefreshAfter              time.Time
}
type tokenStorage struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in,omitempty"`
	TokenType        string `json:"token_type"`
	Domain           string `json:"domain"`
	UserID           string `json:"user_id"`
	Type             string `json:"type"`
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

func (provider) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration{nativeabi.SchemaVersion, metadata{"CodeBuddy", nativeabi.Version, "unstableneutron", "https://github.com/unstableneutron/cpa-plugins", []any{}}, capabilities{true, true, true, "oauth", []string{"chat-completions"}, []string{"chat-completions"}}}, nil
	case "plugin.quiesce", "plugin.shutdown":
		return struct{}{}, nil
	case "executor.identifier", "auth.identifier":
		return map[string]string{"identifier": providerID}, nil
	case "model.static":
		return map[string]any{"Provider": providerID, "Models": models()}, nil
	case "model.for_auth":
		return map[string]any{"Provider": providerID, "Models": models()}, nil
	case "auth.parse":
		return parseAuth(raw)
	case "auth.login.start":
		return startLogin(raw)
	case "auth.login.poll":
		return pollLogin(raw)
	case "auth.refresh":
		return refreshAuth(raw)
	case "executor.execute":
		return execute(raw, false)
	case "executor.execute_stream":
		return execute(raw, true)
	case "executor.count_tokens":
		return nil, failure("unsupported", "codebuddy does not provide a token-count endpoint", http.StatusNotImplemented, "request")
	case "executor.http_request":
		return rawHTTPRequest(raw)
	default:
		return nil, failure("unknown_method", "unknown method: "+method, 0, "request")
	}
}

func execute(raw []byte, stream bool) (any, *nativeabi.Error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), http.StatusBadRequest, "request")
	}
	storage, err := decodeStorage(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, failure("invalid_auth", err.Error(), http.StatusUnauthorized, "credential")
	}
	var body map[string]any
	if err = json.Unmarshal(req.Payload, &body); err != nil {
		return nil, failure("invalid_request", "decode chat-completions payload: "+err.Error(), http.StatusBadRequest, "request")
	}
	body["model"] = stripPrefix(req.Model)
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	payload, _ := json.Marshal(body)
	headers := chatHeaders(storage)
	if stream {
		if strings.TrimSpace(req.StreamID) == "" {
			return nil, failure("invalid_request", "stream_id is required", http.StatusBadRequest, "request")
		}
		var upstream hostStreamResponse
		err = runtime.HostCall(nativeabi.MethodHostHTTPDoStream, hostHTTPRequest(http.MethodPost, baseURL+"/v2/chat/completions", headers, payload, req.HostCallbackID), &upstream)
		if err != nil {
			return nil, hostFailure(err)
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			_ = closeHostStream(upstream.StreamID)
			return nil, failure("upstream_http_error", fmt.Sprintf("codebuddy returned HTTP %d", upstream.StatusCode), upstream.StatusCode, scopeForStatus(upstream.StatusCode))
		}
		go forwardStream(req.StreamID, upstream.StreamID)
		return map[string]any{"headers": upstream.Headers}, nil
	}
	var upstream httpResponse
	err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostHTTPRequest(http.MethodPost, baseURL+"/v2/chat/completions", headers, payload, req.HostCallbackID), &upstream)
	if err != nil {
		return nil, hostFailure(err)
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		return nil, failure("upstream_http_error", string(upstream.Body), upstream.StatusCode, scopeForStatus(upstream.StatusCode))
	}
	aggregated, err := aggregateSSE(upstream.Body)
	if err != nil {
		return nil, failure("invalid_upstream_response", err.Error(), http.StatusBadGateway, "request")
	}
	return map[string]any{"Payload": aggregated, "Headers": upstream.Headers}, nil
}

func forwardStream(pluginStreamID, hostStreamID string) {
	defer func() { _ = closeHostStream(hostStreamID) }()
	for {
		var chunk hostStreamRead
		if err := runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": hostStreamID}, &chunk); err != nil {
			closePluginStream(pluginStreamID, hostFailure(err))
			return
		}
		if chunk.Error != "" {
			closePluginStream(pluginStreamID, failure("upstream_stream_error", chunk.Error, http.StatusBadGateway, "request"))
			return
		}
		if len(chunk.Payload) > 0 {
			if err := runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginStreamID, Payload: chunk.Payload}, nil); err != nil {
				closePluginStream(pluginStreamID, hostFailure(err))
				return
			}
		}
		if chunk.Done {
			closePluginStream(pluginStreamID, nil)
			return
		}
	}
}

func closePluginStream(id string, f *nativeabi.Error) {
	_ = runtime.HostCall(nativeabi.MethodHostStreamClose, nativeabi.StreamCloseRequest{StreamID: id, Failure: f}, nil)
}
func closeHostStream(id string) error {
	return runtime.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}

func hostHTTPRequest(method, endpoint string, headers http.Header, body []byte, callbackID string) map[string]any {
	return map[string]any{"method": method, "url": endpoint, "headers": headers, "body": body, "host_callback_id": callbackID}
}

func chatHeaders(s tokenStorage) http.Header {
	domain := s.Domain
	if domain == "" {
		domain = defaultDomain
	}
	return http.Header{"Authorization": {"Bearer " + s.AccessToken}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}, "User-Agent": {userAgent}, "X-User-Id": {s.UserID}, "X-Domain": {domain}, "X-Product": {"SaaS"}, "X-IDE-Type": {"CLI"}, "X-IDE-Name": {"CLI"}, "X-IDE-Version": {"2.63.2"}, "X-Requested-With": {"XMLHttpRequest"}}
}

func rawHTTPRequest(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		executorRequest
		Method, URL string
		Headers     http.Header
		Body        []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	s, err := decodeStorage(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, failure("invalid_auth", err.Error(), 401, "credential")
	}
	for k, values := range chatHeaders(s) {
		if len(req.Headers.Values(k)) == 0 {
			req.Headers[k] = values
		}
	}
	var resp httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostHTTPRequest(req.Method, req.URL, req.Headers, req.Body, req.HostCallbackID), &resp); err != nil {
		return nil, hostFailure(err)
	}
	return resp, nil
}

func parseAuth(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		Provider, Path, FileName string
		RawJSON                  []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	var s tokenStorage
	if err := json.Unmarshal(req.RawJSON, &s); err != nil || (s.Type != providerID && req.Provider != providerID) {
		return map[string]any{"Handled": false}, nil
	}
	if s.AccessToken == "" {
		return nil, failure("invalid_auth", "codebuddy auth is missing access_token", 401, "credential")
	}
	return map[string]any{"Handled": true, "Auth": makeAuth(s, req.FileName)}, nil
}

func startLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &req)
	headers := noAuthHeaders()
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostHTTPRequest(http.MethodPost, baseURL+"/v2/plugin/auth/state?platform=CLI", headers, []byte("{}"), req.HostCallbackID), &resp); err != nil {
		return nil, hostFailure(err)
	}
	if resp.StatusCode != 200 {
		return nil, failure("login_start_failed", string(resp.Body), resp.StatusCode, "request")
	}
	var result struct {
		Code int
		Msg  string
		Data *struct {
			State   string
			AuthURL string `json:"authUrl"`
		}
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil || result.Code != 0 || result.Data == nil || result.Data.State == "" || result.Data.AuthURL == "" {
		return nil, failure("login_start_failed", "invalid CodeBuddy auth-state response", 502, "request")
	}
	return map[string]any{"Provider": providerID, "URL": result.Data.AuthURL, "State": result.Data.State, "ExpiresAt": time.Now().Add(5 * time.Minute), "Metadata": map[string]any{}}, nil
}

func pollLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		State, HostCallbackID string
		Metadata              map[string]any
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	var resp httpResponse
	endpoint := baseURL + "/v2/plugin/auth/token?state=" + url.QueryEscape(req.State)
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostHTTPRequest(http.MethodGet, endpoint, noAuthHeaders(), nil, req.HostCallbackID), &resp); err != nil {
		return nil, hostFailure(err)
	}
	if resp.StatusCode != 200 {
		return nil, failure("login_poll_failed", string(resp.Body), resp.StatusCode, "request")
	}
	var result struct {
		Code int
		Msg  string
		Data *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			TokenType    string `json:"tokenType"`
			Domain       string
		}
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, failure("login_poll_failed", err.Error(), 502, "request")
	}
	if result.Code == 11217 {
		return map[string]any{"Status": "pending", "Message": result.Msg}, nil
	}
	if result.Code != 0 || result.Data == nil || result.Data.AccessToken == "" {
		return nil, failure("login_poll_failed", result.Msg, 401, "credential")
	}
	s := tokenStorage{AccessToken: result.Data.AccessToken, RefreshToken: result.Data.RefreshToken, ExpiresIn: result.Data.ExpiresIn, TokenType: result.Data.TokenType, Domain: result.Data.Domain, UserID: jwtSubject(result.Data.AccessToken), Type: providerID}
	return map[string]any{"Status": "success", "Message": "CodeBuddy login complete", "Auth": makeAuth(s, "codebuddy-"+safeName(s.UserID)+".json")}, nil
}

func refreshAuth(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		StorageJSON    []byte
		Metadata       map[string]any
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, failure("invalid_request", err.Error(), 400, "request")
	}
	s, err := decodeStorage(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, failure("invalid_auth", err.Error(), 401, "credential")
	}
	if s.RefreshToken == "" {
		return map[string]any{"Auth": makeAuth(s, ""), "NextRefreshAfter": time.Now().Add(time.Hour)}, nil
	}
	domain := s.Domain
	if domain == "" {
		domain = defaultDomain
	}
	headers := noAuthHeaders()
	headers.Set("Authorization", "Bearer "+s.AccessToken)
	headers.Set("X-Refresh-Token", s.RefreshToken)
	headers.Set("X-Auth-Refresh-Source", "plugin")
	headers.Set("X-User-Id", s.UserID)
	headers.Set("X-Domain", domain)
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostHTTPRequest(http.MethodPost, baseURL+"/v2/plugin/auth/token/refresh", headers, []byte("{}"), req.HostCallbackID), &resp); err != nil {
		return nil, hostFailure(err)
	}
	if resp.StatusCode != 200 {
		return nil, failure("token_refresh_failed", string(resp.Body), resp.StatusCode, "credential")
	}
	var result struct {
		Code int
		Msg  string
		Data *struct {
			AccessToken      string `json:"accessToken"`
			RefreshToken     string `json:"refreshToken"`
			ExpiresIn        int64  `json:"expiresIn"`
			RefreshExpiresIn int64  `json:"refreshExpiresIn"`
			TokenType        string `json:"tokenType"`
			Domain           string
		}
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil || result.Code != 0 || result.Data == nil || result.Data.AccessToken == "" {
		return nil, failure("token_refresh_failed", "invalid CodeBuddy refresh response", 502, "credential")
	}
	s.AccessToken = result.Data.AccessToken
	if result.Data.RefreshToken != "" {
		s.RefreshToken = result.Data.RefreshToken
	}
	s.ExpiresIn = result.Data.ExpiresIn
	s.RefreshExpiresIn = result.Data.RefreshExpiresIn
	s.TokenType = result.Data.TokenType
	if result.Data.Domain != "" {
		s.Domain = result.Data.Domain
	}
	if id := jwtSubject(s.AccessToken); id != "" {
		s.UserID = id
	}
	return map[string]any{"Auth": makeAuth(s, ""), "NextRefreshAfter": time.Now().Add(time.Duration(max(60, s.ExpiresIn-300)) * time.Second)}, nil
}

func noAuthHeaders() http.Header {
	return http.Header{"Accept": {"application/json, text/plain, */*"}, "Content-Type": {"application/json"}, "User-Agent": {userAgent}, "X-Requested-With": {"XMLHttpRequest"}, "X-No-Authorization": {"true"}, "X-No-User-Id": {"true"}, "X-No-Enterprise-Id": {"true"}, "X-No-Department-Info": {"true"}, "X-Product": {"SaaS"}, "X-Request-ID": {randomHex(16)}}
}
func makeAuth(s tokenStorage, file string) authData {
	raw, _ := json.Marshal(s)
	return authData{Provider: providerID, ID: firstNonempty(s.UserID, "codebuddy"), FileName: file, Label: firstNonempty(s.UserID, "CodeBuddy"), StorageJSON: raw, Metadata: map[string]any{"type": providerID}, NextRefreshAfter: time.Now().Add(time.Duration(max(60, s.ExpiresIn-300)) * time.Second)}
}
func decodeStorage(raw []byte, metadata map[string]any) (tokenStorage, error) {
	var s tokenStorage
	_ = json.Unmarshal(raw, &s)
	if s.AccessToken == "" {
		s.AccessToken, _ = metadata["access_token"].(string)
		s.RefreshToken, _ = metadata["refresh_token"].(string)
		s.UserID, _ = metadata["user_id"].(string)
		s.Domain, _ = metadata["domain"].(string)
	}
	if s.AccessToken == "" {
		return s, fmt.Errorf("missing access token")
	}
	return s, nil
}
func jwtSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(data, &claims)
	return claims.Sub
}
func stripPrefix(model string) string {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		return model[i+1:]
	}
	return model
}
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			return r
		}
		return -1
	}, s)
}
func randomHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func failure(code, msg string, status int, scope string) *nativeabi.Error {
	return &nativeabi.Error{Code: code, Message: msg, HTTPStatus: status, Retryable: status == 408 || status == 429 || status >= 500, Scope: scope}
}
func hostFailure(err error) *nativeabi.Error {
	if f, ok := err.(*nativeabi.Error); ok {
		return f
	}
	return failure("host_callback_failed", err.Error(), 502, "request")
}
func scopeForStatus(status int) string {
	if status == 401 || status == 403 {
		return "credential"
	}
	if status == 404 {
		return "model"
	}
	return "request"
}

func aggregateSSE(raw []byte) ([]byte, error) {
	var id, model string
	var created int64
	var content, reasoning strings.Builder
	var finish any
	var usage any
	toolCalls := map[int]map[string]any{}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal(data, &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok {
			id = v
		}
		if v, ok := chunk["model"].(string); ok {
			model = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v := chunk["usage"]; v != nil {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice["finish_reason"] != nil {
				finish = choice["finish_reason"]
			}
			delta, _ := choice["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				content.WriteString(v)
			}
			if v, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(v)
			}
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				idx := int(number(call["index"]))
				acc := toolCalls[idx]
				if acc == nil {
					acc = map[string]any{"index": idx, "type": "function", "function": map[string]any{"name": "", "arguments": ""}}
					toolCalls[idx] = acc
				}
				if v, ok := call["id"].(string); ok && v != "" {
					acc["id"] = v
				}
				fn, _ := call["function"].(map[string]any)
				afn := acc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					afn["name"] = afn["name"].(string) + v
				}
				if v, ok := fn["arguments"].(string); ok {
					afn["arguments"] = afn["arguments"].(string) + v
				}
			}
		}
	}
	if id == "" && content.Len() == 0 && len(toolCalls) == 0 {
		return nil, fmt.Errorf("CodeBuddy stream contained no completion chunks")
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		ordered := make([]any, 0, len(toolCalls))
		for i := 0; i < len(toolCalls); i++ {
			if tc := toolCalls[i]; tc != nil {
				delete(tc, "index")
				ordered = append(ordered, tc)
			}
		}
		message["tool_calls"] = ordered
	}
	result := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}
func number(v any) float64 { n, _ := v.(float64); return n }

func models() []map[string]any {
	definitions := []struct {
		id, display string
		context     int64
	}{
		{"auto", "Auto", 128000},
		{"glm-5v-turbo", "GLM-5v Turbo", 200000},
		{"glm-5.1", "GLM-5.1", 200000},
		{"glm-5.0-turbo", "GLM-5.0 Turbo", 200000},
		{"glm-5.0", "GLM-5.0", 200000},
		{"glm-4.7", "GLM-4.7", 200000},
		{"minimax-m2.7", "MiniMax M2.7", 200000},
		{"minimax-m2.5", "MiniMax M2.5", 200000},
		{"kimi-k2.5", "Kimi K2.5", 256000},
		{"kimi-k2.6", "Kimi K2.6", 256000},
		{"kimi-k2-thinking", "Kimi K2 Thinking", 256000},
		{"deepseek-v3-2-volc", "DeepSeek V3.2 (Volc)", 128000},
		{"hy3-preview", "Hy3 Preview", 128000},
	}
	result := make([]map[string]any, 0, len(definitions))
	for _, model := range definitions {
		result = append(result, map[string]any{"ID": model.id, "Object": "model", "Created": int64(1748044800), "OwnedBy": "tencent", "Type": providerID, "DisplayName": model.display, "ContextLength": model.context, "MaxCompletionTokens": int64(32768), "SupportedGenerationMethods": []string{"chat"}})
	}
	return result
}
