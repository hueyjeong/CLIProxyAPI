package cjkevi

import (
	"bytes"
	"encoding/json"
	"mime"
	"sort"
	"strings"
)

type textBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type message struct {
	Type       string      `json:"type"`
	Content    []textBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
}

func finalText(body []byte, contentType, surface string) []string {
	switch surface {
	case SurfaceAnthropic:
		return anthropicText(body, contentType)
	case SurfaceChat:
		return chatText(body, contentType)
	case SurfaceResponses:
		return responsesText(body, contentType)
	default:
		return nil
	}
}

func responseMediaType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func anthropicText(body []byte, contentType string) []string {
	mediaType := responseMediaType(contentType)
	if mediaType == "text/event-stream" {
		return anthropicStreamText(body)
	}
	if mediaType != "application/json" {
		return nil
	}
	var msg message
	if json.Unmarshal(bytes.TrimSpace(body), &msg) != nil || msg.Type != "message" || !finalStop(msg.StopReason) {
		return nil
	}
	return textBlocks(msg.Content)
}

func finalStop(reason string) bool {
	return reason == "end_turn" || reason == "stop_sequence" || reason == "tool_use"
}

func toolMessage(name string, input json.RawMessage) string {
	if name != "SendMessage" && name != "functions.SendMessage" {
		return ""
	}
	var fields struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	return fields.Message
}

func textBlocks(blocks []textBlock) []string {
	var out []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			out = append(out, block.Text)
		} else if block.Type == "tool_use" {
			if text := toolMessage(block.Name, block.Input); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// scanSSE processes only frames terminated by a blank line. Callbacks must not
// retain the frame slice; all frames are from a bounded tee of the original body.
func scanSSE(body []byte, frame func(string, []byte) bool) bool {
	var eventName string
	var data []byte
	for _, rawLine := range bytes.Split(body, []byte{'\n'}) {
		line := bytes.TrimSuffix(rawLine, []byte{'\r'})
		if len(line) == 0 {
			if len(data) > 0 && !frame(eventName, data) {
				return false
			}
			eventName = ""
			data = nil
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = string(bytes.TrimSpace(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(line[5:], []byte{' '})...)
		}
	}
	return len(data) == 0
}

func anthropicStreamText(body []byte) []string {
	blocks := make(map[int]*textBlock)
	stopped := make(map[int]bool)
	stopReason := ""
	started := false
	complete := false
	if !scanSSE(body, func(eventName string, data []byte) bool {
		var event struct {
			Type         string    `json:"type"`
			Index        int       `json:"index"`
			ContentBlock textBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message message `json:"message"`
		}
		if json.Unmarshal(data, &event) != nil || (eventName != "" && eventName != event.Type) || complete {
			return false
		}
		switch event.Type {
		case "message_start":
			if started || event.Message.Type != "message" {
				return false
			}
			started = true
		case "content_block_start":
			if !started || event.Index < 0 {
				return false
			}
			if _, exists := blocks[event.Index]; exists {
				return false
			}
			block := event.ContentBlock
			blocks[event.Index] = &block
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil || stopped[event.Index] {
				return false
			}
			if block.Type == "text" && event.Delta.Type == "text_delta" {
				block.Text += event.Delta.Text
			} else if block.Type == "tool_use" && event.Delta.Type == "input_json_delta" {
				if string(block.Input) == "{}" {
					block.Input = nil
				}
				block.Input = append(block.Input, event.Delta.PartialJSON...)
			}
		case "content_block_stop":
			if blocks[event.Index] == nil || stopped[event.Index] {
				return false
			}
			stopped[event.Index] = true
		case "message_delta":
			stopReason = event.Delta.StopReason
		case "message_stop":
			complete = true
		}
		return true
	}) || !started || !complete || !finalStop(stopReason) {
		return nil
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		if !stopped[index] {
			return nil
		}
		indices = append(indices, index)
	}
	sort.Ints(indices)
	ordered := make([]textBlock, 0, len(indices))
	for _, index := range indices {
		ordered = append(ordered, *blocks[index])
	}
	return textBlocks(ordered)
}
