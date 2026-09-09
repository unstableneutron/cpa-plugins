package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestExecuteUsesKiloOpenRouterProtocol(t *testing.T) {
	var outbound map[string]any
	runtime = nativeabi.Runtime{}
	if err := runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
		if method != "host.http.do" {
			t.Fatalf("method = %q", method)
		}
		_ = json.Unmarshal(request, &outbound)
		result, _ := json.Marshal(httpResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":9}}`)})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
	stored, _ := json.Marshal(storage{Token: "token", OrganizationID: "org"})
	req, _ := json.Marshal(executorRequest{Model: "kilo/openai/gpt-5(9000)", Payload: []byte(`{"reasoning_effort":"low","messages":[]}`), StorageJSON: stored, Headers: http.Header{"X-Trace": {"from-client"}}, AuthAttributes: map[string]string{"header:X-Static": "set", "header:X-Copied": "$X-Trace"}})
	result, callErr := (provider{}).Call("executor.execute", req)
	if callErr != nil {
		t.Fatal(callErr)
	}
	if string(result.(map[string]any)["Payload"].([]byte)) == "" {
		t.Fatal("empty payload")
	}
	if outbound["url"] != apiBase+"/api/openrouter/chat/completions" {
		t.Fatalf("url = %#v", outbound["url"])
	}
	headers := outbound["headers"].(map[string]any)
	if headers["X-Kilocode-Organizationid"].([]any)[0] != "org" {
		t.Fatalf("headers = %#v", headers)
	}
	if headers["X-Static"].([]any)[0] != "set" || headers["X-Copied"].([]any)[0] != "from-client" {
		t.Fatalf("custom headers = %#v", headers)
	}
	bodyRaw, _ := base64.StdEncoding.DecodeString(outbound["body"].(string))
	var body map[string]any
	_ = json.Unmarshal(bodyRaw, &body)
	if body["model"] != "openai/gpt-5" || body["reasoning_effort"] != "low" {
		t.Fatalf("outbound body = %#v", body)
	}
}

func TestCredentialsNativeAndLegacy(t *testing.T) {
	raw, _ := json.Marshal(storage{Token: "native", OrganizationID: "org"})
	got, err := credentials(raw, nil, nil)
	if err != nil || got.Token != "native" || got.OrganizationID != "org" {
		t.Fatalf("native=%#v err=%v", got, err)
	}
	got, err = credentials(nil, map[string]any{"access_token": "legacy"}, map[string]string{"organization_id": "old-org"})
	if err != nil || got.Token != "legacy" || got.OrganizationID != "old-org" {
		t.Fatalf("legacy=%#v err=%v", got, err)
	}
}
func TestHeadersPreserveOrganizationAndStream(t *testing.T) {
	h := headersFor(storage{Token: "t", OrganizationID: "org-7"}, true)
	if h.Get("Authorization") != "Bearer t" || h.Get("X-Kilocode-OrganizationID") != "org-7" || h.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers=%v", h)
	}
}
func TestStripProviderKeepsOpenRouterModel(t *testing.T) {
	if got := stripProvider("kilo/openai/gpt-5"); got != "openai/gpt-5" {
		t.Fatalf("got %q", got)
	}
}

func TestRPCUpstreamThrottlePreservesCooldownClassification(t *testing.T) {
	runtime = nativeabi.Runtime{}
	if err := runtime.Initialize(provider{}, func(_ string, _ []byte) ([]byte, int) {
		result, _ := json.Marshal(httpResponse{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": {"3"}}, Body: []byte(`{"error":"limited"}`)})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Shutdown)
	stored, _ := json.Marshal(storage{Token: "token"})
	req, _ := json.Marshal(executorRequest{Model: "kilo/openai/gpt-5", Payload: []byte(`{"messages":[]}`), StorageJSON: stored})
	raw, status := runtime.Call("executor.execute", req)
	var envelope nativeabi.Envelope
	if status == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Error == nil || envelope.Error.Scope != "" || envelope.Error.RetryAfterMS == nil || *envelope.Error.RetryAfterMS != 3000 {
		t.Fatalf("status=%d failure=%#v raw=%s", status, envelope.Error, raw)
	}
}
