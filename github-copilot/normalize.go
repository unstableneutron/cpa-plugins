package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

func modelSuffix(model string) (string, string) {
	open := strings.LastIndex(model, "(")
	if open < 0 || !strings.HasSuffix(model, ")") {
		return model, ""
	}
	return model[:open], model[open+1 : len(model)-1]
}

func normalizeCopilotRequest(body map[string]any, path string) {
	stripUnsupportedBetas(body)
	if path == "/responses" {
		normalizeResponsesInput(body)
		normalizeResponsesTools(body)
		delete(body, "service_tier")
		if _, ok := body["store"]; !ok {
			body["store"] = false
		}
		if _, ok := body["include"]; !ok {
			body["include"] = []any{"reasoning.encrypted_content"}
		}
		return
	}
	if path == "/chat/completions" {
		flattenAssistantContent(body)
		normalizeChatTools(body)
	}
}

func stripUnsupportedBetas(body map[string]any) {
	filter := func(container map[string]any) {
		values, ok := container["betas"].([]any)
		if !ok {
			return
		}
		filtered := values[:0]
		for _, value := range values {
			if value != "context-1m-2025-08-07" {
				filtered = append(filtered, value)
			}
		}
		if len(filtered) == 0 {
			delete(container, "betas")
		} else {
			container["betas"] = filtered
		}
	}
	filter(body)
	if metadata, _ := body["metadata"].(map[string]any); metadata != nil {
		filter(metadata)
	}
}

func flattenAssistantContent(body map[string]any) {
	messages, _ := body["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message["role"] != "assistant" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		var text strings.Builder
		textOnly := true
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "text" {
				textOnly = false
				break
			}
			if value, ok := part["text"].(string); ok {
				text.WriteString(value)
			}
		}
		if textOnly {
			message["content"] = text.String()
		}
	}
}

func normalizeChatTools(body map[string]any) {
	if tools, ok := body["tools"].([]any); ok {
		filtered := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			if tool["type"] == "function" {
				filtered = append(filtered, tool)
			}
		}
		body["tools"] = filtered
	}
	if choice, ok := body["tool_choice"]; ok {
		value, valid := choice.(string)
		if !valid || value != "auto" && value != "none" && value != "required" {
			body["tool_choice"] = "auto"
		}
	}
}

func normalizeResponsesInput(body map[string]any) {
	if input, ok := body["input"]; ok {
		switch input.(type) {
		case string, []any:
			return
		default:
			encoded, _ := json.Marshal(input)
			body["input"] = string(encoded)
			return
		}
	}
	messages, _ := body["messages"].([]any)
	input := make([]any, 0, len(messages)+1)
	if system, ok := body["system"].(string); ok && system != "" {
		input = append(input, responseMessage("developer", system))
	}
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		if role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": textContent(message["content"])})
			continue
		}
		if content := responseContent(role, message["content"]); len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": function["arguments"]})
			}
		}
	}
	body["input"] = input
	delete(body, "messages")
	delete(body, "system")
}

func responseMessage(role, text string) map[string]any {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": textType, "text": text}}}
}

func responseContent(role string, value any) []any {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": textType, "text": text}}
	}
	parts, _ := value.([]any)
	content := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		switch part["type"] {
		case "text", "input_text", "output_text":
			content = append(content, map[string]any{"type": textType, "text": part["text"]})
		case "image_url", "input_image":
			imageURL := part["image_url"]
			if nested, ok := imageURL.(map[string]any); ok {
				imageURL = nested["url"]
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": imageURL})
		}
	}
	return content
}

func textContent(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	var text strings.Builder
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}

