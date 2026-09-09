package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestRegistrationAndReconfigureReturnFullLifecycleContract(t *testing.T) {
	handler := &pluginHandler{}
	registration, callErr := handler.Call(nativeabi.MethodPluginRegister, json.RawMessage(`{}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	reconfigured, callErr := handler.Call(nativeabi.MethodPluginReconfigure, json.RawMessage(`{}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	if !reflect.DeepEqual(registration, reconfigured) {
		t.Fatalf("reconfigure = %#v, want full registration %#v", reconfigured, registration)
	}
	result := registration.(map[string]any)
	if result["schema_version"] != bedrockSchemaVersion || result["metadata"] == nil || result["capabilities"] == nil {
		t.Fatalf("registration = %#v", result)
	}
}
