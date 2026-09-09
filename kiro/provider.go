// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const providerID = "kiro"

type HTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
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

type Request struct {
	Model, SourceFormat, AuthID string
	Payload                     []byte
	Token                       Token
	CustomHeaders               map[string]string
}
type Response struct {
	Payload   []byte
	Headers   map[string][]string
	Refreshed *Token
}
type Provider struct {
	transport Transport
	now       func() time.Time
}

func NewProvider(transport Transport) *Provider {
	return &Provider{transport: transport, now: time.Now}
}

type ProviderError struct {
	Status               int
	Code, Message, Scope string
	Retryable            bool
	RetryAfterMS         *int64
}

func (e *ProviderError) Error() string { return e.Message }

func (p *Provider) Execute(ctx context.Context, req Request) (Response, error) {
	stream, token, err := p.open(ctx, req)
	if err != nil {
		return Response{}, err
	}
	defer stream.Body.Close()
	result, err := parseKiroEvents(stream.Body, nil)
	if err != nil {
		return Response{}, err
	}
	payload, _ := json.Marshal(claudeResponse(req.Model, result))
	return Response{Payload: payload, Headers: stream.Headers, Refreshed: changedToken(req.Token, token)}, nil
}

func (p *Provider) ExecuteStream(ctx context.Context, req Request, emit func([]byte) error) (*Token, error) {
	stream, token, err := p.open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Body.Close()
	if err := emitSSE(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + accountKey(req.Model+req.AuthID), "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}, emit); err != nil {
		return nil, err
	}
	if err := emitSSE(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}, emit); err != nil {
		return nil, err
	}
	result, err := parseKiroEvents(stream.Body, func(event parsedEvent) error { return emitClaudeEvent(req.Model, event, emit) })
	if err != nil {
		return nil, err
	}
	if err := emitClaudeFinish(result, emit); err != nil {
		return nil, err
	}
	return changedToken(req.Token, token), ctx.Err()
}

func (p *Provider) open(ctx context.Context, req Request) (HTTPStreamResponse, Token, error) {
	token := req.Token
	if exp, err := time.Parse(time.RFC3339, token.ExpiresAt); err == nil && exp.Before(p.now().Add(20*time.Minute)) {
		refreshed, errRefresh := refreshToken(ctx, p.transport, token, p.now())
		if errRefresh != nil {
			return HTTPStreamResponse{}, token, errRefresh
		}
		token = refreshed
	}
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, errBuild := buildKiroRequest(req, token)
		if errBuild != nil {
			return HTTPStreamResponse{}, token, errBuild
		}
		resp, errDo := p.transport.DoStream(ctx, httpReq)
		if errDo != nil {
			return resp, token, errDo
		}
		if resp.StatusCode == 200 {
			return resp, token, nil
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if (resp.StatusCode == 401 || resp.StatusCode == 403) && attempt == 0 {
			refreshed, errRefresh := refreshToken(ctx, p.transport, token, p.now())
			if errRefresh != nil {
				return resp, token, errRefresh
			}
			token = refreshed
			continue
		}
		retryAfter := retryAfterMS(resp.Headers)
		return resp, token, &ProviderError{Status: resp.StatusCode, Code: "kiro_upstream", Message: string(body), Scope: map[bool]string{true: "credential", false: "request"}[resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429], Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500, RetryAfterMS: retryAfter}
	}
	return HTTPStreamResponse{}, token, errors.New("Kiro request retry exhausted")
}

