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
released plugins in this repository yet.

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
