package main

import (
	"context"
	"strings"
	"testing"
)

func TestKiroDynamicModelsAndStaticFallback(t *testing.T) {
	fixture := &transportFixture{responses: []HTTPResponse{{StatusCode: 200, Body: []byte(`{"models":[{"modelId":"anthropic.claude-sonnet-4.5","modelName":"Claude Sonnet 4.5","description":"Fast","rateMultiplier":1.3,"tokenLimits":{"maxInputTokens":123456}}]}`)}}}
	models, err := modelsForToken(context.Background(), fixture, Token{AccessToken: "secret", RefreshToken: "refresh", ProfileARN: "arn:aws:codewhisperer:eu-west-1:1:profile/x"}, "auth-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "kiro-claude-sonnet-4-5" || models[0].ContextLength != 123456 || models[0].DisplayName != "Kiro Claude Sonnet 4.5 (1.3x credit)" {
		t.Fatalf("models = %+v", models)
	}
	request := fixture.requests[0]
	if !strings.HasPrefix(request.URL, "https://q.eu-west-1.amazonaws.com/ListAvailableModels?") || !strings.Contains(request.URL, "profileArn=") {
		t.Fatalf("URL = %q", request.URL)
	}
	if request.Headers["Authorization"][0] != "Bearer secret" || !strings.Contains(request.Headers["User-Agent"][0], "api/codewhispererruntime#1.0.0") {
		t.Fatalf("headers = %+v", request.Headers)
	}

	models, err = modelsForToken(context.Background(), &transportFixture{}, Token{}, "")
	if err != nil || len(models) != 7 || models[0].Thinking == nil || models[0].Thinking.Max != 32000 {
		t.Fatalf("fallback models = %+v, err = %v", models, err)
	}
}
