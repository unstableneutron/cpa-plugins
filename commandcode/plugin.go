package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

var pluginRuntime nativeabi.Runtime

type plugin struct {
	runtime *nativeabi.Runtime

	lifecycleMu sync.Mutex
	quiescing   bool
	streams     map[*asyncStream]string
	streamWG    sync.WaitGroup
}

type asyncStream struct{ _ byte }

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct{ Name, Version, Author, GitHubRepository string }
type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type hostConfig struct {
	OAuthModelAlias map[string][]modelAlias
	ExcludedModels  map[string][]string
}
type modelAlias struct{ Name, Alias string }
type staticModelRequest struct{ Host hostConfig }
type authModelRequest struct {
	AuthID, AuthProvider string
	StorageJSON          []byte
	Metadata             map[string]any
	Attributes           map[string]string
	Host                 hostConfig
	HostCallbackID       string `json:"host_callback_id"`
}
type modelResponse struct {
	Provider string
	Models   []*ModelInfo
}

type authParseRequest struct {
	Provider, Path, FileName string
	RawJSON                  []byte
}
type authData struct {
	Provider, ID, FileName, Label, Prefix, ProxyURL string
	Disabled                                        bool
	StorageJSON                                     []byte
	Metadata                                        map[string]any
	Attributes                                      map[string]string
}
type authParseResponse struct {
	Handled bool
	Auth    authData
}

type executorRequest struct {
	AuthID, AuthProvider, Model, Format, Alt, SourceFormat string
	Stream                                                 bool
	Headers                                                http.Header
	OriginalRequest, Payload, StorageJSON                  []byte
	Metadata, AuthMetadata                                 map[string]any
	AuthAttributes                                         map[string]string
	StreamID                                               string `json:"stream_id,omitempty"`
	HostCallbackID                                         string `json:"host_callback_id,omitempty"`
}
type executorResponse struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any
}
type streamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}
type executionOptions struct {
	Headers  http.Header
	Metadata map[string]any
}

type httpRequest struct {
	HostCallbackID string           `json:"host_callback_id,omitempty"`
	Method         string           `json:"method"`
	URL            string           `json:"url"`
	Headers        http.Header      `json:"headers,omitempty"`
	Body           []byte           `json:"body,omitempty"`
	WireProfile    *httpWireProfile `json:"wire_profile,omitempty"`
}
type httpWireProfile struct {
	HTTP1Only              bool     `json:"http1_only,omitempty"`
	DisableAutoCompression bool     `json:"disable_auto_compression,omitempty"`
	HeaderProfile          []string `json:"header_profile,omitempty"`
}
type bufferedHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type streamHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type streamReadResponse struct {
	Payload []byte           `json:"payload"`
	Error   string           `json:"error"`
	Failure *nativeabi.Error `json:"failure,omitempty"`
	Done    bool             `json:"done"`
}

func (p *plugin) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		var request lifecycleRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		if request.SchemaVersion < nativeabi.SchemaVersion {
			return nil, &nativeabi.Error{Code: "schema_unsupported", Message: "Command Code requires plugin schema 7"}
		}
		p.resume()
		return registration{nativeabi.SchemaVersion, metadata{"Command Code", nativeabi.Version, "unstableneutron", "https://github.com/unstableneutron/cpa-plugins"}, capabilities{true, true, true, "both", []string{"openai"}, []string{"openai"}}}, nil
	case nativeabi.MethodPluginQuiesce, nativeabi.MethodPluginShutdown:
		p.quiesce()
		return struct{}{}, nil
	case "auth.identifier", "executor.identifier":
		return map[string]string{"identifier": commandCodeProviderKey}, nil
	case "auth.parse":
		var request authParseRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return parseAuth(request), nil
	case "auth.refresh":
		var request authModelRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return map[string]any{"Auth": authFromRequest(request)}, nil
	case "model.static":
		var request staticModelRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return modelResponse{commandCodeProviderKey, configuredModels(getCommandCodeModels(), request.Host)}, nil
	case "model.for_auth":
		var request authModelRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		models := p.fetchModels(request)
		return modelResponse{commandCodeProviderKey, configuredModels(models, request.Host)}, nil
	case "executor.execute":
		var request executorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return p.execute(request)
	case "executor.execute_stream":
		var request executorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return p.executeStream(request)
	case "executor.count_tokens":
		var request executorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, failure(err)
		}
		return countTokens(request)
	default:
		return nil, &nativeabi.Error{Code: "method_not_found", Message: "unsupported plugin method " + method}
	}
}

