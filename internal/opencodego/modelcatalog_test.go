package opencodego

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// scriptedHostClient is the plugin's fixture transport: it replaces only the
// network boundary for discovery, like testHTTP does for inference. ModelsForAuth
// is exercised through the kit ModelCatalog pipeline with scripted wire bodies.
type scriptedHostClient struct {
	mu       sync.Mutex
	calls    int
	status   int
	body     string
	block    bool
	blocked  chan struct{}
	lastReq  pluginapi.HTTPRequest
	lastAuth []string
}

func (f *scriptedHostClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	f.lastAuth = append([]string(nil), req.Headers["Authorization"]...)
	status, body, block := f.status, f.body, f.block
	f.mu.Unlock()
	if block {
		if f.blocked != nil {
			select {
			case f.blocked <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return pluginapi.HTTPResponse{}, ctx.Err()
	}
	if status < 200 || status >= 300 {
		return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}}, nil
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: []byte(body)}, nil
}

func (f *scriptedHostClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, fmt.Errorf("discovery does not stream")
}

func (f *scriptedHostClient) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.block = status, body, false
}

func (f *scriptedHostClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func modelCatalogAuthReq(authID, key string, client pluginapi.HostHTTPClient) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{
		AuthID:      authID,
		StorageJSON: []byte(`{"api_key":"` + key + `"}`),
		HTTPClient:  client,
	}
}

func modelIDs(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// contributor fixture: previously unknown ids with and without a contributor
// variant; the pairing is discovered from the source list itself (kit
// contributor dedup), with no hand-added per-model override. The executor
// maps the emitted contributor id upstream after stripping one namespace.
func TestModelCatalogContributorUnknownIDs(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"unknown-a","protocol":"openai"},{"id":"unknown-a-contributor","protocol":"openai"},{"id":"unknown-solo","api":{"npm":"@ai-sdk/anthropic"}}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/unknown-a-contributor", "opencode-go/unknown-solo"}; !slices.Equal(got, want) {
		t.Fatalf("ids %v, want contributor win + kept base %v", got, want)
	}
	for _, m := range resp.Models {
		rest, ok := strings.CutPrefix(m.ID, modelPrefix)
		if !ok || rest == "" || strings.HasPrefix(rest, modelPrefix) {
			t.Fatalf("emitted %q does not survive one-namespace strip", m.ID)
		}
	}
	if got := strings.TrimPrefix(resp.Models[0].ID, modelPrefix); got != "unknown-a-contributor" {
		t.Fatalf("executor upstream id %q, want contributor variant", got)
	}
	// Executor mapping: the emitted contributor id resolves through the route
	// cache populated by the same fetch (no new discovery request).
	route, err := c.resolve(context.Background(), "unknown-a-contributor", []byte(`{"api_key":"example-key"}`), nil, fixture)
	if err != nil || route != "openai" {
		t.Fatalf("contributor route %q err %v", route, err)
	}
	if n := fixture.callCount(); n != 1 {
		t.Fatalf("fetches %d, want 1 (no executor-triggered refetch)", n)
	}
}

// Timeline A: seeded at A, fixture advanced to A+B; the next ModelsForAuth
// serves A+B. Proves no TTL skip: the kit catalog fetches on every
// invocation even though the plugin was constructed with catalogTTL.
func TestModelCatalogTimelineAFreshConverges(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"timeline-a","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	if _, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture)); err != nil {
		t.Fatal(err)
	}
	fixture.set(200, `{"data":[{"id":"timeline-a","protocol":"openai"},{"id":"timeline-b","protocol":"openai"}]}`)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/timeline-a", "opencode-go/timeline-b"}; !slices.Equal(got, want) {
		t.Fatalf("timeline A served %v, want fresh convergence %v", got, want)
	}
	if n := fixture.callCount(); n != 2 {
		t.Fatalf("fetches %d, want every invocation to fetch (2)", n)
	}
}

