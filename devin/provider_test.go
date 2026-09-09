package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

var testHost struct {
	sync.Mutex
	call func(string, []byte) (any, *nativeabi.Error)
}

func TestMain(m *testing.M) {
	err := runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
		testHost.Lock()
		call := testHost.call
		testHost.Unlock()
		if call == nil {
			return []byte(`{"ok":false,"error":{"code":"test","message":"unexpected host callback"}}`), 1
		}
		result, failure := call(method, request)
		raw, _ := json.Marshal(nativeabi.Envelope{OK: failure == nil, Error: failure, Result: mustRaw(result)})
		if failure != nil {
			return raw, 1
		}
		return raw, 0
	})
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func mustRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
func setHost(t *testing.T, call func(string, []byte) (any, *nativeabi.Error)) {
	t.Helper()
	testHost.Lock()
	testHost.call = call
	testHost.Unlock()
	t.Cleanup(func() { testHost.Lock(); testHost.call = nil; testHost.Unlock() })
}

func TestConnectFramingAndCodecs(t *testing.T) {
	tool := toolCall{"call-7", "lookup", `{"q":"x"}`}
	usageRaw := uintField(uintField(uintField(nil, 2, 11), 3, 5), 5, 3)
	p := stringField(nil, 1, "message-1")
	p = stringField(p, 3, "hello")
	p = bytesField(p, 6, encodeToolCall(tool))
	p = bytesField(p, 7, usageRaw)
	p = uintField(p, 5, 2)
	frame := connectFrame(p)
	flag, got, rest, complete, err := nextConnectFrame(frame)
	if err != nil || !complete || flag != 0 || len(rest) != 0 {
		t.Fatalf("frame parse: complete=%v flag=%d rest=%d err=%v", complete, flag, len(rest), err)
	}
	c, err := parseResponse(got)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "message-1" || c.Text != "hello" || c.Stop != 2 || len(c.ToolCalls) != 1 || c.ToolCalls[0] != tool || c.Usage.Input != 11 || c.Usage.Output != 5 || c.Usage.CacheRead != 3 {
		t.Fatalf("chunk = %#v", c)
	}
	if _, _, _, ok, err := nextConnectFrame(frame[:4]); err != nil || ok {
		t.Fatalf("partial frame complete=%v err=%v", ok, err)
	}
	tooLarge := make([]byte, 5)
	binary.BigEndian.PutUint32(tooLarge[1:], maxConnectFrame+1)
	if _, _, _, _, err = nextConnectFrame(tooLarge); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestBuildUpstreamPreservesMessagesToolsAndConfiguration(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":[{"type":"text","text":"ask"}]},{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"answer"}],"tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{"type":"object"}}}],"max_tokens":321,"temperature":0.25,"top_p":0.75}`)
	raw, f := buildUpstream(payload, "glm-5-2", "devin-session-token$secret")
	if f != nil {
		t.Fatal(f)
	}
	_, proto, _, ok, err := nextConnectFrame(raw)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !strings.Contains(string(proto), "devin-session-token$secret") || !strings.Contains(string(proto), "lookup") || !strings.Contains(string(proto), "answer") {
		t.Fatalf("protobuf omitted source fields: %q", proto)
	}
}

func TestExecuteNonStreamUsesHostCallbackAndAggregates(t *testing.T) {
	response := stringField(nil, 1, "id-1")
	response = stringField(response, 3, "hi")
	response = bytesField(response, 7, uintField(uintField(nil, 2, 9), 3, 4))
	response = uintField(response, 5, 2)
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		if method != nativeabi.MethodHostHTTPDo {
			t.Fatalf("method=%s", method)
		}
		var req struct {
			URL            string
			Headers        http.Header
			Body           []byte
			HostCallbackID string `json:"host_callback_id"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.HostCallbackID != "callback-9" || req.URL != chatURL || req.Headers.Get("Authorization") != "Basic devin-session-token$tok-devin-session-token$tok" {
			t.Fatalf("request=%+v", req)
		}
		return httpResponse{StatusCode: 200, Headers: http.Header{"X-Test": {"yes"}}, Body: connectFrame(response)}, nil
	})
	s, _ := json.Marshal(storage{Token: "tok"})
	raw, _ := json.Marshal(executorRequest{Model: "devin/glm-5-2", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), StorageJSON: s, HostCallbackID: "callback-9"})
	result, f := execute(raw, false)
	if f != nil {
		t.Fatal(f)
	}
	encoded, _ := json.Marshal(result)
	var out struct{ Payload []byte }
	if json.Unmarshal(encoded, &out) != nil {
		t.Fatal("decode result")
	}
	var body map[string]any
	if json.Unmarshal(out.Payload, &body) != nil {
		t.Fatalf("payload=%s", out.Payload)
	}
	if body["id"] != "chatcmpl-id-1" || int(body["usage"].(map[string]any)["total_tokens"].(float64)) != 13 {
		t.Fatalf("body=%s", out.Payload)
	}
}

func TestAuthAndTypedFailures(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"Provider": "devin", "FileName": "devin-a.json", "RawJSON": []byte(`{"api_key":"abc"}`)})
	result, f := parseAuth(raw)
	if f != nil {
		t.Fatal(f)
	}
	encoded, _ := json.Marshal(result)
	var parsed struct{ Auth authData }
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	var saved storage
	if err := json.Unmarshal(parsed.Auth.StorageJSON, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Token != "devin-session-token$abc" {
		t.Fatalf("auth=%s", encoded)
	}
	_, f = execute([]byte(`{"Payload":"e30="}`), false)
	if f == nil || f.Scope != "credential" || f.HTTPStatus != 401 {
		t.Fatalf("failure=%+v", f)
	}
}

func TestForwardSplitFramesAndPanicCloseExactlyOnce(t *testing.T) {
	p := stringField(stringField(nil, 1, "m"), 3, "chunk")
	framed := connectFrame(p)
	reads, pluginCloses, hostCloses, emits := 0, 0, 0, 0
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		switch method {
		case nativeabi.MethodHostHTTPStreamRead:
			reads++
			if reads == 1 {
				return hostStreamRead{Payload: framed[:3]}, nil
			}
			return hostStreamRead{Payload: framed[3:], Done: true}, nil
		case nativeabi.MethodHostStreamEmit:
			emits++
			return struct{}{}, nil
		case nativeabi.MethodHostStreamClose:
			pluginCloses++
			return struct{}{}, nil
		case nativeabi.MethodHostHTTPStreamClose:
			hostCloses++
			return struct{}{}, nil
		default:
			t.Fatalf("method=%s", method)
		}
		return nil, nil
	})
	forward("plugin", "host", "devin/model")
	if emits != 1 || pluginCloses != 1 || hostCloses != 1 {
		t.Fatalf("emits=%d plugin closes=%d host closes=%d", emits, pluginCloses, hostCloses)
	}

	pluginCloses, hostCloses = 0, 0
	setHost(t, func(method string, raw []byte) (any, *nativeabi.Error) {
		switch method {
		case nativeabi.MethodHostHTTPStreamRead:
			panic("private stream data")
		case nativeabi.MethodHostStreamClose:
			pluginCloses++
			return struct{}{}, nil
		case nativeabi.MethodHostHTTPStreamClose:
			hostCloses++
			return struct{}{}, nil
		}
		return struct{}{}, nil
	})
	forward("plugin", "host", "devin/model")
	if pluginCloses != 1 || hostCloses != 1 {
		t.Fatalf("panic closes: plugin=%d host=%d", pluginCloses, hostCloses)
	}
}
