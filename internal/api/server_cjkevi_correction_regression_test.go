package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCJKeviCorrectionResponsesOutputTextAndStructuredRequest(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	engine := gin.New()
	body := `{"status":"completed","output_text":"中간 상황","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]}`
	engine.POST("/v1/responses", server.correctCJKevi(cjkevi.SurfaceResponses), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Header("Content-Type", "application/json")
		c.Header("ETag", `"old"`)
		c.Header("Content-MD5", "old")
		c.Header("Digest", "old")
		c.Header("Content-Digest", "old")
		c.Header("Content-Length", strconv.Itoa(len(body)))
		_, _ = c.Writer.WriteString(body)
	})
	send := func(input string) *httptest.ResponseRecorder {
		result := httptest.NewRecorder()
		engine.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(input)))
		return result
	}
	changed := send(`{"input":"Hello."}`)
	if changed.Code != 200 || changed.Header().Get("Content-Length") != strconv.Itoa(changed.Body.Len()) {
		t.Fatalf("invalid rewritten response: %d %v %q", changed.Code, changed.Header(), changed.Body.String())
	}
	for _, field := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest"} {
		if changed.Header().Get(field) != "" {
			t.Fatalf("stale %s header persisted after rewrite", field)
		}
	}
	var result struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(changed.Body.Bytes(), &result); err != nil || result.OutputText != correctionRewritten || len(result.Output) != 1 || len(result.Output[0].Content) != 1 || result.Output[0].Content[0].Text != correctionRewritten {
		t.Fatalf("duplicate Responses text not synchronized: %q (%v)", changed.Body.String(), err)
	}
	body = `{"status":"completed","output_text":"different","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]}`
	mismatched := send(`{"input":"Hello."}`)
	if mismatched.Body.String() != body || mismatched.Header().Get("ETag") != `"old"` {
		t.Fatalf("mismatched output_text changed: %q", mismatched.Body.String())
	}
	body = `{"status":"completed","output_text":"中간 상황","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]}`
	structured := send(`{"input":"Hello.","text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}}}`)
	if structured.Body.String() != body || structured.Header().Get("ETag") != `"old"` {
		t.Fatalf("structured response changed: %q", structured.Body.String())
	}
	if jevCalls.Load() != 2 || llmCalls.Load() != 1 {
		t.Fatalf("unexpected provider calls: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
}

func TestCJKeviCorrectionLongAndEncodedRequests(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	engine := gin.New()
	body := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`
	engine.POST("/v1/chat/completions", server.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.WriteString(body)
	})
	longInput := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", cjkevi.MaxRequestBytes) + `"}]}`
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(longInput)))
	if !strings.Contains(recorder.Body.String(), correctionRewritten) || jevCalls.Load() != 2 || llmCalls.Load() != 1 {
		t.Fatalf("long request failed to correct: %q, Jev=%d LLM=%d", recorder.Body.String(), jevCalls.Load(), llmCalls.Load())
	}
	encoded := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(longInput))
	encoded.Header.Set("Content-Encoding", "zstd")
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, encoded)
	if recorder.Body.String() != body || jevCalls.Load() != 2 || llmCalls.Load() != 1 {
		t.Fatalf("encoded request was corrected: %q", recorder.Body.String())
	}
}

func TestCJKeviCorrectionSlotsBypassWhenFull(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	engine := gin.New()
	arrived := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	bypassed := make(chan bool, 1)
	engine.POST("/v1/chat/completions", server.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		if c.Request.Header.Get("X-Wait") == "true" {
			arrived <- struct{}{}
			<-release
		} else {
			_, wrapped := c.Writer.(*correctionWriter)
			bypassed <- !wrapped
		}
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.WriteString(`{"choices":[]}`)
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			request.Header.Set("X-Wait", "true")
			engine.ServeHTTP(httptest.NewRecorder(), request)
		}()
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("capture slot did not reach handler")
		}
	}
	if len(server.correctionSlots) != 4 {
		t.Fatal("four simultaneous requests did not occupy four slots")
	}
	fifth := httptest.NewRecorder()
	engine.ServeHTTP(fifth, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	select {
	case wasBypassed := <-bypassed:
		if !wasBypassed || fifth.Body.String() != `{"choices":[]}` {
			t.Fatal("full correction slots changed the fifth request")
		}
	default:
		t.Fatal("fifth request blocked instead of bypassing")
	}
	once.Do(func() { close(release) })
	wg.Wait()
	if len(server.correctionSlots) != 0 || jevCalls.Load() != 0 || llmCalls.Load() != 0 {
		t.Fatal("capture slots or providers remained occupied")
	}
}

func TestCJKeviCorrectionReloadPreservesSlots(t *testing.T) {
	cfg, _, _ := correctionProviders(t)
	server := newTestServerWithConfig(t, &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}})
	defer func() { _ = server.Stop(context.Background()) }()
	slots := server.correctionSlots
	next := *server.getConfig()
	next.CJKeviCorrection.MaxBufferBytes = 128 << 10
	if !server.UpdateClientsContext(context.Background(), &next) || server.correctionSlots != slots {
		t.Fatal("reload replaced lifetime correction slots")
	}
}
