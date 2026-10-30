package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

const (
	methodIngressRegister = "ingress.register"
	methodIngressHandle   = "ingress.handle"
	methodHostLog         = "host.log"

	routeEmbeddings = "/v1/embeddings"
	routeRerank     = "/v1/rerank"
	routeSystemOne  = "/v1/systemone"
	routeDecisions  = "/api/alpha/decisions"

	maxBodyBytes = 32 << 20
)

type handler struct {
	mu   sync.RWMutex
	cfg  pluginConfig
	keys *keyState
	do   func(hostHTTPRequest) (hostHTTPResponse, error)
	logf func(callbackID, level, message string, fields map[string]any)
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
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type capabilities struct {
	IngressProxy bool `json:"ingress_proxy"`
}

type ingressRoute struct {
	Methods      []string `json:"methods"`
	Path         string   `json:"path"`
	MaxBodyBytes int64    `json:"max_body_bytes"`
}

type ingressRegistrationResponse struct {
	Routes []ingressRoute `json:"routes"`
}

type ingressRequest struct {
	Method         string              `json:"method"`
	Path           string              `json:"path"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

type ingressResponse struct {
	Handled  bool            `json:"handled"`
	Response *directResponse `json:"response,omitempty"`
}

var configFields = []configField{
	{Name: "embeddings", Type: "object", Description: "OpenAI embeddings providers for POST /v1/embeddings: enabled, providers[] with name, base_url, path, api_keys, models[{name, alias}], strategy, cooldown_seconds."},
	{Name: "rerank", Type: "object", Description: "Cohere rerank providers for POST /v1/rerank, same shape as embeddings."},
	{Name: "decisions", Type: "object", Description: "Decision-model providers shared by POST /v1/systemone and POST /api/alpha/decisions. Per provider also api (decisions|systemone), max_questions and max_concurrency."},
}

func newHandler() *handler {
	h := &handler{keys: newKeyState()}
	h.do = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		var resp hostHTTPResponse
		err := runtime.HostCall(nativeabi.MethodHostHTTPDo, req, &resp)
		return resp, err
	}
	h.logf = func(callbackID, level, message string, fields map[string]any) {
		_ = runtime.HostCall(methodHostLog, map[string]any{"host_callback_id": callbackID, "level": level, "message": message, "fields": fields}, nil)
	}
	return h
}

func (h *handler) log(callbackID, level, message string, fields map[string]any) {
	if h.logf != nil {
		h.logf(callbackID, level, message, fields)
	}
}

func (h *handler) Call(method string, request json.RawMessage) (any, *nativeabi.Error) {
	switch method {
	case nativeabi.MethodPluginRegister, nativeabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, &nativeabi.Error{Code: "invalid_request", Message: "decode lifecycle request", HTTPStatus: http.StatusBadRequest}
		}
		cfg, err := parseConfig(req.ConfigYAML)
		if err != nil {
			// Config errors name fields and providers, never key values.
			return nil, &nativeabi.Error{Code: "invalid_config", Message: err.Error()}
		}
		h.mu.Lock()
		h.cfg = cfg
		h.mu.Unlock()
		return registration{
			SchemaVersion: nativeabi.SchemaVersionIngressHandler,
			Metadata: metadata{
				Name:             "Embeddings, rerank and decisions forward",
				Version:          nativeabi.Version,
				Author:           "unstableneutron (fork of KorenKrita/cliproxy-embeddings-rerank-forward)",
				GitHubRepository: "https://github.com/unstableneutron/cpa-plugins",
				ConfigFields:     configFields,
			},
			Capabilities: capabilities{IngressProxy: true},
		}, nil
	case methodIngressRegister:
		cfg := h.config()
		var routes []ingressRoute
		add := func(enabled bool, paths ...string) {
			for _, path := range paths {
				if enabled {
					routes = append(routes, ingressRoute{Methods: []string{http.MethodPost}, Path: path, MaxBodyBytes: maxBodyBytes})
				}
			}
		}
		add(cfg.Embeddings.Enabled, routeEmbeddings)
		add(cfg.Rerank.Enabled, routeRerank)
		add(cfg.Decisions.Enabled, routeSystemOne, routeDecisions)
		return ingressRegistrationResponse{Routes: routes}, nil
	case methodIngressHandle:
		var req ingressRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, &nativeabi.Error{Code: "invalid_request", Message: "decode ingress request", HTTPStatus: http.StatusBadRequest}
		}
		response, handled := h.handle(req)
		if !handled {
			return ingressResponse{}, nil
		}
		return ingressResponse{Handled: true, Response: &response}, nil
	case nativeabi.MethodPluginQuiesce, nativeabi.MethodPluginShutdown:
		return struct{}{}, nil
	default:
		return nil, &nativeabi.Error{Code: "unsupported_method", Message: "unsupported plugin method"}
	}
}

func (h *handler) config() pluginConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *handler) handle(req ingressRequest) (directResponse, bool) {
	cfg := h.config()
	var mod *module
	name := ""
	switch req.Path {
	case routeEmbeddings:
		mod, name = &cfg.Embeddings, "embeddings"
	case routeRerank:
		mod, name = &cfg.Rerank, "rerank"
	case routeSystemOne, routeDecisions:
		mod, name = &cfg.Decisions, "decisions"
	}
	if mod == nil || !mod.Enabled || req.Method != http.MethodPost {
		return directResponse{}, false
	}
	if encoding := strings.TrimSpace(headerValue(req.Headers, "Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return errorResponse(http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_content_encoding", "request bodies must be uncompressed JSON", ""), true
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &top); err != nil || top == nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object", ""), true
	}
	var model string
	if err := json.Unmarshal(top["model"], &model); err != nil || strings.TrimSpace(model) == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "missing_model", "model must be a non-empty string", ""), true
	}
	matches := mod.resolve(model)
	if len(matches) == 0 {
		return errorResponse(http.StatusNotFound, "invalid_request_error", "model_not_found", "model is not configured: "+model, ""), true
	}
	c := call{h: h, callbackID: req.HostCallbackID, module: name}
	if name == "decisions" {
		return c.decide(matches, top, dangerZoneHeaders(req.Headers)), true
	}
	result := c.forward(matches, func(m match) []byte {
		if m.mapping.Name == model {
			return req.Body
		}
		return withField(top, "model", mustJSON(m.mapping.Name))
	})
	if classify(result.status) != outcomeSuccess {
		h.log(req.HostCallbackID, "warn", "embeddings-forward: request failed", map[string]any{"module": name, "provider": result.provider, "status": result.status})
	}
	return clientResponse(result), true
}

// dangerZoneHeaders forwards TypeSafe opt-in headers (images, long context),
// which change what decision upstreams accept.
func dangerZoneHeaders(headers map[string][]string) map[string][]string {
	out := map[string][]string{}
	for name, values := range headers {
		if canonical := http.CanonicalHeaderKey(name); strings.HasPrefix(canonical, "Typesafe-Danger-Zone-") {
			out[canonical] = values
		}
	}
	return out
}

func withField(top map[string]json.RawMessage, name string, value json.RawMessage) []byte {
	out := make(map[string]json.RawMessage, len(top))
	for key, raw := range top {
		out[key] = raw
	}
	out[name] = value
	return mustJSON(out)
}

func mustJSON(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return bytes.TrimRight(buffer.Bytes(), "\n")
}
