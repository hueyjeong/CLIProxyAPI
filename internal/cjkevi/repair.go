package cjkevi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxRepairBytes    = 8 << 10
	maxReplyBytes     = 64 << 10
	intentionalPrompt = "목표 응답 언어는 한국어다. 제공된 문장을 보고, 한국어와 다른 문자로 적힌 후보가 다음 용도 중 하나로 사용됐는지 판정하라.\n\n" +
		"- 사용자가 요청한 다국어 출력, 원어 표기 또는 번역 비교의 일부\n" +
		"- 문장에서 지칭하는 인명·지명·작품명·상품명 등의 완전한 원문 표기\n" +
		"- 인용문이나 외국어 예문의 일부\n" +
		"- 코드·명령어·파일명·URL·식별자처럼 정확한 문자열을 전달해야 하는 표기\n" +
		"- 후보 표현 자체의 뜻이나 발음을 설명하기 위해 제시한 예시\n\n" +
		"문맥에서 이러한 용도가 확인되면 예, 확인되지 않으면 아니오로 판정하라."
	intrusionPrompt = "목표 응답 언어는 한국어다. 제공된 문장에서 한국어와 다른 문자로 적힌 후보와 그 위치의 문맥을 보고 다음 두 경우에 해당하는지 판정하라.\n\n" +
		"- 한국어 단어의 일부가 일본어·중국어 표기로 바뀌어 한 단어 안에 섞여 있다.\n" +
		"- 한국어 설명문의 단어·어미·구절 자리에 일본어·중국어 표현이 번역되지 않은 채 들어가 문장 성분으로 쓰인다.\n\n" +
		"어느 하나에 해당하면 예로 판정하라. 후보가 문장 속에서 온전한 한국어 한자어 또는 한국어의 한자 표기로 쓰였다면 아니오로 판정하라."
	repairPrompt        = "Correct only unwanted Japanese or Chinese writing in this Korean assistant prose. Make the smallest possible change. Preserve all intentional foreign terms, names, quotations, code, URLs, identifiers, and Markdown syntax exactly. Return only the complete corrected text, with no preface or code fence."
	jpIntentionalPrompt = "目標の回答言語は日本語です。候補の表記が、依頼された多言語出力、原語の固有名詞、引用、外国語の例文、コード、URL、識別子、またはその表記自体の説明として意図的に使われている場合だけ「はい」と判定してください。文脈で確認できなければ「いいえ」と判定してください。"
	jpIntrusionPrompt   = "目標の回答言語は日本語です。日本語の説明文や語句に、翻訳されていない韓国語のハングルまたは中国語固有の漢字表記が不自然に混入している場合だけ「はい」と判定してください。固有名詞、引用、コード、正しい日本語の漢字表記は「いいえ」と判定してください。"
	jpRepairPrompt      = "Correct only unintended Korean Hangul or Chinese-only Han writing mixed into Japanese assistant prose. Make the smallest possible change into natural Japanese. Preserve intentional foreign terms, names, quotations, code, URLs, identifiers, and Markdown syntax exactly. Return only the complete corrected text, with no preface or code fence."
	cnIntentionalPrompt = "目标回答语言是简体中文。只有候选文字是用户要求的多语言内容、专有名称原文、引文、外语例句、代码、网址、标识符或对该文字本身的说明时，才回答是。无法从上下文确认这些用途时回答否。"
	cnIntrusionPrompt   = "目标回答语言是简体中文。只有简体中文叙述中不自然地混入未翻译的韩文谚文或日文假名，作为普通词语或句子成分使用时，才回答是。专有名称、引文、代码和正常的中文汉字写法应回答否。"
	cnRepairPrompt      = "Correct only unintended Korean Hangul or Japanese kana mixed into Simplified Chinese assistant prose. Make the smallest possible change into natural Simplified Chinese. Preserve intentional foreign terms, names, quotations, code, URLs, identifiers, and Markdown syntax exactly. Return only the complete corrected text, with no preface or code fence."
	twIntentionalPrompt = "目標回答語言是繁體中文。只有候選文字是使用者要求的多語內容、專有名稱原文、引文、外語例句、程式碼、網址、識別符或對該文字本身的說明時，才回答是。無法從上下文確認這些用途時回答否。"
	twIntrusionPrompt   = "目標回答語言是繁體中文。只有繁體中文敘述中不自然地混入未翻譯的韓文諺文或日文假名，作為一般詞語或句子成分使用時，才回答是。專有名稱、引文、程式碼和正常的中文漢字寫法應回答否。"
	twRepairPrompt      = "Correct only unintended Korean Hangul or Japanese kana mixed into Traditional Chinese assistant prose. Make the smallest possible change into natural Traditional Chinese. Preserve intentional foreign terms, names, quotations, code, URLs, identifiers, and Markdown syntax exactly. Return only the complete corrected text, with no preface or code fence."
)

