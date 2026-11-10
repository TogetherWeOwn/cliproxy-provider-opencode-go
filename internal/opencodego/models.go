package opencodego

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const provider = "opencode-go"

const displayName = "OpenCode Go"

const modelPrefix = "opencode-go/"
const upstreamBase = "https://opencode.ai"
const catalogTTL = time.Minute

// This documented snapshot is only a discovery-outage fallback. Explicit
// protocol metadata from the authenticated catalog wins, including minimax-m3.
func staticRoutes() map[string]string {
	routes := map[string]string{}
	for format, ids := range map[string]string{
		"openai":          "glm-5.3-flash glm-5.3 glm-5.2 kimi-k3.1-pro kimi-k3 kimi-k2.7-code longcat-2.0 deepseek-v4.1-flash deepseek-v4-pro mimo-v2.6-flash mimo-v2.6-pro mimo-v2.5 mimo-v2.5-pro minimax-m3.1-flash hy4-preview hy3 step-5-preview beam-free iceman-contributor-free",
		"openai-response": "grok-4.7 gpt-6-luna gpt-5.6-luna muse-spark-1.3-contributor muse-spark-1.2-contributor",
		"claude":          "claude-haiku-5-5 minimax-m3 qwen3.8-max qwen3.8-flash qwen3.7-plus",
		"gemini":          "gemini-4-argon",
	} {
		for _, id := range strings.Fields(ids) {
			routes[id] = format
		}
	}
	return routes
}

// staticModelInfos is the documented snapshot as pre-namespace kit Static
// entries. The kit namespace pipeline prefixes every source (fresh,
// last-good, static) identically, so Static carries upstream ids.
func staticModelInfos() []pluginapi.ModelInfo {
	routes := staticRoutes()
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}

type catalogEntry struct {
	routes  map[string]string
	expires time.Time
	// negative marks a failed-refresh backoff entry holding the static
	// snapshot. It stops inference requests from repeating a failing
	// discovery call on every request, and never satisfies validate.
	negative bool
}

// catalog embeds the kit ModelCatalog (model serving, single-flight and the
// durable last-good store) and owns the route cache: the per-credential map
// from upstream model id to protocol used by the executor. The kit catalog
// never sees protocols; they stay plugin-side, layered on the returned ids
// per design §5.
type catalog struct {
	*kit.ModelCatalog
	base     func() string
	client   *kit.QuotaHTTPClient
	mu       sync.Mutex
	cache    map[[32]byte]catalogEntry
	lastGood map[[32]byte]map[string]string
}

func newCatalog(base string, transport http.RoundTripper) *catalog {
	c := &catalog{base: func() string { return base }, client: kit.NewQuotaHTTPClient(transport), cache: map[[32]byte]catalogEntry{}, lastGood: map[[32]byte]map[string]string{}}
	c.ModelCatalog = kit.NewModelCatalogWithTTL(kit.ModelCatalogConfig{
		Namespace: provider,
		Static:    staticModelInfos(),
		Fetch:     c.fetch,
		Validate:  c.validate,
	}, catalogTTL)
	return c
}

// withProvider pins the response provider to the stable provider id. The
// kit ModelCatalog echoes req.Plugin.Name, which is the plugin's
// human-readable display name ("OpenCode Go") since the display-name
// metadata change; the host keys executor registration off the provider
// name, so serving that display name would register the executor under
// "opencode go" instead of "opencode-go". Normalize every model-serving
// entry point to the constant here.
func fixProvider(resp *pluginapi.ModelResponse) {
	if resp.Provider == "" || resp.Provider != provider {
		resp.Provider = provider
	}
	for i := range resp.Registrations {
		resp.Registrations[i].Provider = provider
	}
}

// ModelsForAuth serves through the embedded kit catalog with the
// provider id normalized (see fixProvider).
func (c *catalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.ModelCatalog.ModelsForAuth(ctx, req)
	fixProvider(&resp)
	return resp, err
}

// StaticModels serves the static list through the embedded kit catalog
// with the provider id normalized (see fixProvider).
func (c *catalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.ModelCatalog.StaticModels(ctx, req)
	fixProvider(&resp)
	return resp, err
}

// RegisterModels serves the static list through the embedded kit catalog
// with the provider id normalized (see fixProvider).
func (c *catalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	resp, err := c.ModelCatalog.RegisterModels(ctx, req)
	resp.Provider = provider
	for i := range resp.Registrations {
		resp.Registrations[i].Provider = provider
	}
	return resp, err
}

// fetch is the plugin's FetchModelsFunc: it runs the existing discovery
// fetch through the request's host HTTP client, enforces the post-return
// parse cap itself, populates the route cache for this credential and
// returns every pattern-valid candidate id. Validate then drops the ids
// without a known protocol. Every call fetches: the kit ModelCatalog runs
// one fetch per ModelsForAuth invocation (its TTL only bounds freshness
// markers) and there is no TTL skip here to lose a one-shot callback.
func (c *catalog) fetch(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	fetchFailed := func(message string) error {
		return &pluginabi.Error{Code: "models_fetch_failed", Message: message, HTTPStatus: 502}
	}
	if req.HTTPClient == nil {
		return nil, fetchFailed("model discovery client is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := kit.APIKey(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, err
	}
	resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: c.base() + "/inference/go/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}, "User-Agent": {"cliproxy-provider-opencode-go/0.1.2"}}})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fetchFailed("model discovery fetch failed")
	}
	// Post-return parse cap, mirroring the kit OpenAI fetcher: never parse
	// an oversized buffered body.
	if len(resp.Body) > kit.MaxModelsBody {
		return nil, fetchFailed("model discovery response exceeds the parse cap")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	routes, candidates, err := parseCatalog(resp.Body)
	if err != nil {
		return nil, err
	}
	c.storeRoutes(key, routes)
	sort.Strings(candidates)
	out := make([]pluginapi.ModelInfo, 0, len(candidates))
	for _, id := range candidates {
		out = append(out, pluginapi.ModelInfo{ID: id, Name: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out, nil
}

// validate is the post-fetch hook (design §5): on the fresh path it
// enforces the never-guess invariant, dropping candidate ids without a
// known protocol for this credential instead of letting the executor
// misroute them. Last-good and static results were validated when
// discovered or are trusted configuration, and the kit serves them without
// this hook, so filtering here never hides a previously served model: a
// failed Validate is a failed fetch and the kit serves last-good/static.
func (c *catalog) validate(_ context.Context, req pluginapi.AuthModelRequest, models []pluginapi.ModelInfo) ([]pluginapi.ModelInfo, error) {
	key, err := kit.APIKey(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, err
	}
	routes := c.routes(key)
	if routes == nil {
		return nil, &pluginabi.Error{Code: "models_validate_failed", Message: "model validation failed", HTTPStatus: 502}
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, ok := routes[m.ID]; !ok || !modelIDPattern.MatchString(m.ID) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *catalog) storeRoutes(key string, routes map[string]string) {
	digest := credentialDigest(key)
	c.mu.Lock()
	c.evictLocked()
	if len(c.cache) >= 256 {
		clear(c.cache)
		clear(c.lastGood)
	}
	c.cache[digest] = catalogEntry{routes: routes, expires: time.Now().Add(catalogTTL)}
	// The durable per-credential last-good map keeps routes for models the
	// kit catalog still serves after a later discovery outage, so they do
	// not lose their protocols when the fresh entry expires.
	c.lastGood[digest] = routes
	c.mu.Unlock()
}

// storeNegative records a failed-refresh backoff for this credential: the
// static snapshot with a catalogTTL expiry. It never touches lastGood and
// never satisfies validate, so a failed refresh never publishes routes.
func (c *catalog) storeNegative(key string) {
	digest := credentialDigest(key)
	c.mu.Lock()
	c.evictLocked()
	if len(c.cache) >= 256 {
		clear(c.cache)
	}
	c.cache[digest] = catalogEntry{routes: staticRoutes(), expires: time.Now().Add(catalogTTL), negative: true}
	c.mu.Unlock()
}

// evictLocked drops expired cache entries; the durable lastGood map is
// retained so outage fallback keeps validated routes.
func (c *catalog) evictLocked() {
	for k, v := range c.cache {
		if time.Now().After(v.expires) {
			delete(c.cache, k)
		}
	}
}

// routes returns the fresh route entry for this credential, or nil when it
// is absent, stale, or a failed-refresh backoff entry.
func (c *catalog) routes(key string) map[string]string {
	digest := credentialDigest(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[digest]
	if !ok || entry.negative || !time.Now().Before(entry.expires) {
		return nil
	}
	return entry.routes
}

// backoff reports whether this credential is inside a failed-refresh
// backoff window and inference must not repeat the discovery call.
func (c *catalog) backoff(key string) bool {
	digest := credentialDigest(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[digest]
	return ok && entry.negative && time.Now().Before(entry.expires)
}

// lastGoodRoutes returns the durable last successfully validated routes for
// this credential, or nil when none exist.
func (c *catalog) lastGoodRoutes(key string) map[string]string {
	digest := credentialDigest(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastGood[digest]
}

// resolve maps one executor model id to its upstream protocol. The fresh
// route entry is the source of truth; when it is stale or missing the
// executor refreshes it through its own host HTTP client (same 60-second
// cadence as before), bounded by the kit fetch deadline so a hung discovery
// call cannot hold inference. A failed refresh writes a failed-refresh
// backoff entry so later requests do not repeat the failing call, and falls
// back to this credential's durable last-good routes — which cover models
// the kit catalog still serves during the outage — before the documented
// static snapshot. Unknown ids fail closed: when a refresh was attempted
// and failed, the discovery error surfaces; otherwise invalid_model.
// Some kit fetch failures (e.g. a 401 during discovery) now serve the
// static fallback silently for model serving (kit pipeline semantics,
// design §4); the executor still enforces credentials per request, and an
// invalid catalog never writes routes, so a protocol change is never
// published from an invalid refresh.
func (c *catalog) resolve(ctx context.Context, id string, storage []byte, metadata map[string]any, client pluginapi.HostHTTPClient) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key, err := kit.APIKey(storage, metadata)
	if err != nil {
		return "", err
	}
	routes := c.routes(key)
	var ferr error
	if routes == nil && client != nil && !c.backoff(key) {
		fctx, cancel := context.WithTimeout(ctx, kit.ModelsFetchTimeout)
		_, ferr = c.fetch(fctx, pluginapi.AuthModelRequest{StorageJSON: storage, Metadata: metadata, HTTPClient: client})
		cancel()
		if ferr == nil {
			routes = c.routes(key)
		} else {
			c.storeNegative(key)
		}
	}
	if routes == nil {
		routes = c.lastGoodRoutes(key)
	}
	if routes == nil {
		routes = staticRoutes()
	}
	if format, ok := routes[id]; ok {
		return format, nil
	}
	if ferr != nil {
		return "", ferr
	}
	return "", &pluginabi.Error{Code: "invalid_model", Message: "model absent from OpenCode Go catalog or has no known protocol", HTTPStatus: 400}
}

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// parseCatalog parses the discovery wire into the known-protocol routes and
// the candidate ids. Candidates are every pattern-valid wire id, including
// ids without a resolved protocol, so the kit pipeline and Validate see the
// full list and Validate drops the unknown ones. Invalid or ambiguous
// metadata still fails closed at parse time and never produces partial
// routes.
func parseCatalog(raw []byte) (map[string]string, []string, error) {
	var wire struct {
		Data []struct {
			ID       string `json:"id"`
			Protocol string `json:"protocol"`
			Endpoint string `json:"endpoint"`
			API      struct {
				NPM string `json:"npm"`
				URL string `json:"url"`
			} `json:"api"`
		} `json:"data"`
	}
	invalid := func() (map[string]string, []string, error) {
		return nil, nil, &pluginabi.Error{Code: "invalid_catalog", Message: "invalid or ambiguous model catalog", HTTPStatus: 502}
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Data) == 0 {
		return invalid()
	}
	routes := map[string]string{}
	defaults := staticRoutes()
	var candidates []string
	for _, m := range wire.Data {
		if !modelIDPattern.MatchString(m.ID) {
			continue
		}
		candidates = append(candidates, m.ID)
		format := ""
		openaiFamily := false
		choose := func(candidate string) bool {
			if candidate == "openai-family" {
				openaiFamily = true
				return true
			}
			if candidate == "" {
				return false
			}
			if format != "" && format != candidate {
				return false
			}
			format = candidate
			return true
		}
		if m.Protocol != "" {
			if !choose(normalizeProtocol(m.Protocol)) {
				return invalid()
			}
		}
		if m.Endpoint != "" {
			if !choose(endpointProtocol(m.Endpoint)) {
				return invalid()
			}
		}
		if m.API.URL != "" {
			if !choose(endpointProtocol(m.API.URL)) {
				return invalid()
			}
		}
		if m.API.NPM != "" {
			// The OpenAI SDK serves both Chat and Responses; an explicit endpoint is
			// authoritative within that family, not an arbitrary URL from discovery.
			switch m.API.NPM {
			case "@ai-sdk/anthropic":
				if !choose("claude") {
					return invalid()
				}
			case "@ai-sdk/google":
				if !choose("gemini") {
					return invalid()
				}
			case "@ai-sdk/openai", "@ai-sdk/openai-compatible":
				openaiFamily = true
			default:
				return invalid()
			}
		}
		if format == "" {
			format = defaults[m.ID]
		}
		if openaiFamily && format != "" && format != "openai" && format != "openai-response" {
			return invalid()
		}
		if format == "" {
			continue
		} // Unknown IDs must not be guessed into Chat.
		if previous, exists := routes[m.ID]; exists && previous != format {
			return invalid()
		}
		routes[m.ID] = format
	}
	if len(routes) == 0 {
		return invalid()
	}
	return routes, candidates, nil
}
func normalizeProtocol(s string) string {
	switch strings.ToLower(s) {
	case "openai", "chat", "chat-completions", "chat_completions":
		return "openai"
	case "responses", "openai-response":
		return "openai-response"
	case "anthropic", "messages", "claude":
		return "claude"
	case "google", "gemini":
		return "gemini"
	}
	return ""
}
func endpointProtocol(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if u.Host != "" && !u.IsAbs() || u.IsAbs() && (u.Scheme != "https" || u.Host != "opencode.ai") {
		return ""
	}
	switch strings.TrimSuffix(u.Path, "/") {
	case "/inference/go/openai/v1":
		return "openai-family"
	case "/inference/go/openai/v1/chat/completions":
		return "openai"
	case "/inference/go/openai/v1/responses":
		return "openai-response"
	case "/inference/go/anthropic/v1", "/inference/go/anthropic/v1/messages":
		return "claude"
	case "/inference/go/google/v1beta":
		return "gemini"
	}
	return ""
}
