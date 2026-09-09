# Usage persistence migration qualification

This directory evaluates the published native
[`usage-statistics`](https://github.com/Fwindy/cpa-usage-statistics) plugin as a
replacement for CLIProxyAPIPlus's in-memory usage statistics. It does not copy
the plugin, its SQLite store, or a dashboard into this repository.

## Decision

**Do not deploy v0.1.0 where caller API keys or credential identifiers are
secrets.** The plugin is ABI-compatible and provides useful persistence and
basic diagnostics, but it persists and returns `APIKey`, `AuthID`, `AuthIndex`,
and failure bodies without pseudonymization. Management authentication protects
the HTTP route; it does not encrypt or redact the SQLite file or its response.
Plus hashes unsafe API identifiers and sanitizes source/auth-index values before
exposure.

The preferred minimum fix is a new upstream plugin release that pseudonymizes
those values before insertion and omits or bounds failure bodies. A companion
usage plugin cannot transform records delivered independently to this plugin.
If plugin ownership cannot provide that release, the minimum host seam is one
central redaction step before usage fan-out; it must preserve stable grouping
without exposing caller or upstream credentials. Neither option makes the
plugin a billing authority: attempt/round correctness stays host-owned.

The schema-7 host passes failure diagnostics through `SafeDiagnosticForLog`
before its execution warning: output is bounded to 300 runes and known
credential assignments, Bearer/Basic values, and URL userinfo are redacted.
Arbitrary non-credential upstream text can remain in that sanitized excerpt, as
the fixture marker demonstrates. This is a separate host logging-policy
consideration, not part of the native plugin deployment blocker above.

## Pinned provenance

Machine-readable pins and release checksums are in `provenance.json`.

- Official catalog: `router-for-me/CLIProxyAPI-Plugins-Store` at
  `30db57c833418bf59b59107aad322ab0c4f97041`.
- Plugin tag `v0.1.0`: annotated tag object
  `4226a32689a58e4b8f083261934825e2cba837bd`, peeled source commit
  `4653b2a18e3ffffa8a2cf2ea41c3e0f49be6d337`, MIT licensed.
- Plain host: `router-for-me/CLIProxyAPI`
  `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`.
- Schema-7 qualification host:
  `unstableneutron/cliproxyapi@47cdb06c016302070832d1690328e556143892c9`.
- Plus comparison: `unstableneutron/CLIProxyAPIPlus`
  `1fec8453e63a5bc133555a79164480700e351bfc`.

Review source before building. Release binaries were not executed.

## Coverage

| Plus behavior / requested outcome | v0.1.0 coverage |
| --- | --- |
| Per-request persistence | Yes, SQLite via `modernc.org/sqlite` |
| Input/output/reasoning/cached/total tokens | Yes |
| Cache-read/cache-creation tokens | Yes on hosts that provide them |
| Latency and TTFT | Yes, milliseconds |
| Failure count/details | Records failure flag, status and raw body; no aggregate endpoint |
| Time filtering, deletion, retention | Yes |
| Restart persistence | Yes |
| Management authentication | Host-enforced when `remote-management.secret-key` is configured |
| Stable safe identifiers / privacy | **No; blocker described above** |
| Aggregate dashboard/TUI | No; may be dropped if API/metrics suffice |
| Pricing or cost export | No |
| Plus snapshot import/export | No |
| Raw application log viewer | No |
| Attempt/round accounting authority | No; host must publish one correct terminal record |

At the pinned plain-upstream commit, default CGO loading passes this fixture,
but a `CGO_ENABLED=0 -tags plugin_purego` host reports that standard dynamic
loading requires CGO and does not register the plugin. The schema-7 baseline
passes with both default CGO and `plugin_purego` loaders. This is a host-version
compatibility result, not a property supplied by v0.1.0.

Pricing was not added here. Adding a second price catalog would duplicate a
fast-changing billing authority without fixing persistence. If cost reporting
is mandatory, use a separately maintained pricing/credit plugin and treat it as
an estimate unless the host's terminal usage record is already correct.

## Reproduce

Go 1.26 and a C compiler are required. The native plugin itself always requires
CGO because it is built with `-buildmode=c-shared`; `plugin_purego` only changes
the host loader.

```bash
git clone https://github.com/Fwindy/cpa-usage-statistics /tmp/cpa-usage-statistics
git -C /tmp/cpa-usage-statistics checkout 4653b2a18e3ffffa8a2cf2ea41c3e0f49be6d337
(cd /tmp/cpa-usage-statistics && go test ./...)
(cd /tmp/cpa-usage-statistics && CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/usage-statistics.so .)

# Build the selected pinned host checkout both ways.
CGO_ENABLED=1 go build -o /tmp/cpa-cgo ./cmd/server
CGO_ENABLED=0 go build -tags plugin_purego -o /tmp/cpa-purego ./cmd/server

python3 usage/qualify.py --host /tmp/cpa-cgo --plugin /tmp/usage-statistics.so
python3 usage/qualify.py --host /tmp/cpa-purego --plugin /tmp/usage-statistics.so
```

The fixture starts only loopback services, performs two successful requests and
one 429 request, waits for asynchronous usage delivery, checks independent
token totals and persisted failure diagnostics, proves management auth, restarts
CPA against the same SQLite file, and checks caller, management, and upstream
keys do not appear in host logs. It locks in the known v0.1.0 raw-caller-key and
raw-failure-body persistence gaps. It also records that arbitrary upstream text
can survive the host's bounded, credential-redacted diagnostic sanitization so
a future policy change updates the qualification result visibly.

`config.example.yaml` is a deployment template, not an installer. No plugin is
installed into a running CPA service by this repository.
