# CPA Plugins

Independent native plugins for [CLIProxyAPI](https://github.com/unstableneutron/CLIProxyAPI).
Provider implementation belongs here; generic plugin ABI and host changes belong
in CLIProxyAPI. Plugins are built separately and installed independently, even
when they share a repository release version.

## Migration order

1. Command Code: preserve the Plus provider's streaming, continuation, errors,
   model catalog and accounting before replacing it.
2. ChatGPT backend HTTP passthrough: authenticated ingress, credential selection,
   streaming and transport policy through bounded host capabilities.
3. Evaluate the published usage-statistics plugin instead of duplicating its
   persistence; retain host-owned accounting correctness. The pinned
   qualification and current privacy blocker are documented in
   [`usage/`](usage/README.md).
4. Test Responses/Codex/xAI overlays against upstream and remove redundant fixes.
5. Independently extract remaining used HTTP providers after the contracts settle.

Amp and Gemini CLI are excluded. Cursor transport is deferred. There are no
production-qualified releases; initial artifacts are explicitly prereleases.

## Source and validation policy

The initial parity reference is `unstableneutron/CLIProxyAPIPlus`, commit
`1fec8453e63a5bc133555a79164480700e351bfc`. Preserve source license notices and
record extracted files in each plugin's documentation. Do not import private
CPA packages or depend on a local Plus checkout in published modules.

Each plugin must pass unit, race, native ABI and host integration checks before
release. Use deterministic local upstream fixtures; live checks require explicitly
configured credentials and must never expose those credentials in logs or assets.
Do not replace existing Plus providers until error, stream and usage parity pass.

## Distribution

Each plugin has a separate directory, ID, configuration and shared library. A
shared versioned release contains one ZIP per plugin/platform and SHA-256
checksums. The library must be at its ZIP root. A registry entry per plugin points
to this repository; installing one plugin must not install the others.

The host's `plugin_purego` backend is opt-in and restart-only. Default CPA
packaging is unchanged. Building Go native plugins themselves requires CGO;
the host loader choice does not make a Go `c-shared` library safely unloadable.

## Reproducible tasks and release boundary

Install mise, then run `mise install`. Native builds require a working C compiler.
Smoke tests also require Python 3, unzip, SHA-256 utilities, and separately built
reviewed CPA executables: default CGO and `CGO_ENABLED=0 -tags plugin_purego`.
Publication requires authenticated `gh` with release permission. No live provider
credentials are needed for the deterministic tests.

```bash
mise run check
mise run compile
mise run test
mise run race
mise run vuln
VERSION=0.1.0 mise run package
HOST_CGO=/absolute/path/cpa-cgo HOST_PUREGO=/absolute/path/cpa-purego \
  VERSION=0.1.0 mise run smoke
```

CI calls the same tasks. `build`/`package` currently select only `commandcode` and
`chatgpt-backend`: the pair covered by combined real-server smoke. Output is
`dist/<plugin>_<version>_<os>_<arch>.zip`, with the library at the ZIP root and
`checksums.txt` containing SHA-256 checksums. Artifacts are not committed. The
initial publication gate supports Linux amd64 only; Go target naming is used
(`amd64`, not `x86_64`). Other platforms require their own smoke qualification.

Both libraries use ABI 1. Command Code advertises schema 7; backend ingress
requires host schema 8. Use the schema-8 host source accompanying this migration,
not an older binary merely because it supports the purego loader. Plugin helper
version is injected at build time. Provider provenance is pinned above and in
each provider README; release `BUILDINFO.txt` records the exact plugin commit,
toolchain, version, and checksums of the host executables used for smoke tests.

After an authorized normal source push, publication is explicit and reruns the
gates against that exact clean, pushed commit:

```bash
HOST_CGO=/absolute/path/cpa-cgo HOST_PUREGO=/absolute/path/cpa-purego \
  VERSION=0.1.0 mise run release:publish
```

This creates a plugin-only GitHub prerelease. It refuses existing tags and does
not trigger any application release or deployment. Live OAuth/provider calls,
macOS/Windows execution, and production readiness are not implied by smoke tests.
Bedrock, Kiro, CodeBuddy, Kilo, Copilot, Devin, and Qoder work is preserved on a
local qualification branch, excluded from shipped main and artifacts until
in-flight async shutdown and provider-specific smoke qualification are complete.
GitLab/iFlow remain unintegrated partial work. The existing usage plugin is
excluded for the documented raw-identifier privacy blocker.