// Timeline B: seeded at A, fixture gains B but the one-shot callback's fetch
// fails transiently. ModelsForAuth serves last-good A and B is not published.
// Recovery is NOT guaranteed: the kit performs a single fetch per invocation
// (no bounded retry in v0.5.0), so no further invocation is promised and no
// background work publishes B. This test documents the absence of a recovery
// guarantee; it never asserts recovery.
func TestModelCatalogTimelineBFailureNoRecoveryGuarantee(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"timeline-a","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	if _, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture)); err != nil {
		t.Fatal(err)
	}
	// The fixture gains B but the one-shot callback fetch (503) fails first.
	fixture.set(503, `{"data":[{"id":"timeline-a","protocol":"openai"},{"id":"timeline-b","protocol":"openai"}]}`)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/timeline-a"}; !slices.Equal(got, want) {
		t.Fatalf("timeline B served %v, want last-good %v (B unpublished)", got, want)
	}
	before := fixture.callCount()
	time.Sleep(50 * time.Millisecond)
	if n := fixture.callCount(); n != before {
		t.Fatalf("fetches %d, want %d: no background recovery without a host event", n, before)
	}
}

// Credential-generation isolation: a rotated key fetches independently and
// never inherits the previous generation's last-good on failure; it serves
// the static snapshot instead.
func TestModelCatalogCredentialGenerationIsolation(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"rotated-only","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "key-one", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/rotated-only"}; !slices.Equal(got, want) {
		t.Fatalf("first generation served %v, want %v", got, want)
	}
	fixture.set(503, `{"data":[{"id":"rotated-only","protocol":"openai"}]}`)
	resp, err = c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "key-two", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 30 {
		t.Fatalf("rotated generation served %d models, want static fallback (30)", len(resp.Models))
	}
	for _, m := range resp.Models {
		if m.ID == "opencode-go/rotated-only" {
			t.Fatal("rotated credential inherited the previous generation's last-good")
		}
	}
	if n := fixture.callCount(); n != 2 {
		t.Fatalf("fetches %d, want per-generation fetch (2)", n)
	}
}

// TTL/last-good isolation: route-cache expiry never erases the kit durable
// last-good. Expiring the plugin route cache and then failing the fetch
// still serves the discovered list (the kit last-good store has no TTL
// eviction), while the executor falls back to the static snapshot.
func TestModelCatalogRouteCacheExpiryKeepsLastGood(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"last-good-id","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	if _, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture)); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for k, v := range c.cache {
		v.expires = time.Time{}
		c.cache[k] = v
	}
	c.mu.Unlock()
	fixture.set(503, "")
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/last-good-id"}; !slices.Equal(got, want) {
		t.Fatalf("last-good served %v, want %v", got, want)
	}
}

// Cancellation: a cancelled parent context returns promptly (the blocked
// fetch is abandoned) and the catalog falls back instead of hanging.
func TestModelCatalogParentCancellationFallsBack(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"cancelled-id","protocol":"openai"}]}`, block: true, blocked: make(chan struct{}, 1)}
	c := newCatalog("http://opencode.invalid", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-fixture.blocked
		cancel()
	}()
	started := time.Now()
	resp, err := c.ModelsForAuth(ctx, modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatalf("cancelled fetch error %v (want static fallback)", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancelled fetch took %v", elapsed)
	}
	if len(resp.Models) != 30 {
		t.Fatalf("cancelled fetch served %d models, want static fallback (30)", len(resp.Models))
	}
}

// Enforceable deadline: an earlier parent deadline beats the kit's derived
// 15s timeout, so a blocked fetch falls back by the parent deadline instead
// of the full window.
func TestModelCatalogParentDeadlineFallsBack(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"deadline-id","protocol":"openai"}]}`, block: true}
	c := newCatalog("http://opencode.invalid", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	resp, err := c.ModelsForAuth(ctx, modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatalf("deadline fetch error %v (want static fallback)", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("deadline fetch took %v", elapsed)
	}
	if len(resp.Models) != 30 {
		t.Fatalf("deadline fetch served %d models, want static fallback (30)", len(resp.Models))
	}
}

// Size fallback: a buffered body larger than the kit 1 MiB post-return parse
// cap is a failed fetch (never parsed, never cached); the static snapshot
// serves instead.
func TestModelCatalogOversizedBodyFallsBack(t *testing.T) {
	big := `{"data":[{"id":"` + strings.Repeat("a", 64) + `","protocol":"openai"}]}`
	// Pad past the cap while keeping the wire well-formed.
	padding := strings.Repeat(" ", (1<<20)+1-len(big))
	fixture := &scriptedHostClient{status: 200, body: big + padding}
	c := newCatalog("http://opencode.invalid", nil)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatalf("oversized fetch error %v (want static fallback)", err)
	}
	if len(resp.Models) != 30 {
		t.Fatalf("oversized fetch served %d models, want static fallback (30)", len(resp.Models))
	}
	// Nothing oversized was cached: a good fetch right after still works.
	fixture.set(200, `{"data":[{"id":"after-oversize","protocol":"openai"}]}`)
	resp, err = c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/after-oversize"}; !slices.Equal(got, want) {
		t.Fatalf("next fetch served %v, want %v", got, want)
	}
}

