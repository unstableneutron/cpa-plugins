package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const providerID = "github-copilot"
const (
	githubDeviceURL = "https://github.com/login/device/code"
	githubTokenURL  = "https://github.com/login/oauth/access_token"
	githubUserURL   = "https://api.github.com/user"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotBaseURL  = "https://api.githubcopilot.com"
	clientID        = "Iv1.b507a08c87ecfe98"
)

type provider struct{}
type registration struct {
	SchemaVersion          uint32 `json:"schema_version"`
	Metadata, Capabilities map[string]any
}
type request struct {
	Model, Format, SourceFormat string
	Payload, StorageJSON        []byte
	AuthMetadata                map[string]any
	AuthAttributes              map[string]string
	StreamID                    string `json:"stream_id"`
	HostCallbackID              string `json:"host_callback_id"`
}
type storage struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type"`
}
type authData struct {
	Provider, ID, FileName, Label string
	StorageJSON                   []byte
	Metadata                      map[string]any
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
type apiToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
}

func (provider) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration{nativeabi.SchemaVersion, map[string]any{"Name": "GitHub Copilot", "Version": nativeabi.Version, "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}}, map[string]any{"model_provider": true, "auth_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions", "responses", "claude"}, "executor_output_formats": []string{"chat-completions", "responses", "claude"}}}, nil
	case "plugin.quiesce", "plugin.shutdown":
		return struct{}{}, nil
	case "executor.identifier", "auth.identifier":
		return map[string]string{"identifier": providerID}, nil
	case "auth.parse":
		return parseAuth(raw)
	case "auth.login.start":
		return startLogin(raw)
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
		return nil, fail("unsupported", "GitHub Copilot has no token-count endpoint", 501, "request")
	case "executor.http_request":
		return rawHTTP(raw)
	default:
		return nil, fail("unknown_method", "unknown method: "+method, 0, "request")
	}
}

func execute(raw []byte, stream bool) (any, *nativeabi.Error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	token, endpoint, f := exchangeToken(s.AccessToken, req.HostCallbackID)
	if f != nil {
		return nil, f
	}
	format := first(req.SourceFormat, req.Format, "chat-completions")
	path := "/chat/completions"
	if format == "responses" || format == "openai-response" {
		path = "/responses"
	} else if format == "claude" {
		path = "/v1/messages"
	}
	var body map[string]any
	if err = json.Unmarshal(req.Payload, &body); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	body["model"] = strip(req.Model)
	body["stream"] = stream
	if stream && path == "/chat/completions" {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	payload, _ := json.Marshal(body)
	headers := copilotHeaders(token, payload, stream)
	if hasVision(body) {
		headers.Set("Copilot-Vision-Request", "true")
	}
	if stream {
		if req.StreamID == "" {
			return nil, fail("invalid_request", "stream_id is required", 400, "request")
		}
		var up streamResponse
		if err = runtime.HostCall(nativeabi.MethodHostHTTPDoStream, hostReq("POST", endpoint+path, headers, payload, req.HostCallbackID), &up); err != nil {
			return nil, hostFail(err)
		}
		if up.StatusCode < 200 || up.StatusCode >= 300 {
			_ = closeHost(up.StreamID)
			return nil, fail("upstream_http_error", fmt.Sprintf("GitHub Copilot returned HTTP %d", up.StatusCode), up.StatusCode, statusScope(up.StatusCode))
		}
		go forward(req.StreamID, up.StreamID)
		return map[string]any{"headers": up.Headers}, nil
	}
	var up httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", endpoint+path, headers, payload, req.HostCallbackID), &up); err != nil {
		return nil, hostFail(err)
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		return nil, fail("upstream_http_error", string(up.Body), up.StatusCode, statusScope(up.StatusCode))
	}
	return map[string]any{"Payload": up.Body, "Headers": up.Headers}, nil
}

