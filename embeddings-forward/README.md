# Embeddings, rerank and decisions forward

Native CPA plugin serving embeddings, rerank and decision-model APIs on their
standard paths. Clients authenticate with ordinary CPA client keys
(`api-keys`), so OpenAI, Cohere, TypeSafe and OpenRouter clients work by
changing only their base URL. Upstream keys live in this plugin's config;
client keys never reach the plugin or the upstreams.

| Route | Shape | Module |
| --- | --- | --- |
| `POST /v1/embeddings` | OpenAI embeddings | `embeddings` |
| `POST /v1/rerank` | Cohere rerank | `rerank` |
| `POST /v1/systemone` | TypeSafe System One | `decisions` |
| `POST /api/alpha/decisions` | OpenRouter Decisions | `decisions` |

None of these paths is a built-in CPA route, and built-in routes would take
precedence anyway. Only enabled modules register routes, so a disabled module
answers 404. Requests without a valid client key get 401 before the plugin
runs. Streaming is not supported.

Requires a CPA host with plugin schema 9 (body-aware ingress handler routes),
`unstableneutron/CLIProxyAPI` v8.0.48-un.1 or later. Older hosts reject the
library at registration instead of serving it partially.

## Behaviour

- **Routing.** The JSON `model` selects providers in config order. A model
  entry matches its `alias` or its `name`, and the body is sent with that
  provider's `name`. One entry therefore accepts both name forms, and each
  provider gets its own spelling. A provider without `models` accepts any
  model unchanged.
- **Failover.** 429 and 402 try the provider's next key. Transport errors and
  5xx go to the next provider. Other statuses, such as 400, are returned
  without failover. `cooldown_seconds` moves a key that returned 429 or 402 to
  the end of the key order for that long, or for `Retry-After` when longer.
  Providers whose keys are all cooling are tried last. Cooling keys are still
  tried, so cooldown alone never fails a request. `strategy: round_robin`
  rotates the first key per request. This state is in memory.
- **Batching.** A decision request with more questions than a provider's
  `max_questions` is split into batches sent concurrently, at most
  `max_concurrency` (default 4) at a time. A batch that fails with a retryable
  status moves to the next provider and is re-split for that provider's cap;
  answers already received are kept. Answers are merged in request order,
  numeric `usage` fields are summed, and other top-level fields come from the
  first successful batch. Each batch carries only the `assets` it references.
  A single-batch success is returned byte for byte.
- **Shape translation.** Both decision routes share one provider pool. System
  One and Decisions bodies are the same apart from the Decisions-only
  `provider`, `session_id`, `trace` and `user` fields, which are dropped for
  `api: systemone` providers. OpenRouter documents TypeSafe bodies working on
  Decisions unchanged. The Decisions response (`id`, `model`, `provider`,
  `answers`, `usage`) is a superset of System One's, so responses pass
  through. `TypeSafe-Danger-Zone-*` headers are forwarded.
- **Errors.** Plugin errors use `{"error":{"message","type","code"}}`: 400 for
  invalid JSON, model or questions, 404 for an unknown model, 415 for a
  compressed body and 502 for upstream failure. Upstream JSON errors pass
  through with their status and `Retry-After`. Non-JSON upstream errors, such
  as OpenRouter's plain-text `error code: 502`, are wrapped with the status,
  provider name and a snippet.
- **Limits.** The host caps request bodies at 32 MiB (413). Bodies must be
  uncompressed JSON, so disable TypeSafe SDK compression.
- **Secrets.** Keys are never logged or returned. Failed attempts are logged at
  debug level and failed requests at warn level, with module, provider and
  status only.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    embeddings-forward:
      enabled: true          # the host loads only enabled plugin IDs
      embeddings:            # rerank has the same shape
        enabled: true
        providers:
          - name: tokenrouter                      # unique per module
            base_url: https://api.tokenrouter.com/v1
            path: /embeddings                      # default per module
            api_keys: [sk-1, sk-2]
            strategy: failover                     # or round_robin
            cooldown_seconds: 60
            models:
              - {name: openai/text-embedding-3-small}
      decisions:
        enabled: true
        providers:
          - name: commandcode
            base_url: https://api.commandcode.ai/provider
            api: systemone                         # default: decisions
            api_keys: [cc-key]
            max_questions: 100                     # 0 = unlimited
            max_concurrency: 4
            models:
              - {name: typesafe/jev, alias: jev-latest}
              - {name: "respan/span-01-lite:free", string_instructions: true}
