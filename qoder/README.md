# Qoder native plugin

Native Qoder provider extracted from `unstableneutron/CLIProxyAPIPlus` commit
`1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license. Reference
files are `internal/auth/qoder/*`, `sdk/auth/qoder.go`,
`internal/runtime/executor/qoder_executor.go`, and
`internal/runtime/executor/helps/qoder_encoding.go`.

The plugin independently implements PKCE device authorization, Qoder auth-file
parsing, long-lived-token refresh scheduling, COSY RSA/AES authentication,
encoded request bodies, dynamic model discovery/config persistence, OpenAI
chat-completions requests, tools, reasoning, usage, streaming and non-stream
SSE extraction, approximate token counting, typed errors, and host-owned
HTTP/cancellation.

Limitations: Qoder's observed device tokens cannot use the documented refresh
endpoint (it returns 403), so refresh is intentionally a no-op and expired
credentials require login again. Model discovery must succeed before inference
because Qoder requires its exact per-model configuration. Only the native
chat-completions format is advertised; cross-format translation remains a host
responsibility. No live or billable requests are made by tests.
