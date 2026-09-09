# Kilo native plugin

Native Kilo provider extracted from `unstableneutron/CLIProxyAPIPlus` commit
`1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license. Reference
sources: `internal/runtime/executor/kilo_executor.go`, `internal/auth/kilo/*`,
and `internal/registry/kilo_models.go`.

Implemented here are Kilo device authorization, auth parsing, organization
routing, dynamic curated-free model discovery, OpenRouter chat payloads, raw
SSE streaming, thinking suffixes, custom auth headers, typed errors,
cancellation, and usage payload forwarding. Outbound traffic uses host HTTP
callbacks. Global host payload override rules remain host-owned rather than
duplicated in this plugin.
Upstream 401/403 failures are credential-scoped, 404 is model-scoped, and
throttling/server failures remain unscoped so host health cooldown applies;
`Retry-After` is propagated through the typed failure envelope.
