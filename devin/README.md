# Devin native plugin

Native Devin (Codeium Cascade) provider extracted from
`unstableneutron/CLIProxyAPIPlus` commit
`1fec8453e63a5bc133555a79164480700e351bfc` under its MIT license.
Reference files are `internal/runtime/executor/devin_executor.go`,
`internal/runtime/executor/devin_protobuf.go`, and the Devin model registry.

The plugin independently implements session-token auth parsing, Connect-RPC
framing, protobuf request/response codecs, OpenAI chat-completions requests,
tool calls, reasoning, usage, streaming, non-stream aggregation, typed errors,
and host-owned HTTP/cancellation. Provide `devin_session_token`,
`session_token`, or `api_key` in a Devin auth JSON file.

Limitations: Devin has no known device login, refresh, or token-count endpoint;
its session token is static. The donor currently publishes no static Devin
model catalog, so models must be routed by configured aliases. Only the native
chat-completions format is advertised; cross-format translation remains a host
responsibility.
