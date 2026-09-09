package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tiktoken-go/tokenizer"

	"github.com/unstableneutron/cpa-plugins/internal/nativeabi"
)

func countTokens(raw []byte) (any, *nativeabi.Error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	model, _ := modelSuffix(strip(req.Model))
	codec, err := tokenizerForModel(model)
	if err != nil {
		return nil, fail("tokenizer_failed", err.Error(), 500, "")
	}
	var payload any
	if err = json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, fail("invalid_request", err.Error(), 400, "request")
	}
	segments := make([]string, 0, 32)
	collectTokenStrings(payload, &segments)
	count, err := codec.Count(strings.Join(segments, "\n"))
	if err != nil {
		return nil, fail("tokenizer_failed", err.Error(), 500, "")
	}
	if strings.Contains(strings.ToLower(model), "claude") {
		count = int(float64(count) * 1.1)
	}
	format := first(req.SourceFormat, req.Format, "chat-completions")
	var response []byte
	if format == "claude" || format == "responses" || format == "openai-response" {
		response, _ = json.Marshal(map[string]any{"input_tokens": count})
	} else {
		response = []byte(fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`, count, count))
	}
	return map[string]any{"Payload": response}, nil
}

func tokenizerForModel(model string) (tokenizer.Codec, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(model, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(model, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(model, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(model, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	case strings.HasPrefix(model, "o1"):
		return tokenizer.ForModel(tokenizer.O1)
	case strings.HasPrefix(model, "o3"):
		return tokenizer.ForModel(tokenizer.O3)
	case strings.HasPrefix(model, "o4"):
		return tokenizer.ForModel(tokenizer.O4Mini)
	case strings.Contains(model, "claude"):
		return tokenizer.Get(tokenizer.Cl100kBase)
	default:
		return tokenizer.Get(tokenizer.O200kBase)
	}
}

func collectTokenStrings(value any, segments *[]string) {
	switch value := value.(type) {
	case string:
		if value = strings.TrimSpace(value); value != "" {
			*segments = append(*segments, value)
		}
	case []any:
		for _, item := range value {
			collectTokenStrings(item, segments)
		}
	case map[string]any:
		for _, key := range []string{"system", "instructions", "input", "prompt", "messages", "content", "role", "name", "description", "arguments", "parameters", "input_schema", "tools", "functions", "tool_choice", "tool_calls", "function", "text", "output"} {
			if item, ok := value[key]; ok {
				collectTokenStrings(item, segments)
			}
		}
	}
}