type preparedRequest struct {
	Body    []byte
	Target  string
	Headers http.Header
}

func (p *plugin) prepare(request executorRequest) (preparedRequest, *nativeabi.Error) {
	baseURL, apiKey := credentials(request.StorageJSON, request.AuthMetadata, request.AuthAttributes)
	if apiKey == "" {
		return preparedRequest{}, &nativeabi.Error{Code: "unauthorized", Message: "missing CommandCode API key", HTTPStatus: 401, Scope: "credential"}
	}
	model := strings.TrimSpace(request.Model)
	threadID := commandCodeThreadID(executionOptions{request.Headers, request.Metadata}, request.OriginalRequest)
	body, err := buildCommandCodePayload(commandCodePayloadOptions{Model: model, Payload: request.Payload, WorkingDir: ".", Environment: defaultCommandCodeEnvironment(), ThreadID: threadID})
	if err != nil {
		return preparedRequest{}, failure(err)
	}
	return preparedRequest{
		Body:    body,
		Target:  strings.TrimRight(baseURL, "/") + "/alpha/generate",
		Headers: commandCodeHeaders(apiKey, threadID, request.AuthAttributes),
	}, nil
}

func (p *plugin) open(request executorRequest, prepared preparedRequest) (streamHTTPResponse, *nativeabi.Error) {
	var response streamHTTPResponse
	if err := p.runtime.HostCall(nativeabi.MethodHostHTTPDoStream, httpRequest{HostCallbackID: request.HostCallbackID, Method: http.MethodPost, URL: prepared.Target, Headers: prepared.Headers, Body: prepared.Body}, &response); err != nil {
		return response, failure(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("CommandCode upstream returned HTTP %d", response.StatusCode)
		reader := &hostStreamReader{runtime: p.runtime, streamID: response.StreamID}
		if body, err := io.ReadAll(io.LimitReader(reader, 1<<20)); err == nil && strings.TrimSpace(string(body)) != "" {
			message = strings.TrimSpace(string(body))
		}
		p.closeHTTPStream(response.StreamID)
		return response, upstreamFailure(response.StatusCode, message, response.Headers)
	}
	return response, nil
}

func (p *plugin) execute(request executorRequest) (any, *nativeabi.Error) {
	prepared, prepareErr := p.prepare(request)
	if prepareErr != nil {
		return nil, prepareErr
	}
	response, openErr := p.open(request, prepared)
	if openErr != nil {
		return nil, openErr
	}
	defer func() { p.closeHTTPStream(response.StreamID) }()
	state := newCommandCodeStreamState(request.Model)
	state.toolSchemas = commandCodeToolSchemasFromPayload(request.Payload)
	for continuation := 0; ; continuation++ {
		if err := collectCommandCodeStreamState(nil, &hostStreamReader{runtime: p.runtime, streamID: response.StreamID}, state); err != nil {
			return nil, failure(err)
		}
		if state.Finish != "pause_turn" || continuation >= commandCodeContinuationLimit {
			break
		}
		p.closeHTTPStream(response.StreamID)
		resetCommandCodeContinuationState(state)
		response, openErr = p.open(request, prepared)
		if openErr != nil {
			return nil, openErr
		}
	}
	return executorResponse{Payload: commandCodeResponseFromState(state, request.Model), Headers: response.Headers}, nil
}

func (p *plugin) executeStream(request executorRequest) (any, *nativeabi.Error) {
	active, beginErr := p.beginAsyncStream()
	if beginErr != nil {
		return nil, beginErr
	}
	prepared, prepareErr := p.prepare(request)
	if prepareErr != nil {
		p.finishAsyncStream(active)
		return nil, prepareErr
	}
	response, openErr := p.open(request, prepared)
	if openErr != nil {
		p.finishAsyncStream(active)
		return nil, openErr
	}
	if !p.setAsyncStreamUpstream(active, response.StreamID) {
		p.closeHTTPStream(response.StreamID)
		p.finishAsyncStream(active)
		return nil, quiescingFailure()
	}
	go func() {
		defer p.finishAsyncStream(active)
		defer func() {
			if recover() != nil {
				p.closeOutput(request.StreamID, &nativeabi.Error{Code: "plugin_panic", Message: "Command Code stream handler panicked", HTTPStatus: 500, Scope: "request"})
			}
		}()
		p.stream(request, prepared, active, response)
	}()
	return streamResponse{Headers: response.Headers}, nil
}

func (p *plugin) stream(request executorRequest, prepared preparedRequest, active *asyncStream, response streamHTTPResponse) {
	state := newCommandCodeStreamState(request.Model)
	state.toolSchemas = commandCodeToolSchemasFromPayload(request.Payload)
	defer func() { p.closeHTTPStream(response.StreamID) }()
	for continuation := 0; ; continuation++ {
		reader := &hostStreamReader{runtime: p.runtime, streamID: response.StreamID}
		errRead := commandCodeReadLines(reader, func(line []byte) error {
			chunks, _, err := commandCodeLineToOpenAIChunks(line, state)
			if err != nil {
				return err
			}
			for _, chunk := range chunks {
				if err := p.runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: request.StreamID, Payload: chunk}, nil); err != nil {
					return err
				}
			}
			return nil
		})
		if errRead != nil {
			p.closeOutput(request.StreamID, failure(errRead))
			return
		}
		if !state.Terminal && state.Finish != "pause_turn" {
			p.closeOutput(request.StreamID, &nativeabi.Error{Code: "incomplete_stream", Message: "CommandCode stream ended unexpectedly before completion", HTTPStatus: 502, Scope: "request"})
			return
		}
		if state.Finish != "pause_turn" || continuation >= commandCodeContinuationLimit {
			if state.Finish == "pause_turn" {
				state.Finish = "stop"
				if err := p.runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: request.StreamID, Payload: state.streamChunk(map[string]any{}, state.Finish, state.Usage.openAIUsage())}, nil); err != nil {
					p.closeOutput(request.StreamID, failure(err))
					return
				}
			}
			break
		}
		p.closeHTTPStream(response.StreamID)
		resetCommandCodeContinuationState(state)
		if p.asyncStreamCanceled(active) {
			p.closeOutput(request.StreamID, quiescingFailure())
			return
		}
		var openErr *nativeabi.Error
		response, openErr = p.open(request, prepared)
		if openErr != nil {
			p.closeOutput(request.StreamID, openErr)
			return
		}
		if !p.setAsyncStreamUpstream(active, response.StreamID) {
			p.closeHTTPStream(response.StreamID)
			p.closeOutput(request.StreamID, quiescingFailure())
			return
		}
	}
	_ = p.runtime.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: request.StreamID, Payload: []byte("[DONE]")}, nil)
	p.closeOutput(request.StreamID, nil)
}

