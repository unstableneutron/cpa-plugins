// Portions extracted from CLIProxyAPIPlus at
// 1fec8453e63a5bc133555a79164480700e351bfc (MIT License).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/unstableneutron/cpa-plugins/commandcode/usage"
)

func buildCommandCodePayload(opts commandCodePayloadOptions) ([]byte, error) {
	var req commandCodeOpenAIRequest
	if len(opts.Payload) > 0 {
		if err := json.Unmarshal(opts.Payload, &req); err != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: "invalid CommandCode request: " + err.Error()}
		}
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	workingDir := strings.TrimSpace(opts.WorkingDir)
	if workingDir == "" {
		workingDir = "."
	}
	environment := strings.TrimSpace(opts.Environment)
	if environment == "" {
		environment = defaultCommandCodeEnvironment()
	}

	system, messages := commandCodeMessagesFromOpenAI(req.Messages, req.Tools, opts.Model)
	maxTokens, err := commandCodeMaxTokens(req, opts.Model)
	if err != nil {
		return nil, err
	}
	threadID := strings.TrimSpace(opts.ThreadID)
	if !isUUID(threadID) {
		threadID = uuid.NewString()
	}
	body := commandCodeBody{
		Config: commandCodeConfig{
			WorkingDir:    workingDir,
			Date:          now.Format("2006-01-02"),
			Environment:   environment,
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory:         nil,
		Taste:          nil,
		Skills:         nil,
		PermissionMode: "standard",
		ThreadID:       threadID,
		Params: commandCodeParams{
			Model:           opts.Model,
			ReasoningEffort: commandCodeReasoningEffort(req.ReasoningEffort, opts.Model),
			Messages:        messages,
			Tools:           commandCodeToolsFromOpenAI(req.Tools),
			System:          system,
			MaxTokens:       maxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			Stop:            commandCodeOptionalRaw(req.Stop),
			Stream:          true,
		},
	}
	return json.Marshal(body)
}

func defaultCommandCodeEnvironment() string {
	return fmt.Sprintf("%s-%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func isUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

// commandCodeThreadID derives a UUID from the caller's session identity without
// forwarding that identity (which may contain client metadata) to the provider.
func commandCodeThreadID(opts executionOptions, payload []byte) string {
	identity := ""
	if opts.Metadata != nil {
		if raw, ok := opts.Metadata["execution_session_id"]; ok {
			identity = strings.TrimSpace(fmt.Sprint(raw))
		}
	}
	if identity == "" {
		identity = sessionID(opts.Headers, payload, opts.Metadata)
	}
	if identity == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("commandcode:"+identity)).String()
}

func commandCodeOptionalRaw(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return bytes.Clone(trimmed)
}

func commandCodeMaxTokens(req commandCodeOpenAIRequest, model string) (int, error) {
	limit := commandCodeModelMaxTokens(model)
	for _, field := range []struct {
		name  string
		value *int
	}{
		{name: "max_tokens", value: req.MaxTokens},
		{name: "max_completion_tokens", value: req.MaxCompletionTokens},
	} {
		if field.value == nil {
			continue
		}
		if *field.value <= 0 {
			return 0, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("%s must be greater than 0", field.name)}
		}
		if *field.value > limit {
			return 0, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("%s for CommandCode model %q must be less than or equal to %d", field.name, model, limit)}
		}
	}
	if req.MaxTokens != nil {
		return *req.MaxTokens, nil
	}
	if req.MaxCompletionTokens != nil {
		return *req.MaxCompletionTokens, nil
	}
	return limit, nil
}

// commandCodeModelMaxTokens applies the 64k CLI-envelope ceiling used by the
// MIT-licensed pi-commandcode-provider reference, then narrows it for models
// whose catalog metadata advertises a smaller output limit. Explicit requests
// above this value are rejected by commandCodeMaxTokens rather than truncated.
func commandCodeModelMaxTokens(model string) int {
	limit := commandCodeMaxTokensCap
	if info := lookupCommandCodeModelInfo(model); info != nil && info.MaxCompletionTokens > 0 && info.MaxCompletionTokens < int64(limit) {
		limit = int(info.MaxCompletionTokens)
	}
	return limit
}

