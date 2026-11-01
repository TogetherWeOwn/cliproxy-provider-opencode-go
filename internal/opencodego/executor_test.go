package opencodego

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Only the network boundary is replaced. Each inference uses the real HTTP
// request builder, discovery parser, SDK translators and stream framing.
type testHTTP struct{ client *http.Client }

func (h testHTTP) Do(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	req.Header = r.Headers.Clone()
	resp, err := h.client.Do(req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}, err
}
func (h testHTTP) DoStream(ctx context.Context, r pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := h.Do(ctx, r)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	out := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(out)
		for len(resp.Body) > 0 {
			n := min(7, len(resp.Body))
			select {
			case out <- pluginapi.HTTPStreamChunk{Payload: bytes.Clone(resp.Body[:n])}:
			case <-ctx.Done():
				return
			}
			resp.Body = resp.Body[n:]
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: out}, nil
}

func wireResponse(format string, stream bool) string {
	switch format {
	case "openai":
		if stream {
			return "data: {\"id\":\"chat-example\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
		}
		return `{"id":"chat-example","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"cost":0}`
	case "claude":
		if stream {
			return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-example\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"minimax-m3\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return `{"id":"msg-example","type":"message","role":"assistant","model":"minimax-m3","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2},"cost":0}`
	case "gemini":
		raw := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-4-argon","cost":0}`
		if stream {
			return "data: " + raw + "\n\n"
		}
		return raw
	case "openai-response":
		raw := `{"id":"resp-example","object":"response","status":"completed","model":"gpt-6-luna","output":[{"id":"msg-example","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"cost":0}`
		if stream {
			return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + raw + "}\n\n"
		}
		return raw
	}
	return ""
}

func TestExecutorRoutesEveryProtocolAndTranslatesOpenAIClients(t *testing.T) {
	for _, tt := range []struct{ id, format, path string }{
		{"glm-5.3", "openai", "/inference/go/openai/v1/chat/completions"},
		{"gpt-6-luna", "openai-response", "/inference/go/openai/v1/responses"},
		{"minimax-m3", "claude", "/inference/go/anthropic/v1/messages"},
		{"gemini-4-argon", "gemini", "/inference/go/google/v1beta/models/gemini-4-argon:generateContent"},
	} {
		t.Run(tt.format, func(t *testing.T) {
			var inference atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/inference/go/v1/models" {
					w.WriteHeader(503)
					return
				}
				inference.Add(1)
				if r.Header.Get("Authorization") != "Bearer example-key" || r.Header.Get("User-Agent") != "ExampleCodingClient/1.0" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
					t.Error("incorrect credential/client headers")
				}
				if r.Header.Get("X-Opencode-Session") == "" {
					t.Error("session missing")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid body")
				}
				if tt.format != "gemini" && body["model"] != tt.id {
					t.Errorf("model leaked prefix: %v", body["model"])
				}
				stream := body["stream"] == true || strings.Contains(r.URL.Path, ":streamGenerateContent")
				path := tt.path
				if stream && tt.format == "gemini" {
					path = strings.Replace(path, ":generateContent", ":streamGenerateContent", 1)
					if r.URL.Query().Get("alt") != "sse" {
						t.Error("Gemini not SSE")
					}
				}
				if r.URL.Path != path {
					t.Errorf("path %s", r.URL.Path)
				}
				switch tt.format {
				case "claude":
					if body["messages"] == nil || r.Header.Get("Anthropic-Version") != "2023-06-01" {
						t.Error("not Messages format")
					}
				case "gemini":
					if body["messages"] != nil || body["max_tokens"] != nil {
						t.Error("Chat-only fields leaked into Gemini payload")
					}
					if body["contents"] == nil {
						t.Error("not Gemini format")
					}
				case "openai-response":
					if body["messages"] != nil || body["max_tokens"] != nil {
						t.Error("Chat-only fields leaked into Responses payload")
					}
					if body["max_output_tokens"] != float64(16) {
						t.Error("Responses token limit lost")
					}
					if body["input"] == nil {
						t.Error("not Responses format")
					}
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				io.WriteString(w, wireResponse(tt.format, stream))
			}))
			defer s.Close()
			ex := newExecutor(newCatalog(s.URL, nil))
			for _, stream := range []bool{false, true} {
				payload, _ := json.Marshal(map[string]any{"model": "opencode-go/" + tt.id, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "max_tokens": 16, "stream": stream})
				req := pluginapi.ExecutorRequest{Model: "opencode-go/" + tt.id, Format: "openai", SourceFormat: "openai", Payload: payload, Stream: stream, StorageJSON: []byte(`{"api_key":"example-key"}`), Headers: http.Header{"User-Agent": {"ExampleCodingClient/1.0"}, "Cookie": {"do-not-forward"}, "X-Api-Key": {"do-not-forward"}}, Metadata: map[string]any{"session_id": "example-conversation"}, HTTPClient: testHTTP{s.Client()}}
				if !stream {
					got, err := ex.Execute(context.Background(), req)
					if err != nil || !strings.Contains(string(got.Payload), "hello") || !strings.Contains(string(got.Payload), `"choices"`) || !strings.Contains(string(got.Payload), `"usage"`) {
						t.Fatalf("response %s %v", got.Payload, err)
					}
				} else {
					got, err := ex.ExecuteStream(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					var out strings.Builder
					for chunk := range got.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						out.Write(chunk.Payload)
					}
					if !strings.Contains(out.String(), "[DONE]") {
						t.Error("translated Chat stream missing terminal marker")
					}
					if !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "data:") || !strings.Contains(out.String(), `"choices"`) {
						t.Fatalf("stream %s", out.String())
					}
				}
			}
			if inference.Load() != 2 {
				t.Fatalf("inference count %d", inference.Load())
			}
		})
	}
}