func correctionPrompts(language string) (intentional, intrusion, repair string, supported bool) {
	switch language {
	case "ko-KR":
		return intentionalPrompt, intrusionPrompt, repairPrompt, true
	case "ja-JP":
		return jpIntentionalPrompt, jpIntrusionPrompt, jpRepairPrompt, true
	case "zh-CN":
		return cnIntentionalPrompt, cnIntrusionPrompt, cnRepairPrompt, true
	case "zh-TW":
		return twIntentionalPrompt, twIntrusionPrompt, twRepairPrompt, true
	default:
		return "", "", "", false
	}
}

// RepairOptions configures opt-in, synchronous CJK prose correction.
type RepairOptions struct {
	JevURL             string
	JevModel           string
	JevAPIKey          string
	LLMURL             string
	LLMModel           string
	LLMAPIKey          string
	LLMProtocol        string
	IntentThreshold    float64
	IntrusionThreshold float64
	HTTPClient         *http.Client
}

// RepairOutcome contains decision metadata, never provider credentials or prose.
type RepairOutcome struct {
	IntentionalScore float64
	IntrusionScore   float64
	JevCalled        bool
	LLMCalled        bool
	Changed          bool
	SkipReason       string
}

type Repairer struct {
	opts   RepairOptions
	client *http.Client
}

// NewRepairer validates provider settings without making a remote call.
func NewRepairer(opts RepairOptions) (*Repairer, error) {
	if opts.LLMProtocol == "" {
		switch {
		case strings.HasSuffix(opts.LLMURL, "/chat/completions"):
			opts.LLMProtocol = "chat"
		case strings.HasSuffix(opts.LLMURL, "/messages"):
			opts.LLMProtocol = "messages"
		case opts.LLMURL == "" || strings.HasSuffix(opts.LLMURL, "/responses"):
			opts.LLMProtocol = "responses"
		default:
			// Preserve the legacy chat protocol for custom URLs with no recognizable suffix.
			opts.LLMProtocol = "chat"
		}
	}
	switch opts.LLMProtocol {
	case "chat":
		if opts.LLMURL == "" {
			opts.LLMURL = "https://opencode.ai/zen/go/v1/chat/completions"
		}
		if opts.LLMModel == "" {
			opts.LLMModel = "glm-5.3-flash"
		}
	case "responses":
		if opts.LLMURL == "" {
			opts.LLMURL = "https://opencode.ai/zen/go/v1/responses"
		}
		if opts.LLMModel == "" {
			opts.LLMModel = "muse-spark-1.3-contributor"
		}
	case "messages":
		if opts.LLMURL == "" {
			opts.LLMURL = "https://opencode.ai/zen/go/v1/messages"
		}
		if opts.LLMModel == "" {
			opts.LLMModel = "qwen3.8-flash"
		}
	default:
		return nil, errors.New("repair: invalid protocol")
	}
	if opts.JevURL == "" {
		opts.JevURL = "https://openrouter.ai/api/v1/systemone"
	}
	if opts.JevModel == "" {
		opts.JevModel = "typesafe/jev-1.13"
	}
	for _, address := range []string{opts.JevURL, opts.LLMURL} {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return nil, errors.New("repair: invalid provider URL")
		}
	}
	if opts.JevAPIKey == "" || opts.LLMAPIKey == "" {
		return nil, errors.New("repair: provider API keys required")
	}
	if opts.IntentThreshold == 0 {
		opts.IntentThreshold = 0.8
	}
	if opts.IntrusionThreshold == 0 {
		opts.IntrusionThreshold = 0.55
	}
	if math.IsNaN(opts.IntentThreshold) || math.IsNaN(opts.IntrusionThreshold) || opts.IntentThreshold <= 0 || opts.IntentThreshold > 1 || opts.IntrusionThreshold <= 0 || opts.IntrusionThreshold > 1 {
		return nil, errors.New("repair: invalid thresholds")
	}
	client := &http.Client{}
	if opts.HTTPClient != nil {
		*client = *opts.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Repairer{opts: opts, client: client}, nil
}

