package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodAuthIdentifier        = "auth.identifier"
	methodAuthParse             = "auth.parse"
	methodAuthLoginStart        = "auth.login.start"
	methodAuthLoginPoll         = "auth.login.poll"
	methodAuthRefresh           = "auth.refresh"
	methodModelStatic           = "model.static"
	methodModelForAuth          = "model.for_auth"
)

type rpcExecutorRequest struct {
	AuthID, Model, SourceFormat           string
	Payload, OriginalRequest, StorageJSON []byte
	AuthMetadata                          map[string]any
	AuthAttributes                        map[string]string
	StreamID                              string `json:"stream_id"`
	HostCallbackID                        string `json:"host_callback_id"`
}
type pluginHandler struct{}

func (h *pluginHandler) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		return map[string]any{"schema_version": nativeabi.SchemaVersion, "metadata": map[string]any{"Name": "Kiro", "Version": nativeabi.Version, "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}}, "capabilities": map[string]any{"executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"claude", "openai"}, "executor_output_formats": []string{"claude"}, "auth_provider": true, "model_provider": true}}, nil
	case methodExecutorIdentifier, methodAuthIdentifier:
		return map[string]string{"identifier": providerID}, nil
	case methodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, typedFailure(err, "request", "invalid_request")
		}
		providerReq, err := requestFromRPC(req)
		if err != nil {
			return nil, typedFailure(err, "credential", "invalid_credential")
		}
		resp, err := NewProvider(hostTransport{}).Execute(context.Background(), providerReq)
		if err != nil {
			return nil, typedFailure(err, "request", "kiro_execute")
		}
		return map[string]any{"Payload": resp.Payload, "Headers": resp.Headers, "Metadata": refreshMetadata(resp.Refreshed)}, nil
	case methodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, typedFailure(err, "request", "invalid_request")
		}
		if req.StreamID == "" {
			return nil, &nativeabi.Error{Code: "invalid_stream", Message: "stream ID is required", Scope: "request"}
		}
		providerReq, err := requestFromRPC(req)
		if err != nil {
			return nil, typedFailure(err, "credential", "invalid_credential")
		}
		go streamKiro(req.StreamID, providerReq)
		return map[string]any{"headers": map[string][]string{"content-type": {"text/event-stream"}}}, nil
	case methodAuthParse:
		return parseAuthRPC(raw)
	case methodAuthRefresh:
		return refreshAuthRPC(raw)
	case methodAuthLoginStart:
		return startLoginRPC(raw)
	case methodAuthLoginPoll:
		return pollLoginRPC(raw)
	case methodModelStatic:
		return map[string]any{"Provider": providerID, "Models": staticModels()}, nil
	case methodModelForAuth:
		return modelsForAuthRPC(raw)
	default:
		return nil, &nativeabi.Error{Code: "unknown_method", Message: "unknown method: " + method}
	}
}

func modelsForAuthRPC(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		AuthID      string
		StorageJSON []byte
		Metadata    map[string]any
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, typedFailure(err, "credential", "invalid_auth")
	}
	source := req.StorageJSON
	if len(source) == 0 {
		source, _ = json.Marshal(req.Metadata)
	}
	token, err := parseToken(source)
	if err != nil {
		return map[string]any{"Provider": providerID, "Models": staticModels()}, nil
	}
	models, err := modelsForToken(context.Background(), hostTransport{}, token, req.AuthID)
	if err != nil {
		// Match the source provider's deterministic static fallback when discovery is unavailable.
		return map[string]any{"Provider": providerID, "Models": staticModels()}, nil
	}
	return map[string]any{"Provider": providerID, "Models": models}, nil
}

func requestFromRPC(req rpcExecutorRequest) (Request, error) {
	raw := req.StorageJSON
	if len(raw) == 0 {
		raw, _ = json.Marshal(req.AuthMetadata)
	}
	token, err := parseToken(raw)
	if err != nil {
		return Request{}, err
	}
	headers := map[string]string{}
	if value := req.AuthAttributes["custom_headers"]; value != "" {
		_ = json.Unmarshal([]byte(value), &headers)
	}
	return Request{Model: req.Model, SourceFormat: req.SourceFormat, AuthID: req.AuthID, Payload: req.Payload, Token: token, CustomHeaders: headers}, nil
}
func refreshMetadata(token *Token) map[string]any {
	if token == nil {
		return nil
	}
	return map[string]any{"refreshed_auth": token}
}

func streamKiro(streamID string, req Request) {
	closeReq := nativeabi.StreamCloseRequest{StreamID: streamID}
	defer func() {
		if recover() != nil {
			closeReq.Failure = &nativeabi.Error{Code: "internal", Message: "plugin stream panicked", Scope: "request"}
		}
		_ = runtimeABI.HostCall(nativeabi.MethodHostStreamClose, closeReq, nil)
	}()
	_, err := NewProvider(hostTransport{}).ExecuteStream(context.Background(), req, func(payload []byte) error {
		return runtimeABI.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: streamID, Payload: payload}, nil)
	})
	if err != nil {
		closeReq.Failure = typedFailure(err, "request", "kiro_stream")
	}
}