func buildKiroRequest(req Request, token Token) (HTTPRequest, error) {
	if token.AccessToken == "" {
		return HTTPRequest{}, &ProviderError{Status: 401, Code: "missing_access_token", Message: "Kiro access token is missing", Scope: "credential"}
	}
	region := "us-east-1"
	if token.ProfileARN != "" {
		parts := strings.Split(token.ProfileARN, ":")
		if len(parts) > 3 && parts[3] != "" {
			region = parts[3]
		}
	}
	model := mapModel(req.Model)
	payload, err := buildKiroPayload(req.Payload, req.SourceFormat, model, effectiveProfileARN(token), "AI_EDITOR")
	if err != nil {
		return HTTPRequest{}, err
	}
	seed := req.AuthID
	if seed == "" {
		seed = token.RefreshToken
	}
	if seed == "" {
		seed = token.AccessToken
	}
	fp := fingerprint(accountKey(seed))
	invocation := deterministicUUID(append(payload, byte(len(payload))))
	headers := map[string][]string{"Content-Type": {"application/json"}, "Accept": {"*/*"}, "Authorization": {"Bearer " + token.AccessToken}, "User-Agent": {fp.UserAgent()}, "X-Amz-User-Agent": {fp.AmzUserAgent()}, "Amz-Sdk-Request": {"attempt=1; max=3"}, "Amz-Sdk-Invocation-Id": {invocation}, "x-amzn-kiro-agent-mode": {"vibe"}, "x-amzn-codewhisperer-optout": {"true"}}
	for key, value := range req.CustomHeaders {
		headers[key] = []string{value}
	}
	return HTTPRequest{Method: http.MethodPost, URL: "https://q." + region + ".amazonaws.com/generateAssistantResponse", Headers: headers, Body: payload}, nil
}

func effectiveProfileARN(token Token) string {
	if token.AuthMethod == "builder-id" || token.AuthMethod == "idc" {
		return ""
	}
	return token.ProfileARN
}
func changedToken(before, after Token) *Token {
	if before.AccessToken == after.AccessToken && before.RefreshToken == after.RefreshToken {
		return nil
	}
	return &after
}
func deterministicUUID(data []byte) string {
	sum := sha256.Sum256(data)
	b := hex.EncodeToString(sum[:16])
	return b[:8] + "-" + b[8:12] + "-4" + b[13:16] + "-a" + b[17:20] + "-" + b[20:32]
}
func retryAfterMS(headers map[string][]string) *int64 {
	for key, values := range headers {
		if strings.EqualFold(key, "Retry-After") && len(values) > 0 {
			if seconds, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64); err == nil && seconds >= 0 {
				value := seconds * 1000
				return &value
			}
		}
	}
	return nil
}

func mapModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(strings.TrimPrefix(m, "kiro-"), "amazonq-")
	m = strings.TrimSuffix(strings.TrimSuffix(m, "-agentic"), "-chat")
	for _, pair := range [][2]string{{"claude-sonnet-4-5", "claude-sonnet-4.5"}, {"claude-sonnet-4-6", "claude-sonnet-4.6"}, {"claude-opus-4-5", "claude-opus-4.5"}, {"claude-haiku-4-5", "claude-haiku-4.5"}, {"minimax-m2-1", "minimax-m2.1"}, {"gpt-5-6", "gpt-5.6"}} {
		if m == pair[0] || strings.HasPrefix(m, pair[0]+"-") {
			m = pair[1] + m[len(pair[0]):]
			break
		}
	}
	return m
}