func (p *plugin) beginAsyncStream() (*asyncStream, *nativeabi.Error) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.quiescing {
		return nil, quiescingFailure()
	}
	if p.streams == nil {
		p.streams = make(map[*asyncStream]string)
	}
	active := &asyncStream{}
	p.streams[active] = ""
	p.streamWG.Add(1)
	return active, nil
}

func (p *plugin) setAsyncStreamUpstream(active *asyncStream, streamID string) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.quiescing {
		return false
	}
	if _, exists := p.streams[active]; !exists {
		return false
	}
	p.streams[active] = streamID
	return true
}

func (p *plugin) asyncStreamCanceled(active *asyncStream) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	_, exists := p.streams[active]
	return p.quiescing || !exists
}

func (p *plugin) finishAsyncStream(active *asyncStream) {
	p.lifecycleMu.Lock()
	if _, exists := p.streams[active]; !exists {
		p.lifecycleMu.Unlock()
		return
	}
	delete(p.streams, active)
	p.lifecycleMu.Unlock()
	p.streamWG.Done()
}

func (p *plugin) quiesce() {
	p.lifecycleMu.Lock()
	p.quiescing = true
	streamIDs := make([]string, 0, len(p.streams))
	for _, streamID := range p.streams {
		if streamID != "" {
			streamIDs = append(streamIDs, streamID)
		}
	}
	p.lifecycleMu.Unlock()
	for _, streamID := range streamIDs {
		p.closeHTTPStream(streamID)
	}
	p.streamWG.Wait()
}

