package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

type transportFixture struct {
	request  HTTPRequest
	response HTTPResponse
	stream   HTTPStreamResponse
}

func (f *transportFixture) Do(_ context.Context, req HTTPRequest) (HTTPResponse, error) {
	f.request = req
	return f.response, nil
}

func (f *transportFixture) DoStream(_ context.Context, req HTTPRequest) (HTTPStreamResponse, error) {
	f.request = req
	return f.stream, nil
}

func TestExecuteConverseStructuredOutputAndBearerAuth(t *testing.T) {
	fixture := &transportFixture{response: HTTPResponse{StatusCode: 200, Body: []byte("{\"output\":{\"message\":{\"content\":[{\"text\":\"```json\\n{\\\"ok\\\":true}\\n```\"}]}},\"stopReason\":\"end_turn\",\"usage\":{\"inputTokens\":11,\"outputTokens\":3}}")}}
	provider := NewProvider(fixture)
	original := []byte(`{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}`)
	response, err := provider.Execute(context.Background(), Request{
		Model: "alias", Payload: []byte(`{"messages":[{"role":"user","content":"answer"}],"max_tokens":42}`), OriginalRequest: original,
		Auth: Auth{BaseURL: "https://bedrock.example/runtime/", APIKey: "secret", ModelMap: map[string]string{"alias": "anthropic.claude"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.request.URL != "https://bedrock.example/runtime/model/anthropic.claude/converse" {
		t.Fatalf("URL = %q", fixture.request.URL)
	}
	if got := fixture.request.Headers["Authorization"][0]; got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
	var upstream map[string]any
	if err := json.Unmarshal(fixture.request.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if _, ok := upstream["system"]; !ok {
		t.Fatalf("structured-output instruction missing: %s", fixture.request.Body)
	}
	if !strings.Contains(string(response.Payload), `\"ok\":true`) {
		t.Fatalf("response = %s", response.Payload)
	}
	openAI := ApplyBedrockStructuredOutputResponse([]byte("{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"ok\\\":true}\\n```\"}}]}"), original)
	if strings.Contains(string(openAI), "```") || !strings.Contains(string(openAI), `\"ok\":true`) {
		t.Fatalf("normalized structured response = %s", openAI)
	}
}

func TestPrepareInvokeAndAuthModes(t *testing.T) {
	req := Request{Model: "m", Payload: []byte(`{"messages":[]}`), Auth: Auth{BaseURL: "https://b", APIKey: "Token x", AuthType: "raw", APIMap: map[string]string{"m": "invoke"}}}
	plan, err := preparePlan(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URL != "https://b/model/m/invoke" || !strings.Contains(string(plan.Payload), `"anthropic_version":"bedrock-2023-05-31"`) {
		t.Fatalf("plan = %+v payload=%s", plan, plan.Payload)
	}
	if got := buildHTTPRequest(plan, req.Auth, false).Headers["Authorization"][0]; got != "Token x" {
		t.Fatalf("raw auth = %q", got)
	}
	req.Auth.AuthType = "none"
	if _, ok := buildHTTPRequest(plan, req.Auth, false).Headers["Authorization"]; ok {
		t.Fatal("none auth emitted Authorization")
	}
}

func TestEventStreamSplitLargeUsageAndException(t *testing.T) {
	frames := append(eventFrame(map[string]string{":event-type": "chunk"}, []byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"hello"}}}`)), eventFrame(map[string]string{":event-type": "metadata"}, []byte(`{"metadata":{"usage":{"inputTokens":7,"outputTokens":9,"totalTokens":16}}}`))...)
	fixture := &transportFixture{stream: HTTPStreamResponse{StatusCode: 200, Headers: map[string][]string{"content-type": {"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(&splitReader{data: frames, size: 3})}}
	var output bytes.Buffer
	err := NewProvider(fixture).ExecuteStream(context.Background(), Request{Model: "m", Payload: []byte(`{"messages":[]}`), Auth: Auth{BaseURL: "https://b"}}, func(chunk []byte) error { output.Write(chunk); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"text":"hello"`) || !strings.Contains(output.String(), `"input_tokens":7`) {
		t.Fatalf("stream = %s", output.String())
	}

	large := bytes.Repeat([]byte("x"), 1<<20)
	count := 0
	if err := ForEachBedrockEventStreamPayload(&splitReader{data: eventFrame(nil, large), size: 7}, func(payload []byte) bool { count = len(payload); return true }); err != nil || count != len(large) {
		t.Fatalf("large frame count=%d err=%v", count, err)
	}

	exception := eventFrame(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"slow down"}`))
	fixture.stream.Body = io.NopCloser(bytes.NewReader(exception))
	err = NewProvider(fixture).ExecuteStream(context.Background(), Request{Model: "m", Payload: []byte(`{"messages":[]}`), Auth: Auth{BaseURL: "https://b"}}, func([]byte) error { return nil })
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "throttlingException" || !providerErr.Retryable {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamEmitCancellation(t *testing.T) {
	frame := eventFrame(nil, []byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"stop"}}}`))
	fixture := &transportFixture{stream: HTTPStreamResponse{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(bytes.NewReader(frame))}}
	want := errors.New("consumer canceled")
	err := NewProvider(fixture).ExecuteStream(context.Background(), Request{Model: "m", Payload: []byte(`{"messages":[]}`), Auth: Auth{BaseURL: "https://b"}}, func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

type splitReader struct {
	data []byte
	size int
}

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func eventFrame(headers map[string]string, payload []byte) []byte {
	var headerBytes []byte
	for name, value := range headers {
		headerBytes = append(headerBytes, byte(len(name)))
		headerBytes = append(headerBytes, name...)
		headerBytes = append(headerBytes, 7, byte(len(value)>>8), byte(len(value)))
		headerBytes = append(headerBytes, value...)
	}
	total := 16 + len(headerBytes) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headerBytes)
	copy(frame[12+len(headerBytes):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}
