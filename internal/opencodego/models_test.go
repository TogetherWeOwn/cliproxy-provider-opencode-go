package opencodego

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestDiscoveryCacheAndProtocolOverride(t *testing.T) {
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
	req := pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example-key"}`)}
	for range 2 {
		got, err := c.ModelsForAuth(context.Background(), req)
		if err != nil || len(got.Models) != 2 || got.Models[0].ID != "opencode-go/future-model" || got.Models[1].ID != "opencode-go/minimax-m3" {
			t.Fatalf("models %+v %v", got, err)
		}
	}
	route, err := c.resolve(context.Background(), "minimax-m3", req.StorageJSON, nil)
	if err != nil || route != "openai" || calls.Load() != 1 {
		t.Fatalf("route %s calls %d err %v", route, calls.Load(), err)
	}
	c.mu.Lock()
	for k, v := range c.cache {
		v.expires = time.Time{}
		c.cache[k] = v
	}
	c.mu.Unlock()
	_, err = c.ModelsForAuth(context.Background(), req)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("expired cache calls=%d err=%v", calls.Load(), err)
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
	req := pluginapi.AuthModelRequest{AuthID: "same", StorageJSON: []byte(`{"api_key":"example-key"}`)}
	got, err := c.ModelsForAuth(context.Background(), req)
	if err != nil || len(got.Models) != 30 {
		t.Fatalf("fallback %d %v", len(got.Models), err)
	}
	status.Store(401)
	req.StorageJSON = []byte(`{"api_key":"different-example-key"}`)
	if _, err = c.ModelsForAuth(context.Background(), req); err == nil {
		t.Fatal("bad credential hidden behind cached catalog")
	}
	if calls.Load() != 2 {
		t.Fatal("cache crossed credential boundary")
	}
	if _, err = c.resolve(context.Background(), "opencode-go/unknown", []byte(`{"api_key":"example-key"}`), nil); err == nil {
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
			if _, err := parseModels([]byte(raw)); err == nil {
				t.Fatal("unsafe or ambiguous catalog accepted")
			}
		})
	}
}

func TestOpenAIBaseDoesNotOverrideResponsesOrGuessUnknownProtocol(t *testing.T) {
	got, err := parseModels([]byte(`{"data":[{"id":"gpt-6-luna","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}},{"id":"new-responses","protocol":"responses","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}},{"id":"ambiguous-new","api":{"npm":"@ai-sdk/openai","url":"https://opencode.ai/inference/go/openai/v1"}}]}`))
	if err != nil || got["gpt-6-luna"] != "openai-response" || got["new-responses"] != "openai-response" || got["ambiguous-new"] != "" {
		t.Fatalf("routes %+v %v", got, err)
	}
}

func TestInvalidSuccessfulDiscoveryDoesNotSilentlyUseWrongProtocol(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3","protocol":"unknown-protocol"}]}`)
	}))
	defer s.Close()
	c := newCatalog(s.URL, nil)
	_, err := c.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "one", StorageJSON: []byte(`{"api_key":"example"}`)})
	if err == nil {
		t.Fatal("invalid catalog hidden behind stale protocol fallback")
	}
}
