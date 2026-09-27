package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type shadowUpstreamExecutor struct {
	url string
}

func (e *shadowUpstreamExecutor) Identifier() string { return "claude" }

func (e *shadowUpstreamExecutor) request(ctx context.Context, req coreexecutor.Request, opts coreexecutor.Options) (*http.Response, error) {
	path := "/v1/messages"
	switch opts.SourceFormat {
	case sdktranslator.FormatOpenAI:
		path = "/v1/chat/completions"
	case sdktranslator.FormatOpenAIResponse:
		path = "/v1/responses"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+path, bytes.NewReader(req.Payload))
	if err != nil {
		return nil, err
	}
	if opts.Stream {
		request.Header.Set("X-Stream", "true")
	}
	return http.DefaultClient.Do(request)
}

func (e *shadowUpstreamExecutor) Execute(ctx context.Context, _ *auth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	resp, err := e.request(ctx, req, opts)
	if err != nil {
		return coreexecutor.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return coreexecutor.Response{Payload: body, Headers: resp.Header.Clone()}, err
}

func (e *shadowUpstreamExecutor) ExecuteStream(ctx context.Context, _ *auth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	resp, err := e.request(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk, 8)
	if opts.SourceFormat != sdktranslator.FormatOpenAI {
		// The first chunk is written directly before either stream forwarder.
		split := min(len(body), 11)
		chunks <- coreexecutor.StreamChunk{Payload: body[:split]}
		if len(body) > split {
			chunks <- coreexecutor.StreamChunk{Payload: body[split:]}
		}
	} else {
		// The Chat handler frames each executor JSON chunk as an SSE data event.
		for _, frame := range strings.Split(string(body), "\n\n") {
			if data := strings.TrimPrefix(frame, "data: "); data != frame && data != "[DONE]" {
				chunks <- coreexecutor.StreamChunk{Payload: []byte(data)}
			}
		}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: chunks}, nil
}

func (e *shadowUpstreamExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}
func (e *shadowUpstreamExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (e *shadowUpstreamExecutor) HttpRequest(_ context.Context, _ *auth.Auth, req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

func newShadowRouteServer(t *testing.T, upstreamURL string, enabled bool, kevURL string) *Server {
	t.Helper()
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"test-key"}, CJKeviShadow: config.CJKeviShadowConfig{Enabled: enabled, KevURL: kevURL}}}
	server := newTestServerWithConfig(t, cfg)
	server.handlers.AuthManager.RegisterExecutor(&shadowUpstreamExecutor{url: upstreamURL})
	credential := &auth.Auth{ID: "cjkevi-shadow-" + t.Name(), Provider: "claude", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "upstream-key"}}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, "claude", []*registry.ModelInfo{{ID: "cjkevi-test-model", Type: "claude"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(credential.ID)
		_ = server.Stop(context.Background())
	})
	return server
}

func TestShadowConfigReloadStartsAndStopsObserver(t *testing.T) {
	server := newTestServerWithConfig(t, &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"test-key"}}})
	if server.shadow != nil {
		t.Fatal("default config unexpectedly started shadow workers")
	}
	next := *server.getConfig()
	next.CJKeviShadow.Enabled = true
	if !server.UpdateClientsContext(context.Background(), &next) || server.shadow == nil {
		t.Fatal("enable reload did not start shadow observer")
	}
	next.CJKeviShadow.Enabled = false
	if !server.UpdateClientsContext(context.Background(), &next) || server.shadow != nil {
		t.Fatal("disable reload did not cancel shadow observer")
	}
	_ = server.Stop(context.Background())
	next.CJKeviShadow.Enabled = true
	server.UpdateClientsContext(context.Background(), &next)
	if server.shadow != nil {
		t.Fatal("reload recreated shadow workers after Stop")
	}
}

func TestShadowStopAndReloadCannotRecreateWorker(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"test-key"}, CJKeviShadow: config.CJKeviShadowConfig{Enabled: true}}}
	server := newTestServerWithConfig(t, cfg)
	next := *server.getConfig()
	next.CJKeviShadow.ModelVersion = "next"
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = server.Stop(context.Background())
	}()
	go func() {
		defer wg.Done()
		<-start
		server.UpdateClientsContext(context.Background(), &next)
	}()
	close(start)
	wg.Wait()
	server.cfgMu.RLock()
	shadow, stopped := server.shadow, server.stopped
	server.cfgMu.RUnlock()
	if shadow != nil || !stopped {
		t.Fatal("Stop/reload race left an observer running")
	}
}

