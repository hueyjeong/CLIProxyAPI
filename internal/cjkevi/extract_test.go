package cjkevi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func sse(event string, value any) string {
	body, _ := json.Marshal(value)
	return fmt.Sprintf("event: %s\r\ndata: %s\r\n\r\n", event, body)
}

func TestAnthropicFinalTextExcludesThinkingAndGenericTools(t *testing.T) {
	body := []byte(`{"type":"message","stop_reason":"tool_use","content":[{"type":"text","text":"plain 中간 상황"},{"type":"thinking","text":"秘密"},{"type":"tool_use","name":"Unrelated","input":{"message":"秘密"}},{"type":"tool_use","name":"SendMessage","input":{"message":"中간 상황","path":"秘密"}}]}`)
	got := finalText(body, "application/json; charset=utf-8", SurfaceAnthropic)
	if !reflect.DeepEqual(got, []string{"plain 中간 상황", "中간 상황"}) {
		t.Fatalf("finalText() = %#v", got)
	}
	for _, text := range got {
		spans := candidates(text, "ko-KR")
		if len(spans) != 1 || spans[0].Text != "中" {
			t.Fatalf("candidates(%q) = %#v, want only 中", text, spans)
		}
	}
	if got := finalText([]byte(`{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"中文"}]}`), "application/json", SurfaceAnthropic); len(got) != 0 {
		t.Fatalf("partial message was observed: %#v", got)
	}
}

func TestAnthropicSSESplitUTF8AndToolInput(t *testing.T) {
	stream := sse("message_start", map[string]any{"type": "message_start", "message": map[string]string{"type": "message"}}) +
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}}) +
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "中"}}) +
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "간 상황"}}) +
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}) +
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{}}}) +
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"message":"中`}}) +
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]string{"type": "input_json_delta", "partial_json": `간 상황","command":"秘密"}`}}) +
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}) +
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]string{"type": "thinking"}}) +
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]string{"type": "thinking_delta", "thinking": "秘密"}}) +
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2}) +
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}}) +
		sse("message_stop", map[string]any{"type": "message_stop"})

	utf8Start := strings.Index(stream, "中")
	if utf8Start < 0 {
		t.Fatal("missing UTF-8 test marker")
	}
	var recorder bytes.Buffer
	for _, part := range [][]byte{[]byte(stream[:utf8Start+1]), []byte(stream[utf8Start+1 : utf8Start+2]), []byte(stream[utf8Start+2:])} {
		_, _ = recorder.Write(part)
	}
	got := finalText(recorder.Bytes(), "text/event-stream", SurfaceAnthropic)
	if !reflect.DeepEqual(got, []string{"中간 상황", "中간 상황"}) {
		t.Fatalf("split SSE extraction = %#v", got)
	}
	if got := finalText([]byte(strings.Replace(stream, "event: content_block_stop\r\ndata: {\"index\":0,\"type\":\"content_block_stop\"}\r\n\r\n", "", 1)), "text/event-stream", SurfaceAnthropic); len(got) != 0 {
		t.Fatalf("unfinished block was observed: %#v", got)
	}
	if got := finalText([]byte(stream[:len(stream)-len(sse("message_stop", map[string]any{"type": "message_stop"}))]), "text/event-stream", SurfaceAnthropic); len(got) != 0 {
		t.Fatalf("incomplete SSE was observed: %#v", got)
	}
}

func TestChatExtractionOnlyCompletedContentAndSendMessage(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"中간 상황","reasoning_content":"秘密","tool_calls":[{"function":{"name":"SendMessage","arguments":"{\"message\":\"中간 상황\",\"command\":\"秘密\"}"}},{"function":{"name":"Other","arguments":"{\"message\":\"秘密\"}"}}]}}]}`)
	if got := finalText(body, "application/json", SurfaceChat); !reflect.DeepEqual(got, []string{"中간 상황", "中간 상황"}) {
		t.Fatalf("chat response = %#v", got)
	}
	if got := finalText([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"中文"}}]}`), "application/json", SurfaceChat); len(got) != 0 {
		t.Fatalf("truncated chat reply was observed: %#v", got)
	}

	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中\",\"reasoning_content\":\"秘密\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"간 상황\",\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"SendMessage\",\"arguments\":\"{\\\"message\\\":\\\"中\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"간 상황\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	if got := finalText([]byte(stream), "text/event-stream", SurfaceChat); !reflect.DeepEqual(got, []string{"中간 상황", "中간 상황"}) {
		t.Fatalf("chat SSE = %#v", got)
	}
	if got := finalText([]byte(strings.TrimSuffix(stream, "data: [DONE]\n\n")), "text/event-stream", SurfaceChat); len(got) != 0 {
		t.Fatalf("unfinished chat SSE was observed: %#v", got)
	}
}

