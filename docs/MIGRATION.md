# Migration: staging first, then per-provider cutover

This is an operator-run checklist, not an unattended migration or deployment.
The plugin has been exercised with mocked upstreams through the real v8.0.48
host loader. Live provider validation remains pending. Do not run a migration
against a live config without a private backup and an agreed rollback window.

## 1. Prepare a staging CLIProxy instance

1. Keep the production CLIProxy unchanged. Back up its config and auth directory
   privately. Never commit config backups, keys, account labels or addresses.
2. Use stock CLIProxy v8.0.48 with an isolated staging config, auth directory,
   plugin directory and listen port. Do not share mutable directories with
   production. Build/install only the production artifact, not a binary built
   with the `hostcompat` tag.
3. Validate the release ZIP's external checksum before installation. Add this
   repository's `registry.json` URL to `plugins.store-sources` and install
   `opencode-go` using the native plugin store. Its plugin ID and provider key
   are both `opencode-go`.
4. In the staging config copy, remove the `openai-compatibility` entry named
   `opencode-go`. Native compatibility executors have priority over plugin
   executors, even if model names look identical. Remove the old
   `subscription-pool.providers.opencode` block and disable the obsolete
   `opencode-go-pool` plugin in this staging copy. Quarantine any old
   `opencode-go-pool-v0.2.0.so` outside the staging plugin search directory.
5. Do not install two versions/filenames that claim the same plugin ID.

## 2. Create private auth files offline

The migration tool reads a **config copy**, never the live service. It supports
`openai-compatibility` entries named `opencode-go` with `api-key-entries` or the
`keys` spelling (not both). The optional old module lives under
`plugins.configs.subscription-pool.providers.opencode`.

```sh
go run ./cmd/migrate -config /private/config-copy.yaml -out /private/new-opencode-auths
```

The output directory must not exist. The tool creates it with mode `0700`, and
writes one JSON file per inference key with mode `0600`. **All generated files
are disabled**, regardless of the old routing state. It emits only a redacted
count, never values or account names, and never alters the input config.

If old `accounts` are present, their count must match the inference-key count.
**Pairing is by account order**: review it privately before enabling anything.
Each account's `dashboard-api-key` becomes `quota_api_key`, falling back to the
module-level value, then to the inference key when omitted. Duplicate inference
keys, ambiguous key lists and incomplete mappings are rejected before writing.
Priority, optional host prefix, per-key weight and proxy URL are preserved.
Model aliases are not migrated: update clients to the fixed `opencode-go/` IDs.
Only copy the reviewed auth files into the isolated staging auth directory.

## 3. Staging acceptance gates

1. Establish that each account has a legitimate, independently usable
   entitlement. Do not combine personal accounts to circumvent provider limits.
2. Before any inference, verify in Console that **Extra Usage is OFF** for
   every migrated account/workspace. This plugin cannot infer or disable that
   setting from the usage response. Do not assume included `cost: 0` implies
   there can be no later paid fallback.
3. Deliberately enable only the reviewed staging auth files. Confirm the host
   lists plugin-owned auths and `opencode-go/` model IDs. Check that model
   discovery's current protocol metadata matches each selected model.
4. Verify current quota cards against the account's Console data: rolling,
   weekly and monthly remaining fractions and server reset timestamps. A
   missing schema/window is an error, not zero usage. A 401/403 must not trigger
   legacy fallback; only a missing current usage endpoint (404/410) does.
5. With separate live-test authorization, perform a minimal non-stream and
   stream request for each supported protocol family. Confirm prefix removal,
   actual client identity, stable session header, translated content/tool
   behavior, token accounting and native cooldown. Mock failover tests do not
   authorize deliberately exhausting a real account.
6. Stop on `EntitlementError`, unrecognized catalog/schema, or Cloudflare 1010.
   Leave production unchanged. Do not scrape a dashboard, change identity to
   bypass access controls, or fall back to balance-funded generic `/inference`
   endpoints without `/go`.
7. Record staging evidence privately and rehearse rollback. None of these live
   acceptance steps is automatically run by CI or the migration command.

## 4. Production cutover

Only after staging acceptance and operational approval:

1. Keep a private, restorable backup of the exact pre-cutover config/auths and
   old plugin artifacts. Cut over this provider independently of other lanes.
2. Install the verified artifact through the native plugin store. Quarantine
   the obsolete overlay artifact outside the production plugin directory, and
   disable its config. Do not delete the only rollback copy.
3. Remove the native compatibility entry named `opencode-go` and the old
   subscription-pool OpenCode module. Preserve unrelated providers and settings.
4. Install the reviewed auth files, enable `plugins.configs.opencode-go`, and
   verify registration and actual executor selection. Never leave an overlapping
   native entry in place and assume the plugin is serving traffic.
5. Observe error rates, token accounting and quota visibility before increasing
   traffic. Native host routing owns round-robin/weights and cooldown; do not
   add a plugin Scheduler or an extra host automation loop.

## 5. Rollback

1. Disable `plugins.configs.opencode-go` and remove its auths from active staging
   or production routing. Preserve files privately for diagnosis.
2. Restore the backed-up compatibility entry and subscription-pool OpenCode
   block, and restore the old overlay artifact/config only if it was part of
   the original verified configuration.
3. Linux supports hot loading, but complete eviction of a loaded shared library
   requires a host restart. If a bad artifact must be fully removed, disable it,
   move it outside the plugin directory and perform an explicitly authorized
   restart. Do not assume deleting a loaded `.so` unloads its code.
4. Reconfirm the restored executor, model names and quota behavior. Do not
   restart unrelated services or change unrelated providers during rollback.