func TestShadowConcurrentCaptureSlotsNeverBlockResponses(t *testing.T) {
	budget := cjkevi.NewBudget()
	observer := cjkevi.NewObserver(config.CJKeviShadowConfig{Enabled: true, QueueSize: 1, Workers: 1}, budget)
	if observer == nil {
		t.Fatal("observer not initialized")
	}
	defer observer.Close()
	server := &Server{shadow: observer, shadowBudget: budget}
	engine := gin.New()
	arrived := make(chan bool, cjkevi.MaxCaptureSlots+1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	engine.POST("/v1/messages", server.observeShadow(cjkevi.SurfaceAnthropic), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		_, capturing := c.Writer.(*cjkevi.ResponseTee)
		arrived <- capturing
		<-release
		c.Writer.Header().Set("X-Origin", "stable")
		c.Writer.WriteHeader(http.StatusCreated)
		_, _ = c.Writer.WriteString("first 中간")
		c.Writer.Flush()
	})
	results := make(chan *httptest.ResponseRecorder, cjkevi.MaxCaptureSlots+1)
	send := func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"Hello"}]}`))
		engine.ServeHTTP(recorder, request)
		results <- recorder
	}
	for i := 0; i < cjkevi.MaxCaptureSlots; i++ {
		go send()
		select {
		case capturing := <-arrived:
			if !capturing {
				t.Fatal("in-flight capture slot unexpectedly unavailable")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stream did not reach handler")
		}
	}
	// Reload while earlier streams still hold the old observer's capture slots.
	next := cjkevi.NewObserver(config.CJKeviShadowConfig{Enabled: true, QueueSize: 1, Workers: 1}, budget)
	if next == nil {
		t.Fatal("reloaded observer not initialized")
	}
	defer next.Close()
	server.cfgMu.Lock()
	server.shadow = next
	server.cfgMu.Unlock()
	observer.Close()
	go send()
	select {
	case capturing := <-arrived:
		if capturing {
			t.Fatal("over-capacity stream retained a response capture")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("over-capacity stream blocked")
	}
	close(release)
	for i := 0; i < cjkevi.MaxCaptureSlots+1; i++ {
		select {
		case result := <-results:
			if result.Code != http.StatusCreated || result.Header().Get("X-Origin") != "stable" || result.Body.String() != "first 中간" || !result.Flushed {
				t.Fatalf("response changed under capture saturation: code=%d body=%q", result.Code, result.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("stream did not finish after release")
		}
	}
}

func TestShadowRoutesPreserveResponseAndJudgeOnlyWhenEnabled(t *testing.T) {
	messageSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"中간 상황\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	chatSSE := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中간 상황\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	responsesOutput := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]`
	responsesSSE := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"中간 상황\"}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":\"中간 상황\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":" + responsesOutput + "}}\n\n"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("X-Upstream", "unchanged")
		w.Header().Set("Date", "Thu, 01 Jan 1970 00:00:00 GMT")
		if strings.Contains(r.URL.Path, "responses") {
			if r.Header.Get("X-Stream") == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(responsesSSE))
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"completed","output":` + responsesOutput + `}`))
			}
			return
		}
		if strings.Contains(r.URL.Path, "chat") {
			if r.Header.Get("X-Stream") == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(chatSSE))
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`))
			}
			return
		}
		if r.Header.Get("X-Stream") == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(messageSSE))
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"中간 상황"}]}`))
		}
	}))
	defer upstream.Close()
	judged := make(chan string, 8)
	var kevCalls atomic.Int32
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kevCalls.Add(1)
		var input struct {
			Model     string            `json:"model"`
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &input); err != nil {
			t.Errorf("Kev input: %v", err)
		}
		if input.Model != "jaredpalmer/kev-0.8b" || input.Questions["intrusion"].Type != "noul" || input.Questions["language"].Type != "choice" {
			t.Error("invalid System One judge contract")
		}
		judged <- input.State["surface"]
		_, _ = w.Write([]byte(`{"answers":{"intrusion":{"type":"noul","noul":0.81},"language":{"type":"choice","choice":"ko"}}}`))
	}))
	defer kev.Close()

	baseline := newShadowRouteServer(t, upstream.URL, false, kev.URL+"/v1/systemone")
	active := newShadowRouteServer(t, upstream.URL, true, kev.URL+"/v1/systemone")
	for _, test := range []struct {
		path    string
		stream  bool
		surface string
	}{
		{"/v1/messages", false, cjkevi.SurfaceAnthropic},
		{"/v1/messages", true, cjkevi.SurfaceAnthropic},
		{"/v1/chat/completions", false, cjkevi.SurfaceChat},
		{"/v1/chat/completions", true, cjkevi.SurfaceChat},
		{"/v1/responses", false, cjkevi.SurfaceResponses},
		{"/v1/responses", true, cjkevi.SurfaceResponses},
	} {
		payload := `{"model":"cjkevi-test-model","max_tokens":32,"messages":[{"role":"user","content":"Hello."}]}`
		if test.path == "/v1/responses" {
			payload = `{"model":"cjkevi-test-model","input":"Hello."}`
		}
		if test.stream {
			if test.path == "/v1/responses" {
				payload = `{"model":"cjkevi-test-model","input":"Hello.","stream":true}`
			} else {
				payload = `{"model":"cjkevi-test-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Hello."}]}`
			}
		}
		send := func(server *Server) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)
			return rr
		}
		off := send(baseline)
		on := send(active)
		// The upstream generates a different trace ID for every request.
		offHeaders, onHeaders := off.Header().Clone(), on.Header().Clone()
		if offHeaders.Get("X-CPA-TRACE-ID") == "" || onHeaders.Get("X-CPA-TRACE-ID") == "" {
			t.Fatal("missing upstream trace header")
		}
		offHeaders.Del("X-CPA-TRACE-ID")
		onHeaders.Del("X-CPA-TRACE-ID")
		if off.Code != http.StatusOK || on.Code != off.Code || !reflect.DeepEqual(onHeaders, offHeaders) || !bytes.Equal(on.Body.Bytes(), off.Body.Bytes()) || on.Flushed != off.Flushed {
			t.Fatalf("response changed for %s stream=%v: off=(%d,%v,%q,%v), on=(%d,%v,%q,%v)", test.path, test.stream, off.Code, off.Header(), off.Body.String(), off.Flushed, on.Code, on.Header(), on.Body.String(), on.Flushed)
		}
		select {
		case got := <-judged:
			if got != test.surface {
				t.Fatalf("Kev saw surface %q, want %q", got, test.surface)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no local Kev judgment for %s stream=%v; body=%q", test.path, test.stream, on.Body.String())
		}
	}
	if upstreamCalls.Load() != 12 || kevCalls.Load() != 6 {
		t.Fatalf("upstream calls=%d Kev calls=%d, want 12 and 6", upstreamCalls.Load(), kevCalls.Load())
	}
}
