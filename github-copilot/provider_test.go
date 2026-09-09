package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestExecuteExchangesTokenThenUsesResponsesEndpoint(t *testing.T) {
	var calls []map[string]any
	runtime = nativeabi.Runtime{}
	if err := runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
		var outbound map[string]any
		_ = json.Unmarshal(request, &outbound)
		calls = append(calls, outbound)
		var response httpResponse
		if len(calls) == 1 {
			response = httpResponse{StatusCode: 200, Body: []byte(`{"token":"api-token","endpoints":{"api":"https://api.business.githubcopilot.com"}}`)}
		} else {
			response = httpResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"id":"r1","output":[],"usage":{"total_tokens":5}}`)}
		}
		result, _ := json.Marshal(response)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
	stored, _ := json.Marshal(storage{AccessToken: "github-token"})
	req, _ := json.Marshal(request{Model: "github-copilot/gpt-5", SourceFormat: "responses", Payload: []byte(`{"input":"hi"}`), StorageJSON: stored})
	_, callErr := (provider{}).Call("executor.execute", req)
	if callErr != nil {
		t.Fatal(callErr)
	}
	if len(calls) != 2 || calls[0]["url"] != copilotTokenURL || calls[1]["url"] != "https://api.business.githubcopilot.com/responses" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestTrustedEndpointRejectsSpoofedAndHTTPHosts(t *testing.T) {
	for _, raw := range []string{"https://api.githubcopilot.com.evil.test", "http://api.githubcopilot.com", "https://user@api.githubcopilot.com"} {
		if trustedEndpoint(raw) {
			t.Fatalf("trusted %q", raw)
		}
	}
	if !trustedEndpoint("https://api.business.githubcopilot.com") {
		t.Fatal("business endpoint rejected")
	}
}
func TestInitiatorDistinguishesFollowupFromToolLoop(t *testing.T) {
	followup := []byte(`{"messages":[{"role":"assistant","content":"a"},{"role":"user","content":"new question"}]}`)
	if got := initiator(followup); got != "user" {
		t.Fatalf("followup=%q", got)
	}
	tool := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"ok"}]}]}`)
	if got := initiator(tool); got != "agent" {
		t.Fatalf("tool=%q", got)
	}
}

func TestNormalizeResponsesPreservesImagesToolsAndThinking(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,eA=="}}}},
			map[string]any{"role": "assistant", "content": "calling", "tool_calls": []any{map[string]any{"id": "call-1", "function": map[string]any{"name": "inspect", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "done"},
		},
		"tools":        []any{map[string]any{"type": "function", "function": map[string]any{"name": "inspect", "description": "Inspect", "parameters": map[string]any{"type": "object"}}}, map[string]any{"type": "unsupported"}},
		"reasoning":    map[string]any{"effort": "low"},
		"service_tier": "priority",
	}
	normalizeCopilotRequest(body, "/responses")
	input := body["input"].([]any)
	firstContent := input[0].(map[string]any)["content"].([]any)
	if len(firstContent) != 2 || firstContent[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("input = %#v", input)
	}
	if len(body["tools"].([]any)) != 1 || body["service_tier"] != nil || body["store"] != false {
		t.Fatalf("body = %#v", body)
	}
	if body["reasoning"].(map[string]any)["effort"] != "low" || body["reasoning"].(map[string]any)["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}
}

func TestNormalizeChatFlattensAssistantAndReasoningResponse(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}}}, "tools": []any{map[string]any{"type": "function"}, map[string]any{"type": "web_search"}}, "tool_choice": map[string]any{"type": "function"}}
	normalizeCopilotRequest(body, "/chat/completions")
	if body["messages"].([]any)[0].(map[string]any)["content"] != "ab" || len(body["tools"].([]any)) != 1 || body["tool_choice"] != "auto" {
		t.Fatalf("body = %#v", body)
	}
	response := normalizeCopilotReasoning([]byte(`{"choices":[{"message":{"reasoning_text":"why"}},{"message":{"reasoning_text":"ignored","reasoning_content":"kept"}}]}`))
	var decoded map[string]any
	_ = json.Unmarshal(response, &decoded)
	choices := decoded["choices"].([]any)
	if choices[0].(map[string]any)["message"].(map[string]any)["reasoning_content"] != "why" || choices[1].(map[string]any)["message"].(map[string]any)["reasoning_content"] != "kept" {
		t.Fatalf("response = %#v", decoded)
	}
}

func TestSSEReasoningNormalizationHandlesFragmentedEvents(t *testing.T) {
	normalizer := &sseReasoningNormalizer{}
	if got := normalizer.Push([]byte(`data: {"choices":[{"delta":{"reasoning_`), false); len(got) != 0 {
		t.Fatalf("premature output = %q", got)
	}
	got := normalizer.Push([]byte("text\":\"why\"}}]}\n\ndata: [DONE]\n\n"), true)
	if !strings.Contains(string(got), `"reasoning_content":"why"`) || !strings.Contains(string(got), "data: [DONE]") {
		t.Fatalf("normalized stream = %q", got)
	}
}
func TestCredentialsAcceptsStorageAndLegacyMetadata(t *testing.T) {
	raw, _ := json.Marshal(storage{AccessToken: "native"})
	if got, err := credentials(raw, nil); err != nil || got.AccessToken != "native" {
		t.Fatalf("native=%#v %v", got, err)
	}
	if got, err := credentials(nil, map[string]any{"access_token": "legacy"}); err != nil || got.AccessToken != "legacy" {
		t.Fatalf("legacy=%#v %v", got, err)
	}
}

func TestRPCUpstreamThrottlePreservesCooldownClassification(t *testing.T) {
	var calls int
	runtime = nativeabi.Runtime{}
	if err := runtime.Initialize(provider{}, func(_ string, _ []byte) ([]byte, int) {
		calls++
		response := httpResponse{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": {"11"}}, Body: []byte(`{"error":"limited"}`)}
		if calls == 1 {
			response = httpResponse{StatusCode: http.StatusOK, Body: []byte(`{"token":"api-token"}`)}
		}
		result, _ := json.Marshal(response)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
	stored, _ := json.Marshal(storage{AccessToken: "github-token"})
	req, _ := json.Marshal(request{Model: "github-copilot/gpt-5", Payload: []byte(`{"messages":[]}`), StorageJSON: stored})
	raw, status := runtime.Call("executor.execute", req)
	var envelope nativeabi.Envelope
	if status == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Error == nil || envelope.Error.Scope != "" || envelope.Error.RetryAfterMS == nil || *envelope.Error.RetryAfterMS != 11000 {
		t.Fatalf("status=%d failure=%#v raw=%s", status, envelope.Error, raw)
	}
}

func TestCountTokensUsesLocalTokenizerWithoutAuth(t *testing.T) {
	req, _ := json.Marshal(request{Model: "github-copilot/gpt-5", SourceFormat: "chat-completions", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)})
	result, callErr := countTokens(req)
	if callErr != nil {
		t.Fatal(callErr)
	}
	var response struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &response); err != nil {
		t.Fatal(err)
	}
	if response.Usage.PromptTokens != 3 {
		t.Fatalf("prompt tokens = %d, want 3", response.Usage.PromptTokens)
	}
}
