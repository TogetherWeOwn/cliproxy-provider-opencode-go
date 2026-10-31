package opencodego

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const provider = "opencode-go"
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

type catalogEntry struct {
	routes  map[string]string
	expires time.Time
}
type catalog struct {
	base   func() string
	client *kit.QuotaHTTPClient
	mu     sync.Mutex
	cache  map[[32]byte]catalogEntry
	// hostProxy is the host's global proxy-url from the latest auth parse,
	// refresh or ModelsForAuth. Executor requests do not carry host config, so
	// discovery on that path reuses it when the auth has no proxy_url.
	hostProxy atomic.Pointer[string]
}

func newCatalog(base string, transport http.RoundTripper) *catalog {
	return &catalog{base: func() string { return base }, client: kit.NewQuotaHTTPClient(transport), cache: map[[32]byte]catalogEntry{}}
}
func modelList(routes map[string]string) []pluginapi.ModelInfo {
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.ModelInfo{ID: modelPrefix + id, Name: id, Object: "model", OwnedBy: provider, DisplayName: id})
	}
	return out
}
func (c *catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: provider, Models: modelList(staticRoutes())}, nil
}
func (c *catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: provider, Models: modelList(staticRoutes())}, nil
}
func (c *catalog) setHostProxy(proxyURL string) { c.hostProxy.Store(&proxyURL) }
func (c *catalog) ModelsForAuth(ctx context.Context, r pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	c.setHostProxy(r.Host.ProxyURL)
	routes, err := c.load(ctx, r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return pluginapi.ModelResponse{Provider: provider, Models: modelList(routes)}, nil
}
func (c *catalog) resolve(ctx context.Context, id string, storage []byte, metadata map[string]any) (string, error) {
	routes, err := c.load(ctx, storage, metadata)
	if err != nil {
		return "", err
	}
	if format, ok := routes[id]; ok {
		return format, nil
	}
	return "", &pluginabi.Error{Code: "invalid_model", Message: "model absent from OpenCode Go catalog or has no known protocol", HTTPStatus: 400}
}
func (c *catalog) load(ctx context.Context, storage []byte, metadata map[string]any) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := kit.APIKey(storage, metadata)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(key))
	c.mu.Lock()
	entry, ok := c.cache[digest]
	c.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.routes, nil
	}
	hostProxy := ""
	if p := c.hostProxy.Load(); p != nil {
		hostProxy = *p
	}
	// Discovery sends the key directly, so route it like host requests.
	proxyCtx := kit.WithProxyURL(ctx, kit.AuthProxyURL(storage, metadata, hostProxy))
	resp, err := c.client.Do(proxyCtx, pluginapi.HTTPRequest{URL: c.base() + "/inference/go/v1/models", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}, "User-Agent": {"cliproxy-provider-opencode-go/0.1.0"}}})
	routes := map[string]string(nil)
	if err == nil {
		routes, err = parseModels(resp.Body)
		if err != nil {
			return nil, err
		} // Explicit unknown protocols must fail closed.
	} else {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var pe *pluginabi.Error
		if errors.As(err, &pe) && (pe.HTTPStatus == 401 || pe.HTTPStatus == 403) {
			return nil, err
		}
		routes = staticRoutes()
	}
	c.mu.Lock()
	// Bound retained credential hashes; expiry does not retain stale credentials.
	for k, v := range c.cache {
		if time.Now().After(v.expires) {
			delete(c.cache, k)
		}
	}
	if len(c.cache) >= 256 {
		clear(c.cache)
	}
	c.cache[digest] = catalogEntry{routes: routes, expires: time.Now().Add(catalogTTL)}
	c.mu.Unlock()
	return routes, nil
}

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func parseModels(raw []byte) (map[string]string, error) {
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
	invalid := func() (map[string]string, error) {
		return nil, &pluginabi.Error{Code: "invalid_catalog", Message: "invalid or ambiguous model catalog", HTTPStatus: 502}
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Data) == 0 {
		return invalid()
	}
	routes := map[string]string{}
	defaults := staticRoutes()
	for _, m := range wire.Data {
		if !modelIDPattern.MatchString(m.ID) {
			continue
		}
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
	return routes, nil
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
