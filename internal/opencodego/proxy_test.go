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

// Discovery no longer learns or honors per-auth proxy configuration: it
// runs through the request's host HTTP client, whose transport policy owns
// proxying. The quota poll keeps the auth proxy_url resolution below.
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
