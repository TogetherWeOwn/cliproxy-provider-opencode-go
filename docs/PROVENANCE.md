# Source provenance and compatibility boundaries

This plugin is MIT licensed. Its usage parser and regression fixtures are
adapted from TogetherWeOwn/cpa-plugin-subscription-pool at
`ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`, specifically
`internal/providers/opencodego`. That implementation was independently developed
from first-party provider behavior. No third-party cookie/session scraping or
scheduler implementation is included. The historical bounded usage-fetch helper
and its original tests are retained; the registered quota provider uses the
published kit's bounded read-only HTTP client.

## First-party provider evidence

The documented fallback model/protocol table and current endpoint layout are
from anomalyco/opencode documentation at
`1d479d7642b030a904bf9a28e917b37176ac0c35`:

- [Go model/protocol table and client requirements](https://github.com/anomalyco/opencode/blob/1d479d7642b030a904bf9a28e917b37176ac0c35/services/www/src/docs/content/console/go.mdx)
- [Inference authentication, protocol paths and usage](https://github.com/anomalyco/opencode/blob/1d479d7642b030a904bf9a28e917b37176ac0c35/services/www/src/docs/content/console/api/inference.mdx)
- [First-party endpoint migration guidance](https://github.com/anomalyco/opencode/issues/52227)

Legacy usage schema and error semantics are source-confirmed in stable v1.19.5,
commit `810c0eba1a7348036f8bdf61e6632d0c36c6876c`:

- [Authenticated usage route and exact window schema](https://github.com/anomalyco/opencode/blob/810c0eba1a7348036f8bdf61e6632d0c36c6876c/packages/console/app/src/routes/zen/go/v1/usage.ts)
- [Inference route migration](https://github.com/anomalyco/opencode/blob/810c0eba1a7348036f8bdf61e6632d0c36c6876c/packages/console/app/src/lib/inference-proxy.ts)
- [Error definitions](https://github.com/anomalyco/opencode/blob/810c0eba1a7348036f8bdf61e6632d0c36c6876c/packages/console/app/src/routes/zen/util/error.ts)
- [Limit enforcement, headers and accounting](https://github.com/anomalyco/opencode/blob/810c0eba1a7348036f8bdf61e6632d0c36c6876c/packages/console/app/src/routes/zen/util/handler.ts)
- [Client identity/session headers](https://github.com/anomalyco/opencode/blob/810c0eba1a7348036f8bdf61e6632d0c36c6876c/packages/opencode/src/session/llm/request.ts)

[An independent deployed quota observation](https://github.com/NousResearch/hermes-agent/issues/113404)
corroborates the legacy `usage.rolling/weekly/monthly` schema. It does not prove
the current authenticated migrated backend. See README's pending verification
section. [Provider terms](https://opencode.ai/legal/terms-of-service) and Extra
Usage settings must be assessed before any real cutover.

## Host/kit contracts

Kit commit `de8b318` (v0.1.0 plus quota/discovery proxy support) and CLIProxyAPI v8.0.48 are pinned in `go.mod`.
Relevant paths in the pinned upstream source:

- `sdk/pluginapi/types.go`: capability and transport interfaces.
- `internal/pluginhost/adapters_executors.go`: native executor priority, client
  format selection, response translation and host usage accounting.
- `internal/registry/devin_models.go` and
  `internal/runtime/executor/helps/devin_models.go`: native fixed model prefix
  registration/removal pattern.
- `sdk/cliproxy/auth/conductor_cooldown.go`: native per-auth/per-model cooldown.
- `internal/pluginstore/{registry,github,checksum}.go`: registry schema, asset
  names and ZIP checksum contract.

Three SDK wire-conversion details are handled locally, without patching core:

1. General client-to-Responses conversion is exposed by v8.0.48's built-in
   translator under `codex`. Only its wire conversion is reused. Token limits
   and general generation controls are restored, and synthesized Codex defaults
   are removed. Non-stream Responses JSON is wrapped in the terminal-event
   envelope expected by those converters. No Codex endpoint or auth is used.
2. Its Claude-to-Gemini non-stream converter expects an SSE transcript. A
   Messages JSON response instead uses the SDK's Claude-to-Chat-to-Gemini JSON
   converters.
3. Its Messages-to-Gemini request converter omits `max_tokens`. The plugin
   restores that caller cap as `generationConfig.maxOutputTokens`. Tests verify
   token caps across all client/model protocol pairs.

The host-compatibility module uses the kit's real-loader integration pattern,
not a simulated loader. All provider traffic in tests goes to local mocks;
these tests establish host compatibility, not live provider entitlement.