func commandCodeMessagesFromOpenAI(messages []commandCodeOpenAIMessage, tools []commandCodeOpenAITool, model string) (string, []any) {
	systemParts := make([]string, 0)
	out := make([]any, 0, len(messages))
	toolSchemas := commandCodeToolSchemas(tools)
	pairedToolCallIDs := commandCodePairedToolCallIDs(messages)
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer":
			if text := commandCodeTextFromRawContent(message.Content); text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			content := commandCodeUserContent(message.Content, commandCodeModelSupportsImages(model))
			if parts, ok := content.([]any); ok && len(parts) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		case "assistant":
			parts := make([]any, 0)
			if reasoning := strings.TrimSpace(message.ReasoningContent); reasoning != "" {
				parts = append(parts, map[string]any{"type": "reasoning", "text": reasoning})
			}
			parts = append(parts, commandCodeAssistantContentParts(message.Content)...)
			missingResults := make([]commandCodeToolCall, 0)
			for _, call := range message.ToolCalls {
				if strings.ToLower(strings.TrimSpace(call.Type)) != "" && !strings.EqualFold(call.Type, "function") {
					continue
				}
				if call.ProviderExecuted {
					continue
				}
				callID := strings.TrimSpace(call.ID)
				if callID == "" {
					callID = fmt.Sprintf("call_%d", len(missingResults)+len(parts))
				}
				parts = append(parts, map[string]any{
					"type":       "tool-call",
					"toolCallId": callID,
					"toolName":   call.Function.Name,
					"input":      commandCodeToolInput(call.Function.Arguments, toolSchemas[call.Function.Name]),
				})
				if _, ok := pairedToolCallIDs[call.ID]; !ok {
					missingResults = append(missingResults, commandCodeToolCall{ID: callID, Name: call.Function.Name})
				}
			}
			if len(parts) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": parts})
			}
			for _, call := range missingResults {
				out = append(out, map[string]any{
					"role": "tool",
					"content": []any{map[string]any{
						"type":       "tool-result",
						"toolCallId": call.ID,
						"toolName":   call.Name,
						"output": map[string]any{
							"type":  "text",
							"value": "No result — the tool call did not complete (interrupted or lost).",
						},
					}},
				})
			}
		case "tool":
			if strings.TrimSpace(message.ToolCallID) == "" || message.ProviderExecuted {
				continue
			}
			out = append(out, map[string]any{
				"role": "tool",
				"content": []any{map[string]any{
					"type":       "tool-result",
					"toolCallId": message.ToolCallID,
					"toolName":   message.Name,
					"output": map[string]any{
						"type":  "text",
						"value": commandCodeTextFromRawContent(message.Content),
					},
				}},
			})
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

func commandCodeReasoningEffort(effort, model string) string {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ""
	}
	info := lookupCommandCodeModelInfo(model)
	if info == nil || info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		return ""
	}
	for _, level := range info.Thinking.Levels {
		if strings.EqualFold(strings.TrimSpace(level), effort) {
			return strings.TrimSpace(level)
		}
	}
	return ""
}

func commandCodeToolSchemas(tools []commandCodeOpenAITool) map[string]map[string]any {
	out := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		var schema map[string]any
		if len(bytes.TrimSpace(tool.Function.Parameters)) > 0 {
			_ = json.Unmarshal(tool.Function.Parameters, &schema)
		}
		if schema == nil {
			schema = map[string]any{}
		}
		out[name] = schema
	}
	return out
}

func commandCodeToolSchemasFromPayload(payload []byte) map[string]map[string]any {
	var request commandCodeOpenAIRequest
	if json.Unmarshal(payload, &request) != nil {
		return nil
	}
	return commandCodeToolSchemas(request.Tools)
}

