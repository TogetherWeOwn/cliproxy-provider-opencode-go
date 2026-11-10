package opencodego

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestDocumentedModelsHaveExplicitProtocols(t *testing.T) {
	groups := map[string][]string{
		"openai":          {"glm-5.3-flash", "glm-5.3", "glm-5.2", "kimi-k3.1-pro", "kimi-k3", "kimi-k2.7-code", "longcat-2.0", "deepseek-v4.1-flash", "deepseek-v4-pro", "mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.5", "mimo-v2.5-pro", "minimax-m3.1-flash", "hy4-preview", "hy3", "step-5-preview", "beam-free", "iceman-contributor-free"},
		"openai-response": {"grok-4.7", "gpt-6-luna", "gpt-5.6-luna", "muse-spark-1.3-contributor", "muse-spark-1.2-contributor"},
		"claude":          {"claude-haiku-5-5", "minimax-m3", "qwen3.8-max", "qwen3.8-flash", "qwen3.7-plus"},
		"gemini":          {"gemini-4-argon"},
	}
	c := newCatalog("http://127.0.0.1:0", nil)
	got, err := c.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil || len(got.Models) != 30 {
		t.Fatalf("catalog: %d %v", len(got.Models), err)
	}
	for format, ids := range groups {
		for _, id := range ids {
			route, ok := staticRoutes()[id]
			if !ok || route != format {
				t.Errorf("%s route %q", id, route)
			}
			found := false
			for _, m := range got.Models {
				if m.ID == "opencode-go/"+id {
					found = true
				}
			}
			if !found {
				t.Errorf("unprefixed/missing %s", id)
			}
		}
	}
}

