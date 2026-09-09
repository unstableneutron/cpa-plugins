package main

import (
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
	req, _ := json.Marshal(executorRequest{Model: "kilo/openai/gpt-5", Payload: []byte(`{"messages":[]}`), StorageJSON: stored})
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
