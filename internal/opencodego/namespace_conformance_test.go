package opencodego

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every id this plugin can emit carries the fixed "opencode-go/" namespace
// exactly once: the static snapshot, the registrar, and a discovery refresh.
// No subscription exists, so the discovery leg stays loopback-only (code and
// tests only).
func TestModelNamespaceConformance(t *testing.T) {
	ctx := context.Background()
	var all []pluginapi.ModelInfo
	all = append(all, modelList(staticRoutes())...)

	c := newCatalog("http://127.0.0.1:0", nil)
	reg, err := c.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, reg.Models...)

	static, err := c.StaticModels(ctx, pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, static.Models...)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference/go/v1/models" {
			t.Error("wrong discovery request")
		}
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3-flash","protocol":"openai"},{"id":"minimax-m3","api":{"npm":"@ai-sdk/anthropic"}}]}`)
	}))
	defer s.Close()
	discovered, err := newCatalog(s.URL, nil).ModelsForAuth(ctx, pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, discovered.Models...)

	if len(all) == 0 {
		t.Fatal("no models to check")
	}
	kit.AssertModelNamespace(t, "opencode-go", all)
}