func (p *plugin) resume() {
	p.lifecycleMu.Lock()
	p.quiescing = false
	p.lifecycleMu.Unlock()
}

func quiescingFailure() *nativeabi.Error {
	return &nativeabi.Error{Code: "plugin_quiescing", Message: "Command Code plugin is quiescing", Retryable: true, HTTPStatus: http.StatusServiceUnavailable, Scope: "request"}
}

func (p *plugin) closeOutput(streamID string, streamErr *nativeabi.Error) {
	request := nativeabi.StreamCloseRequest{StreamID: streamID, Failure: streamErr}
	if streamErr != nil {
		request.Error = streamErr.Message
	}
	_ = p.runtime.HostCall(nativeabi.MethodHostStreamClose, request, nil)
}

func (p *plugin) closeHTTPStream(streamID string) {
	if streamID != "" {
		_ = p.runtime.HostCall(nativeabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID}, nil)
	}
}

type hostStreamReader struct {
	runtime  *nativeabi.Runtime
	streamID string
	pending  []byte
	done     bool
}

func (r *hostStreamReader) Read(dst []byte) (int, error) {
	for len(r.pending) == 0 && !r.done {
		var response streamReadResponse
		if err := r.runtime.HostCall(nativeabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": r.streamID}, &response); err != nil {
			return 0, err
		}
		if response.Failure != nil {
			return 0, response.Failure
		}
		if response.Error != "" {
			return 0, fmt.Errorf("%s", response.Error)
		}
		r.pending, r.done = response.Payload, response.Done
	}
	if len(r.pending) == 0 && r.done {
		return 0, io.EOF
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (p *plugin) fetchModels(request authModelRequest) []*ModelInfo {
	baseURL, apiKey := credentials(request.StorageJSON, request.Metadata, request.Attributes)
	headers := commandCodeHeaders(apiKey, "", request.Attributes)
	var response bufferedHTTPResponse
	err := p.runtime.HostCall(nativeabi.MethodHostHTTPDo, httpRequest{HostCallbackID: request.HostCallbackID, Method: http.MethodGet, URL: strings.TrimRight(baseURL, "/") + "/provider/v1/models", Headers: headers}, &response)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return getCommandCodeModels()
	}
	models := commandCodeModelsFromProviderResponse(response.Body)
	if len(models) == 0 {
		return getCommandCodeModels()
	}
	return models
}

func commandCodeModelsFromProviderResponse(body []byte) []*ModelInfo {
	static := map[string]*ModelInfo{}
	for _, model := range getCommandCodeModels() {
		static[model.ID] = model
	}
	var models []*ModelInfo
	seen := map[string]bool{}
	for _, item := range gjson.GetBytes(body, "data").Array() {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(item.Get("name").String())
		if name == "" {
			name = id
		}
		if !strings.HasSuffix(name, " (CC)") {
			name += " (CC)"
		}
		contextLength := item.Get("context_length").Int()
		created := item.Get("created").Int()
		if created == 0 {
			created = time.Now().Unix()
		}
		model := &ModelInfo{ID: id, Object: "model", Created: created, OwnedBy: "command-code", Type: commandCodeProviderKey, DisplayName: name, Version: id, ContextLength: contextLength, MaxCompletionTokens: int64(commandCodeMaxTokensCap), SupportedParameters: []string{"tools"}, SupportedEndpoints: []string{"/v1/chat/completions", "/v1/responses"}, SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"}}
		if contextLength > 0 && contextLength < commandCodeMaxTokensCap {
			model.MaxCompletionTokens = contextLength
		}
		if fallback := static[id]; fallback != nil {
			model.MaxCompletionTokens = fallback.MaxCompletionTokens
			model.SupportedInputModalities = append([]string(nil), fallback.SupportedInputModalities...)
			if fallback.Thinking != nil {
				model.Thinking = &ThinkingSupport{Levels: append([]string(nil), fallback.Thinking.Levels...)}
			}
		}
		models = append(models, model)
	}
	return models
}

func configuredModels(models []*ModelInfo, host hostConfig) []*ModelInfo {
	aliases := host.OAuthModelAlias[commandCodeProviderKey]
	excluded := host.ExcludedModels[commandCodeProviderKey]
	result := make([]*ModelInfo, 0, len(models)+len(aliases))
	for _, model := range models {
		if model == nil || excludedModel(model.ID, excluded) {
			continue
		}
		copyModel := *model
		result = append(result, &copyModel)
		for _, alias := range aliases {
			if alias.Name == model.ID && alias.Alias != "" && !excludedModel(alias.Alias, excluded) {
				aliased := copyModel
				aliased.ID = alias.Alias
				aliased.Name = model.ID
				result = append(result, &aliased)
			}
		}
	}
	return result
}
func excludedModel(model string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.EqualFold(pattern, model) {
			return true
		}
		if ok, _ := filepath.Match(strings.ToLower(pattern), strings.ToLower(model)); ok {
			return true
		}
	}
	return false
}

func parseAuth(request authParseRequest) authParseResponse {
	var data map[string]any
	if json.Unmarshal(request.RawJSON, &data) != nil {
		return authParseResponse{}
	}
	typeName := stringValue(data, "type", "provider")
	if request.Provider != commandCodeProviderKey && !strings.EqualFold(typeName, commandCodeProviderKey) && !strings.EqualFold(typeName, "command-code") {
		return authParseResponse{}
	}
	baseURL, apiKey := credentials(request.RawJSON, data, nil)
	if apiKey == "" {
		return authParseResponse{}
	}
	id := stringValue(data, "id")
	if id == "" {
		id = uuid.NewSHA1(uuid.NameSpaceURL, []byte(request.FileName+":"+apiKey)).String()
	}
	return authParseResponse{Handled: true, Auth: authData{Provider: commandCodeProviderKey, ID: id, FileName: request.FileName, Label: stringValue(data, "label"), StorageJSON: bytes.Clone(request.RawJSON), Metadata: data, Attributes: map[string]string{"api_key": apiKey, "base_url": baseURL}}}
}
func authFromRequest(request authModelRequest) authData {
	return authData{Provider: commandCodeProviderKey, ID: request.AuthID, StorageJSON: request.StorageJSON, Metadata: request.Metadata, Attributes: request.Attributes}
}

func credentials(storage []byte, metadata map[string]any, attrs map[string]string) (string, string) {
	baseURL := strings.TrimSpace(os.Getenv("COMMANDCODE_API_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("COMMANDCODE_API_BASE"))
	}
	if baseURL == "" {
		baseURL = defaultCommandCodeAPIBase
	}
	apiKey := strings.TrimSpace(os.Getenv("COMMAND_CODE_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("COMMANDCODE_API_KEY"))
	}
	var stored map[string]any
	_ = json.Unmarshal(storage, &stored)
	for _, source := range []map[string]any{stored, metadata} {
		if value := stringValue(source, "base_url", "baseURL", "api_base", "apiBase"); value != "" {
			baseURL = value
		}
		if value := stringValue(source, "api_key", "apiKey", "access_token", "access", "commandcode"); value != "" {
			apiKey = value
		}
		if nested, ok := source["commandcode"].(map[string]any); ok {
			if value := stringValue(nested, "api_key", "apiKey", "access_token", "access"); value != "" {
				apiKey = value
			}
		}
	}
	if value := strings.TrimSpace(attrs["base_url"]); value != "" {
		baseURL = value
	}
	if value := strings.TrimSpace(attrs["api_key"]); value != "" {
		apiKey = value
	}
	return baseURL, apiKey
}
func stringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func commandCodeHeaders(apiKey, session string, attrs map[string]string) http.Header {
	headers := http.Header{"Accept": {"text/event-stream"}, "Content-Type": {"application/json"}, "User-Agent": {commandCodeDefaultUserAgent}, "x-command-code-version": {commandCodeVersionHeader}, "x-cli-environment": {"production"}, "x-project-slug": {"pi-cc"}, "x-taste-learning": {"false"}, "x-co-flag": {"false"}}
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	if session == "" {
		session = uuid.NewString()
	}
	headers.Set("x-session-id", session)
	if os.Getenv("CMD_ZDR") == "1" {
		headers.Set("x-cmd-zdr", "1")
	}
	for key, value := range attrs {
		if strings.HasPrefix(strings.ToLower(key), "header:") {
			headers.Set(strings.TrimSpace(key[7:]), value)
		}
	}
	return headers
}

func sessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	for _, key := range []string{"x-session-id", "session-id", "conversation-id"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	for _, key := range []string{"session_id", "sessionId", "conversation_id"} {
		if value := gjson.GetBytes(payload, key).String(); value != "" {
			return value
		}
		if raw := metadata[key]; raw != nil {
			return strings.TrimSpace(fmt.Sprint(raw))
		}
	}
	return ""
}

func failure(err error) *nativeabi.Error {
	if err == nil {
		return nil
	}
	if typed, ok := err.(*nativeabi.Error); ok {
		return typed
	}
	status := 500
	if value, ok := err.(interface{ StatusCode() int }); ok {
		status = value.StatusCode()
	}
	scope := "request"
	code := "commandcode_error"
	retryable := status == 429 || status >= 500
	if _, ok := err.(commandCodeAbortError); ok {
		code, scope, retryable = "aborted", "request", false
	} else if _, ok := err.(commandCodeProviderError); ok {
		scope = "credential"
		if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
			scope = "request"
		} else if status == http.StatusNotFound {
			scope = "model"
		}
	}
	return &nativeabi.Error{Code: code, Message: err.Error(), HTTPStatus: status, Scope: scope, Retryable: retryable}
}

func upstreamFailure(status int, message string, headers http.Header) *nativeabi.Error {
	scope := "credential"
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		scope = "request"
	} else if status == http.StatusNotFound {
		scope = "model"
	}
	failure := &nativeabi.Error{Code: "upstream_http", Message: message, HTTPStatus: status, Scope: scope, Retryable: status == http.StatusTooManyRequests || status >= 500}
	if seconds, err := time.ParseDuration(strings.TrimSpace(headers.Get("Retry-After")) + "s"); err == nil && seconds >= 0 {
		milliseconds := seconds.Milliseconds()
		failure.RetryAfterMS = &milliseconds
	}
	return failure
}
