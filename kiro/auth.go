// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Token struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ProfileARN   string `json:"profile_arn"`
	ExpiresAt    string `json:"expires_at"`
	AuthMethod   string `json:"auth_method"`
	Provider     string `json:"provider"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Region       string `json:"region,omitempty"`
	StartURL     string `json:"start_url,omitempty"`
	Email        string `json:"email,omitempty"`
}

func parseToken(raw []byte) (Token, error) {
	var token Token
	if err := json.Unmarshal(raw, &token); err != nil {
		return token, err
	}
	var aliases map[string]any
	_ = json.Unmarshal(raw, &aliases)
	if token.AccessToken == "" {
		token.AccessToken = stringValue(aliases, "accessToken")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = stringValue(aliases, "refreshToken")
	}
	if token.ProfileARN == "" {
		token.ProfileARN = stringValue(aliases, "profileArn")
	}
	if token.ExpiresAt == "" {
		token.ExpiresAt = stringValue(aliases, "expiresAt")
	}
	if token.Type != "" && token.Type != "kiro" {
		return token, fmt.Errorf("not a Kiro token")
	}
	if token.AccessToken == "" && token.RefreshToken == "" {
		return token, fmt.Errorf("Kiro token has no credentials")
	}
	token.Type = "kiro"
	return token, nil
}

func refreshToken(ctx context.Context, transport Transport, token Token, now time.Time) (Token, error) {
	if token.RefreshToken == "" {
		return token, fmt.Errorf("refresh token is missing")
	}
	region := token.Region
	if region == "" {
		region = "us-east-1"
	}
	url := "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	payload := map[string]string{"refreshToken": token.RefreshToken}
	headers := map[string][]string{"Content-Type": {"application/json"}, "Accept": {"application/json, text/plain, */*"}}
	if token.ClientID != "" && token.ClientSecret != "" && (token.AuthMethod == "builder-id" || token.AuthMethod == "idc") {
		url = "https://oidc." + region + ".amazonaws.com/token"
		payload = map[string]string{"clientId": token.ClientID, "clientSecret": token.ClientSecret, "refreshToken": token.RefreshToken, "grantType": "refresh_token"}
		fp := fingerprint("oidc-session")
		headers["x-amz-user-agent"] = []string{"aws-sdk-js/" + fp.OIDCSDKVersion + " KiroIDE"}
		headers["User-Agent"] = []string{fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/sso-oidc#%s m/E KiroIDE", fp.OIDCSDKVersion, fp.OSType, fp.OSVersion, fp.NodeVersion, fp.OIDCSDKVersion)}
	} else {
		fp := fingerprint("login")
		headers["User-Agent"] = []string{fmt.Sprintf("KiroIDE-%s-%s", fp.KiroVersion, fp.KiroHash)}
	}
	body, _ := json.Marshal(payload)
	resp, err := transport.Do(ctx, HTTPRequest{Method: http.MethodPost, URL: url, Headers: headers, Body: body})
	if err != nil {
		return token, err
	}
	if resp.StatusCode != 200 {
		return token, &ProviderError{Status: resp.StatusCode, Code: "token_refresh_failed", Message: "Kiro token refresh failed", Scope: "credential", Retryable: resp.StatusCode >= 500}
	}
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return token, fmt.Errorf("decode token refresh: %w", err)
	}
	if result.AccessToken == "" {
		return token, fmt.Errorf("token refresh returned no access token")
	}
	token.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		token.RefreshToken = result.RefreshToken
	}
	if result.ProfileARN != "" {
		token.ProfileARN = result.ProfileARN
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	token.ExpiresAt = now.Add(time.Duration(result.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	return token, nil
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
