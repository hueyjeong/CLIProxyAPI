package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const correctionOriginal = "中간 상황"
const correctionRewritten = "중간 상황"

func correctionProviders(t *testing.T) (config.CJKeviCorrectionConfig, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	t.Setenv("CJKEVI_TEST_JEV_KEY", "jev-test-secret")
	t.Setenv("CJKEVI_TEST_LLM_KEY", "llm-test-secret")
	jevCalls, llmCalls := &atomic.Int32{}, &atomic.Int32{}
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jev-test-secret" {
			t.Error("Jev key not resolved from environment")
		}
		var input struct {
			Model string `json:"model"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model != "typesafe/jev-1.13" || input.State != correctionOriginal {
			t.Errorf("unexpected Jev input: %+v (%v)", input, err)
		}
		count := jevCalls.Add(1)
		if count%2 == 1 {
			_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.1}}}`)
		} else {
			_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.9}}}`)
		}
	}))
	t.Cleanup(jev.Close)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer llm-test-secret" {
			t.Error("LLM key not resolved from environment")
		}
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"중간 상황"}}]}`)
	}))
	t.Cleanup(llm.Close)
	return config.CJKeviCorrectionConfig{Enabled: true, JevURL: jev.URL + "/api/v1/systemone", LLMURL: llm.URL + "/v1/chat/completions", JevKeyEnv: "CJKEVI_TEST_JEV_KEY", LLMKeyEnv: "CJKEVI_TEST_LLM_KEY"}, jevCalls, llmCalls
}

