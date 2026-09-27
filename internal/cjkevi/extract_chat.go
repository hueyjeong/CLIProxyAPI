package cjkevi

import (
	"bytes"
	"encoding/json"
	"sort"
)

type chatToolCall struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatChoice struct {
	Index        int     `json:"index"`
	FinishReason *string `json:"finish_reason"`
	Message      struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []chatToolCall  `json:"tool_calls"`
	} `json:"message"`
	Delta struct {
		Content   string         `json:"content"`
		ToolCalls []chatToolCall `json:"tool_calls"`
	} `json:"delta"`
}

func chatContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

func chatChoiceText(choice chatChoice) []string {
	if choice.FinishReason == nil || (*choice.FinishReason != "stop" && *choice.FinishReason != "tool_calls") || choice.Message.Role != "assistant" {
		return nil
	}
	var out []string
	if text := chatContent(choice.Message.Content); text != "" {
		out = append(out, text)
	}
	for _, tool := range choice.Message.ToolCalls {
		if text := toolMessage(tool.Function.Name, json.RawMessage(tool.Function.Arguments)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func chatText(body []byte, contentType string) []string {
	mediaType := responseMediaType(contentType)
	if mediaType == "text/event-stream" {
		return chatStreamText(body)
	}
	if mediaType != "application/json" {
		return nil
	}
	var response struct {
		Choices []chatChoice `json:"choices"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &response) != nil {
		return nil
	}
	var out []string
	for _, choice := range response.Choices {
		out = append(out, chatChoiceText(choice)...)
	}
	return out
}

func chatStreamText(body []byte) []string {
	type streamChoice struct {
		content string
		tools   map[int]*chatToolCall
		finish  string
	}
	choices := make(map[int]*streamChoice)
	done := false
	if !scanSSE(body, func(eventName string, data []byte) bool {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			done = true
			return true
		}
		if done || eventName == "error" {
			return false
		}
		var chunk struct {
			Choices []chatChoice `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return false
		}
		for _, entry := range chunk.Choices {
			state := choices[entry.Index]
			if state == nil {
				state = &streamChoice{tools: make(map[int]*chatToolCall)}
				choices[entry.Index] = state
			}
			if state.finish != "" {
				return false
			}
			state.content += entry.Delta.Content
			for _, delta := range entry.Delta.ToolCalls {
				tool := state.tools[delta.Index]
				if tool == nil {
					tool = &chatToolCall{}
					state.tools[delta.Index] = tool
				}
				tool.Function.Name += delta.Function.Name
				tool.Function.Arguments += delta.Function.Arguments
			}
			if entry.FinishReason != nil {
				state.finish = *entry.FinishReason
			}
		}
		return true
	}) || !done || len(choices) == 0 {
		return nil
	}
	indices := make([]int, 0, len(choices))
	for index := range choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var out []string
	for _, index := range indices {
		choice := choices[index]
		if choice.finish != "stop" && choice.finish != "tool_calls" {
			continue
		}
		if choice.content != "" {
			out = append(out, choice.content)
		}
		toolIndices := make([]int, 0, len(choice.tools))
		for toolIndex := range choice.tools {
			toolIndices = append(toolIndices, toolIndex)
		}
		sort.Ints(toolIndices)
		for _, toolIndex := range toolIndices {
			tool := choice.tools[toolIndex]
			if text := toolMessage(tool.Function.Name, json.RawMessage(tool.Function.Arguments)); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}
