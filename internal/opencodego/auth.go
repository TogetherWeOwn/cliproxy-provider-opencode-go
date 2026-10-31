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

// proxyAuth records the host's global proxy-url from every auth parse and
// refresh. An auth is parsed before it can execute, so catalog discovery on
// the executor path has the host proxy even before any ModelsForAuth call.
type proxyAuth struct {
	kit.APIKeyAuth
	catalog *catalog
}

func (a proxyAuth) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	return a.APIKeyAuth.ParseAuth(ctx, r)
}
func (a proxyAuth) RefreshAuth(ctx context.Context, r pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	a.catalog.setHostProxy(r.Host.ProxyURL)
	return a.APIKeyAuth.RefreshAuth(ctx, r)
}
