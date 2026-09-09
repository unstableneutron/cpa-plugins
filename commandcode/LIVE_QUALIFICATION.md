# Command Code live qualification and quota evidence

Observed 2026-09-09, Linux amd64, Go 1.26.6. No follow-up publication is implied.
The live library was the previously reviewed v0.1.0 artifact from source
`07178463e3ddcfc7a85fcc4ed55fdb6a5bfb1297`; archive SHA-256:
`8f199142a16de2e4c6d46ce4d422a6716b3622500dd5b8e2c6f99b51054ecdea`.
Host: `afaf2d608a103c75d3695ccb22c1c47ce3c96703` plus the local generic
`plugins.api-keys` environment-reference configuration change.

## Opt-in reproduction

Requires Python 3, a C compiler, pinned mise tools, a reviewed host with generic
environment-backed plugin accounts, and the reviewed Linux amd64 ZIP. Build hosts
in the host repository using `OUTPUT=/absolute/path mise run build` and
`OUTPUT=/absolute/path mise run build:purego`. The plugin repo's `mise run smoke`
packages/checksums and runs fake-upstream CGO/purego E2E. Set
`ENVIRONMENT_AUTH_SMOKE=1` to additionally test environment-backed accounts;
`HOST_CGO` and `HOST_PUREGO` identify the two host binaries. Keep real provider
secrets out of deterministic build/test/smoke subprocesses.

Only with explicit live-call authorization and an environment-injected
`COMMANDCODE_API_KEY`, run in the plugin repo:

```sh
LIVE_PHASE=native HOST_CGO=/absolute/path/to/host mise run smoke:live
```

`COMMANDCODE_ARCHIVE` optionally selects the reviewed archive (default
`dist/commandcode_0.1.0_linux_amd64.zip`). The task verifies the release checksum
above before loading native code; set `COMMANDCODE_SHA256` only after separately
reviewing a different artifact (local repackaging can change the archive checksum).
`LIVE_PHASE=documented` observes only
the documented Provider API; `all` runs both. This task is not a CI or release
dependency. It has no retries or model fallback and exits nonzero for unsuccessful
qualification, including documented generation denied by account entitlement.
Do not use shell tracing, dump environment variables, or paste the key into
arguments. Do not rerun just to obtain different quota/error results.

Raw responses/headers and host stdout remain in memory; only field names, numeric
usage, statuses, and bounded error classifications are printed. Plain, URL-encoded,
and base64 key variants are checked before output. Temporary configuration has
only the variable name. Host commercial mode disables request/error log middleware;
`request-log: false` alone does not disable error files. All temporary files and
stdout are scanned before deletion. No auth JSON is created for the live key.

## Observations and their limits

| Endpoint / case | Result | Stability / confidence |
|---|---|---|
| GET `/provider/v1/models` | 200; 67 entries; `id`, `name`, `object`, `owned_by`, `created`, `context_length` | Documented endpoint; one account/time sample, not entitlement proof |
| POST `/provider/v1/chat/completions`, small valid request | 403; `upgrade_required`, `permission_error` | Documented API entitlement boundary; exact subscription identity unknown |
| Same, deliberately invalid model | 400; `unsupported_model`, `invalid_request_error` | Documented error; no quota exhaustion attempted |
| Native plugin `/alpha/generate`, nonstream via host | 200, content; prompt 7650, completion 17, total 7667 | Existing CLI/donor protocol, not a documented Provider API contract |
| Native plugin, stream via host | 200, content, terminal `[DONE]`; prompt 7571, completion 2, total 7573 | Same protocol; real final usage observed |
| Native plugin, cancel after first content | 200 then client disconnect; host exit 0 | Client cancellation observed; upstream billing cessation cannot be proven from the client |
| Invalid frontend key | 401 | Host authorization exercised |

The native CLI path and documented Provider API have distinct access behavior;
the 403 is not evidence of an invalid key or evidence that the account has a
particular named plan. No alternate Provider API route or credential was tried.
The native request uses the already-shipped plugin protocol, not a new endpoint
probe. Its enriched prompt explains why a tiny user prompt need not be cheap;
the token counts are provider-reported, not independently audited billing amounts.

Two early native attempts returned local `503 auth_not_found` before account
initialization. Static models were visible first. Waiting for completed host
account initialization resolved it; no host routing behavior change was needed.

No quota/limit/reset/remaining/credit/retry-after headers appeared on the direct
documented responses or the observed host responses. Native responses are host
processed; this is not proof the unexposed upstream protocol never has such fields.
No 429 was induced. No account identifiers, balance, plan, per-model allowance,
concurrency limit, reset timestamp, or usage-history API was established from
these responses. Secret scans were clean, auth files created: 0, shutdown exit: 0.

