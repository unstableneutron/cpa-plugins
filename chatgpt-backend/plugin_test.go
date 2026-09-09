package main

import (
	"encoding/json"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestHandleIngressPreservesEscapedPathAndRawQuery(t *testing.T) {
	h := newHandler()
	result, callErr := h.Call(methodIngressHandle, mustJSON(t, ingressRequest{
		Method:         "PATCH",
		Path:           "/backend-api/files/a/b",
		EscapedPath:    "/backend-api/files/a%2Fb%20c",
		RawQuery:       "cursor=a%2Bb&cursor=a+b",
		Headers:        map[string][]string{accountIDHeader: {" acct-123 "}},
		PresentHeaders: []string{"authorization", accountIDHeader},
	}))
	if callErr != nil {
		t.Fatalf("Call() error = %v", callErr)
	}
	resp := result.(ingressResponse)
	if !resp.Handled || resp.Plan == nil {
		t.Fatalf("response = %#v, want handled plan", resp)
	}
	if got, want := resp.Plan.UpstreamURL, "https://chatgpt.com/backend-api/files/a%2Fb%20c?cursor=a%2Bb&cursor=a+b"; got != want {
		t.Fatalf("upstream URL = %q, want %q", got, want)
	}
	if got := resp.Plan.Credential.Selector.Equals; got != "acct-123" {
		t.Fatalf("selector equals = %q, want acct-123", got)
	}
	if !resp.Plan.Credential.FallbackToInbound {
		t.Fatal("fallback_to_inbound = false, want true")
	}
}

func TestHandleIngressRejectsIneligibleRequests(t *testing.T) {
	tests := []struct {
		name string
		req  ingressRequest
	}{
		{name: "wrong path", req: ingressRequest{Path: "/backend-api", PresentHeaders: []string{"Authorization"}, Headers: map[string][]string{accountIDHeader: {"acct"}}}},
		{name: "missing authorization", req: ingressRequest{Path: "/backend-api/models", Headers: map[string][]string{accountIDHeader: {"acct"}}}},
		{name: "missing account", req: ingressRequest{Path: "/backend-api/models", PresentHeaders: []string{"Authorization"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler()
			result, callErr := h.Call(methodIngressHandle, mustJSON(t, tc.req))
			if callErr != nil {
				t.Fatalf("Call() error = %v", callErr)
			}
			if resp := result.(ingressResponse); resp.Handled || resp.Plan != nil {
				t.Fatalf("response = %#v, want unhandled", resp)
			}
		})
	}
}

func TestRegistrationUsesConfiguredOriginAndBasePath(t *testing.T) {
	h := newHandler()
	result, callErr := h.Call(nativeabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("base-url: https://example.test/root/\n")}))
	if callErr != nil {
		t.Fatalf("register error = %v", callErr)
	}
	if !result.(registration).Capabilities.IngressProxy {
		t.Fatal("ingress_proxy = false, want true")
	}
	registered, callErr := h.Call(methodIngressRegister, nil)
	if callErr != nil {
		t.Fatalf("ingress register error = %v", callErr)
	}
	route := registered.(ingressRegistrationResponse).Routes[0]
	if got, want := route.UpstreamOrigins[0], "https://example.test"; got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
	handled, _ := h.Call(methodIngressHandle, mustJSON(t, ingressRequest{
		Path:           "/backend-api/models",
		Headers:        map[string][]string{accountIDHeader: {"acct"}},
		PresentHeaders: []string{"Authorization"},
	}))
	if got, want := handled.(ingressResponse).Plan.UpstreamURL, "https://example.test/root/backend-api/models"; got != want {
		t.Fatalf("upstream URL = %q, want %q", got, want)
	}
}

func TestRegisterRejectsUnsafeBaseURL(t *testing.T) {
	h := newHandler()
	_, callErr := h.Call(nativeabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("base-url: file:///etc/passwd\n")}))
	if callErr == nil || callErr.Code != "invalid_config" {
		t.Fatalf("error = %#v, want invalid_config", callErr)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
