package cjkevi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const repairInput = "성능を 조정하면 안정적으로 동작합니다."
const repairOutput = "성능을 조정하면 안정적으로 동작합니다."

func TestRepairerProtocols(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		model    string
		reply    string
	}{
		{"chat", "glm-5.3-flash", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"성능을 조정하면 안정적으로 동작합니다."}}]}`},
		{"responses", "muse-spark-1.3-contributor", `{"status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"성능을 조정하면 안정적으로 동작합니다."}]},{"type":"reasoning"}]}`},
		{"messages", "qwen3.8-flash", `{"type":"message","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"internal"},{"type":"text","text":"성능을 조정하면 안정적으로 동작합니다."},{"type":"redacted_thinking","data":"hidden"}]}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			stage := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
					t.Errorf("invalid request method or content type")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Errorf("invalid request JSON: %v", err)
				}
				if req.URL.Path == "/jev" {
					if req.Header.Get("Authorization") != "Bearer judge-secret" || req.Header.Get("x-api-key") != "" {
						t.Errorf("invalid Jev authorization")
					}
					var state string
					if err := json.Unmarshal(body["state"], &state); err != nil || state != repairInput {
						t.Errorf("Jev state must be original prose only")
					}
					var questions map[string]struct {
						Type         string `json:"type"`
						Instructions string `json:"instructions"`
					}
					if err := json.Unmarshal(body["questions"], &questions); err != nil || len(questions) != 1 || questions["intrusion"].Type != "noul" {
						t.Errorf("Jev must receive exactly one typed noul question")
					}
					wantInstruction := intentionalPrompt
					if stage == 1 {
						wantInstruction = intrusionPrompt
					}
					if questions["intrusion"].Instructions != wantInstruction || strings.Contains(wantInstruction, "span") {
						t.Errorf("Jev prompt at stage %d does not match unmarked state", stage)
					}
					stage++
					_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
					return
				}
				if req.URL.Path != "/llm" || stage != 2 {
					t.Errorf("LLM called out of order")
				}
				if tc.protocol == "messages" {
					if req.Header.Get("x-api-key") != "llm-secret" || req.Header.Get("anthropic-version") != "2023-06-01" || req.Header.Get("Authorization") != "" {
						t.Errorf("invalid Messages authorization")
					}
				} else if req.Header.Get("Authorization") != "Bearer llm-secret" || req.Header.Get("x-api-key") != "" {
					t.Errorf("invalid LLM authorization")
				}
				var model string
				_ = json.Unmarshal(body["model"], &model)
				if model != tc.model {
					t.Errorf("model = %q, want %q", model, tc.model)
				}
				if tc.protocol == "responses" {
					var input, instructions string
					_ = json.Unmarshal(body["input"], &input)
					_ = json.Unmarshal(body["instructions"], &instructions)
					if input != repairInput || instructions != repairPrompt {
						t.Errorf("incorrect Responses input or instructions")
					}
				} else {
					var messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}
					_ = json.Unmarshal(body["messages"], &messages)
					if len(messages) == 0 || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Content != repairInput {
						t.Errorf("incorrect LLM user input")
					}
					if tc.protocol == "chat" && (len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != repairPrompt) {
						t.Errorf("incorrect Chat system instruction")
					}
					if tc.protocol == "messages" {
						var system string
						_ = json.Unmarshal(body["system"], &system)
						if system != repairPrompt || len(messages) != 1 {
							t.Errorf("incorrect Messages instruction")
						}
					}
				}
				stage++
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "judge-secret", LLMURL: server.URL + "/llm", LLMAPIKey: "llm-secret", LLMProtocol: tc.protocol})
			if err != nil {
				t.Fatal(err)
			}
			out, result, err := r.Rewrite(context.Background(), repairInput, "ko-KR")
			if err != nil || out != repairOutput || !result.Changed || !result.JevCalled || !result.LLMCalled || result.IntentionalScore != 0.7 || result.IntrusionScore != 0.7 || stage != 3 {
				t.Fatalf("Rewrite = (%q, %+v, %v), stage = %d", out, result, err, stage)
			}
		})
	}
}

func TestRepairerOpenCodeHeaders(t *testing.T) {
	for _, tc := range []struct{ protocol, reply string }{
		{"chat", `{"choices":[{"finish_reason":"stop","message":{"content":"성능을 조정하면 안정적으로 동작합니다."}}]}`},
		{"responses", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"성능을 조정하면 안정적으로 동작합니다."}]}]}`},
		{"messages", `{"stop_reason":"end_turn","content":[{"type":"text","text":"성능을 조정하면 안정적으로 동작합니다."}]}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			var sessions []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/jev" {
					if req.Header.Get("x-opencode-session") != "" {
						t.Errorf("OpenCode session header sent to Jev")
					}
					_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
					return
				}
				if req.Header.Get("User-Agent") != "CJKevi-correction/0.1" {
					t.Errorf("unexpected OpenCode User-Agent")
				}
				session := req.Header.Get("x-opencode-session")
				if _, err := uuid.Parse(session); err != nil {
					t.Errorf("invalid OpenCode session UUID: %v", err)
				}
				sessions = append(sessions, session)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer server.Close()
			client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}}
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "judge-secret", LLMURL: "http://opencode.ai/llm", LLMAPIKey: "llm-secret", LLMProtocol: tc.protocol, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				out, result, err := r.Rewrite(context.Background(), repairInput, "ko-KR")
				if err != nil || out != repairOutput || !result.Changed {
					t.Fatalf("OpenCode rewrite = (%q, %+v, %v)", out, result, err)
				}
			}
			if len(sessions) != 2 || sessions[0] == sessions[1] {
				t.Fatalf("each independent rewrite needs its own session UUID: %v", sessions)
			}
		})
	}
}

func TestRepairerProviderStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		status int
	}{
		{"jev", http.StatusTooManyRequests},
		{"llm", http.StatusForbidden},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/"+tc.stage {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "secret response body")
					return
				}
				_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "judge-secret", LLMURL: server.URL + "/llm", LLMAPIKey: "llm-secret"})
			if err != nil {
				t.Fatal(err)
			}
			out, result, err := r.Rewrite(context.Background(), repairInput, "ko-KR")
			wantError := fmt.Sprintf("repair: %s provider HTTP %d", tc.stage, tc.status)
			if err == nil || err.Error() != wantError || out != repairInput || result.Changed || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("provider error = (%q, %+v, %v), want %q", out, result, err, wantError)
			}
		})
	}
}

func TestRepairerThresholdsAndLocalSkips(t *testing.T) {
	for _, tc := range []struct {
		name       string
		intent     float64
		intrusion  float64
		wantCalls  int
		wantReason string
	}{
		{"intentional at boundary", 0.8, 1, 1, "intentional"},
		{"intrusion below boundary", 0.79, 0.549, 2, "below_threshold"},
		{"intrusion at boundary", 0.79, 0.55, 3, "unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.URL.Path == "/jev" {
					score := tc.intent
					if calls == 2 {
						score = tc.intrusion
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"intrusion": map[string]any{"type": "noul", "noul": score}}})
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"성능を 조정하면 안정적으로 동작합니다."}}]}`)
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "key", LLMURL: server.URL + "/llm", LLMAPIKey: "key"})
			if err != nil {
				t.Fatal(err)
			}
			out, result, err := r.Rewrite(context.Background(), repairInput, "ko-KR")
			if err != nil || out != repairInput || result.SkipReason != tc.wantReason || calls != tc.wantCalls || result.Changed || result.LLMCalled != (tc.wantCalls == 3) {
				t.Fatalf("Rewrite = (%q, %+v, %v), calls = %d", out, result, err, calls)
			}
		})
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	r, err := NewRepairer(RepairOptions{JevURL: server.URL, JevAPIKey: "key", LLMURL: server.URL, LLMAPIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ text, language, reason string }{
		{"순수한 한국어입니다.", "ko-KR", "no_candidate"},
		{"原語를 설명하면서 성능を 조정합니다.", "ko-KR", "multiple_candidates"},
		{"성능" + strings.Repeat("語", 90) + "을 확인합니다.", "ko-KR", "no_candidate"},
		{repairInput, "en-US", "unsupported_language"},
		{strings.Repeat(repairInput, 1024), "ko-KR", "invalid_input"},
	} {
		out, result, err := r.Rewrite(context.Background(), tc.text, tc.language)
		if err != nil || out != tc.text || result.SkipReason != tc.reason || result.JevCalled || result.LLMCalled || result.Changed {
			t.Fatalf("local skip = (%q, %+v, %v)", out, result, err)
		}
	}
	if calls != 0 {
		t.Fatalf("remote calls before local gate: %d", calls)
	}
}