func TestCJKeviCorrectionRoutesAndReload(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Origin", "same")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"id":"unchanged","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`)
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"unchanged","type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"中간 상황"}]}`)
		case "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"unchanged","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]}`)
		}
	}))
	defer upstream.Close()
	server := newShadowRouteServer(t, upstream.URL, false, "")
	send := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		payload := `{"model":"cjkevi-test-model","max_tokens":32,"messages":[{"role":"user","content":"Hello."}]}`
		if path == "/v1/responses" {
			payload = `{"model":"cjkevi-test-model","input":"Hello."}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	paths := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
	baseline := make(map[string]*httptest.ResponseRecorder)
	for _, path := range paths {
		baseline[path] = send(path)
		if baseline[path].Code != http.StatusOK || !strings.Contains(baseline[path].Body.String(), correctionOriginal) {
			t.Fatalf("invalid baseline for %s: %d %q", path, baseline[path].Code, baseline[path].Body.String())
		}
	}
	if jevCalls.Load() != 0 || llmCalls.Load() != 0 {
		t.Fatal("disabled correction made provider requests")
	}
	next := *server.getConfig()
	next.CJKeviCorrection = cfg
	if !server.UpdateClientsContext(context.Background(), &next) || server.correction == nil {
		t.Fatal("correction reload failed to enable")
	}
	for _, path := range paths {
		recorded := send(path)
		if recorded.Code != http.StatusOK || recorded.Header().Get("X-Origin") != baseline[path].Header().Get("X-Origin") || strings.Contains(recorded.Body.String(), correctionOriginal) || !strings.Contains(recorded.Body.String(), correctionRewritten) {
			t.Fatalf("correction failed on %s: code=%d headers=%v body=%q", path, recorded.Code, recorded.Header(), recorded.Body.String())
		}
		if size := recorded.Header().Get("Content-Length"); size != strconv.Itoa(recorded.Body.Len()) {
			t.Fatalf("incorrect Content-Length for %s: %q vs %d", path, size, recorded.Body.Len())
		}
		var original, changed any
		if err := json.Unmarshal(baseline[path].Body.Bytes(), &original); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(recorded.Body.Bytes(), &changed); err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		want = bytes.Replace(want, []byte(correctionOriginal), []byte(correctionRewritten), 1)
		var expected any
		if err := json.Unmarshal(want, &expected); err != nil || !reflect.DeepEqual(changed, expected) {
			t.Fatalf("other JSON fields changed for %s: %q", path, recorded.Body.String())
		}
	}
	if jevCalls.Load() != 6 || llmCalls.Load() != 3 {
		t.Fatalf("unexpected provider counts: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
	activeRepairer, activeSlots := server.correction, server.correctionSlots
	invalid := next
	invalid.CJKeviShadow.Enabled = true
	if server.UpdateClientsContext(context.Background(), &invalid) || server.correction != activeRepairer || server.correctionSlots != activeSlots || server.shadow != nil {
		t.Fatal("invalid reload replaced active correction")
	}
	if got := send(paths[0]); !strings.Contains(got.Body.String(), correctionRewritten) {
		t.Fatal("invalid reload stopped active correction")
	}
	next.CJKeviCorrection.Enabled = false
	if !server.UpdateClientsContext(context.Background(), &next) || server.correction != nil {
		t.Fatal("correction reload failed to disable")
	}
	if got := send(paths[0]); !bytes.Equal(got.Body.Bytes(), baseline[paths[0]].Body.Bytes()) {
		t.Fatalf("disabled response changed: %q", got.Body.String())
	}
	next.CJKeviCorrection.Enabled = true
	next.CJKeviShadow.Enabled = true
	if server.UpdateClientsContext(context.Background(), &next) || server.correction != nil || server.shadow != nil {
		t.Fatal("conflicting correction/shadow reload accepted")
	}
}

func TestCJKeviCorrectionPassThrough(t *testing.T) {
	cfg, jevCalls, llmCalls := correctionProviders(t)
	cfg.MaxBufferBytes = 256
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		body    string
		request string
		status  int
		mime    string
		flush   bool
	}{
		{"stream", "data: {\"delta\":\"中간 상황\"}\n\n", `{"stream":true}`, 200, "text/event-stream", true},
		{"overflow", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}],"padding":"` + strings.Repeat("x", 300) + `"}`, `{}`, 200, "application/json", false},
		{"request exceeds language capture", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{"messages":[{"role":"user","content":"` + strings.Repeat("a", correctionRequestLimit) + `"}]}`, 200, "application/json", false},
		{"long request language hint", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{"messages":[{"role":"user","content":"` + strings.Repeat("a", cjkevi.MaxRequestBytes) + `"},{"role":"user","content":"Reply in Japanese"}]}`, 200, "application/json", false},
		{"non-2xx", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{}`, 503, "application/json", false},
		{"structured Chat", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{"response_format":{"type":"json_schema"}}`, 200, "application/json", false},
		{"Chat logprobs", `{"choices":[{"finish_reason":"stop","logprobs":{"content":[]},"message":{"role":"assistant","content":"中간 상황"}}]}`, `{}`, 200, "application/json", false},
		{"invalid JSON", `{"choices":`, `{}`, 200, "application/json", false},
		{"tool calls", `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"中간 상황","tool_calls":[{"id":"a"}]}}]}`, `{}`, 200, "application/json", false},
		{"reasoning", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황","reasoning_content":"private"}}]}`, `{}`, 200, "application/json", false},
		{"multi-part", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"中간 상황"}]}}]}`, `{}`, 200, "application/json", false},
		{"incomplete", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"中간 상황"}}]}`, `{}`, 200, "application/json", false},
		{"language conflict", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{"messages":[{"role":"system","content":"Reply in Korean"},{"role":"user","content":"Reply in Japanese"}]}`, 200, "application/json", false},
		{"explicit Japanese", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`, `{"messages":[{"role":"user","content":"Reply in Japanese"}]}`, 200, "application/json", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
			engine := gin.New()
			engine.POST("/v1/chat/completions", server.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
				_, _ = io.ReadAll(c.Request.Body)
				c.Header("Content-Type", test.mime)
				c.Header("X-Origin", "original")
				c.Header("Content-Length", strconv.Itoa(len(test.body)))
				c.Status(test.status)
				if test.status == http.StatusServiceUnavailable {
					c.Header("Retry-After", "7")
				}
				_, _ = c.Writer.WriteString(test.body)
				if test.flush {
					c.Writer.Flush()
				}
			})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.request))
			engine.ServeHTTP(recorder, req)
			if recorder.Code != test.status || recorder.Header().Get("X-Origin") != "original" || recorder.Header().Get("Content-Length") != strconv.Itoa(len(test.body)) || (test.status == http.StatusServiceUnavailable && recorder.Header().Get("Retry-After") != "7") || recorder.Body.String() != test.body || recorder.Flushed != test.flush {
				t.Fatalf("passthrough changed: status=%d headers=%v body=%q flushed=%v", recorder.Code, recorder.Header(), recorder.Body.String(), recorder.Flushed)
			}
		})
	}
	if jevCalls.Load() != 0 || llmCalls.Load() != 0 {
		t.Fatalf("passthrough called providers: Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
}

func TestCJKeviCorrectionRemoteErrorPreservesBody(t *testing.T) {
	cfg, _, _ := correctionProviders(t)
	var failedCalls atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failedCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	cfg.JevURL = broken.URL + "/api/v1/systemone"
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
		c.Header("Content-Length", strconv.Itoa(len(body)))
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.WriteString(body)
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	if recorder.Code != 200 || recorder.Body.String() != body || recorder.Header().Get("Content-Length") != strconv.Itoa(len(body)) || failedCalls.Load() != 1 {
		t.Fatalf("remote error changed original response or bypassed provider: %d %q calls=%d", recorder.Code, recorder.Body.String(), failedCalls.Load())
	}
}