func TestDiscoveryEveryInvocationFetchesAndRouteCacheFeedsExecutor(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/inference/go/v1/models" || r.Header.Get("Authorization") != "Bearer example-key" {
			t.Error("wrong discovery request")
		}
		fmt.Fprint(w, `{"data":[{"id":"minimax-m3","protocol":"openai"},{"id":"future-model","api":{"npm":"@ai-sdk/anthropic","url":"https://opencode.ai/inference/go/anthropic/v1"}},{"id":"unknown-model"},{"id":"bad/path","protocol":"openai"}]}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	req := pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: testHTTP{s.Client()}}
	// The kit ModelCatalog fetches on every ModelsForAuth invocation: its TTL
	// bounds freshness markers only, never fetch gating, so a one-shot
	// catalog callback is never skipped (design §§3-4).
	for range 2 {
		got, err := c.ModelsForAuth(context.Background(), req)
		if err != nil || len(got.Models) != 2 || got.Models[0].ID != "opencode-go/future-model" || got.Models[1].ID != "opencode-go/minimax-m3" {
			t.Fatalf("models %+v %v", got, err)
		}
	}
	// Unknown metadata ids never reach model serving (never guessed into Chat).
	if got := calls.Load(); got != 2 {
		t.Fatalf("discovery calls=%d, want 2 (one per ModelsForAuth invocation)", got)
	}
	// The executor reads the plugin-owned route cache populated by Fetch:
	// minimax-m3's catalog override (openai) wins over its static protocol.
	route, err := c.resolve(context.Background(), "minimax-m3", req.StorageJSON, nil, testHTTP{s.Client()})
	if err != nil || route != "openai" || calls.Load() != 2 {
		t.Fatalf("route %s calls %d err %v", route, calls.Load(), err)
	}
	// Once the route-cache entry goes stale, the executor refreshes it through
	// its own host client (the existing 60-second route-cache behavior).
	c.mu.Lock()
	for k, v := range c.cache {
		v.expires = time.Time{}
		c.cache[k] = v
	}
	c.mu.Unlock()
	route, err = c.resolve(context.Background(), "minimax-m3", req.StorageJSON, nil, testHTTP{s.Client()})
	if err != nil || route != "openai" || calls.Load() != 3 {
		t.Fatalf("stale resolve route %s calls=%d err=%v", route, calls.Load(), err)
	}
}

func TestDiscoveryFallbackAndCredentialIsolation(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"error":{"type":"AuthError"}}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	req := pluginapi.AuthModelRequest{AuthID: "same", StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: testHTTP{s.Client()}}
	got, err := c.ModelsForAuth(context.Background(), req)
	if err != nil || len(got.Models) != 30 {
		t.Fatalf("fallback %d %v", len(got.Models), err)
	}
	// Behavior change (kit pipeline semantics, design §4): a 401 during
	// discovery no longer surfaces as a discovery error while a fallback
	// exists; the static snapshot is served. Inference still enforces the
	// credential per request, and credential generation isolation (the
	// rotated key fetches independently and never inherits a last-good
	// generation) is asserted in TestModelCatalogCredentialGenerationIsolation.
	status.Store(401)
	req.StorageJSON = []byte(`{"api_key":"different-example-key"}`)
	got, err = c.ModelsForAuth(context.Background(), req)
	if err != nil || len(got.Models) != 30 {
		t.Fatalf("401 fallback %d %v", len(got.Models), err)
	}
	if calls.Load() != 2 {
		t.Fatal("cache crossed credential boundary")
	}
	if _, err = c.resolve(context.Background(), "opencode-go/unknown", []byte(`{"api_key":"example-key"}`), nil, testHTTP{s.Client()}); err == nil {
		t.Fatal("unknown model guessed a protocol")
	}
}

func TestDiscoveryRejectsUnsafeOrConflictingMetadata(t *testing.T) {
	for _, raw := range []string{
		`{"data":[{"id":"future","api":{"npm":"@ai-sdk/anthropic","url":"https://example.invalid/messages"}}]}`,
		`{"data":[{"id":"future","protocol":"claude","endpoint":"/inference/go/openai/v1/chat/completions"}]}`,
		`{"data":[{"id":"future","protocol":"openai"},{"id":"future","protocol":"claude"}]}`,
		`{"data":[{"id":"glm-5.3","protocol":"unknown"}]}`,
		`null`,
	} {
		t.Run(strings.ReplaceAll(raw[:min(len(raw), 24)], "/", "_"), func(t *testing.T) {
			if _, _, err := parseCatalog([]byte(raw)); err == nil {
				t.Fatal("unsafe or ambiguous catalog accepted")
			}
		})
	}
}

func TestOpenAIBaseDoesNotOverrideResponsesOrGuessUnknownProtocol(t *testing.T) {
	got, _, err := parseCatalog([]byte(`{"data":[{"id":"gpt-6-luna","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}},{"id":"new-responses","protocol":"responses","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}},{"id":"ambiguous-new","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}}]}`))
	if err != nil || got["gpt-6-luna"] != "openai-response" || got["new-responses"] != "openai-response" || got["ambiguous-new"] != "" {
		t.Fatalf("routes %+v %v", got, err)
	}
}

// TestLastGoodRoutesSurviveOutage asserts the per-credential durable
// last-good routes keep protocol mapping for models the kit catalog still
// serves after discovery starts failing (review: last-good models must not
// lose their routes when the fresh route cache expires).
func TestLastGoodRoutesSurviveOutage(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Int32
	healthy.Store(1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if healthy.Load() == 0 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"type":"ServiceUnavailable"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"future-model","api":{"npm":"@ai-sdk/anthropic","url":"https://opencode.ai/inference/go/anthropic/v1"}},{"id":"minimax-m3","protocol":"openai"}]}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	req := pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: testHTTP{s.Client()}}
	got, err := c.ModelsForAuth(context.Background(), req)
	if err != nil || len(got.Models) != 2 {
		t.Fatalf("models %+v %v", got, err)
	}
	// Expire the fresh route cache, then break discovery. The kit catalog
	// keeps serving its last-good models; resolve must still route the
	// non-static future-model via the durable last-good routes.
	c.mu.Lock()
	for k, v := range c.cache {
		v.expires = time.Time{}
		c.cache[k] = v
	}
	c.mu.Unlock()
	healthy.Store(0)
	route, err := c.resolve(context.Background(), "future-model", req.StorageJSON, nil, testHTTP{s.Client()})
	if err != nil || route != "claude" {
		t.Fatalf("last-good route %q err %v", route, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("discovery calls=%d, want 2 (ModelsForAuth + one failed refresh)", calls.Load())
	}
	// The failed refresh wrote a backoff entry: a later resolve must not
	// repeat the failing discovery call, and still routes via last-good.
	route, err = c.resolve(context.Background(), "future-model", req.StorageJSON, nil, testHTTP{s.Client()})
	if err != nil || route != "claude" {
		t.Fatalf("backoff last-good route %q err %v", route, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("discovery calls=%d after backoff, want 2 (no refetch)", calls.Load())
	}
	// Last-good is per credential: a different credential has no last-good
	// and falls back to the static snapshot, never inheriting the route.
	route, err = c.resolve(context.Background(), "future-model", []byte(`{"api_key":"other-key"}`), nil, testHTTP{s.Client()})
	if err == nil {
		t.Fatalf("other credential routed %q from another credential's last-good", route)
	}
}

// TestResolveSurfacesDiscoveryErrorForUnknownID asserts that when a route
// refresh is attempted and fails, an id absent from every fallback gets
// the discovery error rather than invalid_model.
func TestResolveSurfacesDiscoveryErrorForUnknownID(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"type":"ServiceUnavailable"}}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	_, err := c.resolve(context.Background(), "no-such-model", []byte(`{"api_key":"example-key"}`), nil, testHTTP{s.Client()})
	if err == nil {
		t.Fatal("unknown id with failed discovery got no error")
	}
	var abiErr *pluginabi.Error
	if errors.As(err, &abiErr) && abiErr.Code == "invalid_model" {
		t.Fatalf("got invalid_model %v, want the discovery error", err)
	}
	if abiErr == nil || abiErr.Code != "models_fetch_failed" {
		t.Fatalf("got %T %v, want models_fetch_failed", err, err)
	}
}

// TestLastGoodEvictionIsBoundedOldestFirst asserts the route stores
// evict oldest-first at maxRouteCredentials (mirroring the kit's
// last-good bound) instead of clearing every credential at 256, and
// that the last-good bound holds even when fresh entries expire between
// stores (slow credential rotation).
func TestLastGoodEvictionIsBoundedOldestFirst(t *testing.T) {
	c := newCatalog("", nil)
	routesOf := func(i int) map[string]string { return map[string]string{"model": fmt.Sprintf("proto-%d", i)} }
	for i := 0; i < maxRouteCredentials+40; i++ {
		// Simulate slow rotation: expire the fresh cache between stores
		// so only the last-good bound can limit the durable map.
		c.mu.Lock()
		for k, v := range c.cache {
			v.expires = time.Time{}
			c.cache[k] = v
		}
		c.mu.Unlock()
		c.storeRoutes(fmt.Sprintf("slow-key-%d", i), routesOf(i))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lastGood) != maxRouteCredentials {
		t.Fatalf("lastGood holds %d, want %d", len(c.lastGood), maxRouteCredentials)
	}
	if got := c.lastGood[credentialDigest("slow-key-0")]; got != nil {
		t.Fatalf("oldest credential retained %+v, want evicted", got)
	}
	if got := c.lastGood[credentialDigest("slow-key-100")]; got == nil {
		t.Fatalf("middle credential lost, want retained (wholesale clear)")
	} else if got["model"] != "proto-100" {
		t.Fatalf("middle credential routes %+v, want proto-100", got)
	}
	if got := c.lastGood[credentialDigest(fmt.Sprintf("slow-key-%d", maxRouteCredentials+39))]; got == nil {
		t.Fatal("newest credential lost, want retained")
	}
}

// TestResolveBackoffSurfacesDiscoveryError asserts that while a
// failed-refresh backoff is active, an id no fallback can route gets the
// recorded transient discovery error (and no refetch), not invalid_model.
func TestResolveBackoffSurfacesDiscoveryError(t *testing.T) {
	client := &scriptedHostClient{status: 503}
	c := newCatalog("", nil)
	storage := []byte(`{"api_key":"example-key"}`)
	_, err := c.resolve(context.Background(), "no-such-model", storage, nil, client)
	var abiErr *pluginabi.Error
	if err == nil || !errors.As(err, &abiErr) || abiErr.Code != "models_fetch_failed" {
		t.Fatalf("first resolve got %v, want models_fetch_failed", err)
	}
	_, err = c.resolve(context.Background(), "no-such-model", storage, nil, client)
	if err == nil || !errors.As(err, &abiErr) || abiErr.Code != "models_fetch_failed" {
		t.Fatalf("backoff resolve got %v, want models_fetch_failed", err)
	}
	if got := client.callCount(); got != 1 {
		t.Fatalf("discovery calls=%d, want 1 (backoff suppresses refetch)", got)
	}
}

// cancelParentOnDo cancels the parent context on the first Do, simulating
// a caller disconnecting mid-refresh.
type cancelParentOnDo struct {
	*scriptedHostClient
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelParentOnDo) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	w.once.Do(w.cancel)
	// Block until the derived request context observes the cancellation.
	w.scriptedHostClient.block = true
	return w.scriptedHostClient.Do(ctx, req)
}

// TestCallerCancellationDoesNotOpenBackoff asserts a caller cancellation
// during a route refresh writes no failed-refresh backoff, so the next
// request still attempts discovery.
func TestCallerCancellationDoesNotOpenBackoff(t *testing.T) {
	inner := &scriptedHostClient{status: 503}
	parent, cancel := context.WithCancel(context.Background())
	client := &cancelParentOnDo{scriptedHostClient: inner, cancel: cancel}
	c := newCatalog("", nil)
	storage := []byte(`{"api_key":"example-key"}`)
	_, err := c.resolve(parent, "no-such-model", storage, nil, client)
	if err == nil {
		t.Fatal("canceled refresh got no error")
	}
	_, err = c.resolve(context.Background(), "no-such-model", storage, nil, inner)
	if err == nil {
		t.Fatal("follow-up refresh got no error")
	}
	if got := inner.callCount(); got != 2 {
		t.Fatalf("discovery calls=%d, want 2 (no backoff after caller cancellation)", got)
	}
}

func TestInvalidSuccessfulDiscoveryFallsBackWithoutPublishingRoutes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3","protocol":"unknown-protocol"}]}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	// Kit pipeline semantics (design §4): a 200 but invalid catalog is a
	// failed fetch; the static snapshot serves instead of a 502. The invalid
	// refresh never writes the route cache, so the executor keeps the
	// documented static protocol rather than a wrong one from the catalog.
	req := pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example"}`), HTTPClient: testHTTP{s.Client()}}
	got, err := c.ModelsForAuth(context.Background(), req)
	if err != nil || len(got.Models) != 30 {
		t.Fatalf("invalid catalog fallback %d %v", len(got.Models), err)
	}
	route, err := c.resolve(context.Background(), "glm-5.3", req.StorageJSON, nil, testHTTP{s.Client()})
	if err != nil || route != "openai" {
		t.Fatalf("route %q err %v", route, err)
	}
}