func commandCodePairedToolCallIDs(messages []commandCodeOpenAIMessage) map[string]struct{} {
	callIDs := make(map[string]struct{})
	resultIDs := make(map[string]struct{})
	for _, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "assistant":
			for _, call := range message.ToolCalls {
				if id := strings.TrimSpace(call.ID); id != "" {
					callIDs[id] = struct{}{}
				}
			}
		case "tool":
			if id := strings.TrimSpace(message.ToolCallID); id != "" {
				resultIDs[id] = struct{}{}
			}
		}
	}
	paired := make(map[string]struct{})
	for id := range callIDs {
		if _, ok := resultIDs[id]; ok {
			paired[id] = struct{}{}
		}
	}
	return paired
}

func commandCodeUserContent(raw json.RawMessage, allowImages bool) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []any{}
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	var parts []map[string]any
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return []any{}
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(stringValueFromMap(part, "type"))) {
		case "text", "input_text":
			out = append(out, map[string]any{"type": "text", "text": stringValueFromMap(part, "text")})
		case "image_url":
			imageURL := ""
			mimeType := ""
			if nested, ok := part["image_url"].(map[string]any); ok {
				imageURL = stringValueFromMap(nested, "url")
			} else {
				imageURL = stringValueFromMap(part, "url")
			}
			imageURL, mimeType = commandCodeImageURL(imageURL)
			if imageURL != "" {
				if !allowImages {
					out = append(out, map[string]any{"type": "text", "text": "[image omitted: the active model is text-only]"})
					continue
				}
				image := map[string]any{"type": "image", "image": imageURL}
				if mimeType != "" {
					image["mimeType"] = mimeType
				}
				out = append(out, image)
			}
		case "image":
			imageURL := stringValueFromMap(part, "image")
			imageURL, mimeType := commandCodeImageURL(imageURL)
			if imageURL != "" {
				if !allowImages {
					out = append(out, map[string]any{"type": "text", "text": "[image omitted: the active model is text-only]"})
					continue
				}
				image := map[string]any{"type": "image", "image": imageURL}
				if explicit := stringValueFromMap(part, "mimeType"); explicit != "" {
					mimeType = explicit
				}
				if mimeType != "" {
					image["mimeType"] = mimeType
				}
				out = append(out, image)
			}
		}
	}
	return out
}

func lookupCommandCodeModelInfo(model string) *ModelInfo {
	model = strings.TrimSpace(model)
	for _, info := range getCommandCodeModels() {
		if info != nil && strings.TrimSpace(info.ID) == model {
			return info
		}
	}
	return nil
}

func commandCodeModelSupportsImages(model string) bool {
	info := lookupCommandCodeModelInfo(model)
	if info == nil {
		return false
	}
	for _, modality := range info.SupportedInputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

func commandCodeImageURL(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		if semi := strings.IndexByte(value, ';'); semi > len("data:") {
			return value, value[len("data:"):semi]
		}
	}
	return value, ""
}

func commandCodeAssistantContentParts(raw json.RawMessage) []any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": text}}
	}
	var parts []map[string]any
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(stringValueFromMap(part, "type"))) {
		case "text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": stringValueFromMap(part, "text")})
		case "thinking", "reasoning":
			text := stringValueFromMap(part, "thinking")
			if text == "" {
				text = stringValueFromMap(part, "text")
			}
			if text != "" {
				out = append(out, map[string]any{"type": "reasoning", "text": text})
			}
		}
	}
	return out
}

func commandCodeTextFromRawContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	var parts []map[string]any
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return string(trimmed)
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(stringValueFromMap(part, "type")) {
		case "text", "input_text", "output_text":
			texts = append(texts, stringValueFromMap(part, "text"))
		}
	}
	return strings.Join(texts, "\n")
}

