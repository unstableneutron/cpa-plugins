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

Ship defaults to source only: normal pushes, never published-history rewrites.
Run check/test/race/vuln, commit, rerun against clean source, then fetch origin
immediately before pushing and stop if it changed. Plugin publication requires
separate authorization for this repository, exact source, new version and artifact
set. CPA-only changes never authorize plugin publication. Never tag/release CPA,
deploy services, expose credentials, or package auth files through these tasks.

Install mise, then `mise install`. Builds require a C compiler; qualification uses
Python 3, readelf, and separately built clean CPA hosts. No live credentials are
needed. In CPA, `HOST_DIR=/tmp/cpa-hosts ARCH=amd64 mise run build:smoke` produces
`linux_amd64/cpa-cgo` and `cpa-purego`. Repeat with `ARCH=arm64 CC=aarch64-linux-gnu-gcc`.
The plugin smoke validates both targets, distinct loader settings, clean matching
host revisions and hashes; BUILDINFO includes only allowlisted provenance.

```bash
mise run check
mise run test
mise run race
mise run vuln
VERSION=0.1.1 ARCH=amd64 mise run package
VERSION=0.1.1 ARCH=arm64 CC=aarch64-linux-gnu-gcc mise run package
VERSION=0.1.1 ARCH=amd64 HOST_CGO=/tmp/cpa-hosts/linux_amd64/cpa-cgo \
  HOST_PUREGO=/tmp/cpa-hosts/linux_amd64/cpa-purego mise run smoke
VERSION=0.1.1 ARCH=arm64 HOST_CGO=/tmp/cpa-hosts/linux_arm64/cpa-cgo \
  HOST_PUREGO=/tmp/cpa-hosts/linux_arm64/cpa-purego mise run smoke
```

Versions above are examples, not publication authorization. `package` (`build`
alias) captures/rechecks clean HEAD; optionally set `PLUGIN_COMMIT` to an approved
SHA. Outputs are isolated under `dist/<version>/linux_<arch>/`, with deterministic
`<plugin>_<version>_linux_<arch>.zip` names, root-level libraries, `BUILDINFO.json`
and SHA-256 `checksums.txt`. `smoke` never rebuilds: it verifies/extracts those ZIPs
and tests both loader/account modes and denial classifications. It records ELF
architecture, dependencies, GLIBC symbols and native/emulated execution. Both
libraries use ABI 1; Command Code schema 7, backend minimum host schema 8.

For same-orb arm64 qualification on Debian 12 amd64, the tested provisioning is:

```bash
sudo apt-get install gcc-aarch64-linux-gnu=4:12.2.0-3 \
  gcc-12-aarch64-linux-gnu=12.2.0-14cross1 binutils-aarch64-linux-gnu=2.40-2 \
  libc6-dev-arm64-cross=2.36-8cross1 libc6-arm64-cross=2.36-8cross1 \
  qemu-user=1:7.2+dfsg-7+deb12u18+b3
```

Cross smoke uses `qemu-aarch64 -L /usr/aarch64-linux-gnu` (override sysroot with
`ARM64_SYSROOT`). Pin and requalify toolchain/sysroot/emulator updates; package
availability may require a Debian snapshot. Compilation alone is not qualification.
The libc runtime tested here is 2.36; ELF symbol requirements are recorded, not a
claim that an older runtime was tested. These are glibc, not musl/Alpine artifacts.
macOS requires a macOS runner for load/callback/shutdown qualification and is
excluded from this Linux-only publisher. CI currently runs check/compile/test/race,
not cross-platform smoke or publication. Live smoke remains separate and opt-in.

After an authorized source push, publish the exact four ZIPs only when BOTH Linux
targets qualify. The publisher reruns check/test/race/vuln and all smoke against
the supplied host pairs without rebuilding packages:

```bash
VERSION=<new-authorized-version> HOST_DIR=/tmp/cpa-hosts \
  PLUGIN_COMMIT=<optional-approved-sha> mise run release:publish
```

Publication requires authenticated `gh`, confirmation, clean source matching the
explicit repository's main, and a new tag. It creates only a prerelease with
`latest=false`, four ZIPs, two target BUILDINFO files and combined checksums.
It downloads the published set and compares its manifest and hashes. This detects
post-publication problems; it is not an atomic preventive gate. Existing tags and
partial failures stop: inspect tag target/assets/checksums; never overwrite or
claim idempotent resume. Stable promotion needs separate authorization and
qualification. When host compatibility with v0.1.0 matters, verify downloaded
release checksums and exercise those extracted libraries too; do not rebuild them.

Bedrock, Kiro, CodeBuddy, Kilo, Copilot, Devin, and Qoder work is preserved on a
local qualification branch, excluded from shipped main and artifacts until
in-flight async shutdown and provider-specific smoke qualification are complete.
GitLab/iFlow remain unintegrated partial work. The existing usage plugin is
excluded for the documented raw-identifier privacy blocker.
