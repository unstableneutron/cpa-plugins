package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const providerID = "kilo"
const apiBase = "https://api.kilo.ai"

type provider struct{}
type registration struct {
	SchemaVersion uint32         `json:"schema_version"`
	Metadata      map[string]any `json:"metadata"`
	Capabilities  map[string]any `json:"capabilities"`
}
type executorRequest struct {
	Model                string
	Payload, StorageJSON []byte
	Headers              http.Header
	AuthMetadata         map[string]any
	AuthAttributes       map[string]string
	StreamID             string `json:"stream_id"`
	HostCallbackID       string `json:"host_callback_id"`
}
type storage struct {
	Token          string `json:"kilocodeToken"`
	OrganizationID string `json:"kilocodeOrganizationId"`
	Model          string `json:"kilocodeModel"`
	Email          string `json:"email"`
	Type           string `json:"type"`
}
type httpResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type streamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type streamRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}
type authData struct {
	Provider, ID, FileName, Label string
	StorageJSON                   []byte
	Metadata                      map[string]any
}

func (provider) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration{nativeabi.SchemaVersion, map[string]any{"Name": "Kilo", "Version": nativeabi.Version, "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}}, map[string]any{"model_provider": true, "auth_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}}}, nil
	case "plugin.quiesce", "plugin.shutdown":
		return struct{}{}, nil
	case "executor.identifier", "auth.identifier":
		return map[string]string{"identifier": providerID}, nil
	case "model.static":
		return modelResponse(nil)
	case "model.for_auth":
		return modelResponse(raw)
	case "auth.parse":
		return parseAuth(raw)
	case "auth.login.start":
		return startLogin(raw)
	case "auth.login.poll":
		return pollLogin(raw)
	case "auth.refresh":
		return refresh(raw)
	case "executor.execute":
		return execute(raw, false)
	case "executor.execute_stream":
		return execute(raw, true)
	case "executor.count_tokens":
		return nil, fail("unsupported", "kilo does not provide a token-count endpoint", 501, "request")
	case "executor.http_request":
		return httpRequest(raw)
	default:
		return nil, fail("unknown_method", "unknown method: "+method, 0, "request")
	}
}

func execute(raw []byte, stream bool) (any, *nativeabi.Error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	var body map[string]any
	if err = json.Unmarshal(req.Payload, &body); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	model, suffix := modelSuffix(stripProvider(req.Model))
	body["model"] = model
	applyOpenAIThinking(body, suffix)
	body["stream"] = stream
	payload, _ := json.Marshal(body)
	headers := headersFor(s, stream)
	applyCustomHeaders(headers, req.AuthAttributes, req.Headers)
	if stream {
		if req.StreamID == "" {
			return nil, fail("invalid_request", "stream_id is required", 400, "request")
		}
		var up streamResponse
		if err = runtime.HostCall(nativeabi.MethodHostHTTPDoStream, hostReq("POST", apiBase+"/api/openrouter/chat/completions", headers, payload, req.HostCallbackID), &up); err != nil {
			return nil, hostFail(err)
		}
		if up.StatusCode < 200 || up.StatusCode >= 300 {
			_ = closeHost(up.StreamID)
			return nil, upstreamFailure(fmt.Sprintf("kilo returned HTTP %d", up.StatusCode), up.StatusCode, up.Headers)
		}
		go forward(req.StreamID, up.StreamID)
		return map[string]any{"headers": up.Headers}, nil
	}
	var up httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", apiBase+"/api/openrouter/chat/completions", headers, payload, req.HostCallbackID), &up); err != nil {
		return nil, hostFail(err)
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		return nil, upstreamFailure(string(up.Body), up.StatusCode, up.Headers)
	}
	return map[string]any{"Payload": up.Body, "Headers": up.Headers}, nil
}

func forward(pluginID, hostID string) {
	var streamFailure *nativeabi.Error
	defer func() { _ = recover() }()
	defer func() { _ = closeHost(hostID) }()
	defer func() {
		if recover() != nil {
			streamFailure = fail("plugin_panic", "stream forwarding panic", 0, "request")
		}
		closePlugin(pluginID, streamFailure)
	}()
	for {
		var chunk streamRead
		if err := runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": hostID}, &chunk); err != nil {
			streamFailure = hostFail(err)
			return
		}
		if chunk.Error != "" {
			streamFailure = fail("upstream_stream_error", chunk.Error, 502, "request")
			return
		}
		if len(chunk.Payload) > 0 {
			if err := runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginID, Payload: chunk.Payload}, nil); err != nil {
				streamFailure = hostFail(err)
				return
			}
		}
		if chunk.Done {
			return
		}
	}
}
func closePlugin(id string, f *nativeabi.Error) {
	_ = runtime.HostCall(nativeabi.MethodHostStreamClose, nativeabi.StreamCloseRequest{StreamID: id, Failure: f}, nil)
}
func closeHost(id string) error {
	return runtime.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}
