//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

type executors map[string]coreauth.ProviderExecutor

func (m executors) Executor(p string) (coreauth.ProviderExecutor, bool) { e, ok := m[p]; return e, ok }
func (m executors) RegisterExecutor(e coreauth.ProviderExecutor)        { m[e.Identifier()] = e }
func (m executors) UnregisterExecutor(p string)                         { delete(m, p) }

type modelRecorder struct{ ids map[string]bool }

func (m modelRecorder) RegisterClient(_ string, _ string, models []*registry.ModelInfo) {
	for _, model := range models {
		m.ids[model.ID] = true
	}
}
func (m modelRecorder) UnregisterClient(string)           {}
func (m modelRecorder) GetModelProviders(string) []string { return nil }

type usageSink struct{ records chan usage.Record }

func (s usageSink) HandleUsage(_ context.Context, r usage.Record) {
	select {
	case s.records <- r:
	default:
	}
}

// This is the kit integration/hostcompat pattern: the pinned, unmodified host's
// actual dlopen loader, RPC registration and adapters, not an ABI simulation.
func TestRealHostLoaderAllProviderCapabilities(t *testing.T) {
	var modelCalls, quotaCalls atomic.Int32
	var failure atomic.Int32
	var mu sync.Mutex
	var session string
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer example-key" && r.Header.Get("Authorization") != "Bearer example-sibling-key" {
			t.Error("wrong auth header")
		}
		switch r.URL.Path {
		case "/inference/go/v1/models":
			modelCalls.Add(1)
			io.WriteString(w, `{"data":[{"id":"glm-5.3"},{"id":"gpt-6-luna"},{"id":"minimax-m3","protocol":"claude"},{"id":"gemini-4-argon"}]}`)
			return
		case "/inference/go/v1/usage":
			quotaCalls.Add(1)
			now := time.Now().UTC()
			fmt.Fprintf(w, `{"usage":{"rolling":{"status":"ok","percent":25,"resetsAt":%q},"weekly":{"status":"rate-limited","percent":20,"resetsAt":%q},"monthly":{"status":"ok","percent":50,"resetsAt":%q}}}`, now.Add(time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339), now.Add(30*24*time.Hour).Format(time.RFC3339))
			return
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") == "Bearer example-key" && failure.Load() != 0 {
			if failure.Load() == 429 {
				w.Header().Set("Retry-After", "900")
				w.WriteHeader(429)
				io.WriteString(w, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"weekly"}}`)
			} else {
				w.WriteHeader(403)
				io.WriteString(w, `{"error":{"type":"EntitlementError"}}`)
			}
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid upstream JSON")
		}
		if model, ok := body["model"].(string); ok && strings.HasPrefix(model, "opencode-go/") {
			t.Error("routing prefix leaked upstream")
		}
		if r.Header.Get("User-Agent") != "ExampleCodingClient/1.0" {
			t.Error("client identity lost")
		}
		mu.Lock()
		seen[r.URL.Path]++
		if session == "" {
			session = r.Header.Get("X-Opencode-Session")
		}
		if session == "" || session != r.Header.Get("X-Opencode-Session") {
			t.Error("session unstable")
		}
		mu.Unlock()
		stream := body["stream"] == true || strings.Contains(r.URL.Path, ":streamGenerateContent")
		format := ""
		switch r.URL.Path {
		case "/inference/go/openai/v1/chat/completions":
			format = "openai"
			if body["model"] != "glm-5.3" || body["messages"] == nil {
				t.Error("Chat request malformed")
			}
		case "/inference/go/openai/v1/responses":
			format = "openai-response"
			if body["model"] != "gpt-6-luna" || body["input"] == nil || body["max_output_tokens"] != float64(16) {
				t.Error("Responses request malformed or lost budget")
			}
		case "/inference/go/anthropic/v1/messages":
			format = "claude"
			if body["model"] != "minimax-m3" || body["messages"] == nil {
				t.Error("Messages request malformed")
			}
		case "/inference/go/google/v1beta/models/gemini-4-argon:generateContent", "/inference/go/google/v1beta/models/gemini-4-argon:streamGenerateContent":
			format = "gemini"
			if body["contents"] == nil {
				t.Error("Gemini request malformed")
			}
			if stream && r.URL.Query().Get("alt") != "sse" {
				t.Error("Gemini stream not SSE")
			}
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		io.WriteString(w, fixture(format, stream))
	}))
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", filepath.Join(arch, "opencode-go"+ext), "./cmd/plugin")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_base: "+server.URL+"\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"opencode-go": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	host.ApplyConfig(ctx, cfg)
	if !host.PluginLoaded("opencode-go") || !host.PluginRegistered("opencode-go") || !host.HasAuthProvider("opencode-go") || !host.HasQuotaProvider("opencode-go") {
		t.Fatalf("registration failed: %+v", host.RegisteredPlugins())
	}
	auth, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "opencode-go", FileName: "example.json", RawJSON: []byte(`{"type":"opencode-go","id":"example-a","api_key":"example-key"}`)})
	if err != nil || !handled || auth == nil {
		t.Fatalf("parse %v %v", handled, err)
	}
	savedPath := filepath.Join(cfg.AuthDir, "example.json")
	if err = auth.Storage.SaveTokenToFile(savedPath); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	desc, handled, err := host.DescribeQuota(ctx, "opencode-go")
	if err != nil || !handled || desc.SupportsReset {
		t.Fatalf("quota descriptor %+v %v", desc, err)
	}
	for range 2 {
		q, h, err := host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{AuthID: auth.ID, Provider: "opencode-go", StorageJSON: saved})
		if err != nil || !h || len(q.Groups) != 1 || len(q.Groups[0].Buckets) != 3 || q.Groups[0].Buckets[0].RemainingFraction != .75 || q.Groups[0].Buckets[1].RemainingFraction != 0 {
			t.Fatalf("quota %+v %v", q, err)
		}
	}
	if quotaCalls.Load() != 1 {
		t.Fatal("quota cache not used")
	}
	for range 2 {
		discovered := host.ModelsForAuth(ctx, auth)
		if discovered.Err != nil || !discovered.Handled || len(discovered.Models) != 4 {
			t.Fatalf("discovery %+v", discovered)
		}
		for _, m := range discovered.Models {
			if !strings.HasPrefix(m.ID, "opencode-go/") {
				t.Fatal("unprefixed discovered model")
			}
		}
	}
	if modelCalls.Load() != 1 {
		t.Fatal("discovery cache not used")
	}
	mr := modelRecorder{ids: map[string]bool{}}
	m := executors{}
	host.RegisterModels(ctx, mr)
	host.RegisterExecutors(m, mr)
	if len(mr.ids) != 30 || !mr.ids["opencode-go/gemini-4-argon"] {
		t.Fatalf("static registration %+v", mr.ids)
	}
	ex, ok := m["opencode-go"]
	if !ok {
		t.Fatal("executor absent")
	}
	records := make(chan usage.Record, 64)
	usage.RegisterNamedPlugin("opencode-go-hostcompat", usageSink{records})
	for _, id := range []string{"glm-5.3", "gpt-6-luna", "minimax-m3", "gemini-4-argon"} {
		for _, stream := range []bool{false, true} {
			// An OpenAI client talking to each native upstream protocol verifies that
			// host format negotiation cannot silently send the wrong protocol payload.
			raw, _ := json.Marshal(map[string]any{"model": "opencode-go/" + id, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "max_tokens": 16, "stream": stream})
			req := coreexec.Request{Model: "opencode-go/" + id, Payload: raw, Format: tr.FormatOpenAI}
			opts := coreexec.Options{SourceFormat: tr.FormatOpenAI, ResponseFormat: tr.FormatOpenAI, Stream: stream, Headers: http.Header{"User-Agent": {"ExampleCodingClient/1.0"}}, Metadata: map[string]any{"canonical_session_id": "example-conversation"}}
			if stream {
				result, err := ex.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				for c := range result.Chunks {
					if c.Err != nil {
						t.Fatal(c.Err)
					}
					text.Write(c.Payload)
				}
				if !strings.Contains(text.String(), "hello") {
					t.Fatalf("stream %s", text.String())
				}
			} else {
				result, err := ex.Execute(ctx, auth, req, opts)
				if err != nil || !strings.Contains(string(result.Payload), "hello") {
					t.Fatalf("response %s %v", result.Payload, err)
				}
			}
			select {
			case record := <-records:
				if record.Failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 {
					t.Fatalf("zero/incorrect host usage despite cost=0: %+v", record)
				}
			case <-ctx.Done():
				t.Fatal("host usage not published")
			}
		}
	}
	mu.Lock()
	if len(seen) != 5 {
		t.Errorf("missing protocol routes: %+v", seen)
	}
	mu.Unlock()

	// No plugin scheduler: prove error envelopes drive the real host conductor's
	// sibling selection and that failures do not poison a different credential.
	sibling, handled, err := host.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "opencode-go", RawJSON: []byte(`{"type":"opencode-go","id":"example-b","api_key":"example-sibling-key"}`)})
	if err != nil || !handled || sibling == nil {
		t.Fatal("sibling parse failed")
	}
	for _, status := range []int32{429, 403} {
		failure.Store(status)
		manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		manager.SetRetryConfig(0, 0, 2)
		manager.RegisterExecutor(ex)
		for _, a := range []*coreauth.Auth{auth.Clone(), sibling.Clone()} {
			if _, err := manager.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, "opencode-go", []*registry.ModelInfo{{ID: "opencode-go/glm-5.3"}})
		}
		req := coreexec.Request{Model: "opencode-go/glm-5.3", Payload: []byte(`{"model":"opencode-go/glm-5.3","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`), Format: tr.FormatOpenAI}
		opts := coreexec.Options{SourceFormat: tr.FormatOpenAI, ResponseFormat: tr.FormatOpenAI, Headers: http.Header{"User-Agent": {"ExampleCodingClient/1.0"}}, Metadata: map[string]any{"canonical_session_id": "example-conversation"}}
		result, err := manager.Execute(ctx, []string{"opencode-go"}, req, opts)
		if err != nil || !strings.Contains(string(result.Payload), "hello") {
			t.Fatalf("native failover status=%d: %s %v", status, result.Payload, err)
		}
		first, _ := manager.GetByID(auth.ID)
		second, _ := manager.GetByID(sibling.ID)
		state := first.ModelStates["opencode-go/glm-5.3"]
		if state == nil || !state.Unavailable || second.Unavailable {
			t.Fatalf("cooldown not isolated: first=%+v second=%+v", first, second)
		}
		if status == 429 && time.Until(state.NextRetryAfter) < 14*time.Minute {
			t.Fatalf("Retry-After lost: %+v", state)
		}
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		registry.GetGlobalRegistry().UnregisterClient(sibling.ID)
	}
}

func fixture(format string, stream bool) string {
	switch format {
	case "openai":
		if stream {
			return "data: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
		}
		return `{"id":"chat-example","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"cost":0}`
	case "claude":
		if stream {
			return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-example\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return `{"id":"msg-example","type":"message","role":"assistant","model":"minimax-m3","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2},"cost":0}`
	case "gemini":
		raw := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-4-argon","cost":0}`
		if stream {
			return "data: " + raw + "\n\n"
		}
		return raw
	case "openai-response":
		raw := `{"id":"resp-example","object":"response","status":"completed","model":"gpt-6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"cost":0}`
		if stream {
			return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + raw + "}\n\n"
		}
		return raw
	}
	return ""
}
