# Command Code native plugin

Native Command Code provider for CLIProxyAPI. It declares OpenAI request and
response formats so the host owns all generic protocol translations. Provider
traffic uses only the host HTTP callbacks.

## Build

```sh
go build -buildmode=c-shared \
  -ldflags '-X github.com/unstableneutron/cpa-plugins/internal/nativeabi.Version=VERSION' \
  -o commandcode.so ./commandcode
```

Building a Go `c-shared` library requires CGO. The resulting ABI v1 library is
compatible with both CLIProxyAPI's default CGO loader and its opt-in
`plugin_purego` loader. Go shared libraries are restart-only and must not be
unloaded/reinitialized in-process.

## Configuration and auth

The provider identifier is `commandcode`. Put an auth JSON file in the host's
configured auth directory with `type: "commandcode"` and any of `api_key`,
`apiKey`, `access_token`, or `access`. `base_url`/`baseURL` overrides the default
`https://api.commandcode.ai`. `header:<Name>` auth attributes are forwarded as
custom upstream headers. The canonical `COMMAND_CODE_API_KEY` and
`COMMANDCODE_API_URL` environment variables retain precedence over their
legacy `COMMANDCODE_API_KEY` and `COMMANDCODE_API_BASE` aliases. Auth data
overrides environment defaults. Credentials are never logged.

This plugin parses existing auth JSON; it does not provide an interactive login
flow. Plain CLIProxyAPI does not synthesize these credentials from the legacy
`commandcode-api-key` YAML section.

Live models are read from `/provider/v1/models`; the embedded 1.15.0 catalog is
used when discovery fails or is invalid. Host model aliases and exclusions are
applied to both paths.

## Source provenance

The provider conversion, model catalog, stream state machine, and token-count
logic were extracted and adapted from
[`unstableneutron/CLIProxyAPIPlus`](https://github.com/unstableneutron/CLIProxyAPIPlus)
commit `1fec8453e63a5bc133555a79164480700e351bfc`:

- `internal/runtime/executor/commandcode_executor.go`
- `internal/runtime/executor/commandcode_executor_test.go`
- `internal/registry/commandcode_model_definitions.go`
- `internal/runtime/executor/helps/token_helpers.go`

That source is MIT licensed. Its copyright and permission notice are retained
in [LICENSE](LICENSE).