// Rewrite checks candidate presence locally, then scores and repairs CJK prose.
// On a provider error it returns the original text and a safe error.
func (r *Repairer) Rewrite(ctx context.Context, text, targetLanguage string) (string, RepairOutcome, error) {
	var outcome RepairOutcome
	intentionalPrompt, intrusionPrompt, repairPrompt, supported := correctionPrompts(targetLanguage)
	if !supported {
		outcome.SkipReason = "unsupported_language"
		return text, outcome, nil
	}
	if len(text) > maxRepairBytes || !utf8.ValidString(text) {
		outcome.SkipReason = "invalid_input"
		return text, outcome, nil
	}
	spans := candidates(text, targetLanguage)
	if len(spans) == 0 {
		outcome.SkipReason = "no_candidate"
		return text, outcome, nil
	}
	if len(spans) > 1 {
		outcome.SkipReason = "multiple_candidates"
		return text, outcome, nil
	}
	if err := ctx.Err(); err != nil {
		return text, outcome, err
	}
	outcome.JevCalled = true
	intentional, err := r.score(ctx, text, intentionalPrompt)
	if err != nil {
		return text, outcome, err
	}
	outcome.IntentionalScore = intentional
	if intentional >= r.opts.IntentThreshold {
		outcome.SkipReason = "intentional"
		return text, outcome, nil
	}
	intrusion, err := r.score(ctx, text, intrusionPrompt)
	if err != nil {
		return text, outcome, err
	}
	outcome.IntrusionScore = intrusion
	if intrusion < r.opts.IntrusionThreshold {
		outcome.SkipReason = "below_threshold"
		return text, outcome, nil
	}
	outcome.LLMCalled = true
	corrected, err := r.rewrite(ctx, text, repairPrompt)
	if err != nil {
		return text, outcome, err
	}
	if len(corrected) == 0 || len(corrected) > maxRepairBytes || !utf8.ValidString(corrected) || strings.TrimSpace(corrected) == "" {
		return text, outcome, errors.New("repair: invalid LLM output")
	}
	span := spans[0]
	prefix, suffix := text[:span.Offset], text[span.Offset+len(span.Text):]
	if !strings.HasPrefix(corrected, prefix) || !strings.HasSuffix(corrected, suffix) || len(corrected) <= len(prefix)+len(suffix) {
		outcome.SkipReason = "outside_candidate"
		return text, outcome, errors.New("repair: rewrite outside candidate")
	}
	outcome.Changed = corrected != text
	if !outcome.Changed {
		outcome.SkipReason = "unchanged"
	}
	return corrected, outcome, nil
}

func (r *Repairer) post(ctx context.Context, address, key, protocol, stage string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("repair: %s invalid provider request", stage)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("repair: %s invalid provider request", stage)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.EqualFold(req.URL.Hostname(), "opencode.ai") && stage == "llm" {
		req.Header.Set("User-Agent", "CJKevi-correction/0.1")
		req.Header.Set("x-opencode-session", uuid.NewString())
	}
	if protocol == "messages" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("repair: %s provider request canceled: %w", stage, ctx.Err())
		}
		return nil, fmt.Errorf("repair: %s provider request failed", stage)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repair: %s provider HTTP %d", stage, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("repair: %s provider response canceled: %w", stage, ctx.Err())
		}
		return nil, fmt.Errorf("repair: %s provider response read failed", stage)
	}
	if len(data) > maxReplyBytes {
		return nil, fmt.Errorf("repair: %s provider response too large", stage)
	}
	return data, nil
}

