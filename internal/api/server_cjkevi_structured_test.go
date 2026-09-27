package api

import (
	"bytes"
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

func TestStructuredCorrectionLeafSpan(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		eligible         bool
	}{
		{"nested escape", ` {"中文key": [7,true,{"note":"safe \\n", "value":"中간 상황!"}]} `, ` {"中文key": [7,true,{"note":"safe \\n", "value":"중간 상황!"}]} `, true},
		{"escaped candidate", `{"value":"\u4e2d간 상황","other":"exact"}`, `{"value":"중간 상황","other":"exact"}`, true},
		{"two values", `{"a":"中간 상황","b":"日本"}`, "", false},
		{"nested second", `{"a":[{"v":"中간 상황"}],"b":{"v":"日本"}}`, "", false},
		{"key only", `{"中文":"한글"}`, "", false},
		{"malformed", `{"value":"中간 상황",}`, "", false},
		{"incomplete", `{"value":"中간 상황"`, "", false},
		{"plain array", `["中간 상황",4,false]`, `["중간 상황",4,false]`, true},
		{"plain", "中간 상황", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, patch, ok := structuredCorrectionText(tc.text, "ko-KR")
			if ok != tc.eligible {
				t.Fatalf("eligibility=%v value=%q", ok, value)
			}
			if !ok {
				return
			}
			if value != "中간 상황!" && value != "中간 상황" {
				t.Fatalf("candidate not decoded: %q", value)
			}
			replacement := strings.Replace(value, "中", "중", 1)
			changed, err := patch(replacement)
			if err != nil || changed != tc.want {
				t.Fatalf("patch=%q want=%q err=%v", changed, tc.want, err)
			}
		})
	}
}

func TestStructuredCorrectionTargetLanguageGate(t *testing.T) {
	for _, tc := range []struct {
		name, language, text, want string
		eligible                   bool
	}{
		{"Japanese Hangul", "ja-JP", `{"value":"処理を확인します。"}`, "処理を확인します。", true},
		{"Japanese Chinese form", "ja-JP", `{"value":"処理を请确认します。"}`, "処理を请确认します。", true},
		{"Japanese native Han", "ja-JP", `{"value":"処理を確認します。"}`, "", false},
		{"Chinese kana", "zh-CN", `{"value":"请して结果。"}`, "请して结果。", true},
		{"Chinese native Han", "zh-CN", `{"value":"请确认结果。"}`, "", false},
		{"Traditional Hangul", "zh-TW", `{"value":"請확인結果。"}`, "請확인結果。", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, _, eligible := structuredCorrectionText(tc.text, tc.language)
			if eligible != tc.eligible || eligible && value != tc.want {
				t.Fatalf("value=%q eligible=%v, want %q/%v", value, eligible, tc.want, tc.eligible)
			}
		})
	}
}

func TestStructuredCorrectionSSE(t *testing.T) {
	for _, tc := range []struct{ surface, stream string }{
		{cjkevi.SurfaceChat, strings.Replace(correctionChatSSE, `"content":"中간 상황"`, `"content":"{\"value\":\"中간 상황\",\"count\":7}"`, 1)},
		{cjkevi.SurfaceResponses, strings.ReplaceAll(correctionResponsesSSE, `"中간 상황"`, `"{\"value\":\"中간 상황\",\"count\":7}"`)},
	} {
		original, outerPatch, ok := correctionSSEText(tc.surface, []byte(tc.stream))
		if !ok {
			t.Fatalf("structured SSE rejected: %s", tc.surface)
		}
		value, innerPatch, eligible := structuredCorrectionText(original, "ko-KR")
		if !eligible || value != "中간 상황" {
			t.Fatalf("SSE leaf=%q eligible=%v", value, eligible)
		}
		changed, err := innerPatch("중간 상황")
		if err != nil {
			t.Fatal(err)
		}
		patched, err := outerPatch(changed)
		if err != nil {
			t.Fatal(err)
		}
		text, _, ok := correctionSSEText(tc.surface, patched)
		if !ok || text != `{"value":"중간 상황","count":7}` {
			t.Fatalf("SSE text=%q valid=%v", text, ok)
		}
		if !bytes.Contains(patched, []byte(`\"count\":7`)) {
			t.Fatalf("numeric value lost: %s", patched)
		}
	}
}

func TestStructuredCorrectionRoutes(t *testing.T) {
	var jevCalls, llmCalls atomic.Int32
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jevCalls.Add(1)
		var input struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.State != "中간 상황!" {
			t.Errorf("Jev received non-leaf input: %q %v", input.State, err)
		}
		score := "0.9"
		if strings.Contains(input.Questions["intrusion"].Instructions, "제공된 span이 놓인 문맥") {
			score = "0.1"
		}
		_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":`+score+`}}}`)
	}))
	defer jev.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"중간 상황!"}}]}`)
	}))
	defer llm.Close()
	cfg := config.CJKeviCorrectionConfig{Enabled: true, BufferStreams: true, JevURL: jev.URL + "/systemone", LLMURL: llm.URL + "/chat/completions", JevAPIKey: "jev", LLMAPIKey: "llm"}
	repairer, err := newCorrection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{SDKConfig: config.SDKConfig{CJKeviCorrection: cfg}}, correction: repairer, correctionSlots: make(chan struct{}, 4)}
	for _, tc := range []struct{ name, surface, request, body, want string }{
		{"chat schema", cjkevi.SurfaceChat, `{"response_format":{"type":"json_schema"}}`, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"count\":3,\"flag\":true,\"value\":\"中간 상황!\",\"other\":\"한글\"}"}}]}`, `{"count":3,"flag":true,"value":"중간 상황!","other":"한글"}`},
		{"chat object", cjkevi.SurfaceChat, `{"response_format":{"type":"json_object"}}`, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"value\":\"中간 상황!\",\"other\":false}"}}]}`, `{"value":"중간 상황!","other":false}`},
		{"responses", cjkevi.SurfaceResponses, `{"text":{"format":{"type":"json_schema"}}}`, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"value\":\"中간 상황!\",\"n\":5}"}]}]}`, `{"value":"중간 상황!","n":5}`},
		{"messages", cjkevi.SurfaceAnthropic, `{}`, `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"{\"value\":\"中간 상황!\",\"n\":5}"}]}`, `{"value":"중간 상황!","n":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gin.New()
			engine.POST("/", server.correctCJKevi(tc.surface), func(c *gin.Context) {
				_, _ = io.Copy(io.Discard, c.Request.Body)
				c.Header("Content-Type", "application/json")
				_, _ = c.Writer.WriteString(tc.body)
			})
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.request)))
			text, _, ok := correctionText(tc.surface, w.Body.Bytes())
			if !ok || text != tc.want {
				t.Fatalf("text=%q want=%q body=%q", text, tc.want, w.Body.String())
			}
			if bytes.Equal([]byte(tc.body), w.Body.Bytes()) {
				t.Fatal("body unchanged")
			}
		})
	}
	if jevCalls.Load() != 8 || llmCalls.Load() != 4 {
		t.Fatalf("provider calls Jev=%d LLM=%d", jevCalls.Load(), llmCalls.Load())
	}
}