```

Install the library from the release ZIP as
`<plugins.dir>/linux/<amd64|arm64>/embeddings-forward.so` and restart CPA.

The default paths are `/embeddings`, `/rerank`, `/alpha/decisions` (`api:
decisions`) and `/v1/systemone` (`api: systemone`), appended to `base_url`.
`string_instructions: true` JSON-encodes object or array question
`instructions` for models that accept only strings. Invalid config is rejected
at load or reconfigure; the error names the field but never a key. The
upstream plugin's modular `systemone` module is still accepted: its providers
join the decisions pool with `api: systemone`.

### vn3 rollout example

This example configures OpenRouter, CommandCode, TokenRouter and explabs. The
quirks measured live on 2026-10-30/31 are encoded as caps, cooldowns and model
names. OpenRouter Jev currently returns 402 (TypeSafe credits) and explabs
embeddings return 429 (credits), so both cool down and fail over. TokenRouter
Jev allows 5 requests per minute and explabs 10, so they come last. The
explabs decision path is assumed to be System One's `/v1/systemone`; confirm
it before rollout.

```yaml
api-keys: [hindsight-client-key]
plugins:
  enabled: true
  configs:
    embeddings-forward:
      enabled: true
      embeddings:
        enabled: true
        providers:
          - name: openrouter                      # BYOK OpenAI
            base_url: https://openrouter.ai/api/v1
            api_keys: [sk-or-...]
            cooldown_seconds: 60
            models: [{name: openai/text-embedding-3-small, alias: text-embedding-3-small}]
          - name: tokenrouter
            base_url: https://api.tokenrouter.com/v1
            api_keys: [tr-...]
            cooldown_seconds: 60
            models: [{name: openai/text-embedding-3-small, alias: text-embedding-3-small}]
          - name: explabs                         # accepts only the unprefixed name
            base_url: https://api.experientiallabs.ai/v1
            api_keys: [ex-...]
            cooldown_seconds: 300
            models: [{name: text-embedding-3-small, alias: openai/text-embedding-3-small}]
      rerank:
        enabled: true
        providers:
          - name: openrouter
            base_url: https://openrouter.ai/api/v1
            api_keys: [sk-or-...]
            models: [{name: cohere/rerank-v3.5, alias: rerank-v3.5}]
      decisions:
        enabled: true
        providers:
          - name: commandcode                     # Jev benchmark: nDCG@5 0.72, ~0.9 s p50
            base_url: https://api.commandcode.ai/provider
            api: systemone
            api_keys: [cc-...]
            max_questions: 100
            cooldown_seconds: 60
            models:
              - {name: typesafe/jev, alias: jev-latest}
              - {name: typesafe/jev-1.14.0, alias: jev-1.14}
          - name: openrouter
            base_url: https://openrouter.ai/api
            api_keys: [sk-or-...]
            max_questions: 200
            cooldown_seconds: 600                 # Jev returns 402 until credits return
            models:
              - {name: typesafe/jev-1.14, alias: jev-latest}
              - {name: openai/gpt-6-luna-decisions, alias: gpt-6-luna-decisions}
              - {name: "respan/span-01-lite:free", string_instructions: true}
          - name: tokenrouter
            base_url: https://api.tokenrouter.com/api
            api_keys: [tr-...]
            cooldown_seconds: 60                  # 5 requests per minute
            models:
              - {name: typesafe/jev-1.14, alias: jev-latest}
              - {name: typesafe/jev-1.13, alias: jev-1.13}
          - name: explabs                         # free, 10 requests per minute
            base_url: https://api.experientiallabs.ai
            api: systemone
            api_keys: [ex-...]
            max_questions: 32
            max_concurrency: 2
            cooldown_seconds: 60
            models:
              - {name: jev-latest}
              - {name: clef}
              - {name: clef-flash}
              - {name: gpt-6-luna-decisions}
```

Hindsight 1.1.0, where `<cpa>` is the CPA host and port:

```bash
HINDSIGHT_API_EMBEDDINGS_PROVIDER=openai
HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=http://<cpa>/v1
HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY=hindsight-client-key
HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL=text-embedding-3-small
HINDSIGHT_API_RECALL_DECISION_MODEL_PROVIDER=typesafe
HINDSIGHT_API_RECALL_DECISION_MODEL_BASE_URL=http://<cpa>   # posts to /v1/systemone
HINDSIGHT_API_RECALL_DECISION_MODEL_API_KEY=hindsight-client-key
HINDSIGHT_API_RECALL_DECISION_MODEL_MODEL=jev-latest
```

## Build and test

```bash
go build -buildmode=c-shared \
  -ldflags '-X github.com/unstableneutron/cpa-plugins/internal/nativeabi.Version=VERSION' \
  -o embeddings-forward.so ./embeddings-forward
go test -race ./embeddings-forward
python3 -B tests/forward_e2e.py --host /tmp/cpa-hosts/linux_amd64/cpa-cgo --plugins <dir-with-.so>
go run ./tests/forwardperf -host /tmp/cpa-hosts/linux_amd64/cpa-cgo -plugins <dir-with-.so>
```

Unit tests cover every quirk above against local HTTP fixtures.
`tests/forward_e2e.py` runs the library in a real host with fixtures: client-key
auth, failover, batching, translation, raw responses, large batches, 413 and
log secrecy. It is part of `mise run smoke`. `tests/forwardperf` reports added
latency, throughput and host memory. Live checks are not automated; they need
explicitly supplied credentials, which must never be printed.

## Provenance

Forked from [KorenKrita/cliproxy-embeddings-rerank-forward](https://github.com/KorenKrita/cliproxy-embeddings-rerank-forward)
v0.5.0 (`ffa556e5bcfa7816d2849701864c5a678705e0f5`) under the MIT license; its
notice is in [`UPSTREAM_NOTICES.txt`](UPSTREAM_NOTICES.txt). The plugin ID
`embeddings-forward` and the modular config schema are kept.

Extracted from upstream `main.go` and adapted:

- provider, model-mapping and module config types (`config.go`)
- alias and catch-all model resolution (`config.go`)
- upstream URL construction and validation (`config.go`)
- top-level `model` rewrite (`plugin.go`)
- failover and round-robin key ordering with 429 cooldown (`forward.go`)
- `TypeSafe-Danger-Zone-*` header forwarding (`plugin.go`)

The C shim follows this repository's `chatgpt-backend/main.go` instead of
upstream's. Upstream `main_test.go` was not extracted; the tests are new.

Changes from upstream: standard paths with client-key auth instead of
`/v0/management/*` with the management key; one decisions pool for both
decision routes with shape translation; question batching and merging; 402
handling, `Retry-After`, cooldown-aware provider order and provider failover
on 5xx; structured errors; and `string_instructions`. The flat
single-provider fields (`upstream_*`, `rerank_*`, `decisions_*`,
`systemone_*`) and module `retries` were removed; use the modular objects.
