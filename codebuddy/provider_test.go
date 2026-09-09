package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func initializeTestRuntime(t *testing.T, host nativeabi.HostCaller) {
	t.Helper()
	runtime = nativeabi.Runtime{}
	if err := runtime.Initialize(provider{}, host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
}

func TestExecuteUsesHostHTTPAndAggregatesStream(t *testing.T) {
	var outbound map[string]any
	initializeTestRuntime(t, func(method string, request []byte) ([]byte, int) {
		if method != "host.http.do" {
			t.Fatalf("method = %q", method)
		}
		if err := json.Unmarshal(request, &outbound); err != nil {
			t.Fatal(err)
		}
		result, _ := json.Marshal(httpResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("data: {\"id\":\"c1\",\"model\":\"glm\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	storageJSON, _ := json.Marshal(tokenStorage{AccessToken: "secret", UserID: "u"})
	req, _ := json.Marshal(executorRequest{Model: "codebuddy/glm(high)", Payload: []byte(`{"model":"ignored","messages":[{"role":"user","content":"hi"}]}`), StorageJSON: storageJSON})
	result, callErr := (provider{}).Call("executor.execute", req)
	if callErr != nil {
		t.Fatal(callErr)
	}
	response := result.(map[string]any)
	var completion map[string]any
	if err := json.Unmarshal(response["Payload"].([]byte), &completion); err != nil {
		t.Fatal(err)
	}
	if completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "ok" {
		t.Fatalf("completion = %#v", completion)
	}
	if outbound["url"] != baseURL+"/v2/chat/completions" {
		t.Fatalf("url = %#v", outbound["url"])
	}
	bodyRaw, _ := base64.StdEncoding.DecodeString(outbound["body"].(string))
	var body map[string]any
	_ = json.Unmarshal(bodyRaw, &body)
	if body["model"] != "glm" || body["reasoning_effort"] != "high" {
		t.Fatalf("outbound body = %#v", body)
	}
}

func TestRawHTTPRequestInitializesMissingHeaders(t *testing.T) {
	initializeTestRuntime(t, func(_ string, request []byte) ([]byte, int) {
		var outbound map[string]any
		if err := json.Unmarshal(request, &outbound); err != nil {
			t.Fatal(err)
		}
		if outbound["headers"] == nil {
			t.Fatal("host request headers are nil")
		}
		result, _ := json.Marshal(httpResponse{StatusCode: http.StatusNoContent})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	storageJSON, _ := json.Marshal(tokenStorage{AccessToken: "secret", UserID: "u"})
	raw, _ := json.Marshal(map[string]any{"StorageJSON": storageJSON, "Method": "GET", "URL": "https://example.test"})
	if _, callErr := rawHTTPRequest(raw); callErr != nil {
		t.Fatal(callErr)
	}
}

func TestAggregateSSEPreservesReasoningToolsAndUsage(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"chat-1","created":7,"model":"glm","choices":[{"delta":{"role":"assistant","reasoning_content":"why ","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"look","arguments":"{\"q\":"}}]}}]}`,
		`data: {"id":"chat-1","model":"glm","choices":[{"delta":{"reasoning_content":"now","tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`,
		`data: [DONE]`,
	}, "\n\n")
	got, err := aggregateSSE([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["reasoning_content"] != "why now" {
		t.Fatalf("reasoning = %#v", message["reasoning_content"])
	}
	call := message["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "look" || fn["arguments"] != `{"q":"x"}` {
		t.Fatalf("function = %#v", fn)
	}
	if body["usage"].(map[string]any)["total_tokens"] != float64(14) {
		t.Fatalf("usage = %#v", body["usage"])
	}
}

func TestAggregateSSEPreservesSparseToolsAndMultipleChoices(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"chat-2","choices":[{"index":2,"delta":{"content":"second","tool_calls":[{"index":3,"id":"call-3","function":{"name":"later","arguments":"{"}}]}},{"index":0,"delta":{"content":"first"}}]}`,
		`data: {"id":"chat-2","choices":[{"index":2,"delta":{"tool_calls":[{"index":3,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"},{"index":0,"delta":{"content":" choice"},"finish_reason":"stop"}]}`,
	}, "\n\n")
	got, err := aggregateSSE([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	choices := body["choices"].([]any)
	if len(choices) != 2 || choices[0].(map[string]any)["index"] != float64(0) || choices[1].(map[string]any)["index"] != float64(2) {
		t.Fatalf("choices = %#v", choices)
	}
	if choices[0].(map[string]any)["message"].(map[string]any)["content"] != "first choice" {
		t.Fatalf("choice 0 = %#v", choices[0])
	}
	tools := choices[1].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["id"] != "call-3" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestForwardStreamContainsPanicAndClosesOnce(t *testing.T) {
	var reads, pluginCloses, hostCloses int
	initializeTestRuntime(t, func(method string, request []byte) ([]byte, int) {
		switch method {
		case nativeabi.MethodHostHTTPStreamRead:
			reads++
			panic("upstream callback panic")
		case nativeabi.MethodHostStreamClose:
			pluginCloses++
		case nativeabi.MethodHostHTTPStreamClose:
			hostCloses++
		}
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{}})
		return envelope, 0
	})
	forwardStream("plugin-stream", "host-stream")
	if reads != 1 || pluginCloses != 1 || hostCloses != 1 {
		t.Fatalf("reads=%d plugin closes=%d host closes=%d", reads, pluginCloses, hostCloses)
	}
}

func TestRPCUpstreamThrottlePreservesCooldownClassification(t *testing.T) {
	initializeTestRuntime(t, func(_ string, _ []byte) ([]byte, int) {
		result, _ := json.Marshal(httpResponse{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": {"7"}}, Body: []byte(`{"error":"limited"}`)})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	storageJSON, _ := json.Marshal(tokenStorage{AccessToken: "secret", UserID: "u"})
	req, _ := json.Marshal(executorRequest{Model: "codebuddy/glm", Payload: []byte(`{"messages":[]}`), StorageJSON: storageJSON})
	raw, status := runtime.Call("executor.execute", req)
	if status == 0 {
		t.Fatalf("status = 0, response = %s", raw)
	}
	var envelope nativeabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.Scope != "" || envelope.Error.RetryAfterMS == nil || *envelope.Error.RetryAfterMS != 7000 {
		t.Fatalf("failure = %#v", envelope.Error)
	}
}

func TestDecodeStorageSupportsNativeAndLegacyMetadata(t *testing.T) {
	native, _ := json.Marshal(tokenStorage{AccessToken: "native", UserID: "u"})
	if got, err := decodeStorage(native, nil); err != nil || got.AccessToken != "native" {
		t.Fatalf("native: %#v %v", got, err)
	}
	got, err := decodeStorage(nil, map[string]any{"access_token": "legacy", "user_id": "u"})
	if err != nil || got.AccessToken != "legacy" {
		t.Fatalf("legacy: %#v %v", got, err)
	}
}

func TestJWTSubject(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-7"}`))
	if got := jwtSubject("x." + payload + ".y"); got != "user-7" {
		t.Fatalf("got %q", got)
	}
}
