package opencodego

import (
	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// NewRuntime registers only the auth parser. This experimental build must not
// advertise quota until the current endpoint's contract has been verified, or
// inference capabilities until protocol translation has been implemented.
func NewRuntime(host kit.Caller) (*kit.Runtime, error) {
	return kit.New(kit.Definition{
		Metadata: pluginapi.Metadata{
			Name: "opencode-go", Version: "0.0.0-dev", Author: "TogetherWeOwn",
			GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-opencode-go",
		},
		Capabilities: pluginapi.Capabilities{AuthProvider: newAuth()},
	}, host)
}
