package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/unstableneutron/cpa-plugins/commandcode/usage"
)

const (
	commandCodeProviderKey        = "commandcode"
	defaultCommandCodeAPIBase     = "https://api.commandcode.ai"
	commandCodeVersionHeader      = "1.15.0"
	commandCodeMaxTokensCap       = 64000
	commandCodeDefaultUserAgent   = "cli"
	commandCodeContinuationLimit  = 5
	commandCodeMaxStreamLineBytes = 16 << 20
)

type statusErr struct {
	code int
	msg  string
}

func (e statusErr) Error() string   { return e.msg }
func (e statusErr) StatusCode() int { return e.code }

type commandCodeAbortError struct{ statusErr }
type commandCodeProviderError struct {
	statusErr
	errorCode string
}

func (e commandCodeProviderError) ErrorCode() string { return e.errorCode }

type commandCodePayloadOptions struct {
	Model, WorkingDir, Environment, ThreadID string
	Payload                                  []byte
	Now                                      func() time.Time
}

type commandCodeOpenAIRequest struct {
	Messages            []commandCodeOpenAIMessage `json:"messages"`
	Tools               []commandCodeOpenAITool    `json:"tools"`
	MaxTokens           *int                       `json:"max_tokens"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens"`
	ReasoningEffort     string                     `json:"reasoning_effort"`
	Temperature         *float64                   `json:"temperature"`
	TopP                *float64                   `json:"top_p"`
	Stop                json.RawMessage            `json:"stop"`
}

type commandCodeOpenAIMessage struct {
	Role             string                      `json:"role"`
	Content          json.RawMessage             `json:"content"`
	Name             string                      `json:"name"`
	ToolCallID       string                      `json:"tool_call_id"`
	ToolCalls        []commandCodeOpenAIToolCall `json:"tool_calls"`
	ReasoningContent string                      `json:"reasoning_content"`
	ProviderExecuted bool                        `json:"providerExecuted"`
}

type commandCodeOpenAIToolCall struct {
	ID               string                        `json:"id"`
	Type             string                        `json:"type"`
	ProviderExecuted bool                          `json:"providerExecuted"`
	Function         commandCodeOpenAIToolFunction `json:"function"`
}

type commandCodeOpenAIToolFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type commandCodeOpenAITool struct {
	Type     string                            `json:"type"`
	Function commandCodeOpenAIToolDefinitionFn `json:"function"`
}

type commandCodeOpenAIToolDefinitionFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type commandCodeBody struct {
	Config         commandCodeConfig `json:"config"`
	Memory         any               `json:"memory"`
	Taste          any               `json:"taste"`
	Skills         any               `json:"skills"`
	PermissionMode string            `json:"permissionMode"`
	ThreadID       string            `json:"threadId"`
	Params         commandCodeParams `json:"params"`
}

type commandCodeConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type commandCodeParams struct {
	Model           string          `json:"model"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Messages        []any           `json:"messages"`
	Tools           []any           `json:"tools"`
	System          string          `json:"system"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	Stream          bool            `json:"stream"`
}

type commandCodeToolCall struct{ ID, Name, Arguments string }

type Usage = usage.Detail

type commandCodeUsage struct {
	InputTokens, OutputTokens, ReasoningTokens, CacheReadTokens, CacheWriteTokens, TotalTokens int64
}

type commandCodeStreamState struct {
	ID, Model, Finish, RawFinish, lastAnonymousToolID string
	Created                                           int64
	Text, Reasoning                                   strings.Builder
	ToolCalls                                         []commandCodeToolCall
	toolCallIndexByID                                 map[string]int
	toolCallStartedByID                               map[string]bool
	Usage, pendingUsage                               commandCodeUsage
	Terminal, SawDone, Aborted                        bool
	providerExecutedTools                             map[string]bool
	toolSchemas                                       map[string]map[string]any
}