func TestExecutorRejectsUnknownModelsAndArbitraryHTTPDestinations(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		w.WriteHeader(503)
	}))
	defer s.Close()
	ex := newExecutor(newCatalog(s.URL, nil))
	for _, id := range []string{"glm-5.3", "opencode-go/unknown", "opencode-go/../../example"} {
		_, err := ex.Execute(context.Background(), pluginapi.ExecutorRequest{Model: id, SourceFormat: "openai", Format: "openai", Payload: []byte(`{"messages":[]}`), StorageJSON: []byte(`{"api_key":"example"}`), HTTPClient: testHTTP{s.Client()}})
		if err == nil {
			t.Errorf("model accepted %s", id)
		}
	}
	_, err := ex.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{URL: "https://example.invalid/steal", Method: "GET", StorageJSON: []byte(`{"api_key":"example"}`), HTTPClient: testHTTP{s.Client()}})
	if err == nil || posts.Load() != 0 {
		t.Fatal("unsafe HTTP request or wrong model sent")
	}
}

func TestSessionIdentityStableAndConversationScoped(t *testing.T) {
	a := sessionHeader(pluginapi.ExecutorRequest{AuthID: "a", Model: "one", Metadata: map[string]any{"session_id": "conversation-a"}})
	b := sessionHeader(pluginapi.ExecutorRequest{AuthID: "b", Model: "two", Metadata: map[string]any{"session_id": "conversation-a"}})
	c := sessionHeader(pluginapi.ExecutorRequest{Metadata: map[string]any{"session_id": "conversation-b"}})
	if a == "" || a != b || a == c || strings.Contains(a, "conversation-a") {
		t.Fatal("session not stable, scoped, or redacted")
	}
}

