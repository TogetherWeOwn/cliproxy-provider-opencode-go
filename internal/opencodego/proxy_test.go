package opencodego

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// newProxyRecorder stands in for an HTTP forward proxy: it answers every
// request and records the absolute target URL.
func newProxyRecorder(t *testing.T, body string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var targets []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.URL.String())
		mu.Unlock()
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), targets...) }
}

func TestDiscoveryUsesAuthProxyThenHostProxy(t *testing.T) {
	models := `{"data":[{"id":"glm-5.3","protocol":"openai"}]}`
	authProxy, authSeen := newProxyRecorder(t, models)
	hostProxy, hostSeen := newProxyRecorder(t, models)
	c := newCatalog("http://opencode.invalid", nil)
	defer c.client.CloseIdleConnections()
	host := pluginapi.HostConfigSummary{ProxyURL: hostProxy.URL}
	if _, err := c.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "a", StorageJSON: []byte(`{"api_key":"key-one"}`), Metadata: map[string]any{"proxy_url": authProxy.URL}, Host: host}); err != nil {
		t.Fatal(err)
	}
	// The executor path has no host config; it reuses the last host proxy.
	if _, err := c.resolve(context.Background(), "glm-5.3", []byte(`{"api_key":"key-two"}`), nil); err != nil {
		t.Fatal(err)
	}
	if got := authSeen(); len(got) != 1 || got[0] != "http://opencode.invalid/inference/go/v1/models" {
		t.Fatalf("auth proxy saw %v", got)
	}
	if got := hostSeen(); len(got) != 1 || got[0] != "http://opencode.invalid/inference/go/v1/models" {
		t.Fatalf("host proxy saw %v", got)
	}
}

func TestQuotaPollUsesAuthProxy(t *testing.T) {
	proxy, seen := newProxyRecorder(t, `{"subscriptionPool":{}}`)
	c := newCatalog("http://opencode.invalid", nil)
	defer c.client.CloseIdleConnections()
	q := newQuotaWithClient(c.base, c.client)
	_, _ = q.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: "a", StorageJSON: []byte(`{"api_key":"key"}`), Metadata: map[string]any{"proxy_url": proxy.URL}})
	if got := seen(); len(got) == 0 || got[0] != "http://opencode.invalid/inference/go/v1/usage" {
		t.Fatalf("proxy saw %v", got)
	}
}
