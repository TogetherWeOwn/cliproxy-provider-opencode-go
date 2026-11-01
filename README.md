# OpenCode Go provider for CLIProxyAPI

A first-class OpenCode Go provider plugin for **CLIProxyAPI v8.0.49**, built
against
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at `88850ce` (v0.1.0 plus quota/discovery proxy support and the v8.0.49
plugin ABI: RPC schema 10; hosts older than schema 10 are refused).
It implements API-key auth, model registration/discovery, multi-protocol
inference (including streaming), and read-only quota cards. **Live provider
verification and staging acceptance remain pending.** No provider endpoints are
called by the tests, and this repository does not install or change a service.

## Native routing, not a second scheduler

Each account is a separate `type: opencode-go` auth file. CLIProxy's native
routing strategy chooses credentials and applies per-auth/per-model cooldowns
from classified errors. This plugin **does not register a Scheduler** and does
not use quota percentages to choose or avoid accounts. Quota is display-only;
reset operations are unsupported.

Use only legitimately held entitlements. OpenCode's terms prohibit multiple
accounts used to circumvent usage limits and restrict use to internal purposes;
Teams documents independent per-member entitlements. Do not treat native
failover support as permission to evade these restrictions.

## Auth file

Create one private JSON file per account in the configured auth directory.
These are placeholders, not usable credentials:

```json
{
  "type": "opencode-go",
  "api_key": "example-inference-key",
  "quota_api_key": "example-quota-key",
  "label": "Example account",
  "weight": 1,
  "priority": 0,
  "disabled": false
}
```

Use mode `0600`. `quota_api_key` is optional and defaults to `api_key`.
New Console keys normally begin `oc_sk_`; legacy `sk-` keys may also work.
Console device OAuth is not the Go API-key flow. Login/poll operations return an
API-key-only error; refresh preserves the key. IDs are stable
`opencode-go:<first 12 hex SHA256 characters of api_key>` unless an explicit
`id` is supplied. `prefix`, `proxy_url`, and the host's routing fields are
preserved. Model discovery and quota polls go through the auth's
`proxy_url`, else the host's global `proxy-url`, like inference does. Optional `workspace` is metadata only, never an HTTP path or scraping
input. No browser sessions or dashboard scraping are used.

## Models and protocols

Fixed model namespace `opencode-go/`; auth `prefix` does not replace it. For
example, select `opencode-go/glm-5.3`; upstream receives `glm-5.3`, never the
routing prefix. An additional host auth prefix may further namespace this ID.

| Registered model example | Native upstream protocol | Path under `https://opencode.ai` |
| --- | --- | --- |
| `opencode-go/glm-5.3` | OpenAI Chat Completions | `/inference/go/openai/v1/chat/completions` |
| `opencode-go/gpt-6-luna` | OpenAI Responses | `/inference/go/openai/v1/responses` |
| `opencode-go/minimax-m3` | Anthropic Messages | `/inference/go/anthropic/v1/messages` |
| `opencode-go/gemini-4-argon` | Gemini | `/inference/go/google/v1beta/models/gemini-4-argon:generateContent` |

Gemini streaming uses `:streamGenerateContent?alt=sse`. All requests use Bearer
authentication. The client's actual User-Agent is passed through; absent one,
the plugin identifies itself. A stable, hashed `x-opencode-session` is derived
from host conversation metadata (canonical session first) or session headers.
Without conversation metadata, no global per-account session is fabricated.
Caller credentials, arbitrary destinations and unrelated headers are not
forwarded.

`GET /inference/go/v1/models` is authenticated and cached per credential for
60 seconds. Explicit catalog protocol/endpoint metadata takes precedence over
the documented 30-model snapshot, including the disputed `minimax-m3` protocol.
A catalog containing only known IDs uses the documented protocol table. New
IDs require recognized protocol metadata; unknown IDs are never guessed into
Chat. Network/discovery-endpoint failures use the static snapshot, except
authentication and forbidden responses, which remain errors. A successful but
invalid or ambiguous catalog fails closed rather than hiding a protocol change. Discovery does not invent an
entitlement or establish that fallback models are currently available.

The host chooses formats by client, not by model. The executor therefore uses
the pinned SDK's built-in request/response/stream translators before selecting
the model-specific endpoint. Chat, Responses, Messages and Gemini clients are
covered across all four model families. Unsupported transformations fail before
inference. Streams preserve native bytes when possible; converted streams
buffer complete SSE events with a 1 MiB per-event limit. Token counting is an
explicitly marked local estimate, not a tokenizer or a billable request.

## Quota and errors