// The custom Fetch uses the request host HTTP client directly (URL, method,
// bearer auth), honors cancellation, and enforces its own parse cap — the
// discovery request is forwarded unchanged to the injected transport.
func TestModelCatalogFetchUsesHostClient(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"host-client-id","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	if _, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "fixture-key", fixture)); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	req := fixture.lastReq
	auth := append([]string(nil), fixture.lastAuth...)
	fixture.mu.Unlock()
	if req.Method != http.MethodGet {
		t.Fatalf("discovery method %q, want GET", req.Method)
	}
	if req.URL != "http://opencode.invalid/inference/go/v1/models" {
		t.Fatalf("discovery URL %q", req.URL)
	}
	if len(auth) != 1 || auth[0] != "Bearer fixture-key" {
		t.Fatalf("discovery auth %v, want fixture bearer", auth)
	}
}

// Validate filters unknown-protocol candidates at the pipeline level: a wire
// id without recognized protocol metadata is dropped, while ids with explicit
// protocol metadata serve — and the dropped id fails closed on the executor.
func TestModelCatalogValidateDropsUnknownProtocol(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"known-new","protocol":"openai"},{"id":"mystery-new"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	resp, err := c.ModelsForAuth(context.Background(), modelCatalogAuthReq("auth-1", "example-key", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelIDs(resp.Models), []string{"opencode-go/known-new"}; !slices.Equal(got, want) {
		t.Fatalf("served %v, want only known-protocol ids %v", got, want)
	}
	if _, err := c.resolve(context.Background(), "mystery-new", []byte(`{"api_key":"example-key"}`), nil, fixture); err == nil {
		t.Fatal("unknown-protocol id was guessed into a protocol")
	}
	if route, err := c.resolve(context.Background(), "known-new", []byte(`{"api_key":"example-key"}`), nil, fixture); err != nil || route != "openai" {
		t.Fatalf("known route %q err %v", route, err)
	}
}

// The kit ModelCatalog echoes req.Plugin.Name as the response provider, and
// the plugin's metadata Name is the human-readable display name ("OpenCode
// Go"). The host keys executor registration off the provider name, so every
// model-serving entry point must pin Provider to the stable provider id
// ("opencode-go") even when the request carries the display name.
func TestModelCatalogProviderPinnedToProviderID(t *testing.T) {
	fixture := &scriptedHostClient{status: 200, body: `{"data":[{"id":"pinned-provider-id","protocol":"openai"}]}`}
	c := newCatalog("http://opencode.invalid", nil)
	displayName := pluginapi.Metadata{Name: "OpenCode Go"}
	ctx := context.Background()
	if resp, err := c.RegisterModels(ctx, pluginapi.ModelRegistrationRequest{Plugin: displayName}); err != nil || resp.Provider != provider {
		t.Fatalf("RegisterModels provider %q err %v, want %q", resp.Provider, err, provider)
	}
	if resp, err := c.StaticModels(ctx, pluginapi.StaticModelRequest{Plugin: displayName}); err != nil || resp.Provider != provider {
		t.Fatalf("StaticModels provider %q err %v, want %q", resp.Provider, err, provider)
	}
	req := modelCatalogAuthReq("auth-1", "example-key", fixture)
	req.Plugin = displayName
	if resp, err := c.ModelsForAuth(ctx, req); err != nil || resp.Provider != provider {
		t.Fatalf("ModelsForAuth provider %q err %v, want %q", resp.Provider, err, provider)
	}
}
