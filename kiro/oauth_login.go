// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const oauthLoginTimeout = 10 * time.Minute

type oauthCallback struct {
	code, err string
}

type oauthLoginSession struct {
	flow, provider, verifier, redirectURI    string
	clientID, clientSecret, region, startURL string
	expiresAt                                time.Time
	result                                   chan oauthCallback
	server                                   *http.Server
}

var oauthSessions = struct {
	sync.Mutex
	values map[string]*oauthLoginSession
}{values: make(map[string]*oauthLoginSession)}

func hasOAuthSession(state string) bool {
	oauthSessions.Lock()
	defer oauthSessions.Unlock()
	return oauthSessions.values[state] != nil
}

func startOAuthLogin(flow, region, startURL string) (any, *nativeabi.Error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	state, err := randomURLToken(16)
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	session := &oauthLoginSession{flow: flow, verifier: verifier, region: region, startURL: startURL, expiresAt: time.Now().Add(oauthLoginTimeout), result: make(chan oauthCallback, 1)}
	redirectURI, err := startOAuthCallbackServer(state, session)
	if err != nil {
		return nil, typedFailure(err, "credential", "login_failed")
	}
	session.redirectURI = redirectURI

	var loginURL string
	switch flow {
	case "google", "github":
		session.provider = map[bool]string{true: "Google", false: "Github"}[flow == "google"]
		query := url.Values{"idp": {session.provider}, "redirect_uri": {redirectURI}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}, "prompt": {"select_account"}}
		loginURL = "https://prod.us-east-1.auth.desktop.kiro.dev/login?" + query.Encode()
	case "builder-authcode", "idc-authcode":
		if region == "" {
			region = "us-east-1"
			session.region = region
		}
		if flow == "builder-authcode" {
			session.startURL = "https://view.awsapps.com/start"
		}
		body, _ := json.Marshal(map[string]any{"clientName": "Kiro IDE", "clientType": "public", "scopes": kiroScopes(), "grantTypes": []string{"authorization_code", "refresh_token"}, "redirectUris": []string{redirectURI}, "issuerUrl": session.startURL})
		registered, errCall := oidcCall("https://oidc."+region+".amazonaws.com/client/register", body)
		if errCall != nil {
			_ = session.server.Close()
			return nil, typedFailure(errCall, "credential", "login_failed")
		}
		var client struct {
			ClientID, ClientSecret string
		}
		if err := json.Unmarshal(registered, &client); err != nil {
			_ = session.server.Close()
			return nil, typedFailure(err, "credential", "login_failed")
		}
		session.clientID, session.clientSecret = client.ClientID, client.ClientSecret
		scopes := strings.Join(kiroScopes(), ",")
		query := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {redirectURI}, "scopes": {scopes}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
		loginURL = "https://oidc." + region + ".amazonaws.com/authorize?" + query.Encode()
	default:
		_ = session.server.Close()
		return nil, &nativeabi.Error{Code: "unsupported_login", Message: "unsupported Kiro OAuth flow", Scope: "credential"}
	}

	oauthSessions.Lock()
	oauthSessions.values[state] = session
	oauthSessions.Unlock()
	return map[string]any{"Provider": providerID, "URL": loginURL, "State": state, "ExpiresAt": session.expiresAt}, nil
}

func startOAuthCallbackServer(expectedState string, session *oauthLoginSession) (string, error) {
	listener, err := net.Listen("tcp", "localhost:9876")
	if err != nil {
		listener, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			return "", fmt.Errorf("start Kiro callback listener: %w", err)
		}
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/oauth/callback", listener.Addr().(*net.TCPAddr).Port)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, req *http.Request) {
		result := oauthCallback{code: req.URL.Query().Get("code"), err: req.URL.Query().Get("error")}
		if req.URL.Query().Get("state") != expectedState {
			result = oauthCallback{err: "state mismatch"}
		}
		if result.code == "" && result.err == "" {
			result.err = "authorization code is missing"
		}
		select {
		case session.result <- result:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if result.err != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "<html><body><h1>Login failed</h1><p>%s</p></body></html>", html.EscapeString(result.err))
			return
		}
		_, _ = fmt.Fprint(w, "<html><body><h1>Login successful</h1><p>You can close this window.</p></body></html>")
	})
	session.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = session.server.Serve(listener) }()
	return redirectURI, nil
}