func commandCodeToolsFromOpenAI(tools []commandCodeOpenAITool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		if strings.ToLower(strings.TrimSpace(tool.Type)) != "function" && strings.TrimSpace(tool.Type) != "" {
			continue
		}
		schema := any(map[string]any{})
		if len(bytes.TrimSpace(tool.Function.Parameters)) > 0 {
			var parsed any
			if err := json.Unmarshal(tool.Function.Parameters, &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		out = append(out, map[string]any{
			"name":         tool.Function.Name,
			"description":  tool.Function.Description,
			"input_schema": schema,
		})
	}
	return out
}

func commandCodeToolInput(raw json.RawMessage, schema map[string]any) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			value = text
		} else {
			return map[string]any{}
		}
	}
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			value = decoded
		}
	}
	if array, ok := value.([]any); ok {
		if len(array) == 1 {
			value = array[0]
		} else {
			return map[string]any{}
		}
	}
	if object, ok := value.(map[string]any); ok {
		return object
	}
	if text, ok := value.(string); ok {
		if property, array := commandCodeSingleRequiredProperty(schema); property != "" {
			if array {
				return map[string]any{property: []string{text}}
			}
			return map[string]any{property: text}
		}
	}
	return map[string]any{}
}

func commandCodeSingleRequiredProperty(schema map[string]any) (string, bool) {
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 1 {
		return "", false
	}
	name, ok := required[0].(string)
	if !ok || name == "" {
		return "", false
	}
	properties, _ := schema["properties"].(map[string]any)
	property, _ := properties[name].(map[string]any)
	if property == nil {
		return "", false
	}
	switch propertyType, _ := property["type"].(string); propertyType {
	case "string":
		return name, false
	case "array":
		return name, true
	default:
		return "", false
	}
}

func stringValueFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if value, ok := m[key].(string); ok {
		return value
	}
	return ""
}

func newCommandCodeStreamState(model string) *commandCodeStreamState {
	return &commandCodeStreamState{
		ID:                    "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Created:               time.Now().Unix(),
		Model:                 model,
		toolCallIndexByID:     make(map[string]int),
		toolCallStartedByID:   make(map[string]bool),
		providerExecutedTools: make(map[string]bool),
	}
}

func (s *commandCodeStreamState) ensureToolCall(id, name string) int {
	if s.toolCallIndexByID == nil {
		s.toolCallIndexByID = make(map[string]int)
	}
	if s.toolCallStartedByID == nil {
		s.toolCallStartedByID = make(map[string]bool)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		if s.lastAnonymousToolID != "" {
			id = s.lastAnonymousToolID
		} else {
			id = fmt.Sprintf("call_%d", len(s.ToolCalls))
			s.lastAnonymousToolID = id
		}
	}
	if idx, ok := s.toolCallIndexByID[id]; ok {
		if name != "" && s.ToolCalls[idx].Name == "" {
			s.ToolCalls[idx].Name = name
		}
		return idx
	}
	idx := len(s.ToolCalls)
	s.ToolCalls = append(s.ToolCalls, commandCodeToolCall{ID: id, Name: name, Arguments: ""})
	s.toolCallIndexByID[id] = idx
	return idx
}

func (s *commandCodeStreamState) lookupToolCall(id string) (int, bool) {
	if s == nil || s.toolCallIndexByID == nil {
		return 0, false
	}
	idx, ok := s.toolCallIndexByID[strings.TrimSpace(id)]
	return idx, ok
}