func TestStreamErrorsUseSameNativeClassification(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"GoUsageLimitError"},"metadata":{"limitName":"weekly"}}`)
	}))
	defer s.Close()
	ex := newExecutor(newCatalog(s.URL, nil))
	req := pluginapi.ExecutorRequest{Model: "opencode-go/glm-5.3", Format: "openai", SourceFormat: "openai", Stream: true, Payload: []byte(`{"messages":[],"stream":true}`), StorageJSON: []byte(`{"api_key":"example"}`), HTTPClient: testHTTP{s.Client()}}
	_, err := ex.ExecuteStream(context.Background(), req)
	var pe *pluginabi.Error
	if !errors.As(err, &pe) || pe.Code != "quota_exhausted_weekly" || pe.RetryAfterSeconds != 900 {
		t.Fatalf("classification %v", err)
	}
}

func TestCanonicalHostSessionPrecedesLegacyMetadata(t *testing.T) {
	a := sessionHeader(pluginapi.ExecutorRequest{Metadata: map[string]any{"canonical_session_id": "canonical-example", "session_id": "old-example"}})
	b := sessionHeader(pluginapi.ExecutorRequest{Metadata: map[string]any{"canonical_session_id": "canonical-example", "session_id": "different-old-example"}})
	if a == "" || a != b {
		t.Fatal("host canonical conversation identity ignored")
	}
}

func TestEveryAdvertisedClientFormatAcrossModelFamilies(t *testing.T) {
	for _, source := range []string{"openai", "openai-response", "claude", "gemini"} {
		for _, target := range []struct{ id, format string }{{"glm-5.3", "openai"}, {"gpt-6-luna", "openai-response"}, {"minimax-m3", "claude"}, {"gemini-4-argon", "gemini"}} {
			t.Run(source+"_to_"+target.format, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						w.WriteHeader(503)
						return
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					switch target.format {
					case "openai":
						if body["max_tokens"] != float64(16) && body["max_completion_tokens"] != float64(16) {
							t.Error("Chat budget lost")
						}
					case "openai-response":
						if body["max_output_tokens"] != float64(16) {
							t.Error("Responses budget lost")
						}
					case "claude":
						if body["max_tokens"] != float64(16) {
							t.Error("Messages budget lost")
						}
					case "gemini":
						generation, _ := body["generationConfig"].(map[string]any)
						if generation["maxOutputTokens"] != float64(16) {
							t.Error("Gemini budget lost")
						}
					}
					stream := body["stream"] == true || strings.Contains(r.URL.Path, ":streamGenerateContent")
					io.WriteString(w, wireResponse(target.format, stream))
				}))
				defer s.Close()
				ex := newExecutor(newCatalog(s.URL, nil))
				for _, stream := range []bool{false, true} {
					req := clientRequest(source, target.id, stream)
					req.HTTPClient = testHTTP{s.Client()}
					if !stream {
						got, err := ex.Execute(context.Background(), req)
						if err != nil || !strings.Contains(string(got.Payload), "hello") || !json.Valid(got.Payload) {
							t.Fatalf("nonstream %s %v", got.Payload, err)
						}
					} else {
						got, err := ex.ExecuteStream(context.Background(), req)
						if err != nil {
							t.Fatal(err)
						}
						var text strings.Builder
						for c := range got.Chunks {
							if c.Err != nil {
								t.Fatal(c.Err)
							}
							text.Write(c.Payload)
						}
						if !strings.Contains(text.String(), "hello") {
							t.Fatalf("stream lost content: %s", text.String())
						}
					}
				}
			})
		}
	}
}

func clientRequest(format, id string, stream bool) pluginapi.ExecutorRequest {
	model := "opencode-go/" + id
	body := map[string]any{"model": model, "stream": stream}
	switch format {
	case "openai":
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		body["max_tokens"] = 16
	case "openai-response":
		body["input"] = "hello"
		body["max_output_tokens"] = 16
	case "claude":
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		body["max_tokens"] = 16
	case "gemini":
		body["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}
		body["generationConfig"] = map[string]any{"maxOutputTokens": 16}
	}
	raw, _ := json.Marshal(body)
	return pluginapi.ExecutorRequest{Model: model, SourceFormat: format, Format: format, Stream: stream, Payload: raw, StorageJSON: []byte(`{"api_key":"example-key"}`)}
}

// Regression (Devin Review): in framed mode the kit delivers one
// complete JSON event per chunk with no line terminators, so the
// same-protocol relay must re-frame each chunk into a complete SSE event.
// Table covers both kit chunk shapes: "data:"-framed (no request_path
// metadata) and bare JSON (chat route metadata).
func TestSameProtocolRelayEmitsParseableSSE(t *testing.T) {
	upstream := "data: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-example\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for name, metadata := range map[string]map[string]any{
		"framed": {},
		"bare":   {"request_path": "/v1/chat/completions"},
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, upstream)
			}))
			defer s.Close()
			ex := newExecutor(newCatalog(s.URL, nil))
			req := clientRequest("openai", "glm-5.3", true)
			req.HTTPClient = testHTTP{s.Client()}
			req.Metadata = metadata
			resp, err := ex.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			for c := range resp.Chunks {
				if c.Err != nil {
					t.Fatal(c.Err)
				}
				out.Write(c.Payload)
			}
			raw := out.String()
			var events []string
			for _, block := range strings.Split(raw, "\n\n") {
				if block = strings.TrimSpace(block); block != "" {
					events = append(events, block)
				}
			}
			if len(events) != 3 {
				t.Fatalf("want 3 SSE events, got %d: %q", len(events), raw)
			}
			for _, ev := range events {
				if !strings.HasPrefix(ev, "data:") {
					t.Fatalf("event is not a complete SSE event: %q", ev)
				}
			}
			if !strings.Contains(events[0], `"content":"hello"`) || events[2] != "data: [DONE]" {
				t.Fatalf("content/terminal events wrong: %q", raw)
			}
		})
	}
}

// Regression (Devin Review): a same-protocol close before every
// choice finishes is a truncation (stream_truncated, no [DONE]); a close
// after complete choices keeps the terminal marker, mirroring the
// translated path.
func TestSameProtocolRelayRejectsTruncation(t *testing.T) {
	complete := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	cut := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	for _, tc := range []struct {
		name     string
		upstream string
		wantErr  bool
	}{
		{"cut mid-choice", cut, true},
		{"cut after finish", complete, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.upstream)
			}))
			defer s.Close()
			ex := newExecutor(newCatalog(s.URL, nil))
			req := clientRequest("openai", "glm-5.3", true)
			req.HTTPClient = testHTTP{s.Client()}
			resp, err := ex.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			failed := false
			for c := range resp.Chunks {
				if c.Err != nil {
					failed = true
					continue
				}
				out.Write(c.Payload)
			}
			if failed != tc.wantErr {
				t.Fatalf("truncation error = %v, want %v (output %q)", failed, tc.wantErr, out.String())
			}
			if strings.Contains(out.String(), "[DONE]") == tc.wantErr {
				t.Fatalf("terminal marker wrong for truncated=%v: %q", tc.wantErr, out.String())
			}
		})
	}
}

func TestTranslatedStreamRejectsTruncationAndOversizedEvents(t *testing.T) {
	for _, raw := range []string{
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"data: " + strings.Repeat("x", maxUsageResponse+1),
	} {
		t.Run(raw[:10], func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				io.WriteString(w, raw)
			}))
			defer s.Close()
			ex := newExecutor(newCatalog(s.URL, nil))
			req := clientRequest("openai", "minimax-m3", true)
			req.HTTPClient = testHTTP{s.Client()}
			resp, err := ex.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			for c := range resp.Chunks {
				if c.Err != nil {
					failed = true
				}
				if strings.Contains(string(c.Payload), "[DONE]") {
					t.Fatal("truncated stream marked complete")
				}
			}
			if !failed {
				t.Fatal("incomplete or oversized stream accepted")
			}
		})
	}
}

func TestCountTokensIsLocalEstimate(t *testing.T) {
	ex := newExecutor(newCatalog("http://127.0.0.1:0", nil))
	resp, err := ex.CountTokens(context.Background(), pluginapi.ExecutorRequest{Payload: []byte("abcdefgh")})
	if err != nil || string(resp.Payload) != `{"total_tokens":2}` || resp.Metadata["estimated"] != true {
		t.Fatalf("count %s %+v %v", resp.Payload, resp.Metadata, err)
	}
}

func TestNonStreamRejectsMalformedUpstreamResponses(t *testing.T) {
	for _, id := range []string{"glm-5.3", "gpt-6-luna", "minimax-m3", "gemini-4-argon"} {
		t.Run(id, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				io.WriteString(w, `not-json`)
			}))
			defer s.Close()
			ex := newExecutor(newCatalog(s.URL, nil))
			req := clientRequest("openai", id, false)
			req.HTTPClient = testHTTP{s.Client()}
			if _, err := ex.Execute(context.Background(), req); err == nil {
				t.Fatal("malformed response returned as successful inference")
			}
		})
	}
}

func TestExecutorHTTPRebuildsDestinationAndCredentials(t *testing.T) {
	var called atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(503)
			return
		}
		called.Add(1)
		if r.URL.Path != "/inference/go/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer example-key" || r.Header.Get("X-Api-Key") != "" {
			t.Error("unsafe HTTP request")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "glm-5.3" {
			t.Error("HTTP model prefix leaked")
		}
		io.WriteString(w, wireResponse("openai", false))
	}))
	defer s.Close()
	ex := newExecutor(newCatalog(s.URL, nil))
	got, err := ex.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{URL: "https://example.invalid/untrusted", Method: "POST", Headers: http.Header{"Authorization": {"untrusted"}, "X-Api-Key": {"untrusted"}}, Body: []byte(`{"model":"opencode-go/glm-5.3","messages":[{"role":"user","content":"hello"}]}`), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: testHTTP{s.Client()}})
	if err != nil || called.Load() != 1 || !strings.Contains(string(got.Body), "hello") {
		t.Fatalf("HTTP execute %+v %v", got, err)
	}
}

func TestResponsesBridgePreservesExplicitThinkingWithoutInventingDefaults(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer s.Close()
	ex := newExecutor(newCatalog(s.URL, nil))
	cases := []struct{ format, raw, want string }{
		{"claude", `{"messages":[{"role":"user","content":"hello"}],"max_tokens":16,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, "high"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":16,"thinkingConfig":{"thinkingLevel":"high"}}}`, "high"},
		{"openai", `{"messages":[{"role":"user","content":"hello"}],"max_tokens":16}`, ""},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			p, err := ex.prepare(context.Background(), pluginapi.ExecutorRequest{Model: "opencode-go/gpt-6-luna", Format: tt.format, SourceFormat: tt.format, Payload: []byte(tt.raw), StorageJSON: []byte(`{"api_key":"example-key"}`), HTTPClient: testHTTP{s.Client()}})
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if json.Unmarshal(p.http.Body, &body) != nil || body.Reasoning.Effort != tt.want {
				t.Fatalf("thinking changed %s", p.http.Body)
			}
		})
	}
}