func pollOAuthLogin(state string) (any, *nativeabi.Error) {
	oauthSessions.Lock()
	session := oauthSessions.values[state]
	oauthSessions.Unlock()
	if session == nil {
		return nil, &nativeabi.Error{Code: "invalid_login_state", Message: "Kiro login session was not found", Scope: "credential"}
	}
	if time.Now().After(session.expiresAt) {
		finishOAuthSession(state, session)
		return map[string]any{"Status": "error", "Message": "authorization expired"}, nil
	}
	select {
	case result := <-session.result:
		finishOAuthSession(state, session)
		if result.err != "" {
			return map[string]any{"Status": "error", "Message": result.err}, nil
		}
		token, err := exchangeOAuthCode(session, result.code)
		if err != nil {
			return nil, typedFailure(err, "credential", "login_failed")
		}
		return successfulLogin(token), nil
	default:
		return map[string]any{"Status": "pending", "Message": "waiting for authorization"}, nil
	}
}

func finishOAuthSession(state string, session *oauthLoginSession) {
	oauthSessions.Lock()
	if oauthSessions.values[state] == session {
		delete(oauthSessions.values, state)
	}
	oauthSessions.Unlock()
	_ = session.server.Close()
}

func closeOAuthSessions() {
	oauthSessions.Lock()
	sessions := oauthSessions.values
	oauthSessions.values = make(map[string]*oauthLoginSession)
	oauthSessions.Unlock()
	for _, session := range sessions {
		_ = session.server.Close()
	}
}

func exchangeOAuthCode(session *oauthLoginSession, code string) (Token, error) {
	if session.flow == "google" || session.flow == "github" {
		body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": session.verifier, "redirect_uri": session.redirectURI})
		fp := fingerprint("login")
		resp, err := hostTransport{}.Do(context.Background(), HTTPRequest{Method: http.MethodPost, URL: "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token", Headers: map[string][]string{"Content-Type": {"application/json"}, "Accept": {"application/json, text/plain, */*"}, "User-Agent": {fmt.Sprintf("KiroIDE-%s-%s", fp.KiroVersion, fp.KiroHash)}}, Body: body})
		if err != nil {
			return Token{}, err
		}
		return socialTokenFromResponse(resp, session.provider)
	}
	body, _ := json.Marshal(map[string]string{"clientId": session.clientID, "clientSecret": session.clientSecret, "code": code, "codeVerifier": session.verifier, "redirectUri": session.redirectURI, "grantType": "authorization_code"})
	resp, err := hostTransport{}.Do(context.Background(), HTTPRequest{Method: http.MethodPost, URL: "https://oidc." + session.region + ".amazonaws.com/token", Headers: oidcHeaders(), Body: body})
	if err != nil {
		return Token{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("OIDC token endpoint returned %d", resp.StatusCode)
	}
	var value struct {
		AccessToken, RefreshToken string
		ExpiresIn                 int
	}
	if err := json.Unmarshal(resp.Body, &value); err != nil {
		return Token{}, err
	}
	method := "builder-id"
	if session.flow == "idc-authcode" {
		method = "idc"
	}
	return Token{Type: providerID, AccessToken: value.AccessToken, RefreshToken: value.RefreshToken, ExpiresAt: expiresAt(value.ExpiresIn), AuthMethod: method, Provider: "AWS", ClientID: session.clientID, ClientSecret: session.clientSecret, Region: session.region, StartURL: session.startURL}, nil
}

func socialTokenFromResponse(resp HTTPResponse, provider string) (Token, error) {
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("Kiro social token endpoint returned %d", resp.StatusCode)
	}
	var value struct {
		AccessToken, RefreshToken, ProfileArn string
		ExpiresIn                             int
	}
	if err := json.Unmarshal(resp.Body, &value); err != nil {
		return Token{}, err
	}
	return Token{Type: providerID, AccessToken: value.AccessToken, RefreshToken: value.RefreshToken, ProfileARN: value.ProfileArn, ExpiresAt: expiresAt(value.ExpiresIn), AuthMethod: "social", Provider: provider, Region: "us-east-1", Email: emailFromJWT(value.AccessToken)}, nil
}

func successfulLogin(token Token) map[string]any {
	storage, _ := json.Marshal(token)
	id := "kiro-" + accountKey(token.RefreshToken+token.AccessToken)
	return map[string]any{"Status": "success", "Message": "authorization complete", "Auth": map[string]any{"Provider": providerID, "ID": id, "FileName": id + ".json", "StorageJSON": storage, "Metadata": tokenMetadata(token), "Attributes": map[string]string{"access_token": token.AccessToken, "profile_arn": token.ProfileARN}, "NextRefreshAfter": nextRefresh(token)}}
}

func newPKCE() (string, string, error) {
	verifier, err := randomURLToken(32)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func expiresAt(seconds int) string {
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Now().Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339)
}

func kiroScopes() []string {
	return []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"}
}

func emailFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return stringValue(claims, "email")
}
