package cjkevi

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const MaxRequestBytes = config.CJKeviRequestCaptureLimit

// RequestCapture retains a bounded request sample for direct-user language hints.
// Write always reports all bytes so the original request reader is unaffected.
type RequestCapture struct {
	Body     []byte
	Overflow bool
}

func (c *RequestCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxRequestBytes - len(c.Body)
	if n > remaining {
		c.Overflow = true
		p = p[:max(remaining, 0)]
	}
	c.Body = append(c.Body, p...)
	return n, nil
}

// requestLanguage considers only the first line of direct text in recent user
// messages and top-level system/developer instructions. Quoted or tool text is ignored.
func requestLanguage(body []byte, fallback string) (language, explicit string, locked, conflict bool) {
	language = fallback
	type entry struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var request struct {
		System       json.RawMessage `json:"system"`
		Instructions json.RawMessage `json:"instructions"`
		Messages     []entry         `json:"messages"`
		Input        json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return language, "", false, false
	}
	upper := explicitLanguage(request.System)
	if instruction := explicitLanguage(request.Instructions); instruction != "" {
		if upper != "" && upper != instruction {
			return language, "", true, true
		}
		upper = instruction
	}
	for i := range request.Messages {
		entry := request.Messages[i]
		if entry.Role == "system" || entry.Role == "developer" {
			hint := explicitLanguage(entry.Content)
			if upper != "" && hint != "" && upper != hint {
				return language, "", true, true
			}
			if hint != "" {
				upper = hint
			}
		}
	}
	for i := len(request.Messages) - 1; i >= max(0, len(request.Messages)-8); i-- {
		if request.Messages[i].Role != "user" {
			continue
		}
		if hint := explicitLanguage(request.Messages[i].Content); hint != "" {
			explicit = hint
			break
		}
	}
	if explicit == "" && len(request.Input) > 0 {
		explicit = explicitLanguage(request.Input)
		if explicit == "" {
			var input []entry
			if json.Unmarshal(request.Input, &input) == nil {
				for i := len(input) - 1; i >= max(0, len(input)-8); i-- {
					if input[i].Role == "user" {
						if hint := explicitLanguage(input[i].Content); hint != "" {
							explicit = hint
							break
						}
					}
				}
			}
		}
	}
	if upper != "" && explicit != "" && upper != explicit {
		return language, explicit, true, true
	}
	if upper != "" {
		language = upper
	} else if explicit != "" {
		language = explicit
	}
	return language, explicit, upper != "" || explicit != "", false
}

func explicitLanguage(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
			return ""
		}
		for _, block := range blocks {
			if block.Type == "tool_result" || block.Type == "function_call_output" || block.Type == "output_text" {
				return ""
			}
			if text == "" && (block.Type == "text" || block.Type == "input_text") {
				text = block.Text
			}
		}
		if text == "" {
			return ""
		}
	}
	if len(text) > 512 {
		text = text[:512]
	}
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if strings.HasPrefix(line, ">") || strings.HasPrefix(line, "`") || strings.HasPrefix(line, "~") || strings.HasPrefix(line, "<") || strings.HasPrefix(line, "\"") || strings.HasPrefix(line, "'") {
		return ""
	}
	line = strings.ToLower(line)
	line = strings.TrimPrefix(line, "please ")
	line = strings.TrimPrefix(line, "always ")
	for language, phrases := range map[string][]string{
		"ko-KR": {"answer in korean", "respond in korean", "reply in korean", "write in korean", "한국어로 답변", "한국어로 작성", "한국어로 대답"},
		"en-US": {"answer in english", "respond in english", "reply in english", "write in english", "영어로 답변", "영어로 작성"},
		"ja-JP": {"answer in japanese", "respond in japanese", "reply in japanese", "write in japanese", "日本語で回答"},
		"zh-CN": {"answer in simplified chinese", "respond in simplified chinese", "answer in chinese", "respond in chinese", "reply in chinese", "write in chinese", "用简体中文回答", "请用简体中文回答", "用中文回答", "请用中文回答", "简体中文回答", "중국어로 답변", "중국어로 작성"},
		"zh-TW": {"answer in traditional chinese", "respond in traditional chinese", "用繁體中文回答"},
	} {
		for _, phrase := range phrases {
			if strings.HasPrefix(line, phrase) {
				return language
			}
		}
	}
	return ""
}
