// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const providerID = "bedrock"

type HTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	WireProfile    *HTTPWireProfile    `json:"wire_profile,omitempty"`
}

type HTTPWireProfile struct {
	HTTP1Only              bool     `json:"http1_only,omitempty"`
	DisableAutoCompression bool     `json:"disable_auto_compression,omitempty"`
	HeaderProfile          []string `json:"header_profile,omitempty"`
}

type HTTPResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body,omitempty"`
}

type HTTPStreamResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       io.ReadCloser
}

type Transport interface {
	Do(context.Context, HTTPRequest) (HTTPResponse, error)
	DoStream(context.Context, HTTPRequest) (HTTPStreamResponse, error)
}

type Auth struct {
	BaseURL       string
	APIKey        string
	AuthType      string
	ModelMap      map[string]string
	APIMap        map[string]string
	StreamAPIMap  map[string]string
	CustomHeaders map[string]string
}

type Request struct {
	Model           string
	Payload         []byte
	OriginalRequest []byte
	Auth            Auth
}

type Response struct {
	Payload []byte
	Headers map[string][]string
}

type Provider struct {
	transport Transport
}

func NewProvider(transport Transport) *Provider { return &Provider{transport: transport} }

func (p *Provider) Execute(ctx context.Context, req Request) (Response, error) {
	plan, errPlan := preparePlan(req, false)
	if errPlan != nil {
		return Response{}, errPlan
	}
	httpReq := buildHTTPRequest(plan, req.Auth, false)
	resp, errDo := p.transport.Do(ctx, httpReq)
	if errDo != nil {
		return Response{}, fmt.Errorf("bedrock request: %w", errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, &ProviderError{Status: resp.StatusCode, Code: "bedrock_upstream", Message: string(resp.Body), Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	payload := BedrockResponseToClaudeMessage(plan.Model, resp.Body)
	payload = ApplyBedrockStructuredOutputResponse(payload, req.OriginalRequest)
	return Response{Payload: payload, Headers: resp.Headers}, nil
}

func (p *Provider) ExecuteStream(ctx context.Context, req Request, emit func([]byte) error) error {
	plan, errPlan := preparePlan(req, true)
	if errPlan != nil {
		return errPlan
	}
	resp, errDo := p.transport.DoStream(ctx, buildHTTPRequest(plan, req.Auth, true))
	if errDo != nil {
		return fmt.Errorf("bedrock stream request: %w", errDo)
	}
	if resp.Body == nil {
		return errors.New("bedrock stream response body is nil")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return &ProviderError{Status: resp.StatusCode, Code: "bedrock_upstream", Message: string(body), Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	normalizer := NewBedrockStreamNormalizer(plan.Model)
	var emitErr error
	emitLines := func(lines [][]byte) bool {
		for _, line := range lines {
			if errEmit := emit(append(append([]byte(nil), line...), '\n', '\n')); errEmit != nil {
				emitErr = errEmit
				return false
			}
		}
		return true
	}
	if strings.Contains(strings.ToLower(firstHeader(resp.Headers, "Content-Type")), "application/vnd.amazon.eventstream") {
		var exceptionErr error
		errStream := ForEachBedrockEventStreamMessage(resp.Body, func(msg BedrockEventStreamMessage) bool {
			if errException := eventStreamException(msg); errException != nil {
				exceptionErr = errException
				return false
			}
			return emitLines(normalizer.ConvertLine(msg.Payload))
		})
		if errStream != nil {
			return errStream
		}
		if exceptionErr != nil {
			return exceptionErr
		}
	} else {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, 52_428_800)
		for scanner.Scan() {
			line := bytes.TrimSpace(bytes.Clone(scanner.Bytes()))
			if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte(":")) {
				continue
			}
			if !emitLines(normalizer.ConvertLine(line)) {
				return emitErr
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			return errScan
		}
	}
	if !emitLines(normalizer.Finish()) {
		return emitErr
	}
	return ctx.Err()
}

type ProviderError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
}

func (e *ProviderError) Error() string { return e.Message }

type invokePlan struct {
	Model   string
	API     string
	URL     string
	Payload []byte
}

func preparePlan(req Request, stream bool) (invokePlan, error) {
	if strings.TrimSpace(req.Auth.BaseURL) == "" {
		return invokePlan{}, &ProviderError{Status: http.StatusUnauthorized, Code: "missing_base_url", Message: "missing provider base URL"}
	}
	model := strings.TrimSpace(req.Model)
	if mapped := req.Auth.ModelMap[model]; strings.TrimSpace(mapped) != "" {
		model = strings.TrimSpace(mapped)
	}
	apiMap := req.Auth.APIMap
	defaultAPI := "converse"
	if stream {
		apiMap = req.Auth.StreamAPIMap
		defaultAPI = "converse-stream"
	}
	api := strings.TrimSpace(apiMap[req.Model])
	if api == "" {
		api = strings.TrimSpace(apiMap[model])
	}
	if api == "" && stream && NormalizeBedrockAPI(req.Auth.APIMap[req.Model], false) == "invoke" {
		api = "invoke-stream"
	}
	if api == "" {
		api = defaultAPI
	}
	body := append([]byte(nil), req.Payload...)
	body = ApplyBedrockStructuredOutputFormat(body, req.OriginalRequest)
	return invokePlan{Model: model, API: NormalizeBedrockAPI(api, stream), URL: BedrockRuntimeURL(req.Auth.BaseURL, model, api), Payload: BedrockPayloadForAPI(api, model, body, stream)}, nil
}

func buildHTTPRequest(plan invokePlan, auth Auth, stream bool) HTTPRequest {
	headers := map[string][]string{"Content-Type": {"application/json"}, "User-Agent": {"cli-proxy-bedrock"}}
	if stream {
		headers["Accept"] = []string{"text/event-stream"}
		headers["Cache-Control"] = []string{"no-cache"}
	}
	if auth.APIKey != "" {
		switch strings.ToLower(strings.TrimSpace(auth.AuthType)) {
		case "none":
		case "raw", "authorization":
			headers["Authorization"] = []string{auth.APIKey}
		default:
			headers["Authorization"] = []string{"Bearer " + auth.APIKey}
		}
	}
	for key, value := range auth.CustomHeaders {
		headers[key] = []string{value}
	}
	return HTTPRequest{Method: http.MethodPost, URL: plan.URL, Headers: headers, Body: plan.Payload}
}

func eventStreamException(msg BedrockEventStreamMessage) error {
	if strings.ToLower(strings.TrimSpace(msg.Headers[":message-type"])) != "exception" {
		return nil
	}
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(msg.Payload, &payload)
	kind := strings.TrimSpace(msg.Headers[":exception-type"])
	if kind == "" {
		kind = "bedrockException"
	}
	return &ProviderError{Status: http.StatusBadGateway, Code: kind, Message: strings.TrimSpace(payload.Message), Retryable: true}
}

func firstHeader(headers map[string][]string, key string) string {
	for name, values := range headers {
		if strings.EqualFold(name, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
