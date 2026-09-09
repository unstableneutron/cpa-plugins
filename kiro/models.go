// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type modelInfo struct {
	ID, Object, OwnedBy, Type, DisplayName, Description string
	Created, ContextLength, MaxCompletionTokens         int64
	Thinking                                            *thinkingSupport
}

type thinkingSupport struct {
	Min, Max                    int
	ZeroAllowed, DynamicAllowed bool
}

func staticModels() []modelInfo {
	entries := []struct{ id, name string }{
		{"kiro-claude-sonnet-4-6", "Claude Sonnet 4.6"},
		{"kiro-claude-sonnet-4-5", "Claude Sonnet 4.5"},
		{"kiro-claude-sonnet-4", "Claude Sonnet 4"},
		{"kiro-claude-opus-4-7", "Claude Opus 4.7"},
		{"kiro-claude-opus-4-6", "Claude Opus 4.6"},
		{"kiro-claude-opus-4-5", "Claude Opus 4.5"},
		{"kiro-claude-haiku-4-5", "Claude Haiku 4.5"},
	}
	models := make([]modelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, newKiroModel(entry.id, entry.name, entry.name+" via Kiro", 200000))
	}
	return models
}

func newKiroModel(id, name, description string, contextLength int64) modelInfo {
	if contextLength <= 0 {
		contextLength = 200000
	}
	return modelInfo{ID: id, Object: "model", Created: 1732752000, OwnedBy: "aws", Type: providerID, DisplayName: name, Description: description, ContextLength: contextLength, MaxCompletionTokens: 64000, Thinking: &thinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true}}
}

func modelsForToken(ctx context.Context, transport Transport, token Token, authID string) ([]modelInfo, error) {
	if token.AccessToken == "" {
		return staticModels(), nil
	}
	region := "us-east-1"
	if parts := strings.Split(token.ProfileARN, ":"); len(parts) > 3 && parts[3] != "" {
		region = parts[3]
	}
	query := url.Values{"origin": {"AI_EDITOR"}}
	if token.ProfileARN != "" {
		query.Set("profileArn", token.ProfileARN)
	}
	seed := authID
	if seed == "" {
		seed = token.RefreshToken + token.AccessToken
	}
	fp := fingerprint(accountKey(seed))
	headers := map[string][]string{
		"Authorization":         {"Bearer " + token.AccessToken},
		"User-Agent":            {fmt.Sprintf("aws-sdk-js/%s ua/2.1 os/%s#%s lang/js md/nodejs#%s api/codewhispererruntime#%s m/N,E KiroIDE-%s-%s", fp.RuntimeSDKVersion, fp.OSType, fp.OSVersion, fp.NodeVersion, fp.RuntimeSDKVersion, fp.KiroVersion, fp.KiroHash)},
		"X-Amz-User-Agent":      {fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", fp.RuntimeSDKVersion, fp.KiroVersion, fp.KiroHash)},
		"Amz-Sdk-Request":       {"attempt=1; max=1"},
		"Amz-Sdk-Invocation-Id": {deterministicUUID([]byte("models:" + accountKey(seed)))},
	}
	response, err := transport.Do(ctx, HTTPRequest{Method: http.MethodGet, URL: "https://q." + region + ".amazonaws.com/ListAvailableModels?" + query.Encode(), Headers: headers})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &ProviderError{Status: response.StatusCode, Code: "model_discovery_failed", Message: "Kiro model discovery failed", Scope: "credential", Retryable: response.StatusCode == 429 || response.StatusCode >= 500, RetryAfterMS: retryAfterMS(response.Headers)}
	}
	var result struct {
		Models []struct {
			ModelID, ModelName, Description string
			RateMultiplier                  float64
			TokenLimits                     *struct{ MaxInputTokens int }
		}
	}
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, fmt.Errorf("decode Kiro models: %w", err)
	}
	models := make([]modelInfo, 0, len(result.Models))
	for _, model := range result.Models {
		id := normalizeKiroModelID(model.ModelID)
		if id == "" {
			continue
		}
		name := "Kiro " + model.ModelName
		if model.ModelName == "" {
			name = strings.TrimPrefix(id, "kiro-")
		}
		if model.RateMultiplier > 0 && model.RateMultiplier != 1 {
			name += fmt.Sprintf(" (%.1fx credit)", model.RateMultiplier)
		}
		contextLength := int64(0)
		if model.TokenLimits != nil {
			contextLength = int64(model.TokenLimits.MaxInputTokens)
		}
		models = append(models, newKiroModel(id, name, model.Description, contextLength))
	}
	if len(models) == 0 {
		return staticModels(), nil
	}
	return models, nil
}

func normalizeKiroModelID(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	id = strings.TrimPrefix(id, "anthropic.")
	id = strings.TrimPrefix(id, "amazon.")
	id = strings.NewReplacer(".", "-", "_", "-").Replace(id)
	if id == "" || strings.HasPrefix(id, "kiro-") {
		return id
	}
	return "kiro-" + id
}
