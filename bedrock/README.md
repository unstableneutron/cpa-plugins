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

## Transport parity limitation

The source transport explicitly sets TLS minimum 1.2, ALPN `h2,http/1.1`, and
curves `X25519,P-256,P-384,P-521`. Host baseline `47cdb06` supports HTTP/1-only,
compression, and HTTP/1 header profiles, but it cannot constrain TLS curves.
The plugin therefore uses the host's normal HTTP/2-capable transport. Exact TLS
parity requires one bounded host hook: add an optional `tls_curves` list to
`HTTPWireProfile` and apply it to a profile-private `tls.Config.CurvePreferences`.