func commandCodeLineToOpenAIChunks(line []byte, state *commandCodeStreamState) ([][]byte, Usage, error) {
	if state == nil {
		return nil, usage.Detail{}, fmt.Errorf("commandcode executor: nil stream state")
	}
	if state.providerExecutedTools == nil {
		state.providerExecutedTools = make(map[string]bool)
	}
	trimmed := bytes.TrimSpace(line)
	if bytes.Equal(trimmed, []byte("[DONE]")) || bytes.Equal(trimmed, []byte("data: [DONE]")) {
		state.SawDone = true
		return nil, usage.Detail{}, nil
	}
	payload := commandCodeJSONPayload(line)
	if len(payload) == 0 {
		return nil, usage.Detail{}, nil
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return nil, usage.Detail{}, nil
	}
	eventType := root.Get("type").String()
	if state.Terminal && eventType != "error" {
		return nil, usage.Detail{}, nil
	}
	switch eventType {
	case "text-delta":
		delta := root.Get("text").String()
		state.Text.WriteString(delta)
		return [][]byte{state.streamChunk(map[string]any{"content": delta}, nil, nil)}, usage.Detail{}, nil
	case "reasoning-start":
		return nil, usage.Detail{}, nil
	case "reasoning-delta":
		delta := root.Get("text").String()
		state.Reasoning.WriteString(delta)
		return [][]byte{state.streamChunk(map[string]any{"reasoning_content": delta}, nil, nil)}, usage.Detail{}, nil
	case "reasoning-end":
		return nil, usage.Detail{}, nil
	case "tool-input-start":
		id := commandCodeToolEventID(root)
		if id == "" {
			state.lastAnonymousToolID = ""
		}
		idx := state.ensureToolCall(id, commandCodeToolEventName(root))
		call := state.ToolCalls[idx]
		if state.toolCallStartedByID[call.ID] {
			return nil, usage.Detail{}, nil
		}
		state.toolCallStartedByID[call.ID] = true
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": idx,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": "",
			},
		}}}
		return [][]byte{state.streamChunk(delta, nil, nil)}, usage.Detail{}, nil
	case "tool-input-delta":
		id := commandCodeToolEventID(root)
		idx := state.ensureToolCall(id, commandCodeToolEventName(root))
		deltaText := commandCodeToolInputDelta(root)
		state.ToolCalls[idx].Arguments += deltaText
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": idx,
			"function": map[string]any{
				"arguments": deltaText,
			},
		}}}
		return [][]byte{state.streamChunk(delta, nil, nil)}, usage.Detail{}, nil
	case "tool-input-end":
		return nil, usage.Detail{}, nil
	case "tool-call":
		id := commandCodeToolEventID(root)
		if id == "" {
			id = state.lastAnonymousToolID
			state.lastAnonymousToolID = ""
			if id == "" {
				id = fmt.Sprintf("call_%d", len(state.ToolCalls))
			}
		}
		if root.Get("providerExecuted").Bool() {
			if id != "" {
				state.providerExecutedTools[id] = true
			}
			return nil, usage.Detail{}, nil
		}
		if id != "" && state.providerExecutedTools[id] {
			return nil, usage.Detail{}, nil
		}
		name := commandCodeToolEventName(root)
		arguments := commandCodeToolArguments(root, state.toolSchemas[name])
		if idx, ok := state.lookupToolCall(id); ok {
			if name != "" && state.ToolCalls[idx].Name == "" {
				state.ToolCalls[idx].Name = name
			}
			if arguments != "{}" {
				state.ToolCalls[idx].Arguments = arguments
			}
			return nil, usage.Detail{}, nil
		}
		idx := state.ensureToolCall(id, name)
		state.ToolCalls[idx].Arguments = arguments
		state.toolCallStartedByID[state.ToolCalls[idx].ID] = true
		call := state.ToolCalls[idx]
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": idx,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}}}
		return [][]byte{state.streamChunk(delta, nil, nil)}, usage.Detail{}, nil
	case "tool-result":
		// OpenAI Chat has no server-tool-result block. Provider-executed
		// results are therefore consumed, never surfaced as client tools.
		if root.Get("providerExecuted").Bool() || state.providerExecutedTools[commandCodeToolEventID(root)] {
			return nil, usage.Detail{}, nil
		}
		return nil, usage.Detail{}, nil
	case "finish-step":
		if parsedUsage := commandCodeUsageFromEvent(root); parsedUsage.hasUsage() {
			state.pendingUsage = parsedUsage
		}
		return nil, usage.Detail{}, nil
	case "finish":
		state.RawFinish = strings.TrimSpace(root.Get("rawFinishReason").String())
		if state.RawFinish == "" {
			state.RawFinish = strings.TrimSpace(root.Get("finishReason").String())
		}
		reason := strings.TrimSpace(root.Get("finishReason").String())
		if strings.EqualFold(reason, "pause_turn") || strings.EqualFold(state.RawFinish, "pause_turn") {
			state.Finish = "pause_turn"
			parsedUsage := commandCodeUsageFromEvent(root)
			if !parsedUsage.hasUsage() {
				parsedUsage = state.pendingUsage
			}
			state.pendingUsage = commandCodeUsage{}
			state.Usage.add(parsedUsage)
			return nil, usage.Detail{}, nil
		}
		parsedUsage := commandCodeUsageFromEvent(root)
		if !parsedUsage.hasUsage() {
			parsedUsage = state.pendingUsage
		}
		state.pendingUsage = commandCodeUsage{}
		state.Usage.add(parsedUsage)
		state.Finish = mapCommandCodeFinishReason(reason)
		if state.Finish == "stop" && len(state.ToolCalls) > 0 {
			state.Finish = "tool_calls"
		}
		state.Terminal = true
		usageDetail := state.Usage.detail()
		return [][]byte{state.streamChunk(map[string]any{}, state.Finish, state.Usage.openAIUsage())}, usageDetail, nil
	case "abort":
		state.Aborted = true
		state.Terminal = true
		parsedUsage := commandCodeUsageFromEvent(root)
		if !parsedUsage.hasUsage() {
			parsedUsage = state.pendingUsage
		}
		state.pendingUsage = commandCodeUsage{}
		state.Usage.add(parsedUsage)
		message := strings.TrimSpace(root.Get("message").String())
		if message == "" {
			message = "CommandCode generation aborted before completion"
		}
		return nil, state.Usage.detail(), commandCodeAbortError{statusErr{code: http.StatusBadGateway, msg: message}}
	case "error":
		return nil, usage.Detail{}, commandCodeProviderError{statusErr{code: commandCodeErrorStatus(root), msg: commandCodeErrorMessage(root)}}
	default:
		return nil, usage.Detail{}, nil
	}
}

