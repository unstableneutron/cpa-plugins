package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxRetryAfter = time.Hour

// keyState holds per-provider round-robin and cooldown state in memory.
type keyState struct {
	mu        sync.Mutex
	next      map[string]uint64
	cooldowns map[string]time.Time
	now       func() time.Time
}

func newKeyState() *keyState {
	return &keyState{next: map[string]uint64{}, cooldowns: map[string]time.Time{}, now: time.Now}
}

// ordered returns the keys to try for one request: the round-robin start
// rotates per request and cooling keys move last, so cooldown never makes a
// request fail on its own.
func (s *keyState) ordered(p *provider) []string {
	n := len(p.APIKeys)
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if p.Strategy == "round_robin" {
		start = int(s.next[p.Name] % uint64(n))
		s.next[p.Name]++
	}
	now := s.now()
	hot := make([]string, 0, n)
	var cold []string
	for i := 0; i < n; i++ {
		key := p.APIKeys[(start+i)%n]
		if until, ok := s.cooldowns[p.Name+"\x00"+key]; ok && now.Before(until) {
			cold = append(cold, key)
		} else {
			hot = append(hot, key)
		}
	}
	return append(hot, cold...)
}

func (s *keyState) cool(p *provider, key string, retryAfter time.Duration) {
	duration := time.Duration(p.CooldownSeconds) * time.Second
	if duration == 0 {
		return
	}
	duration = max(duration, retryAfter)
	s.mu.Lock()
	s.cooldowns[p.Name+"\x00"+key] = s.now().Add(duration)
	s.mu.Unlock()
}

func (s *keyState) allCooling(p *provider) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, key := range p.APIKeys {
		if until, ok := s.cooldowns[p.Name+"\x00"+key]; !ok || !now.Before(until) {
			return false
		}
	}
	return true
}

// order keeps config order but moves providers whose keys are all cooling last.
func (s *keyState) order(matches []match) []match {
	out := append([]match(nil), matches...)
	cooling := make(map[*provider]bool, len(out))
	for _, m := range out {
		cooling[m.provider] = s.allCooling(m.provider)
	}
	sort.SliceStable(out, func(i, j int) bool { return !cooling[out[i].provider] && cooling[out[j].provider] })
	return out
}

type outcome int

const (
	outcomeSuccess      outcome = iota
	outcomeFinal                // a non-retryable upstream answer, such as 400
	outcomeNextKey              // 429 or 402: try the provider's next key
	outcomeNextProvider         // transport error or 5xx: try the next provider
)

func classify(status int) outcome {
	switch {
	case status >= 200 && status < 300:
		return outcomeSuccess
	case status == http.StatusTooManyRequests || status == http.StatusPaymentRequired:
		return outcomeNextKey
	case status == 0 || status >= 500:
		return outcomeNextProvider
	default:
		return outcomeFinal
	}
}

// upstreamResult is one upstream attempt. Status 0 means no HTTP response.
type upstreamResult struct {
	provider  string
	status    int
	headers   map[string][]string
	body      []byte
	malformed bool
}

type hostHTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// hostHTTPResponse mirrors pluginapi.HTTPResponse, which has no JSON tags.
type hostHTTPResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// call carries request-scoped host context.
type call struct {
	h          *handler
	callbackID string
	module     string
}

// tryProvider sends body to one provider, rotating keys on 429/402.
func (c call) tryProvider(m match, body []byte, extra map[string][]string) (upstreamResult, outcome) {
	var last upstreamResult
	for _, key := range c.h.keys.ordered(m.provider) {
		headers := map[string][]string{"Content-Type": {"application/json"}, "Authorization": {"Bearer " + key}}
		for name, values := range extra {
			headers[name] = values
		}
		resp, err := c.h.do(hostHTTPRequest{HostCallbackID: c.callbackID, Method: http.MethodPost, URL: m.provider.url, Headers: headers, Body: body})
		last = upstreamResult{provider: m.provider.Name}
		if err == nil {
			last.status, last.headers, last.body = resp.StatusCode, resp.Headers, resp.Body
		}
		result := classify(last.status)
		if result != outcomeSuccess {
			// Never log keys, bodies or transport errors (which may contain URLs).
			c.h.log(c.callbackID, "debug", "embeddings-forward: upstream attempt failed", map[string]any{
				"module": c.module, "provider": m.provider.Name, "status": last.status,
			})
		}
		if result != outcomeNextKey {
			return last, result
		}
		c.h.keys.cool(m.provider, key, retryAfter(last.headers))
	}
	return last, outcomeNextProvider
}

// forward tries matching providers in order until one gives a non-retryable answer.
func (c call) forward(matches []match, body func(match) []byte) upstreamResult {
	var last upstreamResult
	for _, m := range c.h.keys.order(matches) {
		result, out := c.tryProvider(m, body(m), nil)
		last = result
		if out == outcomeSuccess || out == outcomeFinal {
			return result
		}
	}
	return last
}

func retryAfter(headers map[string][]string) time.Duration {
	value := strings.TrimSpace(headerValue(headers, "Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	return 0
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// directResponse mirrors pluginapi.IngressDirectResponse.
type directResponse struct {
	StatusCode int                 `json:"status_code,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body,omitempty"`
}

// clientResponse returns successes and structured upstream errors unchanged,
// and wraps transport failures and non-JSON error bodies.
func clientResponse(result upstreamResult) directResponse {
	switch {
	case result.provider == "":
		return errorResponse(http.StatusBadGateway, "upstream_error", "no_provider_attempted", "no provider could be attempted", "")
	case result.malformed:
		return errorResponse(http.StatusBadGateway, "upstream_error", "invalid_upstream_response", "provider returned a response without answers", result.provider)
	case result.status == 0:
		return errorResponse(http.StatusBadGateway, "upstream_error", "upstream_unavailable", "upstream request failed", result.provider)
	}
	headers := map[string][]string{"Content-Type": {"application/json"}}
	if value := headerValue(result.headers, "Retry-After"); value != "" {
		headers["Retry-After"] = []string{value}
	}
	if result.status >= 200 && result.status < 300 {
		if value := headerValue(result.headers, "Content-Type"); value != "" {
			headers["Content-Type"] = []string{value}
		}
		return directResponse{StatusCode: result.status, Headers: headers, Body: result.body}
	}
	var structured struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(result.body, &structured) == nil && len(structured.Error) > 0 && string(structured.Error) != "null" {
		return directResponse{StatusCode: result.status, Headers: headers, Body: result.body}
	}
	response := errorResponse(result.status, "upstream_error", "upstream_error",
		fmt.Sprintf("provider returned HTTP %d: %s", result.status, snippet(result.body)), result.provider)
	for name, values := range headers {
		response.Headers[name] = values
	}
	return response
}

func errorResponse(status int, errorType, code, message, providerName string) directResponse {
	payload := map[string]any{"message": message, "type": errorType, "code": code}
	if providerName != "" {
		payload["provider"] = providerName
	}
	body, _ := json.Marshal(map[string]any{"error": payload})
	return directResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "..."
	}
	if text == "" {
		return "empty body"
	}
	return text
}
