package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// These streams use the same events as the shadow route fixtures.
const correctionMessageSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\"}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"中간 상황\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const correctionChatSSE = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中간 상황\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"
const correctionResponsesSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"中간 상황\"}\n\n" +
	"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":\"中간 상황\"}\n\n" +
	"event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"中간 상황\"}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"中간 상황\"}]}}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"中간 상황\"}]}]}}\n\n"

func TestCJKeviCorrectionSSEReplaysCompletedText(t *testing.T) {
	for _, test := range []struct {
		name, surface, stream string
	}{
		{"chat", cjkevi.SurfaceChat, correctionChatSSE},
		{"chat CRLF", cjkevi.SurfaceChat, strings.ReplaceAll(correctionChatSSE, "\n", "\r\n")},
		{"messages", cjkevi.SurfaceAnthropic, correctionMessageSSE},
		{"responses", cjkevi.SurfaceResponses, correctionResponsesSSE},
		{"chat split", cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"content":"中간 상황"`, `"content":"中간 "`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := test.stream
			if test.name == "chat split" {
				stream = strings.Replace(stream, "data: [DONE]", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"상황\"},\"finish_reason\":null}]}\n\ndata: [DONE]", 1)
				// Move the finish event after the second content event.
				stop := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
				stream = strings.Replace(stream, stop, "", 1)
				stream = strings.Replace(stream, "data: [DONE]", stop+"data: [DONE]", 1)
			}
			original, patch, ok := correctionSSEText(test.surface, []byte(stream))
			if !ok || original != correctionOriginal {
				t.Fatalf("expected one complete text, got %q valid=%v", original, ok)
			}
			changed, err := patch(correctionRewritten)
			if err != nil || bytes.Equal(changed, []byte(stream)) {
				t.Fatalf("stream was not patched: %v", err)
			}
			reExtracted, _, verified := correctionSSEText(test.surface, changed)
			if !verified || reExtracted != correctionRewritten || !bytes.Contains(changed, []byte("message_stop")) && test.surface == cjkevi.SurfaceAnthropic || !bytes.Contains(changed, []byte("[DONE]")) && test.surface == cjkevi.SurfaceChat || !bytes.Contains(changed, []byte("response.completed")) && test.surface == cjkevi.SurfaceResponses {
				t.Fatalf("rewritten stream not complete: %s", changed)
			}
			if test.name == "chat split" && !bytes.Contains(changed, []byte(`"content":""`)) {
				t.Fatalf("later content delta was not cleared: %s", changed)
			}
		})
	}
}

func TestCJKeviCorrectionSSEThinkingAndResponsesDuplicates(t *testing.T) {
	thinking := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"private\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	message := strings.ReplaceAll(correctionMessageSSE, `"index":0`, `"index":1`)
	message = strings.Replace(message, "event: content_block_start", thinking+"event: content_block_start", 1)
	_, patch, ok := correctionSSEText(cjkevi.SurfaceAnthropic, []byte(message))
	if !ok {
		t.Fatal("text plus thinking stream rejected")
	}
	changed, err := patch(correctionRewritten)
	if err != nil || !bytes.Contains(changed, []byte(thinking)) {
		t.Fatalf("thinking was changed: %v %s", err, changed)
	}
	stream := correctionResponsesSSE
	stream = strings.Replace(stream, `"status":"completed","output"`, `"status":"completed","output_text":"中간 상황","output"`, 1)
	_, patch, ok = correctionSSEText(cjkevi.SurfaceResponses, []byte(stream))
	if !ok {
		t.Fatal("completed Responses stream with redundant fields rejected")
	}
	changed, err = patch(correctionRewritten)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(changed), correctionOriginal) || strings.Count(string(changed), correctionRewritten) != 6 {
		t.Fatalf("Responses duplicate text fields not synchronized: %s", changed)
	}
}

func TestCJKeviCorrectionSSERoutesAndDisabledMode(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	cfg.BufferStreams = true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Origin", "unchanged")
		var stream string
		switch r.URL.Path {
		case "/v1/messages":
			stream = correctionMessageSSE
		case "/v1/chat/completions":
			stream = correctionChatSSE
		case "/v1/responses":
			stream = correctionResponsesSSE
		}
		_, _ = io.WriteString(w, stream[:min(11, len(stream))])
		_, _ = io.WriteString(w, stream[min(11, len(stream)):])
	}))
	defer upstream.Close()
	server := newShadowRouteServer(t, upstream.URL, false, "")
	send := func(path string) *httptest.ResponseRecorder {
		payload := `{"model":"cjkevi-test-model","stream":true,"messages":[{"role":"user","content":"Hello."}],"max_tokens":32}`
		if path == "/v1/responses" {
			payload = `{"model":"cjkevi-test-model","stream":true,"input":"Hello."}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		server.engine.ServeHTTP(result, req)
		return result
	}
	for _, item := range []struct{ path, surface string }{
		{"/v1/messages", cjkevi.SurfaceAnthropic},
		{"/v1/chat/completions", cjkevi.SurfaceChat},
		{"/v1/responses", cjkevi.SurfaceResponses},
	} {
		t.Run(item.path, func(t *testing.T) {
			next := *server.getConfig()
			next.CJKeviCorrection.Enabled = false
			if !server.UpdateClientsContext(context.Background(), &next) {
				t.Fatal("failed to disable correction for baseline")
			}
			baseline := send(item.path)
			next = *server.getConfig()
			next.CJKeviCorrection = cfg
			next.CJKeviCorrection.BufferStreams = false
			if !server.UpdateClientsContext(context.Background(), &next) {
				t.Fatal("failed to enable correction without buffering")
			}
			unbuffered := send(item.path)
			if baseline.Code != unbuffered.Code || !bytes.Equal(baseline.Body.Bytes(), unbuffered.Body.Bytes()) || baseline.Flushed != unbuffered.Flushed {
				t.Fatalf("opt-out changed stream: %q vs %q", baseline.Body.String(), unbuffered.Body.String())
			}
			next.CJKeviCorrection.BufferStreams = true
			if !server.UpdateClientsContext(context.Background(), &next) {
				t.Fatal("failed to enable stream buffering")
			}
			changed := send(item.path)
			text, _, ok := correctionSSEText(item.surface, changed.Body.Bytes())
			if !ok || text != correctionRewritten || changed.Code != baseline.Code || changed.Header().Get("Content-Type") != baseline.Header().Get("Content-Type") || !changed.Flushed {
				t.Fatalf("route failed to correct complete stream: status=%d header=%v body=%q", changed.Code, changed.Header(), changed.Body.String())
			}
		})
	}
	if jevCalls.Load() != 6 || llmCalls.Load() != 3 {
		t.Fatalf("unexpected provider requests: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
}

func TestCJKeviCorrectionSSEFallbackAndDelayedFlush(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	cfg.BufferStreams = true
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	responseFrames := strings.Split(strings.TrimSuffix(correctionResponsesSSE, "\n\n"), "\n\n")
	reorderedDone := append([]string(nil), responseFrames...)
	reorderedDone[5], reorderedDone[6] = reorderedDone[6], reorderedDone[5]
	reorderedStart := append([]string(nil), responseFrames...)
	reorderedStart[2], reorderedStart[3] = reorderedStart[3], reorderedStart[2]
	cases := []struct {
		name, surface, stream string
		limit                 int
	}{
		{"invalid JSON", cjkevi.SurfaceChat, "data: {\n\ndata: [DONE]\n\n", 0},
		{"incomplete chat", cjkevi.SurfaceChat, strings.TrimSuffix(correctionChatSSE, "data: [DONE]\n\n"), 0},
		{"unknown event", cjkevi.SurfaceAnthropic, strings.Replace(correctionMessageSSE, "event: message_stop", "event: unknown", 1), 0},
		{"chat tool", cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"content":"中간 상황"`, `"tool_calls":[{"id":"tool"}],"content":"中간 상황"`, 1), 0},
		{"chat annotations", cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"content":"中간 상황"`, `"content":"中간 상황","annotations":[{"type":"citation"}]`, 1), 0},
		{"chat audio", cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"content":"中간 상황"`, `"content":"中간 상황","audio":{"id":"audio"}`, 1), 0},
		{"messages tool", cjkevi.SurfaceAnthropic, strings.Replace(correctionMessageSSE, `"type":"text","text":""`, `"type":"tool_use","id":"tool"`, 1), 0},
		{"responses tool", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"type":"message","role":"assistant"`, `"type":"function_call","role":"assistant"`, 1), 0},
		{"responses delta logprobs", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"delta":"中간 상황"`, `"delta":"中간 상황","logprobs":[{"token":"中"}]`, 1), 0},
		{"responses done top logprobs", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"text":"中간 상황"`, `"text":"中간 상황","top_logprobs":[{"token":"中"}]`, 1), 0},
		{"responses completed logprobs", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"text":"中간 상황"}]}]}}`, `"text":"中간 상황","logprobs":[{"token":"中"}]}]}]}}`, 1), 0},
		{"responses completed top logprobs", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"text":"中간 상황"}]}]}}`, `"text":"中간 상황","top_logprobs":[{"token":"中"}]}]}]}}`, 1), 0},
		{"responses missing created", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[0]+"\n\n", "", 1), 0},
		{"responses missing item added", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[2]+"\n\n", "", 1), 0},
		{"responses missing part added", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[3]+"\n\n", "", 1), 0},
		{"responses reversed start", cjkevi.SurfaceResponses, strings.Join(reorderedStart, "\n\n") + "\n\n", 0},
		{"responses missing text done", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[5]+"\n\n", "", 1), 0},
		{"responses missing part done", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[6]+"\n\n", "", 1), 0},
		{"responses missing item done", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, responseFrames[7]+"\n\n", "", 1), 0},
		{"responses reversed done", cjkevi.SurfaceResponses, strings.Join(reorderedDone, "\n\n") + "\n\n", 0},
		{"chat multiple", cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"index":0`, `"index":1`, 1), 0},
		{"messages multiple", cjkevi.SurfaceAnthropic, strings.Replace(correctionMessageSSE, "event: message_delta", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"second\"}}\n\nevent: message_delta", 1), 0},
		{"responses multiple", cjkevi.SurfaceResponses, strings.Replace(correctionResponsesSSE, `"content_index":0`, `"content_index":1`, 1), 0},
		{"overflow", cjkevi.SurfaceChat, correctionChatSSE, 20},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			local := *server.getConfig()
			if test.limit != 0 {
				local.CJKeviCorrection.MaxBufferBytes = test.limit
			}
			server.cfgMu.Lock()
			server.cfg = &local
			server.cfgMu.Unlock()
			engine := gin.New()
			engine.POST("/v1/test", server.correctCJKevi(test.surface), func(c *gin.Context) {
				_, _ = io.ReadAll(c.Request.Body)
				c.Header("Content-Type", "text/event-stream; charset=utf-8")
				c.Header("ETag", `"original"`)
				c.Header("Digest", "original")
				c.Header("Content-Length", strconv.Itoa(len(test.stream)))
				_, _ = c.Writer.Write([]byte(test.stream[:min(11, len(test.stream))]))
				c.Writer.Flush()
				_, _ = c.Writer.Write([]byte(test.stream[min(11, len(test.stream)):]))
			})
			result := httptest.NewRecorder()
			engine.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"stream":true}`)))
			if result.Body.String() != test.stream || result.Code != 200 || result.Header().Get("ETag") != `"original"` || result.Header().Get("Digest") != "original" || result.Header().Get("Content-Length") != strconv.Itoa(len(test.stream)) {
				t.Fatalf("fallback changed original stream or headers: %d %v %q", result.Code, result.Header(), result.Body.String())
			}
		})
	}
	if jevCalls.Load() != 0 || llmCalls.Load() != 0 {
		t.Fatalf("invalid streams reached providers: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
	// A normal flush does not release even the first chunk before the handler completes.
	server.cfgMu.Lock()
	server.cfg = &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}
	server.cfgMu.Unlock()
	arrived, release := make(chan struct{}), make(chan struct{})
	engine := gin.New()
	engine.POST("/v1/test", server.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Header("Content-Type", "text/event-stream")
		c.Header("ETag", `"old"`)
		c.Header("Digest", "old")
		c.Header("Content-Length", strconv.Itoa(len(correctionChatSSE)))
		_, _ = c.Writer.WriteString(correctionChatSSE[:11])
		c.Writer.Flush()
		close(arrived)
		<-release
		_, _ = c.Writer.WriteString(correctionChatSSE[11:])
	})
	result := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		engine.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"stream":true}`)))
	}()
	<-arrived
	if result.Body.Len() != 0 || result.Flushed {
		t.Fatal("buffered SSE flushed intermediate bytes")
	}
	close(release)
	<-finished
	text, _, ok := correctionSSEText(cjkevi.SurfaceChat, result.Body.Bytes())
	if !ok || text != correctionRewritten || !result.Flushed || result.Header().Get("Content-Length") != strconv.Itoa(result.Body.Len()) || result.Header().Get("ETag") != "" || result.Header().Get("Digest") != "" {
		t.Fatalf("delayed completion was not corrected: %v %q", result.Header(), result.Body.String())
	}
}

func TestCJKeviCorrectionSSEProviderErrorKeepsOriginal(t *testing.T) {
	cfg, _, _ := correctionProviders(t)
	cfg.BufferStreams = true
	var errors atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		errors.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	cfg.JevURL = broken.URL + "/v1/systemone"
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	engine := gin.New()
	engine.POST("/v1/test", server.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Header("Content-Type", "text/event-stream")
		c.Header("ETag", `"old"`)
		_, _ = c.Writer.WriteString(correctionChatSSE)
		c.Writer.Flush()
	})
	result := httptest.NewRecorder()
	engine.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"stream":true}`)))
	if result.Body.String() != correctionChatSSE || result.Header().Get("ETag") != `"old"` || errors.Load() != 1 {
		t.Fatalf("provider error changed original stream: %v %q calls=%d", result.Header(), result.Body.String(), errors.Load())
	}
}