Quota fetches `GET /inference/go/v1/usage`; only **404 or 410** permits a retry of
legacy `/zen/go/v1/usage`. It uses the separate quota key when configured, has a
15-second timeout, refuses redirects and caps responses at 1 MiB. Successful
observations are cached for 60 seconds per auth/credential. The source-confirmed
legacy `usage.rolling`, `weekly`, and `monthly` windows map to `5h`, `weekly`, and
`monthly` buckets. Remaining fraction is `(100-percent)/100`; a `rate-limited`
status forces zero remaining. Invalid or missing windows fail closed. Reset
times come from the server; no plan tier or dollar balances are fabricated.

| Inference error | Native-host error envelope |
| --- | --- |
| `GoUsageLimitError` | Retryable 429, allowlisted window code, server `Retry-After` (60s fallback) |
| `RateLimitError` / generic 429 | Retryable 429, server `Retry-After` (5s fallback) |
| `AuthError` / generic 401 | Non-retryable 401 |
| `EntitlementError` / legacy balance or account limits | Non-retryable 403, not bad credentials |
| `RegionError` / `DataPolicyError` | Non-retryable 403 with distinct codes |
| `ModelError` | Non-retryable 404 |
| Cloudflare 1010 access block | Retryable 502, never authentication failure; no bypass |
| Other 5xx | Retryable upstream error |

Provider messages, account metadata and raw error bodies are never included in
public errors. The same mapping applies to initial HTTP failures in streams.
The generic, bounded polling helper preserves HTTP statuses for discovery/quota
errors; inference's detailed classification uses its response body.

## Build and verification

Requires Linux/amd64, Go **1.26.10**, a C compiler, `nm`, `zip`, and CGO enabled.
The kit is pinned to its published release; no local `replace` is required.

```sh
go mod download
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o opencode-go.so ./cmd/plugin
nm -D opencode-go.so | grep ' T cliproxy_plugin_init$'
go -C integration/hostcompat test -race -count=1 -v ./...
bash scripts/package-release.sh ./dist
```

The separate `integration/hostcompat` module imports the **real v8.0.49 host
loader** using the kit's import-namespace pattern, without a fork or host
patch. It loads the C shared library and exercises auth, models, quota,
non-stream/stream execution through all protocol families, nonzero host usage
accounting even with `cost: 0`, and native conductor sibling failover/cooldown.
All upstreams are `httptest` servers. Only test-tagged `hostcompat` builds accept
a loopback-only mock base; production artifacts have a fixed provider origin.

CI checks formatting, module verification, vet, race tests, the shared library
entry point, the host registry schema, gitleaks and generic public-file hygiene.
`dist/opencode-go_0.1.2_linux_amd64.zip` contains `opencode-go.so`, its
`checksums.txt`, and `LICENSE`. The external `dist/checksums.txt` hashes the ZIP
for the host store. Publish both assets on tag `v0.1.2`; packaging itself never
publishes, installs or restarts anything.

## Installation and migration

First read [docs/MIGRATION.md](docs/MIGRATION.md), including staging, no-spend
checks and rollback. Add this repository's registry to the host's existing
plugin-store sources, then install `opencode-go` through that store:

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - https://raw.githubusercontent.com/TogetherWeOwn/cliproxy-provider-opencode-go/main/registry.json
  configs:
    opencode-go:
      enabled: true
```

The store installs the release as
`plugins/linux/amd64/opencode-go-v0.1.2.so`. A native `openai-compatibility`
entry named `opencode-go` pre-empts the plugin executor; remove it from the
**staging config copy** before testing, and from production only at cutover.
The offline `cmd/migrate` tool reads a config copy and creates disabled private
auth files; it never modifies config or contacts any provider.

## Pending live verification

Mock and real-host-loader tests are not evidence of paid entitlement or live
endpoint compatibility. Before enabling real traffic, separately verify:

- Current authenticated model-discovery schema and protocol metadata, especially
  `minimax-m3`; unknown metadata fails closed instead of guessing a protocol.
- The migrated usage schema and reset semantics for the relevant plan, including
  whether one API key can serve both inference and quota.
- Each account's entitlement and **Extra Usage disabled**. Teams may enable it
  by default; successful inference can otherwise spend prepaid balance.
- Ordinary-client egress to the provider. If Cloudflare 1010 persists, stop and
  report it; do not imitate a browser or bypass access controls.
- Staging quota-card rendering and authorized live smoke tests for each protocol
  family, streaming, token accounting, and rollback before production cutover.

No live inference/quota probes, purchases, credentials, or service changes are
part of this build. See [source provenance](docs/PROVENANCE.md) for pinned
research and the known SDK conversion adaptations.
