package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBedrockSourceWireProfileFixture(t *testing.T) {
	wantCurves := []string{"X25519", "P-256", "P-384", "P-521"}
	request := buildHTTPRequest(invokePlan{URL: "https://bedrock.example/model", Payload: []byte(`{}`)}, Auth{}, false)
	if request.WireProfile == nil || !reflect.DeepEqual(request.WireProfile.TLSCurves, wantCurves) {
		t.Fatalf("wire profile = %+v, want curves %v", request.WireProfile, wantCurves)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `"wire_profile":{"tls_curves":["X25519","P-256","P-384","P-521"]}`; !strings.Contains(got, want) {
		t.Fatalf("request JSON = %s, want fragment %s", got, want)
	}
}
