# Kilo native plugin

Native Kilo provider extracted from `unstableneutron/CLIProxyAPIPlus` commit
`1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license. Reference
sources: `internal/runtime/executor/kilo_executor.go`, `internal/auth/kilo/*`,
and `internal/registry/kilo_models.go`.

The plugin preserves Kilo device authorization, auth parsing, organization
routing, dynamic curated-free model discovery, OpenRouter chat payloads, raw
SSE streaming, errors, cancellation, and usage payloads. Outbound traffic uses
host HTTP callbacks so proxy and request logging policy remain host-owned.
