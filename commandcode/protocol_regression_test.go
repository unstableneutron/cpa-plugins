package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// These cases are adapted from commandcode_executor_test.go at the pinned Plus
// provenance commit and guard behavior that previously regressed in the facade.
func TestProtocolCapabilityGatesImagesAndReasoning(t *testing.T) {
	textOnly := []byte(`{"reasoning_effort":"low","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "deepseek/deepseek-v4-flash", Payload: textOnly})
	if err != nil {
		t.Fatal(err)
	}
	if part := gjson.GetBytes(payload, "params.messages.0.content.0"); part.Get("type").String() != "text" || !strings.Contains(part.Get("text").String(), "image omitted") {
		t.Fatalf("text-only image = %s", part.Raw)
	}
	if gjson.GetBytes(payload, "params.reasoning_effort").Exists() {
		t.Fatalf("unsupported reasoning was forwarded: %s", payload)
	}
}

func TestProtocolToolInputCoercion(t *testing.T) {
	schema := map[string]any{"required": []any{"paths"}, "properties": map[string]any{"paths": map[string]any{"type": "array"}}}
	input := commandCodeToolInput(json.RawMessage(`"README.md"`), schema)
	paths, ok := input["paths"].([]string)
	if !ok || len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("coerced input = %#v", input)
	}
	state := newCommandCodeStreamState("gpt-5.5")
	state.toolSchemas = map[string]map[string]any{"read_files": schema}
	chunks, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolCallId":"call_1","toolName":"read_files","input":"README.md"}`), state)
	if err != nil || len(chunks) != 1 || gjson.Get(gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String(), "paths.0").String() != "README.md" {
		t.Fatalf("chunks=%q err=%v", chunks, err)
	}
}

func TestProtocolStructuredErrorsPreserveStatus(t *testing.T) {
	for _, testCase := range []struct {
		line string
		want int
	}{
		{`{"type":"error","statusCode":429,"error":{"message":"rate limited"}}`, 429},
		{`{"type":"error","message":"unavailable","statusCode":503}`, 503},
		{`{"type":"error","error":{"message":"unknown"}}`, 500},
		{`{"type":"error","statusCode":200,"message":"not successful"}`, 502},
	} {
		_, _, err := commandCodeLineToOpenAIChunks([]byte(testCase.line), newCommandCodeStreamState("gpt-5.5"))
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != testCase.want {
			t.Fatalf("line=%s error=%v want=%d", testCase.line, err, testCase.want)
		}
	}
}

func TestProtocolDoneCannotHideTruncatedStream(t *testing.T) {
	reader := strings.NewReader(`{"type":"text-delta","text":"partial"}` + "\n[DONE]\n")
	if _, _, err := collectCommandCodeResponse(context.Background(), reader, "gpt-5.5"); err == nil {
		t.Fatal("text followed only by DONE was accepted")
	}
}

func TestProtocolNDJSONSafetyLimit(t *testing.T) {
	large := strings.Repeat("x", 1024*1024)
	response, _, err := collectCommandCodeResponse(context.Background(), strings.NewReader(`{"type":"text-delta","text":"`+large+`"}`+"\n"+`{"type":"finish","finishReason":"stop"}`+"\n"), "gpt-5.5")
	if err != nil || len(gjson.GetBytes(response, "choices.0.message.content").String()) != len(large) {
		t.Fatalf("large valid line failed: %v", err)
	}
	called := false
	err = commandCodeReadLines(strings.NewReader(strings.Repeat("x", commandCodeMaxStreamLineBytes+1)), func([]byte) error { called = true; return nil })
	status, ok := err.(interface{ StatusCode() int })
	if called || !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("oversized line err=%v called=%v", err, called)
	}
}
