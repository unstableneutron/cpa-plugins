package nativeabi

import (
	"encoding/json"
	"errors"
	"testing"
)

type handlerFunc func(string, json.RawMessage) (any, *Error)

func (f handlerFunc) Call(method string, request json.RawMessage) (any, *Error) {
	return f(method, request)
}

func TestRuntimeCallUsesTypedFailureEnvelope(t *testing.T) {
	retryAfter := int64(250)
	var runtime Runtime
	if err := runtime.Initialize(handlerFunc(func(string, json.RawMessage) (any, *Error) {
		return nil, &Error{Code: "aborted", Message: "stopped", HTTPStatus: 499, Scope: "request", RetryAfterMS: &retryAfter}
	}), nil); err != nil {
		t.Fatal(err)
	}

	response, status := runtime.Call("executor.execute", nil)
	if status == 0 {
		t.Fatal("status = 0, want failure")
	}
	var envelope Envelope
	if err := json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Scope != "request" || *envelope.Error.RetryAfterMS != 250 {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestRuntimeHostCallDecodesEnvelope(t *testing.T) {
	var runtime Runtime
	if err := runtime.Initialize(nil, func(method string, request []byte) ([]byte, int) {
		if method != MethodHostHTTPStreamRead {
			t.Fatalf("method = %q", method)
		}
		return []byte(`{"ok":true,"result":{"done":true}}`), 0
	}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Done bool `json:"done"`
	}
	if err := runtime.HostCall(MethodHostHTTPStreamRead, map[string]string{"stream_id": "s"}, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Done {
		t.Fatal("done = false")
	}
}

func TestRuntimeHostCallPreservesHostFailure(t *testing.T) {
	var runtime Runtime
	if err := runtime.Initialize(nil, func(string, []byte) ([]byte, int) {
		return []byte(`{"ok":false,"error":{"code":"upstream","message":"failed","scope":"credential"}}`), 7
	}); err != nil {
		t.Fatal(err)
	}
	err := runtime.HostCall(MethodHostHTTPDo, struct{}{}, nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Scope != "credential" {
		t.Fatalf("error = %#v", err)
	}
}

func TestRuntimeRejectsReinitializationAfterShutdown(t *testing.T) {
	var runtime Runtime
	if err := runtime.Initialize(nil, nil); err != nil {
		t.Fatal(err)
	}
	runtime.Shutdown()
	if err := runtime.Initialize(nil, nil); err == nil {
		t.Fatal("second initialization succeeded")
	}
}

func TestRuntimeRecoversHandlerPanic(t *testing.T) {
	var runtime Runtime
	_ = runtime.Initialize(handlerFunc(func(string, json.RawMessage) (any, *Error) { panic("boom") }), nil)
	response, status := runtime.Call("panic", nil)
	if status == 0 || !json.Valid(response) {
		t.Fatalf("status=%d response=%s", status, response)
	}
}

func TestRuntimeSerializationFailureReturnsNonzero(t *testing.T) {
	var runtime Runtime
	_ = runtime.Initialize(handlerFunc(func(string, json.RawMessage) (any, *Error) { return make(chan int), nil }), nil)
	response, status := runtime.Call("bad", nil)
	var envelope Envelope
	_ = json.Unmarshal(response, &envelope)
	if status == 0 || envelope.OK || envelope.Error == nil || envelope.Error.Code != "internal" {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}
}
