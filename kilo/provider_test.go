package main

import (
	"encoding/json"
	"testing"
)

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