## Local verification

- Host `mise run verify:test`: passed on rerun. The first run failed the existing
  `TestEnsureClientsWaitsForPreviousTargetClose` one-second timing check; that
  unchanged test subsequently passed five repetitions.
- Host `mise run verify:compile`, `mise run build:purego`, targeted config vet,
  and synthesizer `-race -count=2 -shuffle=on`: passed.
- Plugin `mise run test`: Go suite plus four Python safety/qualification tests;
  `mise run check`: format/vet/workflow checks.
- `ENVIRONMENT_AUTH_SMOKE=1 mise run smoke`: four real-server fake-upstream passes,
  auth JSON and environment accounts under both CGO and CGO-disabled purego.
  Covers concurrent stream/nonstream requests, exact usage, frontend errors,
  upstream cancellation and graceful shutdown; fixture credentials are not live.
  Readiness now waits for account initialization rather than static discovery.
- Final harness adds checksum rejection and nonzero failed-qualification exits;
  these were unit-tested, not followed by more billable live requests.

## Documented metadata is currently a UI/CLI contract

Official sources inspected:

- [Provider API](https://commandcode.ai/docs/provider): models, chat completions,
  messages; shared API key, entitlement errors, rate-limit errors and backoff,
  final streaming usage. No documented machine account/quota/history endpoint
  found in this reference.
- [Pricing & Limits](https://commandcode.ai/docs/resources/pricing-limits): included
  subscription credits have 5-hour and 7-day windows, monthly billing budget,
  plan/model/pool distinctions and pooled team credits. Top-up/on-demand and
  pay-as-you-go treatment differs. These are rules, not measured balances for
  this account. Do not compute a reset timestamp from the first local request.
- [Studio](https://commandcode.ai/docs/studio/usage): per-request cost/tokens and
  usage details; each organization has its own keys, usage and billing. The
  documented CLI `/usage` and [usage dashboard](https://commandcode.ai/usage)
  are suitable user links, not REST contracts to scrape. Studio also explicitly
  documents environment-only CLI authentication without `auth.json`.

No dashboard session was authenticated, no private endpoint was guessed or probed,
and no internal browser API is claimed stable. This is a bounded documentation
and response survey, not proof that an undocumented API does not exist.

## Host mapping and conditional minimal schema

Keep current token usage in executor responses and stream terminal usage. Existing
native `Error.RetryAfterMS` already carries an observed retry delay to scheduler
cooldown; no ABI change is needed for that. Host `Auth.Quota` / model quota contain
`Exceeded`, `NextRecoverAt`, `ObservedAt`, and bounded `Signals`; passive header
collection currently only understands Claude/Codex. Plugin `UsageRecord` has
`AuthID`, `AuthIndex`, token detail and response headers, but no typed cost or
credit-balance field. Do not infer a balance by subtracting local token counts,
or reuse account metadata as an untyped quota transport.

Recommended UX now: show request tokens, observed errors/cooldowns, quota/plan as
**unknown**, and a user-initiated dashboard link. Static catalog availability is
not account entitlement. Do not enable Command Code quota scheduling from this
evidence or duplicate Claude/Codex header parsing for it.

Only if a documented or explicitly supported metadata source becomes available,
consider one optional account-bound host callback carrying:

```text
QuotaSnapshot {
  observed_at: timestamp
  limits: [{
    scope: account | model
    model?: string
    unit: requests | tokens | credits
    limit?: decimal-string
    remaining?: decimal-string
    window_seconds?: integer
    reset_at?: timestamp
  }]
}
```

All optional numbers are absent when unknown, never guessed as zero. The host
binds the account from the authorized callback context (not a caller-supplied
account/key), validates finite nonnegative values and model IDs, and bounds list
size. No arbitrary headers, URLs, plan descriptions, email or upstream account
identifiers. Start with a host-owned 60-second freshness TTL, cap any future
provider TTL at 5 minutes, and coalesce reads per credential; no automatic
polling or quota requests until the source permits them. Key rotation invalidates
the snapshot. Replace atomically, do not merge stale watermarks; errors leave a
dated stale display, while stale/missing observations never deny scheduling.
Actual 429 retry-after remains separate authoritative cooldown behavior.

Before implementing, test account isolation and spoof rejection, key rotation,
unknown versus zero, malformed/negative/oversized input, redaction, replacement
versus merge, and TTL boundaries with a controllable clock. Capability negotiation
must make it optional for older plugins. No speculative schema or endpoint has
been implemented in this follow-up.
