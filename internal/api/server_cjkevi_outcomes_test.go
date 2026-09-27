package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCorrectionTerminalOutcomesViaAuthenticatedHistory(t *testing.T) {
	var mode atomic.Int32
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 3 {
			http.Error(w, "private provider response", 503)
			return
		}
		score := "0.9"
		if mode.Load() == 1 {
			score = "0.95"
		} else {
			var input struct {
				Questions map[string]struct {
					Instructions string `json:"instructions"`
				} `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if strings.Contains(input.Questions["intrusion"].Instructions, "제공된 span이 놓인 문맥") {
				score = "0.1"
			}
		}
		_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":`+score+`}}}`)
	}))
	defer jev.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"중간 상황"}]}]}`)
	}))
	defer llm.Close()
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "local-management"
	cfg.CJKeviCorrection = config.CJKeviCorrectionConfig{Enabled: true, JevURL: jev.URL + "/systemone", LLMURL: llm.URL + "/responses", LLMProtocol: "responses", JevAPIKey: "private-jev", LLMAPIKey: "private-llm"}
	s := newTestServerWithConfig(t, cfg, WithLocalManagementPassword("local-management"))
	defer s.Stop(context.Background())
	s.engine.POST("/test-correction-history", s.correctCJKevi(cjkevi.SurfaceChat), func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Header("Content-Type", "application/json")
		c.String(200, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`)
	})
	for _, tc := range []struct {
		mode                  int32
		body, outcome, reason string
	}{
		{0, `{}`, "corrected", "replacement_applied"},
		{1, `{}`, "jev-reviewed", "intentional"},
		{2, `{"response_format":{"type":"json_object"}}`, "bypassed", "ineligible_request"},
		{3, `{}`, "failed", "provider_or_rewrite_error"},
	} {
		mode.Store(tc.mode)
		req := httptest.NewRequest("POST", "/test-correction-history", strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("correction route: %d", w.Code)
		}
		req = httptest.NewRequest("GET", "/v0/management/cjkevi/history", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "local-management")
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		var result struct {
			Events []cjkevi.HistoryEvent `json:"events"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Events) == 0 {
			t.Fatalf("history: %d %s %v", w.Code, w.Body.String(), err)
		}
		got := result.Events[0]
		if got.Outcome != tc.outcome || got.Reason != tc.reason || got.Surface != cjkevi.SurfaceChat || got.Time.IsZero() {
			t.Fatalf("mode %d: %+v", tc.mode, got)
		}
		if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "中간") {
			t.Fatal("history leaked request or credential")
		}
	}
}