func TestMixedCaseContentTypesPreserveExtraction(t *testing.T) {
	for _, test := range []struct {
		surface string
		body    string
	}{
		{SurfaceAnthropic, `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"中간 상황"}]}`},
		{SurfaceChat, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中간 상황"}}]}`},
		{SurfaceResponses, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"中간 상황"}]}]}`},
	} {
		if got := finalText([]byte(test.body), "Application/JSON; Charset=utf-8", test.surface); !reflect.DeepEqual(got, []string{"中간 상황"}) {
			t.Fatalf("mixed-case JSON %s: %#v", test.surface, got)
		}
	}
	anthropic := sse("message_start", map[string]any{"type": "message_start", "message": map[string]string{"type": "message"}}) +
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": "中간 상황"}}) +
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}) +
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}}) +
		sse("message_stop", map[string]string{"type": "message_stop"})
	chat := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中간 상황\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"
	responses := sse("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "中간 상황"}}}}}})
	for _, test := range []struct {
		surface string
		body    string
	}{{SurfaceAnthropic, anthropic}, {SurfaceChat, chat}, {SurfaceResponses, responses}} {
		if got := finalText([]byte(test.body), "Text/Event-Stream; Charset=UTF-8", test.surface); !reflect.DeepEqual(got, []string{"中간 상황"}) {
			t.Fatalf("mixed-case SSE %s: %#v", test.surface, got)
		}
	}
}

func TestCandidatesSkipMarkdownAndQuotedText(t *testing.T) {
	text := "> 中文\n```go\n中文\n```\n~~~\n中文\n~~~\n`中文` and ``中文`` and '中文' and \"中文\" plus “中文” and 「中文」 then it's 中간 상황\n"
	spans := candidates(text, "ko-KR")
	if len(spans) != 1 || spans[0].Text != "中" || spans[0].Offset != strings.LastIndex(text, "中간") {
		t.Fatalf("candidates = %#v", spans)
	}
	if strings.Contains(spans[0].Context, "中文") {
		t.Fatalf("quoted text leaked into local context: %q", spans[0].Context)
	}
}

func TestCandidatesFenceAndHTMLCommentBoundaries(t *testing.T) {
	text := "~~~text\n```\n中文\n~~~\n<!-- quoted\n中文\n--> 中간 상황"
	spans := candidates(text, "ko-KR")
	if len(spans) != 1 || spans[0].Text != "中" || spans[0].Offset != strings.LastIndex(text, "中간") {
		t.Fatalf("unexpected candidates outside fences/comments: %#v", spans)
	}
	if strings.Contains(spans[0].Context, "中文") {
		t.Fatalf("HTML comment leaked to context: %q", spans[0].Context)
	}
}

func TestCandidatesSkipMultilineQuote(t *testing.T) {
	text := "Say \"literal\n中文\" then 中간 상황"
	spans := candidates(text, "ko-KR")
	if len(spans) != 1 || spans[0].Text != "中" || spans[0].Offset != strings.LastIndex(text, "中간") {
		t.Fatalf("multiline quote was treated as prose: %#v", spans)
	}
}

func TestResponseTeePreservesBytesHeaderStatusAndFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := NewResponseTee(c.Writer, 8)
	c.Writer = writer
	c.Writer.Header().Set("X-Test", "preserved")
	c.Writer.WriteHeader(http.StatusCreated)
	if _, err := c.Writer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Writer.WriteString("中간"); err != nil {
		t.Fatal(err)
	}
	c.Writer.Flush()
	if got := recorder.Body.String(); got != "first中간" || recorder.Code != http.StatusCreated || recorder.Header().Get("X-Test") != "preserved" || !recorder.Flushed {
		t.Fatalf("wire changed: code=%d headers=%v body=%q flushed=%v", recorder.Code, recorder.Header(), got, recorder.Flushed)
	}
	captured, overflow := writer.Snapshot()
	if !overflow || len(captured) != 8 || string(captured[:5]) != "first" {
		t.Fatalf("tee capture = %q overflow=%v", captured, overflow)
	}
}
