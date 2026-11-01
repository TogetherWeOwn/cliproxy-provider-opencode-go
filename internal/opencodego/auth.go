package opencodego

import (
	"context"
	"time"

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

// proxyAuth records the host's global proxy-url from every auth parse and
// refresh. An auth is parsed before it can execute, so catalog discovery on
// the executor path has the host proxy even before any ModelsForAuth call.
//
// It also guards against the same API key being configured in two auth
// files through kit.KeyOwnership: ownership is by filename, so a watcher's
// repeated parse of one file is safe, and a provisional login-validation
// parse only reserves the digest until the login session expires, so an
// abandoned login releases the key instead of holding it until restart.
// Only credentialDigest values are retained; keys never appear in duplicate
// errors and digests are useless across restarts.
type proxyAuth struct {
	kit.APIKeyAuth
	catalog *catalog
	guard   kit.KeyOwnership
}

func newProxyAuth(catalog *catalog) (*proxyAuth, error) {
	return newProxyAuthWithLoginTTL(catalog, 0)
}

// newProxyAuthWithLoginTTL is newProxyAuth with an explicit login session
// TTL (zero keeps the kit default); tests use a short TTL to observe an
// abandoned login releasing its key reservation.
func newProxyAuthWithLoginTTL(catalog *catalog, loginTTL time.Duration) (*proxyAuth, error) {
	a := &proxyAuth{APIKeyAuth: newAuth(), catalog: catalog}
	login, err := kit.NewAPIKeyLogin(kit.APIKeyLoginConfig{
		Provider:   "opencode-go",
		Parse:      a.ParseAuth,
		SessionTTL: loginTTL,
		Fields: []kit.APIKeyLoginField{
			{Name: "quota_api_key", Label: "Quota API key", Description: "Optional separate key for quota polling. Leave blank to skip.", Required: false, Secret: true},
			{Name: "workspace", Label: "Workspace", Description: "Optional workspace. Leave blank to skip.", Required: false},
		},
	})
	if err != nil {
		return nil, err
	}
	a.APIKeyAuth.Login = login
	return a, nil
}

func (a *proxyAuth) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	// The kit login submit re-parses the key without host configuration, so
	// only a configured proxy replaces the shared one: a login must never
	// clear the host proxy that earlier parses learned.
	if r.Host.ProxyURL != "" {
		a.catalog.setHostProxy(r.Host.ProxyURL)
	}
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
func (a *proxyAuth) RefreshAuth(ctx context.Context, r pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	return a.APIKeyAuth.RefreshAuth(ctx, r)
}