func TestRepairerCJKTargetLanguages(t *testing.T) {
	for _, tc := range []struct {
		name, language, input, corrected string
	}{
		{"Japanese from Korean", "ja-JP", "処理を확인します。", "処理を確認します。"},
		{"Japanese from Chinese", "ja-JP", "処理を请确认します。", "処理を確認します。"},
		{"Simplified Chinese from Japanese", "zh-CN", "请して结果。", "请确认结果。"},
		{"Traditional Chinese from Korean", "zh-TW", "請확인結果。", "請確認結果。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intentPrompt, intrusionPrompt, rewritePrompt, ok := correctionPrompts(tc.language)
			if !ok || intentPrompt == "" || intrusionPrompt == "" || rewritePrompt == "" {
				t.Fatal("missing target language prompts")
			}
			stage := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if req.URL.Path == "/jev" {
					var state string
					_ = json.Unmarshal(body["state"], &state)
					var questions map[string]struct {
						Instructions string `json:"instructions"`
					}
					_ = json.Unmarshal(body["questions"], &questions)
					want := intentPrompt
					if stage == 1 {
						want = intrusionPrompt
					}
					if state != tc.input || questions["intrusion"].Instructions != want {
						t.Errorf("Jev stage %d received unexpected target text or prompt", stage)
					}
					stage++
					_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
					return
				}
				var messages []struct{ Role, Content string }
				_ = json.Unmarshal(body["messages"], &messages)
				if stage != 2 || len(messages) != 2 || messages[0].Content != rewritePrompt || messages[1].Content != tc.input {
					t.Error("repair provider received incorrect prompt or input")
				}
				stage++
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": tc.corrected}}}})
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "key", LLMURL: server.URL + "/llm", LLMAPIKey: "key"})
			if err != nil {
				t.Fatal(err)
			}
			got, outcome, err := r.Rewrite(context.Background(), tc.input, tc.language)
			if err != nil || got != tc.corrected || !outcome.Changed || stage != 3 {
				t.Fatalf("Rewrite = (%q, %+v, %v), stage %d", got, outcome, err, stage)
			}
		})
	}
}

func TestRepairerOnlyChangesTheCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, input, reply, want string
		changed                  bool
	}{
		{"preserve code and quote", "코드 `run`과 \"원문\"은 그대로 두고 성능を 조정합니다.", "코드 `run`과 \"원문\"은 그대로 두고 성능을 조정합니다.", "코드 `run`과 \"원문\"은 그대로 두고 성능을 조정합니다.", true},
		{"code changed", "코드 `run`과 성능を 조정합니다.", "코드 `exec`과 성능을 조정합니다.", "코드 `run`과 성능を 조정합니다.", false},
		{"quote changed", "성능を 조정합니다. \"원문\"", "성능을 조정합니다. \"바뀐 문구\"", "성능を 조정합니다. \"원문\"", false},
		{"other sentence changed", "성능を 조정합니다. 기록합니다.", "성능을 조정합니다. 정리합니다.", "성능を 조정합니다. 기록합니다.", false},
		{"candidate deleted", "성능を 조정합니다.", "성능 조정합니다.", "성능を 조정합니다.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.URL.Path == "/jev" {
					_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": tc.reply}}}})
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "key", LLMURL: server.URL + "/llm", LLMAPIKey: "key"})
			if err != nil {
				t.Fatal(err)
			}
			out, result, err := r.Rewrite(context.Background(), tc.input, "ko-KR")
			if out != tc.want || result.Changed != tc.changed || !result.LLMCalled || calls != 3 {
				t.Fatalf("rewrite = (%q, %+v, %v), calls = %d", out, result, err, calls)
			}
			if tc.changed && err != nil || !tc.changed && (err == nil || result.SkipReason != "outside_candidate") {
				t.Fatalf("unexpected rewrite acceptance: %+v, %v", result, err)
			}
		})
	}
}

