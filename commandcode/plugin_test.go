package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestBuildPayloadPreservesToolsImagesAndReasoning(t *testing.T) {
	input := []byte(`{"messages":[{"role":"system","content":"be concise"},{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}],"reasoning_effort":"high","max_tokens":123}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "xai/grok-4.5", Payload: input, WorkingDir: "/repo", Environment: "test", ThreadID: "11111111-1111-1111-1111-111111111111", Now: func() time.Time { return time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(payload)
	if root.Get("config.workingDir").String() != "/repo" || root.Get("params.max_tokens").Int() != 123 {
		t.Fatalf("payload = %s", payload)
	}
	if root.Get("params.messages.0.content.1.type").String() != "image" {
		t.Fatalf("image missing: %s", payload)
	}
	if root.Get("params.tools.0.input_schema.required.0").String() != "q" {
		t.Fatalf("tool schema missing: %s", payload)
	}
	if root.Get("params.reasoning_effort").String() != "high" {
		t.Fatalf("reasoning missing: %s", payload)
	}
}

func TestIncrementalNDJSONProducesUsageAndToolCall(t *testing.T) {
	state := newCommandCodeStreamState("deepseek/deepseek-v4-flash")
	lines := []string{
		`{"type":"text-delta","text":"Hi"}`,
		`{"type":"tool-input-start","id":"call_1","toolName":"lookup"}`,
		`{"type":"tool-input-delta","id":"call_1","delta":"{\"q\":\"go\"}"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"inputTokenDetails":{"cacheReadTokens":4},"outputTokens":2,"outputTokenDetails":{"reasoningTokens":1},"totalTokens":12}}`,
	}
	var chunks [][]byte
	for _, line := range lines {
		out, _, err := commandCodeLineToOpenAIChunks([]byte(line), state)
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, out...)
	}
	if len(chunks) != 4 || gjson.GetBytes(chunks[0], "choices.0.delta.content").String() != "Hi" {
		t.Fatalf("chunks = %q", chunks)
	}
	last := chunks[len(chunks)-1]
	if gjson.GetBytes(last, "choices.0.finish_reason").String() != "tool_calls" || gjson.GetBytes(last, "usage.prompt_tokens_details.cached_tokens").Int() != 4 || gjson.GetBytes(last, "usage.total_tokens").Int() != 12 {
		t.Fatalf("finish = %s", last)
	}
}

func TestConfiguredModelsApplyAliasAndExclusion(t *testing.T) {
	models := configuredModels(getCommandCodeModels(), hostConfig{OAuthModelAlias: map[string][]modelAlias{"commandcode": {{Name: "deepseek/deepseek-v4-flash", Alias: "flash"}}}, ExcludedModels: map[string][]string{"commandcode": {"claude-*"}}})
	var alias bool
	for _, model := range models {
		if strings.HasPrefix(model.ID, "claude-") {
			t.Fatalf("excluded model %q present", model.ID)
		}
		if model.ID == "flash" && model.Name == "deepseek/deepseek-v4-flash" {
			alias = true
		}
	}
	if !alias {
		t.Fatal("configured alias missing")
	}
}

func TestFetchModelsDecodesBufferedHostHTTPWireShape(t *testing.T) {
	var runtime nativeabi.Runtime
	if err := runtime.Initialize(nil, func(method string, _ []byte) ([]byte, int) {
		if method != nativeabi.MethodHostHTTPDo {
			t.Fatalf("method = %q", method)
		}
		result := bufferedHTTPResponse{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"data":[{"id":"live/only-model","name":"Live Only","context_length":32000}]}`)}
		body, _ := json.Marshal(nativeabi.Envelope{OK: true, Result: mustJSON(result)})
		return body, 0
	}); err != nil {
		t.Fatal(err)
	}
	models := (&plugin{runtime: &runtime}).fetchModels(authModelRequest{Attributes: map[string]string{"api_key": "secret", "base_url": "https://example.test"}})
	if len(models) != 1 || models[0].ID != "live/only-model" || models[0].MaxCompletionTokens != 32000 {
		t.Fatalf("models = %+v", models)
	}
}