func hostReq(method, url string, headers http.Header, body []byte, callback string) map[string]any {
	return map[string]any{"method": method, "url": url, "headers": headers, "body": body, "host_callback_id": callback}
}
func headersFor(s storage, stream bool) http.Header {
	h := http.Header{"Authorization": {"Bearer " + s.Token}, "Content-Type": {"application/json"}, "User-Agent": {"cli-proxy-kilo"}}
	if s.OrganizationID != "" {
		h.Set("X-Kilocode-OrganizationID", s.OrganizationID)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
	}
	return h
}

func parseAuth(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		Provider, FileName string
		RawJSON            []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	var s storage
	if json.Unmarshal(req.RawJSON, &s) != nil || (s.Type != providerID && req.Provider != providerID) {
		return map[string]any{"Handled": false}, nil
	}
	if s.Token == "" {
		return nil, fail("invalid_auth", "kilo auth is missing kilocodeToken", 401, "credential")
	}
	return map[string]any{"Handled": true, "Auth": makeAuth(s, req.FileName)}, nil
}
func startLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &req)
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", apiBase+"/api/device-auth/codes", http.Header{"Content-Type": {"application/json"}}, nil, req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fail("login_start_failed", string(resp.Body), resp.StatusCode, "request")
	}
	var data struct {
		Code, VerificationURL string
		ExpiresIn             int
	}
	if json.Unmarshal(resp.Body, &data) != nil || data.Code == "" || data.VerificationURL == "" {
		return nil, fail("login_start_failed", "invalid Kilo device-code response", 502, "request")
	}
	return map[string]any{"Provider": providerID, "URL": data.VerificationURL, "State": data.Code, "ExpiresAt": time.Now().Add(time.Duration(data.ExpiresIn) * time.Second)}, nil
}
func pollLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		State          string
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", apiBase+"/api/device-auth/codes/"+req.State, nil, nil, req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fail("login_poll_failed", string(resp.Body), resp.StatusCode, "request")
	}
	var data struct{ Status, Token, UserEmail string }
	if json.Unmarshal(resp.Body, &data) != nil {
		return nil, fail("login_poll_failed", "invalid Kilo poll response", 502, "request")
	}
	switch data.Status {
	case "pending":
		return map[string]any{"Status": "pending"}, nil
	case "denied", "expired":
		return nil, fail("login_denied", "Kilo device flow "+data.Status, 401, "credential")
	case "approved":
		if data.Token == "" {
			return nil, fail("login_poll_failed", "approved response omitted token", 502, "credential")
		}
		s := storage{Token: data.Token, Email: data.UserEmail, Type: providerID}
		return map[string]any{"Status": "success", "Auth": makeAuth(s, "kilo-"+safe(data.UserEmail)+".json")}, nil
	default:
		return nil, fail("login_poll_failed", "unknown Kilo device status: "+data.Status, 502, "request")
	}
}
func refresh(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		StorageJSON []byte
		Metadata    map[string]any
		Attributes  map[string]string
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	return map[string]any{"Auth": makeAuth(s, "")}, nil
}
func httpRequest(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		executorRequest
		Method, URL string
		Headers     http.Header
		Body        []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	for k, v := range headersFor(s, false) {
		if len(req.Headers.Values(k)) == 0 {
			req.Headers[k] = v
		}
	}
	var resp httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq(req.Method, req.URL, req.Headers, req.Body, req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	return resp, nil
}

func modelResponse(raw []byte) (any, *nativeabi.Error) {
	models := []map[string]any{staticModel()}
	if len(raw) > 0 {
		var req executorRequest
		_ = json.Unmarshal(raw, &req)
		if s, err := credentials(req.StorageJSON, req.AuthMetadata, req.AuthAttributes); err == nil {
			var resp httpResponse
			if runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", apiBase+"/api/openrouter/models", headersFor(s, false), nil, req.HostCallbackID), &resp) == nil && resp.StatusCode == 200 {
				var root struct {
					Data []struct {
						ID, Name       string
						PreferredIndex int   `json:"preferredIndex"`
						IsFree         bool  `json:"is_free"`
						ContextLength  int64 `json:"context_length"`
						Pricing        struct{ Prompt string }
					}
				}
				if json.Unmarshal(resp.Body, &root) == nil {
					for _, m := range root.Data {
						if m.PreferredIndex > 0 && (m.IsFree || strings.HasSuffix(m.ID, ":free") || m.ID == "giga-potato" || m.Pricing.Prompt == "0" || m.Pricing.Prompt == "0.0") {
							models = append(models, map[string]any{"ID": m.ID, "Object": "model", "OwnedBy": "kilo", "Type": "kilo", "DisplayName": m.Name, "ContextLength": m.ContextLength, "SupportedGenerationMethods": []string{"chat"}})
						}
					}
				}
			}
		}
	}
	return map[string]any{"Provider": providerID, "Models": models}, nil
}
func staticModel() map[string]any {
	return map[string]any{"ID": "kilo/auto", "Object": "model", "Created": int64(1732752000), "OwnedBy": "kilo", "Type": "kilo", "DisplayName": "Kilo Auto", "Description": "Automatic model selection by Kilo", "ContextLength": int64(200000), "MaxCompletionTokens": int64(64000), "SupportedGenerationMethods": []string{"chat"}, "Thinking": map[string]any{"Min": 1024, "Max": 32000, "ZeroAllowed": true, "DynamicAllowed": true}}
}
func credentials(raw []byte, meta map[string]any, attrs map[string]string) (storage, error) {
	var s storage
	_ = json.Unmarshal(raw, &s)
	if s.Token == "" {
		s.Token = stringAny(meta, "kilocodeToken", "access_token")
		if s.Token == "" {
			s.Token = first(attrs["kilocodeToken"], attrs["access_token"])
		}
	}
	if s.OrganizationID == "" {
		s.OrganizationID = stringAny(meta, "kilocodeOrganizationId", "organization_id")
		if s.OrganizationID == "" {
			s.OrganizationID = first(attrs["kilocodeOrganizationId"], attrs["organization_id"])
		}
	}
	if s.Token == "" {
		return s, fmt.Errorf("missing Kilo access token")
	}
	return s, nil
}
func makeAuth(s storage, file string) authData {
	s.Type = providerID
	raw, _ := json.Marshal(s)
	return authData{Provider: providerID, ID: first(s.Email, "kilo"), FileName: file, Label: first(s.Email, "Kilo"), StorageJSON: raw, Metadata: map[string]any{"type": providerID}}
}
func stringAny(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
func stripProvider(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
func modelSuffix(model string) (string, string) {
	open := strings.LastIndex(model, "(")
	if open < 0 || !strings.HasSuffix(model, ")") {
		return model, ""
	}
	return model[:open], model[open+1 : len(model)-1]
}
func applyOpenAIThinking(body map[string]any, suffix string) {
	effort := strings.ToLower(strings.TrimSpace(suffix))
	if budget, err := strconv.Atoi(effort); err == nil && budget >= 0 {
		switch {
		case budget == 0:
			effort = "none"
		case budget <= 512:
			effort = "minimal"
		case budget <= 1024:
			effort = "low"
		case budget <= 8192:
			effort = "medium"
		case budget <= 24576:
			effort = "high"
		default:
			effort = "xhigh"
		}
	}
	switch effort {
	case "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
		body["reasoning_effort"] = effort
	}
}
func applyCustomHeaders(headers http.Header, attributes map[string]string, clientHeaders http.Header) {
	for key, value := range attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "$") {
			value = clientHeaders.Get(strings.TrimSpace(strings.TrimPrefix(value, "$")))
		}
		if name != "" && value != "" {
			headers.Set(name, value)
		}
	}
}
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			return r
		}
		return -1
	}, s)
}
func fail(code, msg string, status int, scope string) *nativeabi.Error {
	return &nativeabi.Error{Code: code, Message: msg, HTTPStatus: status, Scope: scope, Retryable: status == 408 || status == 429 || status >= 500}
}
func hostFail(err error) *nativeabi.Error {
	if f, ok := err.(*nativeabi.Error); ok {
		return f
	}
	return fail("host_callback_failed", err.Error(), 502, "request")
}
func statusScope(s int) string {
	if s == 401 || s == 403 {
		return "credential"
	}
	if s == 404 {
		return "model"
	}
	return ""
}
func upstreamFailure(message string, status int, headers http.Header) *nativeabi.Error {
	f := fail("upstream_http_error", message, status, statusScope(status))
	f.RetryAfterMS = retryAfterMS(headers)
	return f
}
func retryAfterMS(headers http.Header) *int64 {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		milliseconds := seconds * 1000
		return &milliseconds
	}
	if at, err := http.ParseTime(value); err == nil {
		milliseconds := time.Until(at).Milliseconds()
		if milliseconds < 0 {
			milliseconds = 0
		}
		return &milliseconds
	}
	return nil
}