func exchangeToken(githubToken, callback string) (string, string, *nativeabi.Error) {
	headers := http.Header{"Authorization": {"token " + githubToken}, "Accept": {"application/json"}, "User-Agent": {"GithubCopilot/1.0"}, "Editor-Version": {"vscode/1.100.0"}, "Editor-Plugin-Version": {"copilot/1.300.0"}}
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", copilotTokenURL, headers, nil, callback), &resp); err != nil {
		return "", "", hostFail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fail("token_exchange_failed", string(resp.Body), resp.StatusCode, "credential")
	}
	var token apiToken
	if json.Unmarshal(resp.Body, &token) != nil || token.Token == "" {
		return "", "", fail("token_exchange_failed", "invalid Copilot API token response", 502, "credential")
	}
	endpoint := copilotBaseURL
	if trustedEndpoint(token.Endpoints.API) {
		endpoint = strings.TrimRight(token.Endpoints.API, "/")
	}
	return token.Token, endpoint, nil
}
func trustedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.githubcopilot.com", "api.individual.githubcopilot.com", "api.business.githubcopilot.com", "copilot-proxy.githubusercontent.com":
		return true
	}
	return false
}
func copilotHeaders(token string, body []byte, stream bool) http.Header {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}, "Accept": {accept}, "User-Agent": {"GitHubCopilotChat/0.35.0"}, "Editor-Version": {"vscode/1.107.0"}, "Editor-Plugin-Version": {"copilot-chat/0.35.0"}, "Openai-Intent": {"conversation-edits"}, "Copilot-Integration-Id": {"vscode-chat"}, "X-Github-Api-Version": {"2025-04-01"}, "X-Request-Id": {randomID()}, "X-Initiator": {initiator(body)}}
}
func initiator(payload []byte) string {
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil {
		return "user"
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return "user"
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	role, _ := last["role"].(string)
	if role == "assistant" || role == "tool" {
		return "agent"
	}
	if role == "user" {
		parts, _ := last["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if part["type"] == "tool_result" {
				return "agent"
			}
		}
	}
	return "user"
}
func hasVision(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		parts, _ := msg["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if part["type"] == "image_url" || part["type"] == "input_image" || part["type"] == "image" {
				return true
			}
		}
	}
	return false
}

func startLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &req)
	form := url.Values{"client_id": {clientID}, "scope": {"read:user user:email"}}
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", githubDeviceURL, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}}, []byte(form.Encode()), req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fail("login_start_failed", string(resp.Body), resp.StatusCode, "request")
	}
	var d struct {
		DeviceCode          string `json:"device_code"`
		UserCode            string `json:"user_code"`
		VerificationURI     string `json:"verification_uri"`
		ExpiresIn, Interval int
	}
	if json.Unmarshal(resp.Body, &d) != nil || d.DeviceCode == "" {
		return nil, fail("login_start_failed", "invalid GitHub device-code response", 502, "request")
	}
	stateRaw, _ := json.Marshal(map[string]any{"device_code": d.DeviceCode, "interval": max(5, d.Interval), "user_code": d.UserCode})
	return map[string]any{"Provider": providerID, "URL": d.VerificationURI, "State": string(stateRaw), "ExpiresAt": time.Now().Add(time.Duration(d.ExpiresIn) * time.Second), "Metadata": map[string]any{"user_code": d.UserCode}}, nil
}
func pollLogin(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		State          string
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	var state struct {
		DeviceCode string `json:"device_code"`
	}
	if json.Unmarshal([]byte(req.State), &state) != nil || state.DeviceCode == "" {
		return nil, fail("invalid_request", "invalid GitHub device state", 400, "request")
	}
	form := url.Values{"client_id": {clientID}, "device_code": {state.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	var resp httpResponse
	if err := runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("POST", githubTokenURL, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}}, []byte(form.Encode()), req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	var token struct {
		Error, ErrorDescription string
		AccessToken             string `json:"access_token"`
		TokenType               string `json:"token_type"`
		Scope                   string
	}
	if json.Unmarshal(resp.Body, &token) != nil {
		return nil, fail("login_poll_failed", "invalid GitHub token response", 502, "request")
	}
	switch token.Error {
	case "authorization_pending", "slow_down":
		return map[string]any{"Status": "pending", "Message": token.Error}, nil
	case "expired_token", "access_denied":
		return nil, fail("login_denied", first(token.ErrorDescription, token.Error), 401, "credential")
	}
	if token.AccessToken == "" {
		return nil, fail("login_poll_failed", first(token.ErrorDescription, "GitHub omitted access token"), 502, "credential")
	}
	user := fetchUser(token.AccessToken, req.HostCallbackID)
	s := storage{AccessToken: token.AccessToken, TokenType: token.TokenType, Scope: token.Scope, Username: first(user.Login, "github-user"), Email: user.Email, Name: user.Name, Type: providerID}
	return map[string]any{"Status": "success", "Auth": makeAuth(s, "github-copilot-"+safe(s.Username)+".json")}, nil
}

type githubUser struct{ Login, Email, Name string }

func fetchUser(token, callback string) githubUser {
	var resp httpResponse
	_ = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", githubUserURL, http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/json"}, "User-Agent": {"CLIProxyAPI"}}, nil, callback), &resp)
	var user githubUser
	if resp.StatusCode == 200 {
		_ = json.Unmarshal(resp.Body, &user)
	}
	return user
}
func refresh(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		StorageJSON    []byte
		Metadata       map[string]any
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	if _, _, f := exchangeToken(s.AccessToken, req.HostCallbackID); f != nil {
		return nil, f
	}
	return map[string]any{"Auth": makeAuth(s, "")}, nil
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
	if s.AccessToken == "" {
		return nil, fail("invalid_auth", "GitHub Copilot auth is missing access_token", 401, "credential")
	}
	return map[string]any{"Handled": true, "Auth": makeAuth(s, req.FileName)}, nil
}
func discoverModels(raw []byte) (any, *nativeabi.Error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return map[string]any{"Provider": providerID, "Models": []any{}}, nil
	}
	token, endpoint, f := exchangeToken(s.AccessToken, req.HostCallbackID)
	if f != nil {
		return nil, f
	}
	var resp httpResponse
	if err = runtime.HostCall(nativeabi.MethodHostHTTPDo, hostReq("GET", endpoint+"/models", copilotHeaders(token, nil, false), nil, req.HostCallbackID), &resp); err != nil {
		return nil, hostFail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fail("model_discovery_failed", string(resp.Body), resp.StatusCode, statusScope(resp.StatusCode))
	}
	var root struct {
		Data []struct {
			ID, Object, OwnedBy, Name, Version string
			Created                            int64
			Capabilities                       map[string]any
		}
	}
	if json.Unmarshal(resp.Body, &root) != nil {
		return nil, fail("model_discovery_failed", "invalid Copilot models response", 502, "request")
	}
	models := make([]map[string]any, 0, len(root.Data))
	for _, m := range root.Data {
		limits, _ := m.Capabilities["limits"].(map[string]any)
		models = append(models, map[string]any{"ID": m.ID, "Object": first(m.Object, "model"), "Created": m.Created, "OwnedBy": first(m.OwnedBy, "github-copilot"), "Type": providerID, "DisplayName": first(m.Name, m.ID), "Version": m.Version, "ContextLength": int64(num(limits["max_context_window_tokens"])), "MaxCompletionTokens": int64(num(limits["max_output_tokens"])), "SupportedGenerationMethods": []string{"chat"}})
	}
	return map[string]any{"Provider": providerID, "Models": models}, nil
}

