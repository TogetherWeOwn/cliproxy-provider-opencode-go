# OpenCode Go provider (work in progress)

**Not ready for installation or cutover.** An experimental c-shared entry point
registers only the auth parser, and a host-loader test checks that limited
capability. It does not register an executor, model catalog, or quota provider.
The repository also lacks a migration tool and release packaging. Do not use
this artifact for inference or a cutover.

The quota parser is adapted from
[TogetherWeOwn/cpa-plugin-subscription-pool](https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool)
commit `ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b`, MIT licensed. The
current endpoint and response shape have not been verified with a real account.
The unregistered adapter tries the legacy usage path only when the current path
returns 404 or 410; it must not be enabled based on mock fixtures alone. Tests
use local mocks; they do not call the provider.

An eventual auth file has the following shape; values are placeholders only:

```json
{"type":"opencode-go","api_key":"example-key","quota_api_key":"example-quota-key","label":"Example account","workspace":"example","weight":1,"priority":0,"disabled":false}
```

Keep actual auth files private, with mode `0600`, and do not commit them.
`quota_api_key` is optional and defaults to `api_key`.

TODO: remove the local kit `replace` in `go.mod` once the kit v0.1.0 is tagged;
use a released dependency and reproducible CI before publishing this project.

## Unresolved protocol integration

The v8.0.48 host's `internal/pluginhost/adapters_executors.go`
`selectExecutorInputFormat` chooses the client's requested format whenever it
is among the advertised formats. It does not select a protocol from the model
catalog. Merely advertising `openai`, `claude`, and `gemini` and routing by
model would therefore send a client-format payload to a potentially different
model-specific upstream protocol. A safe executor needs request, response, and
stream conversion for those cross-format calls, with real-host tests, or another
verified design; the kit passthrough does not provide that conversion. This is
not implemented here.
