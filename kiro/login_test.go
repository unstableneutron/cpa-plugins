package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func TestBuilderIDLoginStartAndPoll(t *testing.T) {
	runtimeABI = nativeabi.Runtime{}
	calls := 0
	err := runtimeABI.Initialize(&pluginHandler{}, func(_ string, request []byte) ([]byte, int) {
		calls++
		var req HTTPRequest
		_ = json.Unmarshal(request, &req)
		var response HTTPResponse
		switch {
		case strings.HasSuffix(req.URL, "/client/register"):
			response = HTTPResponse{StatusCode: 200, Body: []byte(`{"clientId":"cid","clientSecret":"csecret"}`)}
		case strings.HasSuffix(req.URL, "/device_authorization"):
			response = HTTPResponse{StatusCode: 200, Body: []byte(`{"deviceCode":"dev","verificationUriComplete":"https://verify.example/code","expiresIn":600,"interval":5}`)}
		case strings.HasSuffix(req.URL, "/token"):
			if !strings.Contains(string(req.Body), `"deviceCode":"dev"`) {
				t.Fatalf("poll body = %s", req.Body)
			}
			response = HTTPResponse{StatusCode: 400, Body: []byte(`{"error":"authorization_pending"}`)}
		default:
			t.Fatalf("URL = %q", req.URL)
		}
		result, _ := json.Marshal(response)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	if err != nil {
		t.Fatal(err)
	}
	start, callErr := startLoginRPC([]byte(`{"Metadata":{"region":"us-west-2"}}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	startMap := start.(map[string]any)
	if startMap["URL"] != "https://verify.example/code" {
		t.Fatalf("start = %#v", startMap)
	}
	state := startMap["State"].(string)
	pollRaw, _ := json.Marshal(map[string]string{"State": state})
	poll, callErr := pollLoginRPC(pollRaw)
	if callErr != nil {
		t.Fatal(callErr)
	}
	if poll.(map[string]any)["Status"] != "pending" || calls != 3 {
		t.Fatalf("poll=%#v calls=%d", poll, calls)
	}
}

func TestLoginStartRejectsUnsupportedFlow(t *testing.T) {
	_, callErr := startLoginRPC([]byte(`{"Metadata":{"login_method":"google"}}`))
	if callErr == nil || callErr.Code != "unsupported_login" || callErr.Scope != "credential" {
		t.Fatalf("error = %+v", callErr)
	}
}