func TestCountTokensIsNonzeroAndDeterministic(t *testing.T) {
	request := executorRequest{Model: "gpt-5", Payload: []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)}
	first, firstErr := countTokens(request)
	second, secondErr := countTokens(request)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("errors = %v, %v", firstErr, secondErr)
	}
	firstCount := gjson.GetBytes(first.(executorResponse).Payload, "usage.prompt_tokens").Int()
	secondCount := gjson.GetBytes(second.(executorResponse).Payload, "usage.prompt_tokens").Int()
	if firstCount <= 0 || firstCount != secondCount {
		t.Fatalf("counts = %d, %d", firstCount, secondCount)
	}
}

func TestExecuteStreamReturnsBeforeIncrementalUpstreamCompletes(t *testing.T) {
	var mu sync.Mutex
	var emitted [][]byte
	closed := make(chan nativeabi.StreamCloseRequest, 1)
	release := make(chan struct{})
	reads := 0
	var runtime nativeabi.Runtime
	if err := runtime.Initialize(nil, func(method string, request []byte) ([]byte, int) {
		result := any(struct{}{})
		switch method {
		case nativeabi.MethodHostHTTPDoStream:
			result = streamHTTPResponse{StatusCode: 200, StreamID: "upstream", Headers: map[string][]string{"Content-Type": {"text/event-stream"}}}
		case nativeabi.MethodHostHTTPStreamRead:
			reads++
			if reads == 1 {
				<-release
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"text-delta\",\"text\":\"first\"}\n")}
			} else {
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"), Done: true}
			}
		case nativeabi.MethodHostStreamEmit:
			var emit nativeabi.StreamEmitRequest
			_ = json.Unmarshal(request, &emit)
			mu.Lock()
			emitted = append(emitted, emit.Payload)
			mu.Unlock()
		case nativeabi.MethodHostStreamClose:
			var closeRequest nativeabi.StreamCloseRequest
			_ = json.Unmarshal(request, &closeRequest)
			closed <- closeRequest
		}
		body, _ := json.Marshal(nativeabi.Envelope{OK: true, Result: mustJSON(result)})
		return body, 0
	}); err != nil {
		t.Fatal(err)
	}
	p := &plugin{runtime: &runtime}
	request := executorRequest{Model: "deepseek/deepseek-v4-flash", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), AuthAttributes: map[string]string{"api_key": "secret", "base_url": "https://example.test"}, StreamID: "downstream", HostCallbackID: "callback"}
	returned := make(chan struct{})
	go func() {
		_, callErr := p.executeStream(request)
		if callErr != nil {
			t.Errorf("executeStream: %v", callErr)
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("executeStream buffered until upstream completion")
	}
	close(release)
	select {
	case closeRequest := <-closed:
		if closeRequest.Failure != nil {
			t.Fatalf("close failure = %+v", closeRequest.Failure)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(emitted) < 3 || gjson.GetBytes(emitted[0], "choices.0.delta.content").String() != "first" || string(emitted[len(emitted)-1]) != "[DONE]" {
		t.Fatalf("emitted = %q", emitted)
	}
}

func TestAbortClosesWithRequestScopedFailure(t *testing.T) {
	state := newCommandCodeStreamState("model")
	_, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"abort","reason":"user canceled"}`), state)
	if err == nil {
		t.Fatal("abort error = nil")
	}
	f := failure(err)
	if f.Scope != "request" || f.Retryable || f.Code != "aborted" {
		t.Fatalf("failure = %+v", f)
	}
}

func TestExecuteContinuesPauseTurnOnSameThread(t *testing.T) {
	var runtime nativeabi.Runtime
	var sessions, threadIDs []string
	openCount := 0
	if err := runtime.Initialize(nil, func(method string, request []byte) ([]byte, int) {
		result := any(struct{}{})
		switch method {
		case nativeabi.MethodHostHTTPDoStream:
			var upstream httpRequest
			_ = json.Unmarshal(request, &upstream)
			openCount++
			sessions = append(sessions, upstream.Headers.Get("x-session-id"))
			threadIDs = append(threadIDs, gjson.GetBytes(upstream.Body, "threadId").String())
			result = streamHTTPResponse{StatusCode: 200, StreamID: string(rune('0' + openCount))}
		case nativeabi.MethodHostHTTPStreamRead:
			var read map[string]string
			_ = json.Unmarshal(request, &read)
			if read["stream_id"] == "1" {
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"text-delta\",\"text\":\"A\"}\ndata: {\"type\":\"finish\",\"finishReason\":\"pause_turn\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"), Done: true}
			} else {
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"text-delta\",\"text\":\"B\"}\ndata: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"), Done: true}
			}
		}
		body, _ := json.Marshal(nativeabi.Envelope{OK: true, Result: mustJSON(result)})
		return body, 0
	}); err != nil {
		t.Fatal(err)
	}
	p := &plugin{runtime: &runtime}
	result, executeErr := p.execute(executorRequest{Model: "deepseek/deepseek-v4-flash", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), AuthAttributes: map[string]string{"api_key": "secret", "base_url": "https://example.test"}})
	if executeErr != nil {
		t.Fatal(executeErr)
	}
	payload := result.(executorResponse).Payload
	if openCount != 2 || gjson.GetBytes(payload, "choices.0.message.content").String() != "AB" || gjson.GetBytes(payload, "usage.total_tokens").Int() != 4 {
		t.Fatalf("opens=%d payload=%s", openCount, payload)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] || threadIDs[0] != threadIDs[1] || sessions[0] != threadIDs[0] {
		t.Fatalf("sessions=%v threads=%v", sessions, threadIDs)
	}
}

func TestExecuteStreamContinuesPauseTurnWithoutClientSession(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var sessions []string
	openCount := 0
	closed := make(chan nativeabi.StreamCloseRequest, 1)
	var runtime nativeabi.Runtime
	if err := runtime.Initialize(nil, func(method string, request []byte) ([]byte, int) {
		result := any(struct{}{})
		switch method {
		case nativeabi.MethodHostHTTPDoStream:
			var upstream httpRequest
			_ = json.Unmarshal(request, &upstream)
			mu.Lock()
			openCount++
			bodies = append(bodies, append([]byte(nil), upstream.Body...))
			sessions = append(sessions, upstream.Headers.Get("x-session-id"))
			streamID := string(rune('0' + openCount))
			mu.Unlock()
			result = streamHTTPResponse{StatusCode: 200, StreamID: streamID}
		case nativeabi.MethodHostHTTPStreamRead:
			var read map[string]string
			_ = json.Unmarshal(request, &read)
			if read["stream_id"] == "1" {
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"finish\",\"finishReason\":\"pause_turn\"}\n"), Done: true}
			} else {
				result = streamReadResponse{Payload: []byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\"}\n"), Done: true}
			}
		case nativeabi.MethodHostStreamClose:
			var closeRequest nativeabi.StreamCloseRequest
			_ = json.Unmarshal(request, &closeRequest)
			closed <- closeRequest
		}
		body, _ := json.Marshal(nativeabi.Envelope{OK: true, Result: mustJSON(result)})
		return body, 0
	}); err != nil {
		t.Fatal(err)
	}
	p := &plugin{runtime: &runtime}
	_, executeErr := p.executeStream(executorRequest{Model: "gpt-5.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), AuthAttributes: map[string]string{"api_key": "secret", "base_url": "https://example.test"}, StreamID: "downstream"})
	if executeErr != nil {
		t.Fatal(executeErr)
	}
	select {
	case closeRequest := <-closed:
		if closeRequest.Failure != nil {
			t.Fatalf("close = %+v", closeRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close")
	}
	mu.Lock()
	defer mu.Unlock()
	if openCount != 2 || len(bodies) != 2 || !json.Valid(bodies[0]) || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("opens=%d bodies=%q", openCount, bodies)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] || sessions[0] != gjson.GetBytes(bodies[0], "threadId").String() {
		t.Fatalf("sessions=%v thread=%q", sessions, gjson.GetBytes(bodies[0], "threadId").String())
	}
}

func TestUpstreamFailureScopesAndRetryHint(t *testing.T) {
	f := upstreamFailure(429, "limited", map[string][]string{"Retry-After": {"2"}})
	if f.Scope != "credential" || !f.Retryable || f.RetryAfterMS == nil || *f.RetryAfterMS != 2000 {
		t.Fatalf("429 failure = %+v", f)
	}
	if request := upstreamFailure(400, "bad", nil); request.Scope != "request" {
		t.Fatalf("400 failure = %+v", request)
	}
	if model := upstreamFailure(404, "missing", nil); model.Scope != "model" {
		t.Fatalf("404 failure = %+v", model)
	}
}

func TestCredentialAliasesAndCanonicalEnvironmentPrecedence(t *testing.T) {
	t.Setenv("COMMAND_CODE_API_KEY", "canonical")
	t.Setenv("COMMANDCODE_API_KEY", "legacy")
	t.Setenv("COMMANDCODE_API_URL", "https://canonical.example")
	t.Setenv("COMMANDCODE_API_BASE", "https://legacy.example")
	baseURL, apiKey := credentials(nil, nil, nil)
	if baseURL != "https://canonical.example" || apiKey != "canonical" {
		t.Fatalf("environment credentials = %q %q", baseURL, apiKey)
	}
	baseURL, apiKey = credentials([]byte(`{"baseURL":"https://stored.example","commandcode":{"access":"nested"}}`), nil, nil)
	if baseURL != "https://stored.example" || apiKey != "nested" {
		t.Fatalf("stored credentials = %q %q", baseURL, apiKey)
	}
}

func TestAsyncStreamPanicClosesOnceWithoutLeakingValue(t *testing.T) {
	closed := make(chan nativeabi.StreamCloseRequest, 2)
	var runtime nativeabi.Runtime
	if err := runtime.Initialize(nil, func(method string, request []byte) ([]byte, int) {
		result := any(struct{}{})
		switch method {
		case nativeabi.MethodHostHTTPDoStream:
			result = streamHTTPResponse{StatusCode: 200, StreamID: "upstream"}
		case nativeabi.MethodHostHTTPStreamRead:
			result = streamReadResponse{Payload: []byte("data: {\"type\":\"text-delta\",\"text\":\"trigger\"}\n"), Done: true}
		case nativeabi.MethodHostStreamEmit:
			panic("sensitive panic value")
		case nativeabi.MethodHostStreamClose:
			var closeRequest nativeabi.StreamCloseRequest
			_ = json.Unmarshal(request, &closeRequest)
			closed <- closeRequest
		}
		body, _ := json.Marshal(nativeabi.Envelope{OK: true, Result: mustJSON(result)})
		return body, 0
	}); err != nil {
		t.Fatal(err)
	}
	p := &plugin{runtime: &runtime}
	_, executeErr := p.executeStream(executorRequest{Model: "gpt-5.5", Payload: []byte(`{"messages":[]}`), AuthAttributes: map[string]string{"api_key": "secret", "base_url": "https://example.test"}, StreamID: "downstream"})
	if executeErr != nil {
		t.Fatal(executeErr)
	}
	select {
	case closeRequest := <-closed:
		if closeRequest.Failure == nil || closeRequest.Failure.Code != "plugin_panic" || strings.Contains(closeRequest.Failure.Message, "sensitive") {
			t.Fatalf("close = %+v", closeRequest)
		}
	case <-time.After(time.Second):
		t.Fatal("panic did not close stream")
	}
	select {
	case duplicate := <-closed:
		t.Fatalf("duplicate close = %+v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func mustJSON(value any) json.RawMessage { body, _ := json.Marshal(value); return body }
