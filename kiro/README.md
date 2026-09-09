# Kiro native plugin

Independent Kiro/Amazon Q executor and auth provider for CPA. It emits the
native `conversationState` request protocol, consumes binary AWS EventStream,
preserves Kiro IDE account fingerprints, and supports imported token files,
Google/GitHub social OAuth, Builder ID device and authorization-code login,
IAM Identity Center device and authorization-code login, and every matching
refresh flow.

The host auth-directory watcher feeds changed files to `auth.parse`; the plugin
does not watch files itself. Social and authorization-code flows use a
plugin-owned loopback callback listener and return the browser URL to the host.
The login variant is selected with `Metadata.login_method`; IDC flows also use
`Metadata.start_url` and optionally `Metadata.region`.

Inline token refresh persists through `host.auth.save` when the host supplies
the selected physical credential path in executor auth attributes. The plugin
uses only its validated `.json` basename and preserves unrelated auth metadata.

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
