package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
	_, callErr := startLoginRPC([]byte(`{"Metadata":{"login_method":"unknown"}}`))
	if callErr == nil || callErr.Code != "unsupported_login" || callErr.Scope != "credential" {
		t.Fatalf("error = %+v", callErr)
	}
}

func TestSocialLoginLoopbackAndTokenExchange(t *testing.T) {
	runtimeABI = nativeabi.Runtime{}
	err := runtimeABI.Initialize(&pluginHandler{}, func(_ string, request []byte) ([]byte, int) {
		var req HTTPRequest
		if err := json.Unmarshal(request, &req); err != nil {
			t.Fatal(err)
		}
		if req.URL != "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token" || !strings.Contains(string(req.Body), `"code":"auth-code"`) || !strings.Contains(string(req.Body), `"code_verifier":`) {
			t.Fatalf("token request = %+v", req)
		}
		jwt := "header." + encodeRawURL([]byte(`{"email":"user@example.com"}`)) + ".signature"
		response, _ := json.Marshal(HTTPResponse{StatusCode: 200, Body: []byte(`{"accessToken":"` + jwt + `","refreshToken":"refresh","profileArn":"arn","expiresIn":3600}`)})
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(response)})
		return envelope, 0
	})
	if err != nil {
		t.Fatal(err)
	}
	start, callErr := startLoginRPC([]byte(`{"Metadata":{"login_method":"google"}}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	started := start.(map[string]any)
	loginURL, err := url.Parse(started["URL"].(string))
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := loginURL.Query().Get("redirect_uri") + "?code=auth-code&state=" + url.QueryEscape(started["State"].(string))
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	pollRaw, _ := json.Marshal(map[string]string{"State": started["State"].(string)})
	poll, callErr := pollLoginRPC(pollRaw)
	if callErr != nil {
		t.Fatal(callErr)
	}
	auth := poll.(map[string]any)["Auth"].(map[string]any)
	if poll.(map[string]any)["Status"] != "success" || auth["Metadata"].(map[string]any)["email"] != "user@example.com" {
		t.Fatalf("poll = %#v", poll)
	}
}

func TestIDCDeviceLoginUsesStartURL(t *testing.T) {
	runtimeABI = nativeabi.Runtime{}
	err := runtimeABI.Initialize(&pluginHandler{}, func(_ string, request []byte) ([]byte, int) {
		var req HTTPRequest
		_ = json.Unmarshal(request, &req)
		response := HTTPResponse{StatusCode: 200, Body: []byte(`{"clientId":"cid","clientSecret":"secret"}`)}
		if strings.HasSuffix(req.URL, "/device_authorization") {
			if !strings.Contains(string(req.Body), `"startUrl":"https://example.awsapps.com/start"`) {
				t.Fatalf("device request = %s", req.Body)
			}
			response.Body = []byte(`{"deviceCode":"dev","verificationUri":"https://verify.example","expiresIn":600}`)
		}
		result, _ := json.Marshal(response)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	if err != nil {
		t.Fatal(err)
	}
	start, callErr := startLoginRPC([]byte(`{"Metadata":{"login_method":"idc-device","start_url":"https://example.awsapps.com/start","region":"eu-west-1"}}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	stateRaw, _ := base64RawURLDecode(start.(map[string]any)["State"].(string))
	var state loginState
	_ = json.Unmarshal(stateRaw, &state)
	if state.AuthMethod != "idc" || state.StartURL != "https://example.awsapps.com/start" || state.Region != "eu-west-1" {
		t.Fatalf("state = %+v", state)
	}
}

func TestIDCAuthCodeLoopbackAndTokenExchange(t *testing.T) {
	runtimeABI = nativeabi.Runtime{}
	err := runtimeABI.Initialize(&pluginHandler{}, func(_ string, request []byte) ([]byte, int) {
		var req HTTPRequest
		_ = json.Unmarshal(request, &req)
		response := HTTPResponse{StatusCode: 200, Body: []byte(`{"clientId":"cid","clientSecret":"secret"}`)}
		if strings.HasSuffix(req.URL, "/token") {
			if !strings.Contains(string(req.Body), `"grantType":"authorization_code"`) || !strings.Contains(string(req.Body), `"code":"idc-code"`) {
				t.Fatalf("token request = %s", req.Body)
			}
			response.Body = []byte(`{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}`)
		} else if !strings.Contains(string(req.Body), `"issuerUrl":"https://example.awsapps.com/start"`) || !strings.Contains(string(req.Body), `"redirectUris":["http://localhost:`) {
			t.Fatalf("registration request = %s", req.Body)
		}
		result, _ := json.Marshal(response)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
		return envelope, 0
	})
	if err != nil {
		t.Fatal(err)
	}
	start, callErr := startLoginRPC([]byte(`{"Metadata":{"login_method":"idc-authcode","start_url":"https://example.awsapps.com/start","region":"eu-west-1"}}`))
	if callErr != nil {
		t.Fatal(callErr)
	}
	started := start.(map[string]any)
	loginURL, _ := url.Parse(started["URL"].(string))
	callbackURL := loginURL.Query().Get("redirect_uri") + "?code=idc-code&state=" + url.QueryEscape(started["State"].(string))
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	pollRaw, _ := json.Marshal(map[string]string{"State": started["State"].(string)})
	poll, callErr := pollLoginRPC(pollRaw)
	if callErr != nil {
		t.Fatal(callErr)
	}
	metadata := poll.(map[string]any)["Auth"].(map[string]any)["Metadata"].(map[string]any)
	if metadata["auth_method"] != "idc" || metadata["start_url"] != "https://example.awsapps.com/start" || metadata["region"] != "eu-west-1" {
		t.Fatalf("poll = %#v", poll)
	}
}

func TestPersistRefreshedAuthUsesSelectedHostPath(t *testing.T) {
	runtimeABI = nativeabi.Runtime{}
	var saved map[string]json.RawMessage
	err := runtimeABI.Initialize(&pluginHandler{}, func(method string, request []byte) ([]byte, int) {
		if method != "host.auth.save" {
			t.Fatalf("method = %q", method)
		}
		_ = json.Unmarshal(request, &saved)
		envelope, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]string{"name": "kiro.json"}})
		return envelope, 0
	})
	if err != nil {
		t.Fatal(err)
	}
	token := &Token{Type: providerID, AccessToken: "new", RefreshToken: "refresh"}
	if err := persistRefreshedAuth(Request{AuthPath: "/managed/auths/kiro.json", AuthMetadata: map[string]any{"weight": 2}}, token); err != nil {
		t.Fatal(err)
	}
	if string(saved["name"]) != `"kiro.json"` || !strings.Contains(string(saved["json"]), `"weight":2`) || !strings.Contains(string(saved["json"]), `"access_token":"new"`) {
		t.Fatalf("save request = %s", mustJSON(saved))
	}
}

func encodeRawURL(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
func base64RawURLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}
func mustJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }
