// Package nativeabi implements the provider-neutral JSON portion of the
// CLIProxyAPI native plugin ABI. Plugin main packages retain the small C ABI
// shim because cgo function-pointer and //export types are package-local.
package nativeabi

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Version is replaced by release builds with -X.
var Version = "dev"

const (
	ABIVersion    uint32 = 1
	SchemaVersion uint32 = 7
	// SchemaVersionIngressProxy is required only by plugins using ingress plans.
	SchemaVersionIngressProxy uint32 = 8
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

type HostCaller func(method string, request []byte) ([]byte, int)

// Runtime safely publishes one handler and host callback to concurrent native calls.
type Runtime struct {
	mu          sync.RWMutex
	handler     Handler
	host        HostCaller
	initialized bool
}

func (r *Runtime) Initialize(handler Handler, host HostCaller) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initialized {
		return fmt.Errorf("native plugin runtime was already initialized; restart is required")
	}
	r.initialized = true
	r.handler = handler
	r.host = host
	return nil
}

func (r *Runtime) Shutdown() {
	r.mu.Lock()
	r.handler = nil
	r.host = nil
	r.mu.Unlock()
}

func (r *Runtime) Call(method string, request []byte) (response []byte, status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// Panic values may contain request bodies or credentials.
			response, _ = marshalEnvelope(nil, &Error{Code: "plugin_panic", Message: "plugin handler panicked"})
			status = 1
		}
	}()
	r.mu.RLock()
	handler := r.handler
	r.mu.RUnlock()
	if handler == nil {
		response, _ := marshalEnvelope(nil, &Error{Code: "plugin_unavailable", Message: "plugin is not initialized"})
		return response, 1
	}
	result, callErr := handler.Call(method, json.RawMessage(request))
	if callErr != nil {
		response, _ := marshalEnvelope(nil, callErr)
		return response, 1
	}
	response, ok := marshalEnvelope(result, nil)
	if !ok {
		return response, 1
	}
	return response, 0
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
	response, status := host(method, payload)
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(response, &envelope); errUnmarshal != nil {
		if status != 0 {
			return fmt.Errorf("host callback %s returned %d", method, status)
		}
		return fmt.Errorf("decode host callback %s: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return fmt.Errorf("host callback %s failed", method)
		}
		return envelope.Error
	}
	if status != 0 {
		return fmt.Errorf("host callback %s returned %d", method, status)
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

func marshalEnvelope(result any, callErr *Error) ([]byte, bool) {
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
	return encoded, envelope.OK
}
