package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestExecuteUsesHostHTTPAndAggregatesStream(t *testing.T) {
	var outbound map[string]any
	runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
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
	t.Cleanup(runtime.Shutdown)
	storageJSON, _ := json.Marshal(tokenStorage{AccessToken: "secret", UserID: "u"})
	req, _ := json.Marshal(executorRequest{Model: "codebuddy/glm", Payload: []byte(`{"model":"ignored","messages":[{"role":"user","content":"hi"}]}`), StorageJSON: storageJSON})
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
