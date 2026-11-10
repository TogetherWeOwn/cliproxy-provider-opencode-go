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
	// API-key only: no AuthProvider and no ManagementAPI login
	// routes, so the management UI shows no login card. Auth files load
	// through the host's generic file path.
	q := newQuotaWithClient(selectedBase, c.client)
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: displayName, Version: "0.3.0", Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-opencode-go", Logo: "https://www.google.com/s2/favicons?domain=opencode.ai"},
		ModelNamespace: "opencode-go",
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			ModelProvider: c, ModelRegistrar: c, Executor: newExecutor(c), QuotaProvider: q,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude", "gemini"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude", "gemini"},
		},
		OnShutdown: func(context.Context) error { c.client.CloseIdleConnections(); return nil },
	}, host)
}
