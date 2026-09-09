// Package nativeabi implements the provider-neutral JSON portion of the
// CLIProxyAPI native plugin ABI. Plugin main packages retain the small C ABI
// shim because cgo function-pointer and //export types are package-local.
package nativeabi

import (
	"encoding/json"
	"fmt"
	"sync"
)

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 7
)

const (
	MethodPluginRegister      = "plugin.register"
	MethodPluginQuiesce       = "plugin.quiesce"
	MethodPluginReconfigure   = "plugin.reconfigure"
	MethodPluginShutdown      = "plugin.shutdown"
	MethodHostHTTPDo          = "host.http.do"
	MethodHostHTTPDoStream    = "host.http.do_stream"
	MethodHostHTTPStreamRead  = "host.http.stream_read"
	MethodHostHTTPStreamClose = "host.http.stream_close"
	MethodHostStreamEmit      = "host.stream.emit"
	MethodHostStreamClose     = "host.stream.close"
)

type Error struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Scope        string `json:"scope,omitempty"`
	RetryAfterMS *int64 `json:"retry_after_ms,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Handler interface {
	Call(method string, request json.RawMessage) (any, *Error)
}

type HostCaller func(method string, request []byte) ([]byte, error)

// Runtime safely publishes one handler and host callback to concurrent native calls.
type Runtime struct {
	mu      sync.RWMutex
	handler Handler
	host    HostCaller
}

func (r *Runtime) Initialize(handler Handler, host HostCaller) {
	r.mu.Lock()
	r.handler = handler
	r.host = host
	r.mu.Unlock()
}

func (r *Runtime) Shutdown() {
	r.mu.Lock()
	r.handler = nil
	r.host = nil
	r.mu.Unlock()
}

func (r *Runtime) Call(method string, request []byte) ([]byte, int) {
	r.mu.RLock()
	handler := r.handler
	r.mu.RUnlock()
	if handler == nil {
		return marshalEnvelope(nil, &Error{Code: "plugin_unavailable", Message: "plugin is not initialized"}), 1
	}
	result, callErr := handler.Call(method, json.RawMessage(request))
	if callErr != nil {
		return marshalEnvelope(nil, callErr), 1
	}
	return marshalEnvelope(result, nil), 0
}

func (r *Runtime) HostCall(method string, request any, result any) error {
	r.mu.RLock()
	host := r.host
	r.mu.RUnlock()
	if host == nil {
		return fmt.Errorf("host callback is unavailable")
	}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	response, errCall := host(method, payload)
	if errCall != nil {
		return fmt.Errorf("host callback %s: %w", method, errCall)
	}
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(response, &envelope); errUnmarshal != nil {
		return fmt.Errorf("decode host callback %s: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return fmt.Errorf("host callback %s failed", method)
		}
		return envelope.Error
	}
	if result == nil || len(envelope.Result) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, result); errUnmarshal != nil {
		return fmt.Errorf("decode host callback %s result: %w", method, errUnmarshal)
	}
	return nil
}

type StreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
	Failure  *Error `json:"failure,omitempty"`
}

type StreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
	Failure  *Error `json:"failure,omitempty"`
}

func marshalEnvelope(result any, callErr *Error) []byte {
	envelope := Envelope{OK: callErr == nil, Error: callErr}
	if callErr == nil {
		encoded, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			envelope.OK = false
			envelope.Error = &Error{Code: "internal", Message: "encode plugin result: " + errMarshal.Error()}
		} else {
			envelope.Result = encoded
		}
	}
	encoded, _ := json.Marshal(envelope)
	return encoded
}
