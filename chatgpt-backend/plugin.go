package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
	"gopkg.in/yaml.v3"
)

const (
	methodIngressRegister = "ingress.register"
	methodIngressHandle   = "ingress.handle"
	backendPathPrefix     = "/backend-api/"
	accountIDHeader       = "ChatGPT-Account-ID"
	defaultBaseURL        = "https://chatgpt.com"
)

var ingressMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodConnect,
	http.MethodOptions,
	http.MethodTrace,
}

type config struct {
	BaseURL string `yaml:"base-url"`
}

type handler struct {
	mu        sync.RWMutex
	baseURL   string
	origin    string
	transport string
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string `json:"Name"`
	Version          string `json:"Version"`
	Author           string `json:"Author"`
	GitHubRepository string `json:"GitHubRepository"`
}

type capabilities struct {
	IngressProxy bool `json:"ingress_proxy"`
}

type ingressRegistrationResponse struct {
	Routes []ingressRoute `json:"routes"`
}

type ingressRoute struct {
	Methods         []string `json:"methods"`
	PathPrefix      string   `json:"path_prefix"`
	RequiredHeaders []string `json:"required_headers,omitempty"`
	UpstreamOrigins []string `json:"upstream_origins"`
}

type ingressRequest struct {
	Method         string              `json:"method"`
	Path           string              `json:"path"`
	EscapedPath    string              `json:"escaped_path"`
	RawQuery       string              `json:"raw_query,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	PresentHeaders []string            `json:"present_headers,omitempty"`
}

type ingressResponse struct {
	Handled bool              `json:"handled"`
	Plan    *ingressProxyPlan `json:"plan,omitempty"`
}

type ingressProxyPlan struct {
	UpstreamURL string         `json:"upstream_url"`
	Credential  *credentialUse `json:"credential,omitempty"`
	Transport   string         `json:"transport,omitempty"`
}

type credentialUse struct {
	Selector          credentialSelector  `json:"selector"`
	Injection         credentialInjection `json:"injection"`
	FallbackToInbound bool                `json:"fallback_to_inbound,omitempty"`
}

type credentialSelector struct {
	Provider        string                  `json:"provider"`
	IdentitySources []credentialValueSource `json:"identity_sources"`
	Equals          string                  `json:"equals"`
	CaseInsensitive bool                    `json:"case_insensitive,omitempty"`
	Order           string                  `json:"order"`
}

type credentialInjection struct {
	Header       string                  `json:"header"`
	Prefix       string                  `json:"prefix,omitempty"`
	ValueSources []credentialValueSource `json:"value_sources"`
}

type credentialValueSource struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Claim string `json:"claim,omitempty"`
}

func newHandler() *handler {
	h := &handler{}
	_ = h.configure(nil)
	return h
}

func (h *handler) Call(method string, request json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, invalidRequest("decode lifecycle request")
		}
		if err := h.configure(req.ConfigYAML); err != nil {
			return nil, &nativeabi.Error{Code: "invalid_config", Message: err.Error()}
		}
		return registration{
			SchemaVersion: nativeabi.SchemaVersionIngressProxy,
			Metadata: metadata{
				Name:             "ChatGPT backend passthrough",
				Version:          nativeabi.Version,
				Author:           "unstableneutron",
				GitHubRepository: "https://github.com/unstableneutron/cpa-plugins",
			},
			Capabilities: capabilities{IngressProxy: true},
		}, nil
	case methodIngressRegister:
		h.mu.RLock()
		origin := h.origin
		h.mu.RUnlock()
		return ingressRegistrationResponse{Routes: []ingressRoute{{
			Methods:         append([]string(nil), ingressMethods...),
			PathPrefix:      backendPathPrefix,
			RequiredHeaders: []string{"Authorization", accountIDHeader},
			UpstreamOrigins: []string{origin},
		}}}, nil
	case methodIngressHandle:
		var req ingressRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, invalidRequest("decode ingress request")
		}
		return h.handleIngress(req), nil
	case nativeabi.MethodPluginQuiesce, nativeabi.MethodPluginShutdown:
		return struct{}{}, nil
	default:
		return nil, &nativeabi.Error{Code: "unsupported_method", Message: "unsupported plugin method"}
	}
}

func invalidRequest(message string) *nativeabi.Error {
	return &nativeabi.Error{Code: "invalid_request", Message: message, HTTPStatus: http.StatusBadRequest}
}

func (h *handler) configure(raw []byte) error {
	cfg := config{BaseURL: defaultBaseURL}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	baseURL, origin, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.baseURL = baseURL
	h.origin = origin
	if strings.HasPrefix(origin, "http://") {
		h.transport = "standard"
	} else {
		h.transport = "utls"
	}
	h.mu.Unlock()
	return nil
}

func normalizeBaseURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", &url.Error{Op: "parse", URL: raw, Err: errInvalidBaseURL{}}
	}
	origin := parsed.Scheme + "://" + parsed.Host
	return strings.TrimRight(parsed.String(), "/"), origin, nil
}

type errInvalidBaseURL struct{}

func (errInvalidBaseURL) Error() string {
	return "base-url must be an HTTP(S) origin or base path without credentials, query, or fragment"
}

func (h *handler) handleIngress(req ingressRequest) ingressResponse {
	if !strings.HasPrefix(req.Path, backendPathPrefix) || !containsFold(req.PresentHeaders, "Authorization") {
		return ingressResponse{}
	}
	accountID := strings.TrimSpace(firstHeader(req.Headers, accountIDHeader))
	if accountID == "" {
		return ingressResponse{}
	}
	escapedPath := req.EscapedPath
	if escapedPath == "" {
		escapedPath = req.Path
	}
	h.mu.RLock()
	upstreamURL := h.baseURL + ensureLeadingSlash(escapedPath)
	transport := h.transport
	h.mu.RUnlock()
	if req.RawQuery != "" {
		upstreamURL += "?" + req.RawQuery
	}
	return ingressResponse{
		Handled: true,
		Plan: &ingressProxyPlan{
			UpstreamURL: upstreamURL,
			Transport:   transport,
			Credential: &credentialUse{
				Selector: credentialSelector{
					Provider:        "codex",
					IdentitySources: accountIdentitySources(),
					Equals:          accountID,
					CaseInsensitive: true,
					Order:           "oldest",
				},
				Injection: credentialInjection{
					Header:       "Authorization",
					Prefix:       "Bearer ",
					ValueSources: accessTokenSources(),
				},
				FallbackToInbound: true,
			},
		},
	}
}

func accountIdentitySources() []credentialValueSource {
	return []credentialValueSource{
		{Kind: "attribute", Path: "chatgpt_account_id"},
		{Kind: "attribute", Path: "account_id"},
		{Kind: "metadata", Path: "chatgpt_account_id"},
		{Kind: "metadata", Path: "account_id"},
		{Kind: "metadata", Path: "tokens.account_id"},
		{Kind: "metadata", Path: "tokens.chatgpt_account_id"},
		{Kind: "jwt_claim", Path: "id_token", Claim: "/https:~1~1api.openai.com~1auth/chatgpt_account_id"},
		{Kind: "jwt_claim", Path: "tokens.id_token", Claim: "/https:~1~1api.openai.com~1auth/chatgpt_account_id"},
	}
}

func accessTokenSources() []credentialValueSource {
	return []credentialValueSource{
		{Kind: "metadata", Path: "access_token"},
		{Kind: "metadata", Path: "auth_token"},
		{Kind: "metadata", Path: "bearer_token"},
		{Kind: "metadata", Path: "tokens.access_token"},
		{Kind: "metadata", Path: "tokens.auth_token"},
		{Kind: "metadata", Path: "tokens.bearer_token"},
		{Kind: "attribute", Path: "access_token"},
		{Kind: "attribute", Path: "auth_token"},
		{Kind: "attribute", Path: "bearer_token"},
	}
}

func firstHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func containsFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), expected) {
			return true
		}
	}
	return false
}

func ensureLeadingSlash(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}
