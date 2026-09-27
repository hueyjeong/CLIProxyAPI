package cjkevi

import (
	"bytes"
	"encoding/json"
	"sort"
)

type responsesResponse struct {
	Status string                `json:"status"`
	Output []responsesOutputItem `json:"output"`
}

type responsesOutputItem struct {
	Type      string      `json:"type"`
	Status    string      `json:"status"`
	Role      string      `json:"role"`
	Content   []textBlock `json:"content"`
	Name      string      `json:"name"`
	Arguments string      `json:"arguments"`
}

func responsesText(body []byte, contentType string) []string {
	mediaType := responseMediaType(contentType)
	if mediaType == "text/event-stream" {
		return responsesStreamText(body)
	}
	if mediaType != "application/json" {
		return nil
	}
	var response responsesResponse
	if json.Unmarshal(bytes.TrimSpace(body), &response) != nil || response.Status != "completed" {
		return nil
	}
	return responsesOutputText(response.Output)
}

func responsesOutputText(output []responsesOutputItem) []string {
	var out []string
	for _, item := range output {
		switch item.Type {
		case "message":
			if item.Role != "assistant" {
				continue
			}
			for _, content := range item.Content {
				if content.Type == "output_text" {
					out = append(out, textBlocks([]textBlock{{Type: "text", Text: content.Text}})...)
				}
			}
		case "function_call":
			if item.Status == "completed" {
				if text := toolMessage(item.Name, json.RawMessage(item.Arguments)); text != "" {
					out = append(out, text)
				}
			}
		}
	}
	return out
}

type responsesTextPosition struct {
	outputIndex  int
	contentIndex int
}

type responsesStreamTextPart struct {
	delta     string
	deltaSeen bool
	text      string
	done      bool
}

func responsesStreamText(body []byte) []string {
	parts := make(map[responsesTextPosition]*responsesStreamTextPart)
	var completed *responsesResponse
	if !scanSSE(body, func(eventName string, data []byte) bool {
		if completed != nil {
			return false
		}
		var event struct {
			Type         string             `json:"type"`
			Response     *responsesResponse `json:"response"`
			OutputIndex  *int               `json:"output_index"`
			ContentIndex *int               `json:"content_index"`
			Delta        string             `json:"delta"`
			Text         string             `json:"text"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		eventType := event.Type
		if eventName != "" {
			if eventType != "" && eventType != eventName {
				return false
			}
			eventType = eventName
		}
		switch eventType {
		case "response.output_text.delta", "response.output_text.done":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || event.ContentIndex == nil || *event.ContentIndex < 0 {
				return false
			}
			position := responsesTextPosition{outputIndex: *event.OutputIndex, contentIndex: *event.ContentIndex}
			part := parts[position]
			if part == nil {
				part = &responsesStreamTextPart{}
				parts[position] = part
			}
			if eventType == "response.output_text.delta" {
				if part.done {
					return false
				}
				part.delta += event.Delta
				part.deltaSeen = true
			} else {
				if part.done || (part.deltaSeen && part.delta != event.Text) {
					return false
				}
				part.text = event.Text
				part.done = true
			}
		case "response.completed":
			if event.Response == nil || (event.Response.Status != "" && event.Response.Status != "completed") {
				return false
			}
			completed = event.Response
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "response.error", "error":
			return false
		default:
			if eventType == "" {
				return false
			}
		}
		return true
	}) || completed == nil {
		return nil
	}
	if len(completed.Output) > 0 {
		return responsesOutputText(completed.Output)
	}
	positions := make([]responsesTextPosition, 0, len(parts))
	for position := range parts {
		positions = append(positions, position)
	}
	sort.Slice(positions, func(i, j int) bool {
		if positions[i].outputIndex != positions[j].outputIndex {
			return positions[i].outputIndex < positions[j].outputIndex
		}
		return positions[i].contentIndex < positions[j].contentIndex
	})
	var out []string
	for _, position := range positions {
		part := parts[position]
		if !part.done {
			return nil
		}
		if part.text != "" {
			out = append(out, part.text)
		}
	}
	return out
}
