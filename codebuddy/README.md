# CodeBuddy native plugin

Native CodeBuddy provider extracted from `unstableneutron/CLIProxyAPIPlus` at
`1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license. Reference
sources: `internal/runtime/executor/codebuddy_executor.go`,
`internal/auth/codebuddy/*`, and `internal/registry/codebuddy_models.go`.

Implemented here are CodeBuddy browser-state login, polling, refresh, auth
parsing, the complete pinned model catalog, OpenAI chat request/response
handling, tool/reasoning/usage aggregation, and incremental SSE forwarding.
All outbound requests use host HTTP callbacks. Plus's payload-config and
thinking-suffix transforms are not yet extracted.
