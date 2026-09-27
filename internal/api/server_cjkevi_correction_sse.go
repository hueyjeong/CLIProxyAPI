package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
)

type correctionSSEFrame struct {
	raw                []byte
	data               []byte
	event              string
	dataStart, dataEnd int
}

type correctionSSEPatch struct {
	frame int
	path  []string
	first bool
}

// parseCorrectionSSE accepts complete single-data-line events, retaining original framing for replay.
func parseCorrectionSSE(body []byte) ([]correctionSSEFrame, bool) {
	var frames []correctionSSEFrame
	for start := 0; start < len(body); {
		frame := correctionSSEFrame{dataStart: -1}
		pos := start
		for {
			end := bytes.IndexByte(body[pos:], '\n')
			if end < 0 {
				return nil, false
			}
			end += pos
			line := bytes.TrimSuffix(body[pos:end], []byte("\r"))
			if len(line) == 0 {
				frame.raw = body[start : end+1]
				if frame.dataStart < 0 {
					return nil, false
				}
				frames = append(frames, frame)
				start = end + 1
				if start < len(body) && len(bytes.Trim(body[start:], "\r\n")) == 0 {
					frames[len(frames)-1].raw = body[start-len(frame.raw):]
					start = len(body)
				}
				break
			}
			switch {
			case bytes.HasPrefix(line, []byte("data:")):
				if frame.dataStart >= 0 {
					return nil, false
				}
				value := line[5:]
				frame.dataStart = pos - start + 5
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
					frame.dataStart++
				}
				frame.dataEnd = frame.dataStart + len(value)
				frame.data = value
			case bytes.HasPrefix(line, []byte("event:")):
				if frame.event != "" {
					return nil, false
				}
				frame.event = strings.TrimPrefix(string(line[6:]), " ")
			case bytes.HasPrefix(line, []byte("id:")), bytes.HasPrefix(line, []byte("retry:")), line[0] == ':':
			default:
				return nil, false
			}
			pos = end + 1
		}
	}
	return frames, len(frames) != 0
}

func correctionSSEText(surface string, body []byte) (string, func(string) ([]byte, error), bool) {
	text, patches, frames, ok := inspectCorrectionSSE(surface, body)
	if !ok || strings.TrimSpace(text) == "" || len(patches) == 0 {
		return "", nil, false
	}
	return text, func(revised string) ([]byte, error) {
		changes := make(map[int][]correctionSSEPatch)
		for _, patch := range patches {
			changes[patch.frame] = append(changes[patch.frame], patch)
		}
		var result bytes.Buffer
		for i, frame := range frames {
			data := frame.data
			for _, patch := range changes[i] {
				value := ""
				if patch.first {
					value = revised
				}
				var err error
				data, err = replaceCorrectionSSEField(data, value, patch.path)
				if err != nil {
					return nil, err
				}
			}
			if len(changes[i]) == 0 {
				result.Write(frame.raw)
				continue
			}
			result.Write(frame.raw[:frame.dataStart])
			result.Write(data)
			result.Write(frame.raw[frame.dataEnd:])
		}
		patched := result.Bytes()
		actual, _, _, valid := inspectCorrectionSSE(surface, patched)
		if !valid || actual != revised {
			return nil, fmt.Errorf("rewritten SSE does not reproduce assistant text")
		}
		return patched, nil
	}, true
}

