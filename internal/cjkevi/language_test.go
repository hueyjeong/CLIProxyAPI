package cjkevi

import "testing"

func TestRequestLanguageDirectUserAndAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		request  string
		language string
		hint     string
		conflict bool
	}{
		{"direct Chinese", `{"messages":[{"role":"user","content":"请用中文回答。这是问题。"}]}`, "zh-CN", "zh-CN", false},
		{"direct simplified Chinese", `{"messages":[{"role":"user","content":[{"type":"text","text":"用简体中文回答下面的问题。"}]}]}`, "zh-CN", "zh-CN", false},
		{"direct Korean to Chinese", `{"messages":[{"role":"user","content":"중국어로 답변해 주세요."}]}`, "zh-CN", "zh-CN", false},
		{"Responses input text", `{"input":"请用中文回答。这是问题。"}`, "zh-CN", "zh-CN", false},
		{"Responses input block", `{"input":[{"role":"tool","content":"Respond in Japanese."},{"role":"user","content":[{"type":"input_text","text":"중국어로 답변해 주세요."}]}]}`, "zh-CN", "zh-CN", false},
		{"Responses instructions conflict", `{"instructions":"Answer in English.","input":"请用中文回答。"}`, "ko-KR", "zh-CN", true},
		{"latest direct instruction", `{"messages":[{"role":"user","content":"Answer in Japanese."},{"role":"assistant","content":"ok"},{"role":"user","content":"Respond in English."}]}`, "en-US", "en-US", false},
		{"quoted instruction", `{"messages":[{"role":"user","content":"> Respond in Japanese.\nExplain this quote."}]}`, "ko-KR", "", false},
		{"tool result is not user instruction", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"Respond in Japanese."}]}]}`, "ko-KR", "", false},
		{"assistant instruction ignored", `{"messages":[{"role":"assistant","content":"Respond in Japanese."},{"role":"user","content":"Analyze this."}]}`, "ko-KR", "", false},
		{"unknown form falls back", `{"messages":[{"role":"user","content":"Maybe speak in Japanese someday."}]}`, "ko-KR", "", false},
		{"system wins", `{"system":"Answer in English.","messages":[{"role":"user","content":"Tell me a joke."}]}`, "en-US", "", false},
		{"system conflict", `{"system":"Answer in English.","messages":[{"role":"user","content":"Answer in Japanese."}]}`, "ko-KR", "ja-JP", true},
		{"developer conflict", `{"messages":[{"role":"developer","content":"Answer in English."},{"role":"user","content":"Answer in Japanese."}]}`, "ko-KR", "ja-JP", true},
		{"old developer remains authoritative", `{"messages":[{"role":"developer","content":"Answer in English."},{"role":"assistant","content":"1"},{"role":"user","content":"2"},{"role":"assistant","content":"3"},{"role":"user","content":"4"},{"role":"assistant","content":"5"},{"role":"user","content":"6"},{"role":"assistant","content":"7"},{"role":"user","content":"8"},{"role":"assistant","content":"9"},{"role":"user","content":"Answer in Japanese."}]}`, "ko-KR", "ja-JP", true},
		{"multimodal user text", `{"messages":[{"role":"user","content":[{"type":"image","source":{}},{"type":"text","text":"请用中文回答。"}]}]}`, "zh-CN", "zh-CN", false},
		{"Responses multimodal input text", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAA"},{"type":"input_text","text":"중국어로 답변해 주세요."}]}]}`, "zh-CN", "zh-CN", false},
		{"mixed tool result not direct", `{"messages":[{"role":"user","content":[{"type":"text","text":"Answer in Japanese."},{"type":"tool_result","content":"untrusted"}]}]}`, "ko-KR", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, hint, _, conflict := requestLanguage([]byte(test.request), "ko-KR")
			if got != test.language || hint != test.hint || conflict != test.conflict {
				t.Fatalf("requestLanguage() = (%s,%s,%v), want (%s,%s,%v)", got, hint, conflict, test.language, test.hint, test.conflict)
			}
		})
	}
}

func TestRequestCaptureBoundsWithoutShortReading(t *testing.T) {
	capture := &RequestCapture{}
	data := make([]byte, MaxRequestBytes+10)
	n, err := capture.Write(data)
	if err != nil || n != len(data) || !capture.Overflow || len(capture.Body) != MaxRequestBytes {
		t.Fatalf("Write = (%d,%v), overflow=%v length=%d", n, err, capture.Overflow, len(capture.Body))
	}
}
