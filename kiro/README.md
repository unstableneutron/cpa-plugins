# Kiro native plugin

Independent Kiro/Amazon Q executor and auth provider for CPA. It emits the
native `conversationState` request protocol, consumes binary AWS EventStream,
preserves Kiro IDE account fingerprints, and supports imported token files,
Builder ID device login, Builder ID/IDC OIDC refresh, and social-token refresh.

The host auth-directory watcher feeds changed files to `auth.parse`; the plugin
does not watch files itself. Native login start supports only the Builder ID
device flow. Google/GitHub social login start and IAM Identity Center login
start remain unsupported; already imported tokens for those variants can be
parsed and refreshed.

## Provenance

Derived under the included MIT license from
`unstableneutron/CLIProxyAPIPlus@1fec8453e63a5bc133555a79164480700e351bfc`:

- `internal/runtime/executor/kiro_executor.go` and tests
- `internal/auth/kiro/`
- `internal/translator/kiro/{common,claude,openai}/`
- `internal/registry/kiro_model_converter.go`

The plugin uses CPA host HTTP callbacks and has no CLIProxyAPI or
CLIProxyAPIPlus dependency. Kiro's source transport uses ordinary Go HTTP/2;
schema 7's standard host transport preserves that behavior without a custom
wire profile.