func TestCJKeviCorrectionSSEEventDataRemainJSON(t *testing.T) {
	for _, stream := range []string{correctionMessageSSE, correctionChatSSE, correctionResponsesSSE} {
		frames, valid := parseCorrectionSSE([]byte(stream))
		if !valid {
			t.Fatal("fixture is not complete SSE")
		}
		for _, frame := range frames {
			if bytes.Equal(frame.data, []byte("[DONE]")) {
				continue
			}
			var event json.RawMessage
			if err := json.Unmarshal(frame.data, &event); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCJKeviCorrectionJSONMetadataPassesThrough(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	for _, test := range []struct{ name, surface, body string }{
		{"Chat annotations", cjkevi.SurfaceChat, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황","annotations":[{"type":"citation"}]}}]}`},
		{"Chat audio", cjkevi.SurfaceChat, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황","audio":{"id":"audio"}}}]}`},
		{"Responses logprobs", cjkevi.SurfaceResponses, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황","logprobs":[{"token":"中"}]}]}]}`},
		{"Responses top_logprobs", cjkevi.SurfaceResponses, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황","top_logprobs":[{"token":"中"}]}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.POST("/v1/test", server.correctCJKevi(test.surface), func(c *gin.Context) {
				_, _ = io.ReadAll(c.Request.Body)
				c.Header("Content-Type", "application/json")
				c.Header("ETag", `"original"`)
				c.Header("Content-Length", strconv.Itoa(len(test.body)))
				_, _ = c.Writer.WriteString(test.body)
			})
			result := httptest.NewRecorder()
			engine.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{}`)))
			if result.Body.String() != test.body || result.Header().Get("ETag") != `"original"` || result.Header().Get("Content-Length") != strconv.Itoa(len(test.body)) {
				t.Fatalf("metadata response changed: %v %q", result.Header(), result.Body.String())
			}
		})
	}
	if jevCalls.Load() != 0 || llmCalls.Load() != 0 {
		t.Fatalf("metadata responses reached providers: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
}
