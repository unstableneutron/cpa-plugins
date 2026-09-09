package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestExecuteExchangesTokenThenUsesResponsesEndpoint(t *testing.T) {
	var calls []map[string]any
	runtime.Initialize(provider{}, func(method string, request []byte) ([]byte, int) {
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
	})
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
func TestCredentialsAcceptsStorageAndLegacyMetadata(t *testing.T) {
	raw, _ := json.Marshal(storage{AccessToken: "native"})
	if got, err := credentials(raw, nil); err != nil || got.AccessToken != "native" {
		t.Fatalf("native=%#v %v", got, err)
	}
	if got, err := credentials(nil, map[string]any{"access_token": "legacy"}); err != nil || got.AccessToken != "legacy" {
		t.Fatalf("legacy=%#v %v", got, err)
	}
}