func replaceCorrectionSSEField(raw []byte, value string, path []string) ([]byte, error) {
	if len(path) == 0 {
		return json.Marshal(value)
	}
	if index, err := strconv.Atoi(path[0]); err == nil {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || index < 0 || index >= len(items) {
			return nil, fmt.Errorf("missing SSE array entry")
		}
		items[index], err = replaceCorrectionSSEField(items[index], value, path[1:])
		if err != nil {
			return nil, err
		}
		return json.Marshal(items)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields[path[0]]) == 0 {
		return nil, fmt.Errorf("missing SSE text field")
	}
	var err error
	fields[path[0]], err = replaceCorrectionSSEField(fields[path[0]], value, path[1:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func correctionSSEObject(raw []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	return fields, json.Unmarshal(raw, &fields) == nil && fields != nil
}

func correctionSSEIndex(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("0"))
}

func correctionSSEString(raw json.RawMessage) (string, bool) {
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err == nil
}

func inspectCorrectionSSE(surface string, body []byte) (string, []correctionSSEPatch, []correctionSSEFrame, bool) {
	frames, ok := parseCorrectionSSE(body)
	if !ok {
		return "", nil, nil, false
	}
	var text strings.Builder
	var patches []correctionSSEPatch
	valid := false
	switch surface {
	case cjkevi.SurfaceChat:
		stopped, done := false, false
		for i, frame := range frames {
			if done || (frame.event != "" && frame.event != "message") {
				return "", nil, nil, false
			}
			if bytes.Equal(frame.data, []byte("[DONE]")) {
				if !stopped || i != len(frames)-1 {
					return "", nil, nil, false
				}
				done = true
				continue
			}
			root, good := correctionSSEObject(frame.data)
			if !good || hasNonNull(root, "error") {
				return "", nil, nil, false
			}
			var choices []json.RawMessage
			if json.Unmarshal(root["choices"], &choices) != nil {
				return "", nil, nil, false
			}
			if len(choices) == 0 && stopped && len(root["usage"]) > 0 {
				continue
			}
			if len(choices) != 1 {
				return "", nil, nil, false
			}
			choice, good := correctionSSEObject(choices[0])
			if !good || !correctionSSEIndex(choice["index"]) || hasNonNull(choice, "logprobs", "message") {
				return "", nil, nil, false
			}
			delta, good := correctionSSEObject(choice["delta"])
			if !good || hasNonNull(delta, "tool_calls", "function_call", "refusal", "reasoning", "reasoning_content", "annotations", "audio") || (len(delta["role"]) > 0 && !jsonStringEquals(delta["role"], "assistant")) {
				return "", nil, nil, false
			}
			if len(delta["content"]) > 0 {
				part, good := correctionSSEString(delta["content"])
				if !good || stopped {
					return "", nil, nil, false
				}
				text.WriteString(part)
				patches = append(patches, correctionSSEPatch{i, []string{"choices", "0", "delta", "content"}, len(patches) == 0})
			}
			if jsonStringEquals(choice["finish_reason"], "stop") {
				if stopped {
					return "", nil, nil, false
				}
				stopped = true
			} else if len(choice["finish_reason"]) != 0 && !bytes.Equal(choice["finish_reason"], []byte("null")) {
				return "", nil, nil, false
			}
		}
		valid = done
	case cjkevi.SurfaceAnthropic:
		started, stopped, stopReason := false, false, false
		blocks := make(map[int]string)
		textBlocks := 0
		for i, frame := range frames {
			root, good := correctionSSEObject(frame.data)
			if !good || frame.event == "" || !jsonStringEquals(root["type"], frame.event) || stopped {
				return "", nil, nil, false
			}
			switch frame.event {
			case "message_start":
				message, good := correctionSSEObject(root["message"])
				if started || !good || !jsonStringEquals(message["type"], "message") || len(message["role"]) > 0 && !jsonStringEquals(message["role"], "assistant") || hasNonNull(message, "content", "stop_reason") {
					return "", nil, nil, false
				}
				started = true
			case "content_block_start":
				var index int
				block, good := correctionSSEObject(root["content_block"])
				if !started || stopReason || json.Unmarshal(root["index"], &index) != nil || !good || blocks[index] != "" {
					return "", nil, nil, false
				}
				kind, good := correctionSSEString(block["type"])
				if !good || kind != "text" && kind != "thinking" || kind == "text" && (textBlocks != 0 || hasNonNull(block, "citations")) {
					return "", nil, nil, false
				}
				blocks[index] = kind
				if kind == "text" {
					textBlocks++
					if len(block["text"]) > 0 {
						part, good := correctionSSEString(block["text"])
						if !good {
							return "", nil, nil, false
						}
						text.WriteString(part)
						patches = append(patches, correctionSSEPatch{i, []string{"content_block", "text"}, len(patches) == 0})
					}
				}
			case "content_block_delta":
				var index int
				delta, good := correctionSSEObject(root["delta"])
				if json.Unmarshal(root["index"], &index) != nil || !good || blocks[index] == "" || stopReason {
					return "", nil, nil, false
				}
				if blocks[index] == "text" {
					if !jsonStringEquals(delta["type"], "text_delta") {
						return "", nil, nil, false
					}
					part, good := correctionSSEString(delta["text"])
					if !good {
						return "", nil, nil, false
					}
					text.WriteString(part)
					patches = append(patches, correctionSSEPatch{i, []string{"delta", "text"}, len(patches) == 0})
				} else if !jsonStringEquals(delta["type"], "thinking_delta") && !jsonStringEquals(delta["type"], "signature_delta") {
					return "", nil, nil, false
				}
			case "content_block_stop":
				var index int
				if json.Unmarshal(root["index"], &index) != nil || blocks[index] == "" {
					return "", nil, nil, false
				}
				delete(blocks, index)
			case "message_delta":
				delta, good := correctionSSEObject(root["delta"])
				if !started || stopReason || len(blocks) != 0 || !good || !jsonStringEquals(delta["stop_reason"], "end_turn") {
					return "", nil, nil, false
				}
				stopReason = true
			case "message_stop":
				if !stopReason || i != len(frames)-1 {
					return "", nil, nil, false
				}
				stopped = true
			default:
				return "", nil, nil, false
			}
		}
		valid = stopped && textBlocks == 1
	case cjkevi.SurfaceResponses:
		completed := false
		var created, inProgress, itemAdded, partAdded, deltaSeen, textDone, partDone, outputDone bool
		for i, frame := range frames {
			root, good := correctionSSEObject(frame.data)
			if !good || frame.event == "" || !jsonStringEquals(root["type"], frame.event) || completed || hasNonNull(root, "logprobs", "top_logprobs") {
				return "", nil, nil, false
			}
			switch frame.event {
			case "response.created", "response.in_progress":
				response, good := correctionSSEObject(root["response"])
				if !good || hasNonNull(response, "output", "output_text", "logprobs", "top_logprobs") {
					return "", nil, nil, false
				}
				if frame.event == "response.created" {
					if created || i != 0 {
						return "", nil, nil, false
					}
					created = true
				} else {
					if !created || inProgress || itemAdded {
						return "", nil, nil, false
					}
					inProgress = true
				}
			case "response.output_item.added", "response.output_item.done":
				item, good := correctionSSEObject(root["item"])
				if !correctionSSEIndex(root["output_index"]) || !good || !jsonStringEquals(item["type"], "message") || (len(item["role"]) > 0 && !jsonStringEquals(item["role"], "assistant")) || hasNonNull(item, "logprobs", "top_logprobs") {
					return "", nil, nil, false
				}
				if frame.event == "response.output_item.added" {
					if !created || itemAdded || hasNonNull(item, "content") {
						return "", nil, nil, false
					}
					itemAdded = true
				}
				if frame.event == "response.output_item.done" {
					if !partDone || outputDone || !correctionSSEOutputText(item["content"], text.String()) {
						return "", nil, nil, false
					}
					outputDone = true
					patches = append(patches, correctionSSEPatch{i, []string{"item", "content", "0", "text"}, true})
				}
			case "response.content_part.added", "response.content_part.done":
				part, good := correctionSSEObject(root["part"])
				if !correctionSSEIndex(root["output_index"]) || !correctionSSEIndex(root["content_index"]) || !good || !jsonStringEquals(part["type"], "output_text") || hasNonNull(part, "annotations", "logprobs", "top_logprobs") {
					return "", nil, nil, false
				}
				if frame.event == "response.content_part.done" {
					if !partAdded || !textDone || partDone || !jsonStringEquals(part["text"], text.String()) {
						return "", nil, nil, false
					}
					partDone = true
					patches = append(patches, correctionSSEPatch{i, []string{"part", "text"}, true})
				} else {
					if !itemAdded || partAdded || len(part["text"]) != 0 && !jsonStringEquals(part["text"], "") {
						return "", nil, nil, false
					}
					partAdded = true
				}
			case "response.output_text.delta", "response.output_text.done":
				if !partAdded || !correctionSSEIndex(root["output_index"]) || !correctionSSEIndex(root["content_index"]) || outputDone {
					return "", nil, nil, false
				}
				if frame.event == "response.output_text.delta" {
					part, good := correctionSSEString(root["delta"])
					if !good || textDone || partDone {
						return "", nil, nil, false
					}
					text.WriteString(part)
					patches = append(patches, correctionSSEPatch{i, []string{"delta"}, !deltaSeen})
					deltaSeen = true
				} else {
					if !deltaSeen || textDone || partDone || !jsonStringEquals(root["text"], text.String()) {
						return "", nil, nil, false
					}
					textDone = true
					patches = append(patches, correctionSSEPatch{i, []string{"text"}, true})
				}
			case "response.completed":
				original, _, good := correctionText(cjkevi.SurfaceResponses, root["response"])
				if !created || !itemAdded || !partAdded || !deltaSeen || !textDone || !partDone || !outputDone || !good || original != text.String() || i != len(frames)-1 {
					return "", nil, nil, false
				}
				patches = append(patches, correctionSSEPatch{i, []string{"response", "output", "0", "content", "0", "text"}, true})
				response, _ := correctionSSEObject(root["response"])
				if len(response["output_text"]) > 0 {
					patches = append(patches, correctionSSEPatch{i, []string{"response", "output_text"}, true})
				}
				completed = true
			default:
				return "", nil, nil, false
			}
		}
		valid = completed
	default:
		return "", nil, nil, false
	}
	return text.String(), patches, frames, valid
}

func correctionSSEOutputText(raw json.RawMessage, text string) bool {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 1 {
		return false
	}
	part, ok := correctionSSEObject(parts[0])
	return ok && jsonStringEquals(part["type"], "output_text") && jsonStringEquals(part["text"], text) && !hasNonNull(part, "annotations", "logprobs", "top_logprobs")
}
