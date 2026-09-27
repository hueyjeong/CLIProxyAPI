package cjkevi

import (
	"bytes"
	"reflect"
	"testing"
)

func TestResponsesTextOnlyCompletedAssistantOutput(t *testing.T) {
	body := []byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Visible 中"},{"type":"reasoning","text":"hidden analysis"}]},{"type":"message","role":"user","content":[{"type":"output_text","text":"hidden user content"}]},{"type":"function_call","status":"completed","name":"SendMessage","arguments":"{\"message\":\"Visible tool 中\",\"command\":\"hidden code\"}"},{"type":"function_call","status":"completed","name":"OtherTool","arguments":"{\"message\":\"hidden\"}"},{"type":"function_call","status":"in_progress","name":"SendMessage","arguments":"{\"message\":\"hidden partial\"}"}]}`)
	original := append([]byte(nil), body...)
	got := responsesText(body, "application/json; charset=utf-8")
	if !reflect.DeepEqual(got, []string{"Visible 中", "Visible tool 中"}) {
		t.Fatalf("responsesText() = %#v", got)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("observer modified the response bytes")
	}

	for _, status := range []string{"incomplete", "failed", "cancelled"} {
		payload := []byte(`{"status":"` + status + `","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"must not escape"}]}]}`)
		if got := responsesText(payload, "application/json"); len(got) != 0 {
			t.Errorf("status %q exposed partial output: %#v", status, got)
		}
	}
	if got := responsesText(body, "text/plain"); len(got) != 0 {
		t.Fatalf("unsupported content type exposed output: %#v", got)
	}
}

func TestResponsesStreamWaitsForCompletionAndPreservesBytes(t *testing.T) {
	stream := sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "split ",
	}) + sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "中",
	}) + sse("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": "split 中",
	}) + sse("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}},
	})
	streamBytes := []byte(stream)
	marker := []byte("中")
	utf8Start := bytes.Index(streamBytes, marker)
	if utf8Start < 0 {
		t.Fatal("missing UTF-8 test marker")
	}
	var fixture bytes.Buffer
	for _, part := range [][]byte{streamBytes[:utf8Start+1], streamBytes[utf8Start+1 : utf8Start+2], streamBytes[utf8Start+2:]} {
		_, _ = fixture.Write(part)
	}
	body := fixture.Bytes()
	original := append([]byte(nil), body...)
	if got := responsesText(body, "text/event-stream"); !reflect.DeepEqual(got, []string{"split 中"}) {
		t.Fatalf("completed stream = %#v", got)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("observer modified the SSE bytes")
	}

	withoutCompletion := []byte(sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "partial",
	}))
	if got := responsesText(withoutCompletion, "text/event-stream"); len(got) != 0 {
		t.Fatalf("stream without response.completed exposed output: %#v", got)
	}
	withoutTextDone := []byte(sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "partial",
	}) + sse("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}},
	}))
	if got := responsesText(withoutTextDone, "text/event-stream"); len(got) != 0 {
		t.Fatalf("undone text block exposed output: %#v", got)
	}
}

func TestResponsesStreamUsesCompletedOutputAndSendMessageOnly(t *testing.T) {
	stream := sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "partial text",
	}) + sse("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": "partial text",
	}) + sse("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"status": "completed",
			"output": []any{
				map[string]any{"type": "message", "role": "assistant", "content": []any{
					map[string]any{"type": "output_text", "text": "final 中"},
					map[string]any{"type": "reasoning", "text": "hidden"},
				}},
				map[string]any{"type": "function_call", "status": "completed", "name": "SendMessage", "arguments": `{"message":"tool 中","command":"hidden"}`},
				map[string]any{"type": "function_call", "status": "completed", "name": "Other", "arguments": `{"message":"hidden"}`},
			},
		},
	})
	if got := responsesText([]byte(stream), "text/event-stream"); !reflect.DeepEqual(got, []string{"final 中", "tool 中"}) {
		t.Fatalf("completed output = %#v", got)
	}
}

func TestResponsesStreamRejectsFailedIncompleteAndCancelled(t *testing.T) {
	prefix := sse("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "hidden",
	}) + sse("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": "hidden",
	})
	for _, eventName := range []string{"response.failed", "response.incomplete", "response.cancelled"} {
		stream := prefix + sse(eventName, map[string]any{"type": eventName})
		if got := responsesText([]byte(stream), "text/event-stream"); len(got) != 0 {
			t.Errorf("%s stream exposed output: %#v", eventName, got)
		}
	}
	incompleteCompletion := prefix + sse("response.completed", map[string]any{
		"type": "response.completed", "response": map[string]any{"status": "incomplete", "output": []any{}},
	})
	if got := responsesText([]byte(incompleteCompletion), "text/event-stream"); len(got) != 0 {
		t.Fatalf("incomplete response.completed exposed output: %#v", got)
	}
}
