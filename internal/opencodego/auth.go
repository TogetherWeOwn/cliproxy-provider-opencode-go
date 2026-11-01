package opencodego

import (
	"context"
	"sync"

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
// files: ownership is by filename, so a watcher's repeated parse of one file
// is safe. Only credentialDigest values are retained; keys never appear in
// duplicate errors and digests are useless across restarts.
type proxyAuth struct {
	kit.APIKeyAuth
	catalog *catalog
	mu      sync.Mutex
	files   map[string][32]byte
	owners  map[[32]byte]string
}

func newProxyAuth(catalog *catalog) (*proxyAuth, error) {
	a := &proxyAuth{APIKeyAuth: newAuth(), catalog: catalog, files: make(map[string][32]byte), owners: make(map[[32]byte]string)}
	login, err := kit.NewAPIKeyLogin(kit.APIKeyLoginConfig{
		Provider: "opencode-go",
		Parse:    a.ParseAuth,
		Fields: []kit.APIKeyLoginField{
			{Name: "quota_api_key", Label: "Quota API key", Description: "Optional separate key for quota polling. Leave blank to skip.", Required: false},
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
	a.catalog.setHostProxy(r.Host.ProxyURL)
	out, err := a.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	key, err := kit.APIKey(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := credentialDigest(key)
	a.mu.Lock()
	defer a.mu.Unlock()
	if file, ok := a.owners[digest]; ok && file != r.FileName {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "duplicate_auth", Message: "the same API key is already configured in another auth file", HTTPStatus: 400}
	}
	if old, ok := a.files[r.FileName]; ok && old != digest {
		delete(a.owners, old)
	}
	a.files[r.FileName] = digest
	a.owners[digest] = r.FileName
	return out, nil
}
func (a *proxyAuth) RefreshAuth(ctx context.Context, r pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	return a.APIKeyAuth.RefreshAuth(ctx, r)
}