type kiroPayload struct {
	ConversationState conversationState `json:"conversationState"`
	ProfileARN        string            `json:"profileArn,omitempty"`
}
type conversationState struct {
	ChatTriggerType string           `json:"chatTriggerType"`
	ConversationID  string           `json:"conversationId"`
	CurrentMessage  currentMessage   `json:"currentMessage"`
	History         []historyMessage `json:"history,omitempty"`
}
type currentMessage struct {
	UserInputMessage userMessage `json:"userInputMessage"`
}
type historyMessage struct {
	UserInputMessage         *userMessage      `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *assistantMessage `json:"assistantResponseMessage,omitempty"`
}
type userMessage struct {
	Content                 string       `json:"content"`
	ModelID                 string       `json:"modelId"`
	Origin                  string       `json:"origin"`
	UserInputMessageContext *userContext `json:"userInputMessageContext,omitempty"`
}
type assistantMessage struct {
	Content  string    `json:"content"`
	ToolUses []toolUse `json:"toolUses,omitempty"`
}
type userContext struct {
	ToolResults []toolResult  `json:"toolResults,omitempty"`
	Tools       []toolWrapper `json:"tools,omitempty"`
}
type toolWrapper struct {
	ToolSpecification toolSpec `json:"toolSpecification"`
}
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}
type toolResult struct {
	Content   []map[string]string `json:"content"`
	Status    string              `json:"status"`
	ToolUseID string              `json:"toolUseId"`
}
type toolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

func buildKiroPayload(raw []byte, format, model, profile, origin string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	messages, _ := root["messages"].([]any)
	history := make([]historyMessage, 0, len(messages))
	var current userMessage
	system := contentText(root["system"])
	tools := convertTools(root["tools"], format)
	for i, item := range messages {
		msg, _ := item.(map[string]any)
		role, _ := msg["role"].(string)
		text := contentText(msg["content"])
		if role == "system" {
			system = joinText(system, text)
			continue
		}
		user := userMessage{Content: text, ModelID: model, Origin: origin}
		if role == "assistant" {
			assistant := assistantMessage{Content: text, ToolUses: messageToolUses(msg, format)}
			history = append(history, historyMessage{AssistantResponseMessage: &assistant})
			continue
		}
		results := messageToolResults(msg)
		if i == len(messages)-1 {
			current = user
			if len(tools) > 0 || len(results) > 0 {
				current.UserInputMessageContext = &userContext{Tools: tools, ToolResults: results}
			}
		} else {
			history = append(history, historyMessage{UserInputMessage: &user})
		}
	}
	if current.Content == "" && len(messages) == 0 {
		current = userMessage{ModelID: model, Origin: origin}
	}
	if system != "" {
		current.Content = joinText("<system-reminder>\n"+system+"\n</system-reminder>", current.Content)
	}
	conversationHash := sha256.Sum256(raw)
	payload := kiroPayload{ConversationState: conversationState{ChatTriggerType: "MANUAL", ConversationID: hex.EncodeToString(conversationHash[:16]), CurrentMessage: currentMessage{UserInputMessage: current}, History: history}, ProfileARN: profile}
	return json.Marshal(payload)
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			part, _ := item.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}
func convertTools(value any, format string) []toolWrapper {
	items, _ := value.([]any)
	out := make([]toolWrapper, 0, len(items))
	for _, item := range items {
		tool, _ := item.(map[string]any)
		if strings.Contains(strings.ToLower(format), "openai") {
			tool, _ = tool["function"].(map[string]any)
		}
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		schema, _ := tool["input_schema"].(map[string]any)
		if schema == nil {
			schema, _ = tool["parameters"].(map[string]any)
		}
		if name != "" {
			out = append(out, toolWrapper{toolSpec{Name: name, Description: description, InputSchema: map[string]any{"json": schema}}})
		}
	}
	return out
}
func messageToolUses(msg map[string]any, format string) []toolUse {
	var out []toolUse
	if strings.Contains(strings.ToLower(format), "openai") {
		items, _ := msg["tool_calls"].([]any)
		for _, item := range items {
			call, _ := item.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			var input map[string]any
			_ = json.Unmarshal([]byte(stringValue(fn, "arguments")), &input)
			out = append(out, toolUse{ToolUseID: stringValue(call, "id"), Name: stringValue(fn, "name"), Input: input})
		}
		return out
	}
	items, _ := msg["content"].([]any)
	for _, item := range items {
		part, _ := item.(map[string]any)
		if part["type"] == "tool_use" {
			input, _ := part["input"].(map[string]any)
			out = append(out, toolUse{ToolUseID: stringValue(part, "id"), Name: stringValue(part, "name"), Input: input})
		}
	}
	return out
}
func messageToolResults(msg map[string]any) []toolResult {
	items, _ := msg["content"].([]any)
	var out []toolResult
	for _, item := range items {
		part, _ := item.(map[string]any)
		if part["type"] == "tool_result" {
			out = append(out, toolResult{Content: []map[string]string{{"text": contentText(part["content"])}}, Status: map[bool]string{true: "error", false: "success"}[part["is_error"] == true], ToolUseID: stringValue(part, "tool_use_id")})
		}
	}
	return out
}

type Usage struct{ Input, Output, CacheRead, CacheWrite int64 }
type parsedResult struct {
	Content    string
	Tools      []toolUse
	Usage      Usage
	StopReason string
}
type parsedEvent struct {
	Kind  string
	Text  string
	Tool  *toolUse
	Usage *Usage
}

func parseKiroEvents(r io.Reader, onEvent func(parsedEvent) error) (parsedResult, error) {
	var result parsedResult
	var pendingTool *toolUse
	var pendingInput strings.Builder
	emitTool := func() error {
		if pendingTool == nil {
			return nil
		}
		if pendingInput.Len() > 0 {
			if err := json.Unmarshal([]byte(pendingInput.String()), &pendingTool.Input); err != nil {
				return fmt.Errorf("decode Kiro tool input: %w", err)
			}
		}
		if pendingTool.Input == nil {
			pendingTool.Input = map[string]any{}
		}
		result.Tools = append(result.Tools, *pendingTool)
		if onEvent != nil {
			if err := onEvent(parsedEvent{Kind: "tool", Tool: pendingTool}); err != nil {
				return err
			}
		}
		pendingTool = nil
		pendingInput.Reset()
		return nil
	}
	for {
		msg, err := readEventMessage(r)
		if err != nil {
			return result, err
		}
		if msg == nil {
			break
		}
		var event map[string]any
		if err := json.Unmarshal(msg.Payload, &event); err != nil {
			return result, fmt.Errorf("decode %s event: %w", msg.EventType, err)
		}
		if msg.MessageType == "exception" || msg.ExceptionType != "" {
			return result, &ProviderError{Status: 502, Code: msg.ExceptionType, Message: stringValue(event, "message"), Scope: "request", Retryable: true}
		}
		switch msg.EventType {
		case "assistantResponseEvent":
			inner, _ := event["assistantResponseEvent"].(map[string]any)
			text := stringValue(inner, "content")
			if text == "" {
				text = stringValue(event, "content")
			}
			result.Content += text
			if onEvent != nil && text != "" {
				if err := onEvent(parsedEvent{Kind: "text", Text: text}); err != nil {
					return result, err
				}
			}
			if rawTools, ok := inner["toolUses"].([]any); ok {
				for _, rawTool := range rawTools {
					value, _ := rawTool.(map[string]any)
					input, _ := value["input"].(map[string]any)
					tool := toolUse{ToolUseID: stringValue(value, "toolUseId"), Name: stringValue(value, "name"), Input: input}
					result.Tools = append(result.Tools, tool)
					if onEvent != nil {
						if err := onEvent(parsedEvent{Kind: "tool", Tool: &tool}); err != nil {
							return result, err
						}
					}
				}
			}
		case "toolUseEvent":
			inner, _ := event["toolUseEvent"].(map[string]any)
			if inner == nil {
				inner = event
			}
			id, name := stringValue(inner, "toolUseId"), stringValue(inner, "name")
			if id != "" && name != "" && (pendingTool == nil || pendingTool.ToolUseID != id) {
				if err := emitTool(); err != nil {
					return result, err
				}
				pendingTool = &toolUse{ToolUseID: id, Name: name}
			}
			if fragment, ok := inner["input"].(string); ok {
				pendingInput.WriteString(fragment)
			} else if input, ok := inner["input"].(map[string]any); ok && pendingTool != nil {
				pendingTool.Input = input
			}
			if stopped, _ := inner["stop"].(bool); stopped {
				if err := emitTool(); err != nil {
					return result, err
				}
			}
		case "messageMetadataEvent", "metadataEvent":
			inner, _ := event[msg.EventType].(map[string]any)
			if inner == nil {
				inner = event
			}
			usage, _ := inner["tokenUsage"].(map[string]any)
			result.Usage = usageFrom(usage)
			if onEvent != nil {
				u := result.Usage
				if err := onEvent(parsedEvent{Kind: "usage", Usage: &u}); err != nil {
					return result, err
				}
			}
		case "messageStopEvent", "message_stop":
			result.StopReason = stringValue(event, "stopReason")
			if result.StopReason == "" {
				result.StopReason = stringValue(event, "stop_reason")
			}
		case "error", "exception", "internalServerException":
			return result, &ProviderError{Status: 502, Code: msg.EventType, Message: stringValue(event, "message"), Scope: "request", Retryable: true}
		}
	}
	if err := emitTool(); err != nil {
		return result, err
	}
	if result.StopReason == "" {
		if len(result.Tools) > 0 {
			result.StopReason = "tool_use"
		} else {
			result.StopReason = "end_turn"
		}
	}
	return result, nil
}
func usageFrom(values map[string]any) Usage {
	return Usage{Input: number(values["uncachedInputTokens"]), Output: number(values["outputTokens"]), CacheRead: number(values["cacheReadInputTokens"]), CacheWrite: number(values["cacheWriteInputTokens"])}
}
func number(v any) int64 {
	if n, ok := v.(float64); ok {
		return int64(n)
	}
	return 0
}
func claudeResponse(model string, r parsedResult) map[string]any {
	content := []any{}
	if r.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": r.Content})
	}
	for _, tool := range r.Tools {
		content = append(content, map[string]any{"type": "tool_use", "id": tool.ToolUseID, "name": tool.Name, "input": tool.Input})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	usage := map[string]any{"input_tokens": r.Usage.Input, "output_tokens": r.Usage.Output}
	if r.Usage.CacheRead > 0 {
		usage["cache_read_input_tokens"] = r.Usage.CacheRead
	}
	if r.Usage.CacheWrite > 0 {
		usage["cache_creation_input_tokens"] = r.Usage.CacheWrite
	}
	sum := sha256.Sum256([]byte(r.Content + model))
	return map[string]any{"id": "msg_" + hex.EncodeToString(sum[:12]), "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": r.StopReason, "usage": usage}
}
func emitClaudeEvent(model string, event parsedEvent, emit func([]byte) error) error {
	var data any
	switch event.Kind {
	case "text":
		data = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": event.Text}}
	case "tool":
		raw, _ := json.Marshal(event.Tool.Input)
		data = map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": event.Tool.ToolUseID, "name": event.Tool.Name, "input": map[string]any{}}}
		if err := emitSSE(data, emit); err != nil {
			return err
		}
		data = map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(raw)}}
		if err := emitSSE(data, emit); err != nil {
			return err
		}
		return emitSSE(map[string]any{"type": "content_block_stop", "index": 1}, emit)
	default:
		return nil
	}
	return emitSSE(data, emit)
}
func emitClaudeFinish(r parsedResult, emit func([]byte) error) error {
	if err := emitSSE(map[string]any{"type": "content_block_stop", "index": 0}, emit); err != nil {
		return err
	}
	if err := emitSSE(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": r.StopReason}, "usage": map[string]any{"output_tokens": r.Usage.Output}}, emit); err != nil {
		return err
	}
	return emitSSE(map[string]any{"type": "message_stop"}, emit)
}
func emitSSE(value any, emit func([]byte) error) error {
	raw, _ := json.Marshal(value)
	return emit(append(append([]byte("data: "), raw...), '\n', '\n'))
}
