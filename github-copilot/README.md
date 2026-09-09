# GitHub Copilot native plugin

Native GitHub Copilot provider extracted from `unstableneutron/CLIProxyAPIPlus`
commit `1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license.
Reference sources are `internal/runtime/executor/github_copilot_executor.go`,
`internal/auth/copilot/*`, and `internal/registry/model_definitions.go`.

Implemented here are GitHub device authorization, Copilot API-token exchange,
trusted per-account endpoints, dynamic models, native chat/Responses/Claude
gateway paths, vision and continuation headers, raw incremental streaming,
typed errors, cancellation, and upstream usage payload forwarding. Outbound
traffic always uses host HTTP callbacks. Plus's extra payload normalization and
local tokenizer-based count endpoint are not yet extracted.
