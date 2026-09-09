package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
)

type rpcExecutorRequest struct {
	AuthID          string            `json:"AuthID"`
	Model           string            `json:"Model"`
	Payload         []byte            `json:"Payload"`
	OriginalRequest []byte            `json:"OriginalRequest"`
	AuthMetadata    map[string]any    `json:"AuthMetadata"`
	AuthAttributes  map[string]string `json:"AuthAttributes"`
	StreamID        string            `json:"stream_id"`
	HostCallbackID  string            `json:"host_callback_id"`
}

type pluginHandler struct{}

func (h *pluginHandler) Call(method string, raw json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		return map[string]any{
			"schema_version": nativeabi.SchemaVersion,
			"metadata":       map[string]any{"Name": "Bedrock", "Version": "0.1.0", "Author": "unstableneutron", "GitHubRepository": "https://github.com/unstableneutron/cpa-plugins", "ConfigFields": []any{}},
			"capabilities":   map[string]any{"executor": true, "executor_model_scope": "both", "executor_input_formats": []string{"claude"}, "executor_output_formats": []string{"claude"}},
		}, nil
	case methodExecutorIdentifier:
		return map[string]string{"identifier": providerID}, nil
	case methodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, failure(err, "request", "invalid_request")
		}
		resp, err := NewProvider(hostTransport{}).Execute(context.Background(), providerRequest(req))
		if err != nil {
			return nil, failure(err, "request", "bedrock_execute")
		}
		return map[string]any{"Payload": resp.Payload, "Headers": resp.Headers}, nil
	case methodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, failure(err, "request", "invalid_request")
		}
		if req.StreamID == "" {
			return nil, &nativeabi.Error{Code: "invalid_stream", Message: "stream ID is required", Scope: "request"}
		}
		go streamBedrock(req)
		return map[string]any{"headers": map[string][]string{"content-type": {"text/event-stream"}}}, nil
	default:
		return nil, &nativeabi.Error{Code: "unknown_method", Message: "unknown method: " + method}
	}
}

func providerRequest(req rpcExecutorRequest) Request {
	attrs := req.AuthAttributes
	auth := Auth{BaseURL: attrs["base_url"], APIKey: attrs["api_key"], AuthType: attrs["auth_type"], CustomHeaders: decodeStringMap(attrs["custom_headers"]), ModelMap: decodeStringMap(attrs["bedrock_model_map"]), APIMap: decodeStringMap(attrs["bedrock_api_map"]), StreamAPIMap: decodeStringMap(attrs["bedrock_stream_map"])}
	return Request{Model: req.Model, Payload: req.Payload, OriginalRequest: req.OriginalRequest, Auth: auth}
}

func decodeStringMap(raw string) map[string]string {
	var values map[string]string
	_ = json.Unmarshal([]byte(raw), &values)
	return values
}

func streamBedrock(req rpcExecutorRequest) {
	err := NewProvider(hostTransport{}).ExecuteStream(context.Background(), providerRequest(req), func(payload []byte) error {
		return runtimeABI.HostCall(nativeabi.MethodHostStreamEmit, nativeabi.StreamEmitRequest{StreamID: req.StreamID, Payload: payload}, nil)
	})
	closeReq := nativeabi.StreamCloseRequest{StreamID: req.StreamID}
	if err != nil {
		closeReq.Failure = failure(err, "request", "bedrock_stream")
	}
	_ = runtimeABI.HostCall(nativeabi.MethodHostStreamClose, closeReq, nil)
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

func failure(err error, scope, fallbackCode string) *nativeabi.Error {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return &nativeabi.Error{Code: providerErr.Code, Message: providerErr.Message, Retryable: providerErr.Retryable, HTTPStatus: providerErr.Status, Scope: scope}
	}
	code := fallbackCode
	status := 0
	if errors.Is(err, context.Canceled) {
		code = "canceled"
		status = 499
	}
	if strings.Contains(strings.ToLower(err.Error()), "timeout") {
		code = "timeout"
	}
	return &nativeabi.Error{Code: code, Message: err.Error(), Retryable: code == "timeout", HTTPStatus: status, Scope: scope}
}
