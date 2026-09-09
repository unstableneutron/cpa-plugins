# GitHub Copilot native plugin

Native GitHub Copilot provider extracted from `unstableneutron/CLIProxyAPIPlus`
commit `1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license.
Reference sources are `internal/runtime/executor/github_copilot_executor.go`,
`internal/auth/copilot/*`, and `internal/registry/model_definitions.go`.

Implemented here are GitHub device authorization, Copilot API-token exchange,
trusted per-account endpoints, dynamic models, native chat/Responses/Claude
gateway paths, vision and continuation headers, provider request normalization,
thinking suffixes, local token counting, raw incremental streaming, typed
errors, cancellation, and upstream usage payload forwarding. Outbound traffic
always uses host HTTP callbacks. Token counting pins
`github.com/tiktoken-go/tokenizer` v0.8.1, also MIT licensed, whose vocabularies
are compiled into the plugin and require no runtime download.
Upstream 401/403 failures are credential-scoped, 404 is model-scoped, and
throttling/server failures remain unscoped so host health cooldown applies;
`Retry-After` is propagated through the typed failure envelope.
