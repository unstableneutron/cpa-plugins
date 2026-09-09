# Bedrock native plugin

Independent Bedrock Runtime executor for CPA. It preserves the owner-added Plus
implementation's Converse, ConverseStream, InvokeModel, and
InvokeModelWithResponseStream codecs. Authentication is intentionally limited to
the inspected `bearer`, raw `Authorization`, and `none` modes; this plugin does
not claim AWS SigV4 support.

## Provenance

Derived under the included MIT license from
`unstableneutron/CLIProxyAPIPlus@1fec8453e63a5bc133555a79164480700e351bfc`:

- `internal/runtime/executor/bedrock_executor.go`
- `internal/runtime/executor/helps/bedrock_runtime.go`
- `internal/runtime/executor/helps/bedrock_eventstream.go`
- `internal/runtime/executor/helps/bedrock_http_client.go`
- `internal/config/bedrock.go`

The module has no dependency on CLIProxyAPI or CLIProxyAPIPlus.

## Host compatibility

The source transport explicitly sets TLS minimum 1.2, ALPN `h2,http/1.1`, and
curves `X25519,P-256,P-384,P-521`. Every Bedrock request selects those curves
through `wire_profile.tls_curves`; normal host transport preserves TLS 1.2+
and HTTP/2 ALPN behavior. The plugin therefore requires CPA schema 8 or newer.