type hostTransport struct{}

func (hostTransport) Do(_ context.Context, req HTTPRequest) (HTTPResponse, error) {
	var response HTTPResponse
	if err := runtimeABI.HostCall(nativeabi.MethodHostHTTPDo, req, &response); err != nil {
		return response, err
	}
	return response, nil
}
func (hostTransport) DoStream(_ context.Context, req HTTPRequest) (HTTPStreamResponse, error) {
	var response struct {
		StatusCode int                 `json:"status_code"`
		Headers    map[string][]string `json:"headers"`
		StreamID   string              `json:"stream_id"`
	}
	if err := runtimeABI.HostCall(nativeabi.MethodHostHTTPDoStream, req, &response); err != nil {
		return HTTPStreamResponse{}, err
	}
	return HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: &hostStreamReader{id: response.StreamID}}, nil
}

type hostStreamReader struct {
	id      string
	pending []byte
	done    bool
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	var response struct {
		Payload []byte `json:"payload"`
		Error   string `json:"error"`
		Done    bool   `json:"done"`
	}
	if err := runtimeABI.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": r.id}, &response); err != nil {
		return 0, err
	}
	if response.Error != "" {
		return 0, errors.New(response.Error)
	}
	r.done = response.Done
	if len(response.Payload) == 0 {
		if r.done {
			return 0, io.EOF
		}
		return r.Read(p)
	}
	n := copy(p, response.Payload)
	r.pending = append(r.pending, response.Payload[n:]...)
	return n, nil
}
func (r *hostStreamReader) Close() error {
	r.done = true
	return runtimeABI.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": r.id}, nil)
}

func parseAuthRPC(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		FileName string
		RawJSON  []byte
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, typedFailure(err, "credential", "invalid_auth")
	}
	token, err := parseToken(req.RawJSON)
	if err != nil {
		return map[string]any{"Handled": false}, nil
	}
	storage, _ := json.Marshal(token)
	id := strings.TrimSuffix(filepath.Base(req.FileName), filepath.Ext(req.FileName))
	if id == "" {
		id = "kiro-" + accountKey(token.RefreshToken+token.AccessToken)
	}
	metadata := tokenMetadata(token)
	return map[string]any{"Handled": true, "Auth": map[string]any{"Provider": providerID, "ID": id, "FileName": req.FileName, "Label": token.Email, "StorageJSON": storage, "Metadata": metadata, "Attributes": map[string]string{"access_token": token.AccessToken, "profile_arn": token.ProfileARN}, "NextRefreshAfter": nextRefresh(token)}}, nil
}
func refreshAuthRPC(raw []byte) (any, *nativeabi.Error) {
	var req struct {
		AuthID      string
		StorageJSON []byte
		Metadata    map[string]any
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, typedFailure(err, "credential", "invalid_auth")
	}
	source := req.StorageJSON
	if len(source) == 0 {
		source, _ = json.Marshal(req.Metadata)
	}
	token, err := parseToken(source)
	if err != nil {
		return nil, typedFailure(err, "credential", "invalid_auth")
	}
	updated, err := refreshToken(context.Background(), hostTransport{}, token, time.Now())
	if err != nil {
		return nil, typedFailure(err, "credential", "refresh_failed")
	}
	storage, _ := json.Marshal(updated)
	next := nextRefresh(updated)
	return map[string]any{"Auth": map[string]any{"Provider": providerID, "ID": req.AuthID, "StorageJSON": storage, "Metadata": tokenMetadata(updated), "Attributes": map[string]string{"access_token": updated.AccessToken, "profile_arn": updated.ProfileARN}, "NextRefreshAfter": next}, "NextRefreshAfter": next}, nil
}
func tokenMetadata(token Token) map[string]any {
	raw, _ := json.Marshal(token)
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	return values
}
func nextRefresh(token Token) time.Time {
	expires, _ := time.Parse(time.RFC3339, token.ExpiresAt)
	if expires.IsZero() {
		return time.Time{}
	}
	return expires.Add(-20 * time.Minute)
}

