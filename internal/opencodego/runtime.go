package opencodego

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// NewRuntime owns per-instance discovery and display-only quota caches. Account
// selection and error-driven cooldown remain entirely with the native host.
func NewRuntime(host kit.Caller) (*kit.Runtime, error) {
	c := newCatalog(upstreamBase, nil)
	c.base = selectedBase
	auth, err := newProxyAuth(c)
	if err != nil {
		return nil, err
	}
	q := newQuotaWithClient(selectedBase, c.client)
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: provider, Version: "0.2.0", Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-opencode-go"},
		ModelNamespace: "opencode-go",
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider: auth, ManagementAPI: auth.Login, ModelProvider: c, ModelRegistrar: c, Executor: newExecutor(c), QuotaProvider: q,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude", "gemini"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude", "gemini"},
		},
		OnShutdown: func(context.Context) error { c.client.CloseIdleConnections(); return nil },
	}, host)
}