func normalizeResponsesTools(body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		typeName, _ := tool["type"].(string)
		if typeName == "computer" || typeName == "computer_use_preview" {
			filtered = append(filtered, tool)
			continue
		}
		function, _ := tool["function"].(map[string]any)
		name, _ := tool["name"].(string)
		if name == "" {
			name, _ = function["name"].(string)
		}
		if name == "" || typeName != "" && typeName != "function" {
			continue
		}
		normalized := map[string]any{"type": "function", "name": name}
		for _, key := range []string{"description", "parameters"} {
			if value, exists := tool[key]; exists {
				normalized[key] = value
			} else if value, exists = function[key]; exists {
				normalized[key] = value
			}
		}
		if _, ok := normalized["parameters"]; !ok {
			if schema, exists := tool["input_schema"]; exists {
				normalized["parameters"] = schema
			}
		}
		filtered = append(filtered, normalized)
	}
	body["tools"] = filtered
}

func applyThinkingSuffix(body map[string]any, suffix, path string) {
	effort := strings.ToLower(strings.TrimSpace(suffix))
	budget, numericErr := strconv.Atoi(effort)
	if numericErr == nil && budget >= 0 && path == "/v1/messages" {
		applyClaudeBudget(body, budget)
		return
	}
	if numericErr == nil && budget >= 0 {
		effort = budgetEffort(budget)
	}
	switch effort {
	case "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return
	}
	if path == "/responses" {
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = make(map[string]any)
			body["reasoning"] = reasoning
		}
		reasoning["effort"] = effort
		if _, ok := reasoning["summary"]; !ok {
			reasoning["summary"] = "auto"
		}
	} else if path == "/v1/messages" {
		model, _ := body["model"].(string)
		if strings.Contains(strings.ToLower(model), "4.6") {
			body["thinking"] = map[string]any{"type": "adaptive"}
			body["output_config"] = map[string]any{"effort": effort}
		} else if effort == "auto" {
			body["thinking"] = map[string]any{"type": "enabled"}
		} else {
			applyClaudeBudget(body, map[string]int{"none": 0, "minimal": 512, "low": 1024, "medium": 8192, "high": 24576, "xhigh": 32768, "max": 128000}[effort])
		}
	} else {
		body["reasoning_effort"] = effort
	}
}

func applyClaudeBudget(body map[string]any, budget int) {
	delete(body, "output_config")
	if budget == 0 {
		body["thinking"] = map[string]any{"type": "disabled"}
		return
	}
	if maxTokens, ok := body["max_tokens"].(float64); ok && maxTokens > 0 && budget >= int(maxTokens) {
		budget = int(maxTokens) - 1
	}
	body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
}

func budgetEffort(budget int) string {
	switch {
	case budget == 0:
		return "none"
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

func normalizeCopilotReasoning(raw []byte) []byte {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	choices, _ := body["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		for _, key := range []string{"message", "delta"} {
			part, _ := choice[key].(map[string]any)
			if part == nil || part["reasoning_content"] != nil {
				continue
			}
			if reasoning, ok := part["reasoning_text"].(string); ok && reasoning != "" {
				part["reasoning_content"] = reasoning
			}
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return encoded
}

type sseReasoningNormalizer struct {
	pending []byte
}

func (n *sseReasoningNormalizer) Push(chunk []byte, final bool) []byte {
	n.pending = append(n.pending, chunk...)
	var output []byte
	for {
		index, separatorLength := nextSSESeparator(n.pending)
		if index < 0 {
			break
		}
		output = append(output, normalizeSSEEvent(n.pending[:index])...)
		output = append(output, n.pending[index:index+separatorLength]...)
		n.pending = n.pending[index+separatorLength:]
	}
	if final && len(n.pending) > 0 {
		output = append(output, normalizeSSEEvent(n.pending)...)
		n.pending = nil
	}
	return output
}

func nextSSESeparator(raw []byte) (int, int) {
	lf := bytes.Index(raw, []byte("\n\n"))
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	if lf < 0 {
		return crlf, 4
	}
	if crlf < 0 || lf < crlf {
		return lf, 2
	}
	return crlf, 4
}

func normalizeSSEEvent(event []byte) []byte {
	lines := bytes.Split(event, []byte("\n"))
	for index, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		lines[index] = append([]byte("data: "), normalizeCopilotReasoning(data)...)
	}
	return bytes.Join(lines, []byte("\n"))
}