func TestRepairerPreservesCallerTimeout(t *testing.T) {
	client := &http.Client{Timeout: 20 * time.Second}
	r, err := NewRepairer(RepairOptions{JevAPIKey: "key", LLMAPIKey: "key", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if r.client.Timeout != client.Timeout || client.CheckRedirect != nil {
		t.Fatalf("caller client timeout or redirect policy changed")
	}
	defaultRepairer, err := NewRepairer(RepairOptions{JevAPIKey: "key", LLMAPIKey: "key"})
	if err != nil || defaultRepairer.client.Timeout != 0 {
		t.Fatalf("default client must have no deadline: %v", err)
	}
}

func TestRepairerRejectsBadRepliesWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		jev      string
		llm      string
	}{
		{"missing score", "chat", `{"answers":{"intrusion":{"type":"noul"}}}`, ""},
		{"wrong score type", "chat", `{"answers":{"intrusion":{"type":"choice","noul":0.9}}}`, ""},
		{"out of range", "chat", `{"answers":{"intrusion":{"type":"noul","noul":1.1}}}`, ""},
		{"incomplete chat", "chat", "", `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`},
		{"non-text chat", "chat", "", `{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"image"}]}}]}`},
		{"empty chat", "chat", "", `{"choices":[{"finish_reason":"stop","message":{"content":"  "}}]}`},
		{"chat tool calls despite stop", "chat", "", `{"choices":[{"finish_reason":"stop","message":{"content":"성능을 조정하면 안정적으로 동작합니다.","tool_calls":[{"id":"call"}]}}]}`},
		{"chat function call despite stop", "chat", "", `{"choices":[{"finish_reason":"stop","message":{"content":"성능을 조정하면 안정적으로 동작합니다.","function_call":{"name":"tool"}}}]}`},
		{"chat refusal despite stop", "chat", "", `{"choices":[{"finish_reason":"stop","message":{"content":"성능을 조정하면 안정적으로 동작합니다.","refusal":"cannot answer"}}]}`},
		{"oversized chat", "chat", "", ""},
		{"output too long", "chat", "", ""},
		{"incomplete responses", "responses", "", `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`},
		{"non-text responses", "responses", "", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_image","text":"not prose"}]}]}`},
		{"non-text after text", "responses", "", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"valid"}]},{"type":"function_call"}]}`},
		{"empty responses", "responses", "", `{"status":"completed","output":[]}`},
		{"two response messages", "responses", "", `{"status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"성능을"}]},{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":" 조정하면 안정적으로 동작합니다."}]}]}`},
		{"incomplete messages", "messages", "", `{"stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`},
		{"thinking only", "messages", "", `{"stop_reason":"end_turn","content":[{"type":"thinking","thinking":"hidden"},{"type":"redacted_thinking","data":"hidden"}]}`},
		{"non-text messages", "messages", "", `{"stop_reason":"end_turn","content":[{"type":"tool_use","text":"not prose"}]}`},
		{"tool after text", "messages", "", `{"stop_reason":"end_turn","content":[{"type":"text","text":"partial"},{"type":"tool_use","id":"call"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/jev" {
					if tc.jev != "" {
						_, _ = io.WriteString(w, tc.jev)
						return
					}
					_, _ = io.WriteString(w, `{"answers":{"intrusion":{"type":"noul","noul":0.7}}}`)
					return
				}
				if tc.name == "oversized chat" {
					_, _ = io.WriteString(w, strings.Repeat("x", maxReplyBytes+1))
					return
				}
				if tc.name == "output too long" {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": strings.Repeat("한", maxRepairBytes/3+1)}}}})
					return
				}
				_, _ = io.WriteString(w, tc.llm)
			}))
			defer server.Close()
			r, err := NewRepairer(RepairOptions{JevURL: server.URL + "/jev", JevAPIKey: "key", LLMURL: server.URL + "/llm", LLMAPIKey: "key", LLMProtocol: tc.protocol})
			if err != nil {
				t.Fatal(err)
			}
			out, result, err := r.Rewrite(context.Background(), repairInput, "ko-KR")
			if err == nil || out != repairInput || result.Changed || strings.Contains(err.Error(), repairInput) || strings.Contains(err.Error(), "key") {
				t.Fatalf("bad reply = (%q, %+v, %v)", out, result, err)
			}
		})
	}
}

func TestRepairerCancellationAndRedirect(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Location", "/secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	r, err := NewRepairer(RepairOptions{JevURL: server.URL, JevAPIKey: "key", LLMURL: server.URL, LLMAPIKey: "key", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, result, err := r.Rewrite(ctx, repairInput, "ko-KR")
	if !errors.Is(err, context.Canceled) || out != repairInput || result.Changed || calls != 0 {
		t.Fatalf("canceled = (%q, %+v, %v), calls = %d", out, result, err, calls)
	}
	out, result, err = r.Rewrite(context.Background(), repairInput, "ko-KR")
	if err == nil || out != repairInput || result.Changed || calls != 1 {
		t.Fatalf("redirect = (%q, %+v, %v), calls = %d", out, result, err, calls)
	}
}