func (r *Repairer) score(ctx context.Context, text, instruction string) (float64, error) {
	data, err := r.post(ctx, r.opts.JevURL, r.opts.JevAPIKey, "chat", "jev", struct {
		Model     string `json:"model"`
		State     string `json:"state"`
		Questions any    `json:"questions"`
	}{r.opts.JevModel, text, map[string]any{"intrusion": map[string]string{"type": "noul", "instructions": instruction}}})
	if err != nil {
		return 0, err
	}
	var reply struct {
		Answers struct {
			Intrusion struct {
				Type string   `json:"type"`
				Noul *float64 `json:"noul"`
			} `json:"intrusion"`
		} `json:"answers"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Answers.Intrusion.Type != "noul" || reply.Answers.Intrusion.Noul == nil || math.IsNaN(*reply.Answers.Intrusion.Noul) || *reply.Answers.Intrusion.Noul < 0 || *reply.Answers.Intrusion.Noul > 1 {
		return 0, errors.New("repair: invalid Jev response")
	}
	return *reply.Answers.Intrusion.Noul, nil
}

func (r *Repairer) rewrite(ctx context.Context, text, repairPrompt string) (string, error) {
	var payload any
	switch r.opts.LLMProtocol {
	case "chat":
		payload = map[string]any{"model": r.opts.LLMModel, "messages": []any{map[string]string{"role": "system", "content": repairPrompt}, map[string]string{"role": "user", "content": text}}, "max_tokens": 4096}
	case "responses":
		payload = map[string]any{"model": r.opts.LLMModel, "instructions": repairPrompt, "input": text, "max_output_tokens": 4096}
	case "messages":
		payload = map[string]any{"model": r.opts.LLMModel, "system": repairPrompt, "messages": []any{map[string]string{"role": "user", "content": text}}, "max_tokens": 4096}
	}
	data, err := r.post(ctx, r.opts.LLMURL, r.opts.LLMAPIKey, r.opts.LLMProtocol, "llm", payload)
	if err != nil {
		return "", err
	}
	var result string
	switch r.opts.LLMProtocol {
	case "chat":
		var reply struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content      string                     `json:"content"`
					ToolCalls    []json.RawMessage          `json:"tool_calls"`
					FunctionCall map[string]json.RawMessage `json:"function_call"`
					Refusal      string                     `json:"refusal"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &reply) == nil && len(reply.Choices) == 1 && reply.Choices[0].FinishReason == "stop" {
			message := reply.Choices[0].Message
			if len(message.ToolCalls) == 0 && len(message.FunctionCall) == 0 && message.Refusal == "" {
				result = message.Content
			}
		}
	case "responses":
		var reply struct {
			Status string `json:"status"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(data, &reply) == nil && reply.Status == "completed" {
			valid := true
			messages := 0
			for _, item := range reply.Output {
				if item.Type == "reasoning" {
					continue
				}
				if item.Type != "message" || len(item.Content) == 0 {
					valid = false
					break
				}
				messages++
				for _, part := range item.Content {
					if part.Type != "output_text" {
						valid = false
						break
					}
					result += part.Text
				}
				if !valid {
					break
				}
			}
			if !valid || messages != 1 {
				result = ""
			}
		}
	case "messages":
		var reply struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &reply) == nil && reply.StopReason == "end_turn" && len(reply.Content) > 0 {
			for _, part := range reply.Content {
				if part.Type == "thinking" || part.Type == "redacted_thinking" {
					continue
				}
				if part.Type != "text" {
					result = ""
					break
				}
				result += part.Text
			}
		}
	}
	if result == "" {
		return "", errors.New("repair: invalid LLM output")
	}
	return result, nil
}
