package main

import (
	"encoding/json"
	"testing"
)

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
