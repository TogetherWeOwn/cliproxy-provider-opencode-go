package opencodego

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

type executor struct{ catalog *catalog }

func newExecutor(c *catalog) *executor { return &executor{catalog: c} }
func (*executor) Identifier() string   { return provider }
func invalidRequest(code, message string) *pluginabi.Error {
	return &pluginabi.Error{Code: code, Message: message, HTTPStatus: 400}
}

type preparedRequest struct {
	http                 pluginapi.HTTPRequest
	native, output       tr.Format
	model                string
	original, translated []byte
}

func (e *executor) prepare(ctx context.Context, r pluginapi.ExecutorRequest) (preparedRequest, error) {
	if r.HTTPClient == nil {
		return preparedRequest{}, &pluginabi.Error{Code: "host_unavailable", Message: "host HTTP client is required", HTTPStatus: 503}
	}
	if !strings.HasPrefix(r.Model, modelPrefix) {
		return preparedRequest{}, invalidRequest("invalid_model", "expected opencode-go/ model")
	}
	model := strings.TrimPrefix(r.Model, modelPrefix)
	if !modelIDPattern.MatchString(model) {
		return preparedRequest{}, invalidRequest("invalid_model", "invalid model identifier")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.Payload, &body) != nil || body == nil {
		return preparedRequest{}, invalidRequest("invalid_request", "payload must be a JSON object")
	}
	key, err := kit.APIKey(r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return preparedRequest{}, err
	}
	format, err := e.catalog.resolve(ctx, model, r.StorageJSON, r.AuthMetadata)
	if err != nil {
		return preparedRequest{}, err
	}
	native := tr.Format(format)
	input := tr.Format(r.SourceFormat)
	if input == "" {
		input = tr.Format(r.Format)
	}
	output := tr.Format(r.Format)
	if output == "" {
		output = input
	}
	if input == "" || output == "" {
		return preparedRequest{}, invalidRequest("unsupported_format", "explicit protocol format is required")
	}
	requestTarget := native
	if native == tr.FormatOpenAIResponse && input != native {
		requestTarget = tr.FormatCodex
	}
	if input != native && !tr.HasRequestTransformer(input, requestTarget) {
		return preparedRequest{}, invalidRequest("unsupported_format", "no request translator for selected model")
	}
	responseNative := native
	if native == tr.FormatOpenAIResponse && output != native {
		responseNative = tr.FormatCodex
	}
	if output != native {
		supported := tr.HasNonStreamResponseTransformer(output, responseNative)
		if r.Stream {
			supported = tr.HasStreamResponseTransformer(output, responseNative)
		}
		if !supported {
			return preparedRequest{}, invalidRequest("unsupported_format", "no response translator for selected model")
		}
	}
	translated := tr.RequestEnvelope{Body: r.Payload}
	if input != native {
		translated = tr.Default().TranslateRequestEnvelope(ctx, input, requestTarget, tr.RequestEnvelope{Format: input, Model: model, Stream: r.Stream, Body: r.Payload})
	}
	if translated.Err != nil {
		return preparedRequest{}, invalidRequest("invalid_request", "request protocol translation failed")
	}
	body = nil // Unmarshal into a fresh map; absent source fields must not survive.
	if json.Unmarshal(translated.Body, &body) != nil || body == nil {
		return preparedRequest{}, invalidRequest("invalid_request", "translated payload must be a JSON object")
	}
	// The fixed prefix belongs to host routing, never to the upstream request.
	body["model"], _ = json.Marshal(model)
	path := ""
	switch format {
	case "openai":
		path = "/inference/go/openai/v1/chat/completions"
		body["stream"], _ = json.Marshal(r.Stream)
		if r.Stream {
			var opts map[string]json.RawMessage
			_ = json.Unmarshal(body["stream_options"], &opts)
			if opts == nil {
				opts = map[string]json.RawMessage{}
			}
			opts["include_usage"] = json.RawMessage("true")
			body["stream_options"], _ = json.Marshal(opts)
		}
	case "openai-response":
		path = "/inference/go/openai/v1/responses"
		body["stream"], _ = json.Marshal(r.Stream)
		if input != native {
			restoreResponsesControls(body, r.Payload, input)
		}
	case "claude":
		path = "/inference/go/anthropic/v1/messages"
		body["stream"], _ = json.Marshal(r.Stream)
	case "gemini":
		// The pinned Messages -> Gemini request converter omits max_tokens.
		// Preserve the caller's cap instead of silently requesting an uncapped
		// generation from a general Gemini endpoint.
		if input == tr.FormatClaude {
			var original map[string]json.RawMessage
			_ = json.Unmarshal(r.Payload, &original)
			if limit, ok := original["max_tokens"]; ok {
				var generation map[string]json.RawMessage
				_ = json.Unmarshal(body["generationConfig"], &generation)
				if generation == nil {
					generation = map[string]json.RawMessage{}
				}
				generation["maxOutputTokens"] = limit
				body["generationConfig"], _ = json.Marshal(generation)
			}
		}
		path = "/inference/go/google/v1beta/models/" + model + ":generateContent"
		if r.Stream {
			path = "/inference/go/google/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
		}
		delete(body, "model")
		delete(body, "stream")
	default:
		return preparedRequest{}, invalidRequest("unsupported_format", "unsupported upstream protocol")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return preparedRequest{}, err
	}
	headers := http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}, "User-Agent": {"cliproxy-provider-opencode-go/0.1.2"}}
	if ua := r.Headers.Get("User-Agent"); ua != "" {
		headers.Set("User-Agent", ua)
	}
	if session := sessionHeader(r); session != "" {
		headers.Set("X-Opencode-Session", session)
	}
	if r.Stream {
		headers.Set("Accept", "text/event-stream")
	}
	if native == tr.FormatClaude {
		headers.Set("Anthropic-Version", "2023-06-01")
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if value := r.Headers.Get(name); value != "" {
				headers.Set(name, value)
			}
		}
	}
	original := r.Payload
	if input != output {
		target := output
		if output == tr.FormatOpenAIResponse {
			target = tr.FormatCodex
		}
		if !tr.HasRequestTransformer(input, target) {
			return preparedRequest{}, invalidRequest("unsupported_format", "no request translator for response context")
		}
		original = tr.TranslateRequest(input, target, model, r.Payload, r.Stream)
	}
	return preparedRequest{http: pluginapi.HTTPRequest{Method: http.MethodPost, URL: e.catalog.base() + path, Headers: headers, Body: payload}, native: responseNative, output: output, model: r.Model, original: original, translated: payload}, nil
}
func passthrough(p preparedRequest) kit.Passthrough {
	return kit.Passthrough{Provider: provider, BuildRequest: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) { return p.http, nil }, ClassifyError: classifyResponse}
}
func (e *executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	r.Stream = false
	p, err := e.prepare(ctx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	resp, err := passthrough(p).Execute(ctx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if !validResponseObject(resp.Payload) {
		return pluginapi.ExecutorResponse{}, &pluginabi.Error{Code: "invalid_response", Message: "upstream response must be a JSON object", HTTPStatus: 502}
	}
	if p.native == tr.FormatCodex {
		var object struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(resp.Payload, &object) != nil {
			return pluginapi.ExecutorResponse{}, &pluginabi.Error{Code: "invalid_response", Message: "invalid Responses payload", HTTPStatus: 502}
		}
		kind := "response.completed"
		if object.Status == "incomplete" {
			kind = "response.incomplete"
		}
		resp.Payload, _ = json.Marshal(map[string]any{"type": kind, "response": json.RawMessage(resp.Payload)})
	}
	// The pinned SDK's Claude -> Gemini non-stream converter consumes an SSE
	// transcript rather than a Messages JSON object. Bridge JSON via its tested
	// Claude -> Chat and Chat -> Gemini converters instead of patching the SDK.
	if p.native == tr.FormatClaude && p.output == tr.FormatGemini {
		chatRequest := tr.TranslateRequest(tr.FormatGemini, tr.FormatOpenAI, p.model, p.original, false)
		var param any
		resp.Payload = tr.TranslateNonStream(ctx, tr.FormatClaude, tr.FormatOpenAI, p.model, chatRequest, p.translated, resp.Payload, &param)
		p.native = tr.FormatOpenAI
		p.translated = chatRequest
	}
	if p.native != p.output {
		var param any
		resp.Payload = tr.TranslateNonStream(ctx, p.native, p.output, p.model, p.original, p.translated, resp.Payload, &param)
		resp.Headers = translatedHeaders(resp.Headers, false)
	}
	if !validResponseObject(resp.Payload) {
		return pluginapi.ExecutorResponse{}, &pluginabi.Error{Code: "invalid_response", Message: "response protocol translation failed", HTTPStatus: 502}
	}
	return resp, nil
}
func validResponseObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}
func translatedHeaders(h http.Header, stream bool) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, name := range []string{"Content-Length", "Content-Encoding", "ETag"} {
		out.Del(name)
	}
	ct := "application/json"
	if stream {
		ct = "text/event-stream"
	}
	out.Set("Content-Type", ct)
	return out
}
func (e *executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	p, err := e.prepare(ctx, r)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := passthrough(p).ExecuteStream(streamCtx, r)
	if err != nil {
		cancel()
		return pluginapi.ExecutorStreamResponse{}, err
	}
	// The kit re-frames SSE responses to one event per chunk (and drops the
	// upstream [DONE] marker) whenever the requested output format is OpenAI.
	// Framed chunks carry no line terminators, so they must be consumed as
	// whole events, not re-split on byte boundaries.
	framed := p.output == tr.FormatOpenAI && sseStream(resp.Headers)
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		defer cancel()
		send := func(payload []byte, err error) bool {
			select {
			case out <- pluginapi.ExecutorStreamChunk{Payload: payload, Err: err}:
				return true
			case <-streamCtx.Done():
				return false
			}
		}
		if p.native == p.output {
			for {
				select {
				case <-streamCtx.Done():
					return
				case chunk, ok := <-resp.Chunks:
					if !ok {
						// The kit drops the upstream [DONE] marker when it
						// re-frames; re-emit it so same-protocol OpenAI
						// streams keep their terminal marker.
						if framed {
							send([]byte("data: [DONE]\n\n"), nil)
						}
						return
					}
					if !send(chunk.Payload, chunk.Err) || chunk.Err != nil {
						return
					}
				}
			}
		}
		// Host HTTP chunks have arbitrary byte boundaries. Feed complete SSE data
		// events into the SDK, maintaining one translator state per request.
		// In framed mode each kit chunk is already one complete event.
		var pending, data []byte
		terminal, doneSent := false, false
		var param any
		var emit func() bool
		framedEvent := func(payload []byte) bool {
			event := bytes.TrimSpace(payload)
			if len(event) == 0 || event[0] == ':' {
				return true // keep-alive comment
			}
			if rest, ok := bytes.CutPrefix(event, []byte("data:")); ok {
				event = bytes.TrimPrefix(rest, []byte(" "))
			}
			if len(event) > maxUsageResponse {
				return false
			}
			data = event
			return emit()
		}
		emit = func() bool {
			if len(data) == 0 {
				return true
			}
			terminal = terminal || terminalData(p.native, data)
			frames := tr.TranslateStream(streamCtx, p.native, p.output, p.model, p.original, p.translated, append([]byte("data: "), data...), &param)
			data = nil
			for _, frame := range frames {
				if len(bytes.TrimSpace(frame)) == 0 {
					continue
				}
				if bytes.Contains(frame, []byte("[DONE]")) {
					doneSent = true
				}
				if !send(sseFrame(frame), nil) {
					return false
				}
			}
			return true
		}
		line := func(raw []byte) bool {
			raw = bytes.TrimSuffix(raw, []byte("\r"))
			if len(raw) == 0 {
				return emit()
			}
			if bytes.HasPrefix(raw, []byte("data:")) {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimPrefix(raw[5:], []byte(" "))...)
			}
			return len(data) <= maxUsageResponse
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case chunk, ok := <-resp.Chunks:
				if !ok {
					if len(pending) > 0 && !line(pending) {
						send(nil, invalidStream())
						return
					}
					if !emit() {
						return
					}
					if !terminal {
						send(nil, &pluginabi.Error{Code: "stream_truncated", Message: "upstream stream ended before its terminal event", HTTPStatus: 502, Retryable: true})
						return
					}
					if p.output == tr.FormatOpenAI && !doneSent {
						send([]byte("data: [DONE]\n\n"), nil)
					}
					return
				}
				if chunk.Err != nil {
					send(nil, chunk.Err)
					return
				}
				if framed {
					if !framedEvent(chunk.Payload) {
						send(nil, invalidStream())
						return
					}
					continue
				}
				pending = append(pending, chunk.Payload...)
				for {
					idx := bytes.IndexByte(pending, '\n')
					if idx < 0 {
						break
					}
					if !line(pending[:idx]) {
						send(nil, invalidStream())
						return
					}
					pending = pending[idx+1:]
				}
				if len(pending) > maxUsageResponse {
					send(nil, invalidStream())
					return
				}
			}
		}
	}()
	return pluginapi.ExecutorStreamResponse{Headers: translatedHeaders(resp.Headers, true), Chunks: out}, nil
}
func invalidStream() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_stream", Message: "upstream SSE event exceeds maximum size", HTTPStatus: 502}
}
func sseStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}
func (*executor) CountTokens(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return (kit.Passthrough{}).CountTokens(ctx, r)
}
func (e *executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if r.Method != "POST" {
		return pluginapi.ExecutorHTTPResponse{}, invalidRequest("invalid_request", "only model inference POST requests are supported")
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(r.Body, &body) != nil || body.Stream {
		return pluginapi.ExecutorHTTPResponse{}, invalidRequest("invalid_request", "use ExecuteStream for streaming inference")
	}
	// Never send credentials to a caller-supplied URL. Rebuild the destination
	// from the model catalog, just as Execute does.
	format, err := e.catalog.resolve(ctx, strings.TrimPrefix(body.Model, modelPrefix), r.StorageJSON, r.Metadata)
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	req := pluginapi.ExecutorRequest{AuthID: r.AuthID, Model: body.Model, SourceFormat: format, Format: format, Payload: r.Body, Headers: r.Headers, StorageJSON: r.StorageJSON, AuthMetadata: r.Metadata, HTTPClient: r.HTTPClient}
	p, err := e.prepare(ctx, req)
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	return passthrough(p).HttpRequest(ctx, pluginapi.ExecutorHTTPRequest{Method: p.http.Method, URL: p.http.URL, Headers: p.http.Headers, Body: p.http.Body, HTTPClient: r.HTTPClient})
}
func sessionHeader(r pluginapi.ExecutorRequest) string {
	session := ""
	for _, key := range []string{"canonical_session_id", "execution_session_id", "derived_session_id", "session_id", "sessionId", "sessionID", "conversation_id"} {
		if v, ok := r.Metadata[key].(string); ok && v != "" {
			session = v
			break
		}
	}
	if session == "" {
		for _, name := range []string{"X-Opencode-Session", "X-Session-ID", "Session-Id", "X-Claude-Code-Session-Id"} {
			if value := r.Headers.Get(name); value != "" {
				session = value
				break
			}
		}
	}
	if session == "" {
		return ""
	} // Do not invent one global conversation per account.
	digest := sha256.Sum256([]byte("opencode-go/session/" + session))
	return hex.EncodeToString(digest[:16])
}

var _ pluginapi.ProviderExecutor = (*executor)(nil)

// SDK translators return either complete SSE events or JSON data. Normalize
// only translated output; same-protocol upstream bytes are preserved verbatim.
func sseFrame(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if bytes.HasPrefix(raw, []byte("data:")) || bytes.HasPrefix(raw, []byte("event:")) {
		return append(bytes.Clone(raw), '\n', '\n')
	}
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

// v8.0.49 exposes Chat/Messages/Gemini -> Responses wire conversion under the
// SDK's "codex" format, not "openai-response". We reuse that schema conversion
// only: never a Codex endpoint/auth flow, and never its token-limit stripping.
func restoreResponsesControls(out map[string]json.RawMessage, raw []byte, source tr.Format) {
	var in map[string]json.RawMessage
	_ = json.Unmarshal(raw, &in)
	for _, name := range []string{"temperature", "top_p", "max_output_tokens", "user", "service_tier", "parallel_tool_calls", "include", "reasoning"} {
		if value, ok := in[name]; ok {
			out[name] = value
		}
	}
	if source == tr.FormatOpenAI || source == tr.FormatClaude {
		if v, ok := in["max_tokens"]; ok {
			out["max_output_tokens"] = v
		}
		if v, ok := in["max_completion_tokens"]; ok {
			out["max_output_tokens"] = v
		}
	}
	if source == tr.FormatGemini {
		var generation map[string]json.RawMessage
		_ = json.Unmarshal(in["generationConfig"], &generation)
		for a, b := range map[string]string{"maxOutputTokens": "max_output_tokens", "temperature": "temperature", "topP": "top_p"} {
			if v, ok := generation[a]; ok {
				out[b] = v
			}
		}
	}
	// Do not introduce Codex-only reasoning/cache/tool defaults into a general
	// Responses provider when the caller did not ask for them.
	explicitThinking := in["reasoning"] != nil || in["reasoning_effort"] != nil || in["thinking"] != nil
	if source == tr.FormatClaude {
		var config map[string]json.RawMessage
		_ = json.Unmarshal(in["output_config"], &config)
		explicitThinking = explicitThinking || config["effort"] != nil
	}
	if source == tr.FormatGemini {
		var config map[string]json.RawMessage
		_ = json.Unmarshal(in["generationConfig"], &config)
		explicitThinking = explicitThinking || config["thinkingConfig"] != nil || config["thinkingLevel"] != nil || config["thinking_level"] != nil
	}
	if !explicitThinking {
		delete(out, "reasoning")
	}
	if in["include"] == nil {
		delete(out, "include")
	}
	if in["parallel_tool_calls"] == nil {
		delete(out, "parallel_tool_calls")
	}
}

func terminalData(format tr.Format, data []byte) bool {
	if format == tr.FormatOpenAI {
		return bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
	}
	var event struct {
		Type       string `json:"type"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	switch format {
	case tr.FormatClaude:
		return event.Type == "message_stop"
	case tr.FormatCodex, tr.FormatOpenAIResponse:
		return event.Type == "response.completed" || event.Type == "response.incomplete"
	case tr.FormatGemini:
		for _, c := range event.Candidates {
			if c.FinishReason != "" {
				return true
			}
		}
	}
	return false
}
