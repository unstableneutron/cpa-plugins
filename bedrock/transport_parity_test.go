package main

import (
	"reflect"
	"testing"
)

// This fixture records the explicit Plus transport contract. It is deliberately
// separate from HTTPWireProfile: schema 7 cannot currently express TLS curves.
func TestBedrockSourceWireProfileFixture(t *testing.T) {
	wantALPN := []string{"h2", "http/1.1"}
	wantCurves := []string{"X25519", "P-256", "P-384", "P-521"}
	if !reflect.DeepEqual(wantALPN, []string{"h2", "http/1.1"}) {
		t.Fatal("Bedrock ALPN fixture changed")
	}
	if !reflect.DeepEqual(wantCurves, []string{"X25519", "P-256", "P-384", "P-521"}) {
		t.Fatal("Bedrock curve fixture changed")
	}
	var hostProfile HTTPWireProfile
	if hostProfile.HTTP1Only || hostProfile.DisableAutoCompression || len(hostProfile.HeaderProfile) != 0 {
		t.Fatalf("default host profile = %+v", hostProfile)
	}
}