type loginState struct {
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	DeviceCode   string    `json:"device_code"`
	Region       string    `json:"region"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func startLoginRPC(raw []byte) (any, *nativeabi.Error) {
	var req struct{ Metadata map[string]any }
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	flow := strings.ToLower(strings.TrimSpace(stringValue(req.Metadata, "login_method")))
	if flow == "" {
		flow = strings.ToLower(strings.TrimSpace(stringValue(req.Metadata, "auth_method")))
	}
	switch flow {
	case "", "builder-id", "builder_id", "device", "device-code", "device_code":
	default:
		return nil, &nativeabi.Error{Code: "unsupported_login", Message: "Kiro native login supports only the Builder ID device flow", Scope: "credential"}
	}
	region := stringValue(req.Metadata, "region")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := "https://oidc." + region + ".amazonaws.com"
	registerBody, _ := json.Marshal(map[string]any{"clientName": "Kiro IDE", "clientType": "public", "scopes": []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"}, "grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"}})
	register, err := oidcCall(endpoint+"/client/register", registerBody)
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	var client struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(register, &client); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	deviceBody, _ := json.Marshal(map[string]string{"clientId": client.ClientID, "clientSecret": client.ClientSecret, "startUrl": "https://view.awsapps.com/start"})
	deviceRaw, err := oidcCall(endpoint+"/device_authorization", deviceBody)
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	var device struct {
		DeviceCode              string `json:"deviceCode"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		VerificationURI         string `json:"verificationUri"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(deviceRaw, &device); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	expires := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	stateRaw, _ := json.Marshal(loginState{ClientID: client.ClientID, ClientSecret: client.ClientSecret, DeviceCode: device.DeviceCode, Region: region, ExpiresAt: expires})
	url := device.VerificationURIComplete
	if url == "" {
		url = device.VerificationURI
	}
	return map[string]any{"Provider": providerID, "URL": url, "State": base64.RawURLEncoding.EncodeToString(stateRaw), "ExpiresAt": expires, "Metadata": map[string]any{"interval": device.Interval}}, nil
}
func pollLoginRPC(raw []byte) (any, *nativeabi.Error) {
	var req struct{ State string }
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	stateRaw, err := base64.RawURLEncoding.DecodeString(req.State)
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	var state loginState
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	if time.Now().After(state.ExpiresAt) {
		return map[string]any{"Status": "error", "Message": "device authorization expired"}, nil
	}
	body, _ := json.Marshal(map[string]string{"clientId": state.ClientID, "clientSecret": state.ClientSecret, "deviceCode": state.DeviceCode, "grantType": "urn:ietf:params:oauth:grant-type:device_code"})
	response, err := hostTransport{}.Do(context.Background(), HTTPRequest{Method: "POST", URL: "https://oidc." + state.Region + ".amazonaws.com/token", Headers: oidcHeaders(), Body: body})
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	if response.StatusCode == 400 && strings.Contains(string(response.Body), "authorization_pending") {
		return map[string]any{"Status": "pending", "Message": "waiting for authorization"}, nil
	}
	if response.StatusCode != 200 {
		return map[string]any{"Status": "error", "Message": fmt.Sprintf("token endpoint returned %d", response.StatusCode)}, nil
	}
	var tokenResult struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(response.Body, &tokenResult); err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	token := Token{Type: "kiro", AccessToken: tokenResult.AccessToken, RefreshToken: tokenResult.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(tokenResult.ExpiresIn) * time.Second).UTC().Format(time.RFC3339), AuthMethod: "builder-id", Provider: "AWS", ClientID: state.ClientID, ClientSecret: state.ClientSecret, Region: state.Region}
	storage, _ := json.Marshal(token)
	return map[string]any{"Status": "success", "Message": "authorization complete", "Auth": map[string]any{"Provider": providerID, "ID": "kiro-" + accountKey(state.ClientID), "FileName": "kiro-" + accountKey(state.ClientID) + ".json", "StorageJSON": storage, "Metadata": tokenMetadata(token), "Attributes": map[string]string{"access_token": token.AccessToken}, "NextRefreshAfter": nextRefresh(token)}}, nil
}
func oidcCall(url string, body []byte) ([]byte, error) {
	response, err := hostTransport{}.Do(context.Background(), HTTPRequest{Method: "POST", URL: url, Headers: oidcHeaders(), Body: body})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("OIDC endpoint returned %d", response.StatusCode)
	}
	return response.Body, nil
}
func oidcHeaders() map[string][]string {
	fp := fingerprint("oidc-session")
	return map[string][]string{"Content-Type": {"application/json"}, "x-amz-user-agent": {"aws-sdk-js/" + fp.OIDCSDKVersion + " KiroIDE"}, "User-Agent": {fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/sso-oidc#%s m/E KiroIDE", fp.OIDCSDKVersion, fp.OSType, fp.OSVersion, fp.NodeVersion, fp.OIDCSDKVersion)}}
}

func typedFailure(err error, scope, code string) *nativeabi.Error {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return &nativeabi.Error{Code: providerErr.Code, Message: providerErr.Message, Retryable: providerErr.Retryable, HTTPStatus: providerErr.Status, Scope: providerErr.Scope, RetryAfterMS: providerErr.RetryAfterMS}
	}
	if errors.Is(err, context.Canceled) {
		return &nativeabi.Error{Code: "canceled", Message: "request canceled", HTTPStatus: 499, Scope: "request"}
	}
	return &nativeabi.Error{Code: code, Message: err.Error(), Scope: scope}
}
