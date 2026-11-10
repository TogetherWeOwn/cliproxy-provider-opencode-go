package opencodego

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newAuth() kit.APIKeyAuth {
	return kit.APIKeyAuth{Provider: "opencode-go", Validate: func(fields map[string]any) error {
		for _, name := range []string{"quota_api_key", "workspace"} {
			if v, ok := fields[name]; ok {
				if _, ok := v.(string); !ok {
					return &pluginabi.Error{Code: "invalid_auth", Message: "invalid provider auth field", HTTPStatus: 400}
				}
			}
		}
		return nil
	}}
}

// proxyAuth guards against the same API key being configured in two auth
// files through kit.KeyOwnership: ownership is by filename, so a watcher's
// repeated parse of one file is safe. Only credentialDigest values are
// retained; keys never appear in duplicate errors and digests are useless
// across restarts.
//
// Discovery runs through the host HTTP client and quota resolves the auth
// proxy_url per request, so parses learn no host proxy configuration.
//
// The provider is API-key only: it registers no AuthProvider, so the
// management UI shows no login card and auth files load through the host's
// generic file path. ParseAuth stays as the documented auth-file
// validation (field shapes, duplicate-key guard) beside the JSON shape in the
// README; re-register it as the plugin AuthProvider to restore upload-time
// validation.
type proxyAuth struct {
	kit.APIKeyAuth
	guard kit.KeyOwnership
}

func newProxyAuth() *proxyAuth {
	return &proxyAuth{APIKeyAuth: newAuth()}
}

func (a *proxyAuth) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	out, err := a.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := kit.APIKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := credentialDigest(key)
	if err := a.guard.Claim(ctx, r.FileName, digest); err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	return out, nil
}