func commandCodeErrorStatus(root gjson.Result) int {
	paths := []string{"statusCode", "status_code", "status", "error.statusCode", "error.status_code", "error.status"}
	reportedStatus := false
	for _, path := range paths {
		value := root.Get(path)
		if !value.Exists() {
			continue
		}
		reportedStatus = true
		if status := int(value.Int()); status >= 400 && status <= 599 {
			return status
		}
	}
	for _, path := range []string{"message", "error", "error.message"} {
		raw := root.Get(path)
		if raw.Type != gjson.String {
			continue
		}
		if status, _, ok := commandCodeEmbeddedError(raw.String()); ok {
			if status != 0 {
				reportedStatus = true
			}
			if status >= 400 && status <= 599 {
				return status
			}
		}
	}
	if reportedStatus {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

func commandCodeEmbeddedError(message string) (int, string, bool) {
	start := strings.IndexByte(message, '{')
	if start < 0 {
		return 0, "", false
	}
	embedded := gjson.Parse(message[start:])
	innerMessage := strings.TrimSpace(embedded.Get("error.message").String())
	if innerMessage == "" {
		return 0, "", false
	}
	status, _ := strconv.Atoi(strings.TrimSpace(message[:start]))
	errorType := strings.TrimSpace(embedded.Get("error.type").String())
	if errorType == "" {
		errorType = "error"
	}
	return status, errorType + ": " + innerMessage, true
}

func commandCodeJSONPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[5:])
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || !gjson.ValidBytes(trimmed) {
		return nil
	}
	return trimmed
}