func rawHTTP(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		request
		Method, URL string
		Headers     http.Header
		Body        []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	s, err := credentials(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, fail("invalid_auth", err.Error(), 401, "credential")
	}
	token, _, f := exchangeToken(s.AccessToken, req.HostCallbackID)
	if f != nil {
		return nil, f
	}
	for k, v := range copilotHeaders(token, req.Body, false) {
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
		var c streamRead
		if err := runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": hostID}, &c); err != nil {
			streamFailure = hostFail(err)
			return
		}
		if c.Error != "" {
			streamFailure = fail("upstream_stream_error", c.Error, 502, "request")
			return
		}
		if len(c.Payload) > 0 {
			if err := runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: pluginID, Payload: c.Payload}, nil); err != nil {
				streamFailure = hostFail(err)
				return
			}
		}
		if c.Done {
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
func hostReq(method, endpoint string, headers http.Header, body []byte, callback string) map[string]any {
	return map[string]any{"method": method, "url": endpoint, "headers": headers, "body": body, "host_callback_id": callback}
}
func credentials(raw []byte, meta map[string]any) (storage, error) {
	var s storage
	_ = json.Unmarshal(raw, &s)
	if s.AccessToken == "" {
		s.AccessToken, _ = meta["access_token"].(string)
	}
	if s.AccessToken == "" {
		return s, fmt.Errorf("missing GitHub access token")
	}
	return s, nil
}
func makeAuth(s storage, file string) authData {
	s.Type = providerID
	b, _ := json.Marshal(s)
	return authData{Provider: providerID, ID: first(s.Username, "github-user"), FileName: file, Label: first(s.Username, "GitHub Copilot"), StorageJSON: b, Metadata: map[string]any{"type": providerID}}
}
func strip(s string) string {
	if strings.HasPrefix(s, providerID+"/") {
		return strings.TrimPrefix(s, providerID+"/")
	}
	return s
}
func randomID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._-", r) {
			return r
		}
		return -1
	}, s)
}
func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
func num(v any) float64 { n, _ := v.(float64); return n }
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
	return "request"
}
