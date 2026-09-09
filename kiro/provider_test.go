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
	"time"
)

type transportFixture struct {
	requests  []HTTPRequest
	responses []HTTPResponse
	streams   []HTTPStreamResponse
}

func (f *transportFixture) Do(_ context.Context, req HTTPRequest) (HTTPResponse, error) {
	f.requests = append(f.requests, req)
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response, nil
}
func (f *transportFixture) DoStream(_ context.Context, req HTTPRequest) (HTTPStreamResponse, error) {
	f.requests = append(f.requests, req)
	response := f.streams[0]
	f.streams = f.streams[1:]
	return response, nil
}

func TestKiroPayloadAndFingerprintParity(t *testing.T) {
	claude := []byte(`{"system":"be precise","tools":[{"name":"read","description":"read a file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"old"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{"path":"a"}}]},{"role":"user","content":"new"}]}`)
	first, err := buildKiroPayload(claude, "claude", "claude-sonnet-4.5", "arn:aws:codewhisperer:us-west-2:1:profile/x", "AI_EDITOR")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := buildKiroPayload(claude, "claude", "claude-sonnet-4.5", "arn:aws:codewhisperer:us-west-2:1:profile/x", "AI_EDITOR")
	if !bytes.Equal(first, second) {
		t.Fatal("payload is not deterministic")
	}
	var payload map[string]any
	if err := json.Unmarshal(first, &payload); err != nil {
		t.Fatal(err)
	}
	state := payload["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if current["modelId"] != "claude-sonnet-4.5" || !strings.Contains(current["content"].(string), "<system-reminder>") {
		t.Fatalf("current = %#v", current)
	}
	history := state["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history count = %d", len(history))
	}
	openAI := []byte(`{"tools":[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"go"}]}`)
	converted, err := buildKiroPayload(openAI, "openai", "m", "", "AI_EDITOR")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"name":"shell"`) || !strings.Contains(string(converted), `"inputSchema":{"json":{"type":"object"}}`) {
		t.Fatalf("OpenAI tool conversion = %s", converted)
	}
	fp1, fp2 := fingerprint("account"), fingerprint("account")
	if fp1 != fp2 || !strings.Contains(fp1.UserAgent(), "api/codewhispererstreaming#1.0.27") {
		t.Fatalf("fingerprints = %+v %+v", fp1, fp2)
	}
}

func TestKiroEventStreamSplitLargeErrorAndUsage(t *testing.T) {
	frames := append(kiroFrame(map[string]string{":event-type": "assistantResponseEvent"}, []byte(`{"assistantResponseEvent":{"content":"hello"}}`)), kiroFrame(map[string]string{":event-type": "messageMetadataEvent"}, []byte(`{"messageMetadataEvent":{"tokenUsage":{"uncachedInputTokens":7,"cacheReadInputTokens":2,"cacheWriteInputTokens":3,"outputTokens":11}}}`))...)
	frames = append(frames, kiroFrame(map[string]string{":event-type": "toolUseEvent"}, []byte(`{"toolUseEvent":{"toolUseId":"tool-1","name":"read","input":"{\"path\":"}}`))...)
	frames = append(frames, kiroFrame(map[string]string{":event-type": "toolUseEvent"}, []byte(`{"toolUseEvent":{"input":"\"a.txt\"}","stop":true}}`))...)
	result, err := parseKiroEvents(&splitReader{data: frames, size: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello" || result.Usage != (Usage{Input: 7, Output: 11, CacheRead: 2, CacheWrite: 3}) {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "read" || result.Tools[0].Input["path"] != "a.txt" {
		t.Fatalf("fragmented tool = %+v", result.Tools)
	}
	large := bytes.Repeat([]byte("x"), 1<<20)
	msg, err := readEventMessage(&splitReader{data: kiroFrame(nil, large), size: 5})
	if err != nil || !bytes.Equal(msg.Payload, large) {
		t.Fatalf("large message len=%d err=%v", len(msg.Payload), err)
	}
	exception := kiroFrame(map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException"}, []byte(`{"message":"slow"}`))
	_, err = parseKiroEvents(bytes.NewReader(exception), nil)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "ThrottlingException" || !providerErr.Retryable {
		t.Fatalf("error = %#v", err)
	}
}

func TestExecuteStreamCancellationAndUsage(t *testing.T) {
	frames := append(kiroFrame(map[string]string{":event-type": "assistantResponseEvent"}, []byte(`{"assistantResponseEvent":{"content":"chunk"}}`)), kiroFrame(map[string]string{":event-type": "messageMetadataEvent"}, []byte(`{"messageMetadataEvent":{"tokenUsage":{"outputTokens":4}}}`))...)
	fixture := &transportFixture{streams: []HTTPStreamResponse{{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(frames))}}}
	provider := NewProvider(fixture)
	req := Request{Model: "kiro-claude-sonnet-4-5", SourceFormat: "claude", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Token: Token{AccessToken: "secret", AuthMethod: "builder-id"}}
	var output bytes.Buffer
	if _, err := provider.ExecuteStream(context.Background(), req, func(chunk []byte) error { output.Write(chunk); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"text":"chunk"`) || !strings.Contains(output.String(), `"output_tokens":4`) {
		t.Fatalf("stream = %s", output.String())
	}
	if got := fixture.requests[0].Headers["Authorization"][0]; got != "Bearer secret" {
		t.Fatalf("auth = %q", got)
	}
	fixture.streams = []HTTPStreamResponse{{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(frames))}}
	want := errors.New("consumer canceled")
	_, err := provider.ExecuteStream(context.Background(), req, func([]byte) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestTokenParseAndRefreshVariants(t *testing.T) {
	token, err := parseToken([]byte(`{"type":"kiro","accessToken":"old","refreshToken":"refresh","profileArn":"arn"}`))
	if err != nil || token.AccessToken != "old" || token.RefreshToken != "refresh" {
		t.Fatalf("token=%+v err=%v", token, err)
	}
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	social := &transportFixture{responses: []HTTPResponse{{StatusCode: 200, Body: []byte(`{"accessToken":"new","refreshToken":"next","expiresIn":120}`)}}}
	updated, err := refreshToken(context.Background(), social, token, now)
	if err != nil {
		t.Fatal(err)
	}
	if social.requests[0].URL != "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken" || updated.ExpiresAt != "2030-01-02T03:06:05Z" {
		t.Fatalf("social request=%+v token=%+v", social.requests[0], updated)
	}
	oidcToken := Token{RefreshToken: "r", ClientID: "id", ClientSecret: "secret", AuthMethod: "idc", Region: "eu-west-1"}
	oidc := &transportFixture{responses: []HTTPResponse{{StatusCode: 200, Body: []byte(`{"accessToken":"aws","refreshToken":"r2","expiresIn":3600}`)}}}
	if _, err := refreshToken(context.Background(), oidc, oidcToken, now); err != nil {
		t.Fatal(err)
	}
	if oidc.requests[0].URL != "https://oidc.eu-west-1.amazonaws.com/token" || !strings.Contains(string(oidc.requests[0].Body), `"grantType":"refresh_token"`) {
		t.Fatalf("OIDC request = %+v", oidc.requests[0])
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
func kiroFrame(headers map[string]string, payload []byte) []byte {
	var h []byte
	for name, value := range headers {
		h = append(h, byte(len(name)))
		h = append(h, name...)
		h = append(h, 7, byte(len(value)>>8), byte(len(value)))
		h = append(h, value...)
	}
	total := 16 + len(h) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(h)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], h)
	copy(frame[12+len(h):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
	return frame
}