func commandCodeToolEventID(root gjson.Result) string {
	for _, path := range []string{"toolCallId", "tool_call_id", "id"} {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func commandCodeToolEventName(root gjson.Result) string {
	for _, path := range []string{"toolName", "tool_name", "name"} {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func commandCodeToolInputDelta(root gjson.Result) string {
	for _, path := range []string{"delta", "inputTextDelta", "text"} {
		value := root.Get(path)
		if value.Exists() {
			return value.String()
		}
	}
	return ""
}

func commandCodeToolArguments(root gjson.Result, schema map[string]any) string {
	for _, path := range []string{"input", "args", "arguments"} {
		value := root.Get(path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		input := commandCodeToolInput(json.RawMessage(value.Raw), schema)
		encoded, err := json.Marshal(input)
		if err == nil {
			return string(encoded)
		}
	}
	return "{}"
}

func commandCodeUsageFromEvent(root gjson.Result) commandCodeUsage {
	usageNode := root.Get("totalUsage")
	if !usageNode.IsObject() {
		usageNode = root.Get("usage")
	}
	if !usageNode.IsObject() {
		return commandCodeUsage{}
	}
	details := usageNode.Get("inputTokenDetails")
	outputDetails := usageNode.Get("outputTokenDetails")
	cacheReadTokens := details.Get("cacheReadTokens").Int()
	if cacheReadTokens == 0 {
		cacheReadTokens = usageNode.Get("cachedInputTokens").Int()
	}
	reasoningTokens := usageNode.Get("reasoningTokens").Int()
	if reasoningTokens == 0 {
		reasoningTokens = outputDetails.Get("reasoningTokens").Int()
	}
	return commandCodeUsage{
		InputTokens:      usageNode.Get("inputTokens").Int(),
		OutputTokens:     usageNode.Get("outputTokens").Int(),
		ReasoningTokens:  reasoningTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: details.Get("cacheWriteTokens").Int(),
		TotalTokens:      usageNode.Get("totalTokens").Int(),
	}
}

func (u *commandCodeUsage) add(other commandCodeUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.TotalTokens += other.TotalTokens
}

func (u commandCodeUsage) hasUsage() bool {
	return u.InputTokens != 0 ||
		u.OutputTokens != 0 ||
		u.ReasoningTokens != 0 ||
		u.CacheReadTokens != 0 ||
		u.CacheWriteTokens != 0 ||
		u.TotalTokens != 0
}

func (u commandCodeUsage) openAIUsage() map[string]any {
	promptTokens := u.InputTokens
	completionTokens := u.OutputTokens
	total := u.TotalTokens
	promptWithCache := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	if total == 0 {
		promptTokens = promptWithCache
		total = promptTokens + completionTokens
	} else {
		switch total {
		case u.InputTokens + u.OutputTokens:
			// Cache and reasoning are child buckets.
		case promptWithCache + u.OutputTokens:
			promptTokens = promptWithCache
		case u.InputTokens + u.OutputTokens + u.ReasoningTokens:
			completionTokens += u.ReasoningTokens
		case promptWithCache + u.OutputTokens + u.ReasoningTokens:
			promptTokens = promptWithCache
			completionTokens += u.ReasoningTokens
		}
	}
	if total == 0 {
		return nil
	}
	out := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      total,
	}
	if u.CacheReadTokens != 0 {
		out["prompt_tokens_details"] = map[string]any{
			"cached_tokens": u.CacheReadTokens,
		}
	}
	if u.ReasoningTokens != 0 {
		out["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": u.ReasoningTokens,
		}
	}
	return out
}

func (u commandCodeUsage) detail() Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
	return Usage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		ReasoningTokens:     u.ReasoningTokens,
		CachedTokens:        u.CacheReadTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheWriteTokens,
		TotalTokens:         total,
	}
}

func (s *commandCodeStreamState) streamChunk(delta map[string]any, finishReason any, usage map[string]any) []byte {
	chunk := map[string]any{
		"id":      s.ID,
		"object":  "chat.completion.chunk",
		"created": s.Created,
		"model":   s.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	data, _ := json.Marshal(chunk)
	return data
}

func commandCodeReadLines(reader io.Reader, handle func([]byte) error) error {
	buffered := bufio.NewReader(reader)
	var pending bytes.Buffer
	for {
		fragment, errRead := buffered.ReadSlice('\n')
		if pending.Len()+len(fragment) > commandCodeMaxStreamLineBytes {
			return statusErr{code: http.StatusBadGateway, msg: "CommandCode stream line exceeds the 16 MiB safety limit"}
		}
		_, _ = pending.Write(fragment)
		if errRead == bufio.ErrBufferFull {
			continue
		}
		if pending.Len() > 0 {
			line := bytes.TrimSuffix(pending.Bytes(), []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if errHandle := handle(bytes.Clone(line)); errHandle != nil {
				return errHandle
			}
			pending.Reset()
		}
		if errRead != nil {
			if errRead == io.EOF {
				return nil
			}
			return errRead
		}
	}
}

func commandCodeStreamReadError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := err.(interface{ StatusCode() int }); ok {
		return err
	}
	return statusErr{code: http.StatusBadGateway, msg: "CommandCode stream transport failed: " + err.Error()}
}

func collectCommandCodeStreamState(ctx context.Context, reader io.Reader, state *commandCodeStreamState) error {
	errRead := commandCodeReadLines(reader, func(line []byte) error {
		_, _, errHandle := commandCodeLineToOpenAIChunks(line, state)
		return errHandle
	})
	if errRead != nil {
		return commandCodeStreamReadError(ctx, errRead)
	}
	if !state.Terminal && state.Finish != "pause_turn" {
		return statusErr{code: http.StatusBadGateway, msg: "CommandCode stream ended unexpectedly before completion (no finish event)"}
	}
	return nil
}

func resetCommandCodeContinuationState(state *commandCodeStreamState) {
	state.Finish = ""
	state.RawFinish = ""
	state.Terminal = false
	state.SawDone = false
	state.Aborted = false
	state.pendingUsage = commandCodeUsage{}
}

func commandCodeResponseFromState(state *commandCodeStreamState, model string) []byte {
	finish := state.Finish
	if finish == "" || finish == "pause_turn" {
		finish = "stop"
	}
	message := map[string]any{"role": "assistant", "content": state.Text.String()}
	if reasoning := state.Reasoning.String(); reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(state.ToolCalls) > 0 {
		toolCalls := make([]any, 0, len(state.ToolCalls))
		for _, call := range state.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		message["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id": state.ID, "object": "chat.completion", "created": state.Created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if openAIUsage := state.Usage.openAIUsage(); openAIUsage != nil {
		resp["usage"] = openAIUsage
	}
	data, _ := json.Marshal(resp)
	return data
}

func collectCommandCodeResponse(ctx context.Context, reader io.Reader, model string) ([]byte, Usage, error) {
	state := newCommandCodeStreamState(model)
	if err := collectCommandCodeStreamState(ctx, reader, state); err != nil {
		return nil, Usage{}, err
	}
	return commandCodeResponseFromState(state, model), state.Usage.detail(), nil
}

func mapCommandCodeFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool-calls", "tool_calls", "tooluse", "tool_use":
		return "tool_calls"
	case "length", "max_tokens", "max-tokens", "max_output_tokens":
		return "length"
	case "pause_turn", "pause-turn":
		return "pause_turn"
	case "content-filter", "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func commandCodeErrorMessage(root gjson.Result) string {
	format := func(message string) string {
		message = strings.TrimSpace(message)
		if _, embedded, ok := commandCodeEmbeddedError(message); ok {
			return embedded
		}
		return message
	}
	if message := format(root.Get("message").String()); message != "" {
		return message
	}
	errorNode := root.Get("error")
	if errorNode.IsObject() {
		if message := format(errorNode.Get("message").String()); message != "" {
			return message
		}
	}
	if errorNode.Type == gjson.String {
		if message := format(errorNode.String()); message != "" {
			return message
		}
	}
	return "CommandCode stream error"
}
