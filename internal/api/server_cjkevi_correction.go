package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func newCorrection(cfg config.CJKeviCorrectionConfig) (*cjkevi.Repairer, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	jevAPIKey, llmAPIKey := cfg.JevAPIKey, cfg.LLMAPIKey
	if jevAPIKey == "" {
		jevAPIKey = os.Getenv(cfg.JevKeyEnv)
	}
	if llmAPIKey == "" {
		llmAPIKey = os.Getenv(cfg.LLMKeyEnv)
	}
	return cjkevi.NewRepairer(cjkevi.RepairOptions{
		JevURL: cfg.JevURL, JevModel: cfg.JevModel, JevAPIKey: jevAPIKey,
		LLMURL: cfg.LLMURL, LLMModel: cfg.LLMModel, LLMAPIKey: llmAPIKey,
		LLMProtocol: cfg.LLMProtocol, IntentThreshold: cfg.IntentThreshold, IntrusionThreshold: cfg.IntrusionThreshold,
	})
}

// correctCJKevi rewrites only complete, bounded assistant text.
func (s *Server) correctCJKevi(surface string) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.cfgMu.RLock()
		repairer := s.correction
		cfg := s.cfg.CJKeviCorrection.WithDefaults()
		s.cfgMu.RUnlock()
		if repairer == nil {
			c.Next()
			return
		}
		event := cjkevi.HistoryEvent{Time: time.Now().UTC(), Surface: surface, Outcome: "bypassed", Reason: "ineligible_request"}
		defer func() {
			if s.correctionHistory != nil {
				s.correctionHistory.Add(event)
			}
		}()
		if c.Request.Header.Get("Content-Encoding") != "" {
			c.Next()
			return
		}
		select {
		case s.correctionSlots <- struct{}{}:
			defer func() { <-s.correctionSlots }()
		default:
			event.Reason = "capacity_full"
			c.Next()
			return
		}

		var request correctionRequestCapture
		if original := c.Request.Body; original != nil {
			c.Request.Body = struct {
				io.Reader
				io.Closer
			}{Reader: io.TeeReader(original, &request), Closer: original}
		}
		writer := &correctionWriter{
			ResponseWriter: c.Writer,
			header:         c.Writer.Header().Clone(),
			status:         c.Writer.Status(),
			size:           -1,
			limit:          cfg.MaxBufferBytes,
			request:        &request,
			bufferStreams:  cfg.BufferStreams,
		}
		c.Writer = writer
		c.Next()
		if writer.passthrough || c.Request.Context().Err() != nil || writer.status < 200 || writer.status >= 300 || request.Overflow || len(request.Body) == 0 || (!isJSONResponse(writer.header) && !(cfg.BufferStreams && isSSEResponse(writer.header))) {
			event.Reason = "ineligible_response"
			writer.commit(nil)
			return
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(request.Body, &input) != nil {
			event.Reason = "ineligible_request"
			writer.commit(nil)
			return
		}
		structured, valid := structuredCorrectionRequest(surface, input)
		if !valid {
			event.Reason = "ineligible_request"
			writer.commit(nil)
			return
		}
		stream := bytes.Equal(bytes.TrimSpace(input["stream"]), []byte("true"))
		if stream != isSSEResponse(writer.header) || (stream && !cfg.BufferStreams) {
			event.Reason = "ineligible_response"
			writer.commit(nil)
			return
		}
		language, conflict := cjkevi.TargetLanguage(request.Body, cfg.DefaultLanguage)
		if conflict {
			event.Reason = "language_conflict"
			writer.commit(nil)
			return
		}
		var text string
		var replace func(string) ([]byte, error)
		var ok bool
		if stream {
			text, replace, ok = correctionSSEText(surface, writer.body)
		} else {
			text, replace, ok = correctionText(surface, writer.body)
		}
		if !ok {
			event.Reason = "ineligible_response"
			writer.commit(nil)
			return
		}
		if structured || surface == cjkevi.SurfaceAnthropic && structuredJSONText(text) {
			value, patch, eligible := structuredCorrectionText(text, language)
			if !eligible {
				if structured && !structuredJSONText(text) {
					event.Reason = "ineligible_request"
				} else {
					event.Reason = "ineligible_response"
				}
				writer.commit(nil)
				return
			}
			outerReplace := replace
			replace = func(revised string) ([]byte, error) {
				updated, err := patch(revised)
				if err != nil {
					return nil, err
				}
				return outerReplace(updated)
			}
			text = value
		}
		changed, decision, err := repairer.Rewrite(c.Request.Context(), text, language)
		event.JevCalled, event.LLMCalled = decision.JevCalled, decision.LLMCalled
		if decision.JevCalled && (decision.IntentionalScore != 0 || decision.LLMCalled || decision.SkipReason == "intentional" || decision.SkipReason == "below_threshold") {
			score := decision.IntentionalScore
			event.IntentionalScore = &score
		}
		if decision.JevCalled && (decision.IntrusionScore != 0 || decision.LLMCalled || decision.SkipReason == "below_threshold") {
			score := decision.IntrusionScore
			event.IntrusionScore = &score
		}
		if err != nil {
			event.Outcome, event.Reason = "failed", "provider_or_rewrite_error"
			writer.commit(nil)
			return
		}
		if changed == text {
			event.Reason = decision.SkipReason
			switch event.Reason {
			case "unsupported_language", "invalid_input", "no_candidate", "multiple_candidates":
				event.Outcome = "bypassed"
			case "intentional", "below_threshold", "unchanged":
				event.Outcome = "jev-reviewed"
			default:
				event.Reason = "no_change"
			}
			writer.commit(nil)
			return
		}
		patched, err := replace(changed)
		if err != nil || len(patched) > cfg.MaxBufferBytes {
			event.Outcome, event.Reason = "failed", "replacement_invalid"
			writer.commit(nil)
			return
		}
		event.Outcome, event.Reason = "corrected", "replacement_applied"
		for _, header := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
			writer.header.Del(header)
		}
		writer.header.Set("Content-Length", strconv.Itoa(len(patched)))
		writer.commit(patched)
	}
}

func structuredCorrectionRequest(surface string, input map[string]json.RawMessage) (bool, bool) {
	var format json.RawMessage
	switch surface {
	case cjkevi.SurfaceChat:
		format = input["response_format"]
	case cjkevi.SurfaceResponses:
		if len(input["text"]) != 0 {
			var text map[string]json.RawMessage
			if json.Unmarshal(input["text"], &text) != nil {
				return false, false
			}
			format = text["format"]
		}
	}
	if len(format) == 0 || bytes.Equal(format, []byte("null")) {
		return false, true
	}
	var field map[string]json.RawMessage
	if json.Unmarshal(format, &field) != nil || field == nil {
		return false, false
	}
	if jsonStringEquals(field["type"], "text") {
		return false, true
	}
	structured := jsonStringEquals(field["type"], "json_schema") || jsonStringEquals(field["type"], "json_object")
	return structured, structured
}

func isSSEResponse(header http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]), "text/event-stream") && header.Get("Content-Encoding") == ""
}

func isJSONResponse(header http.Header) bool {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
	return (contentType == "application/json" || strings.HasSuffix(contentType, "+json")) && header.Get("Content-Encoding") == ""
}

const correctionRequestLimit = 1 << 20

type correctionRequestCapture struct {
	Body     []byte
	Overflow bool
}

func (r *correctionRequestCapture) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := correctionRequestLimit - len(r.Body); n > remaining {
		r.Overflow = true
		p = p[:max(remaining, 0)]
	}
	r.Body = append(r.Body, p...)
	return n, nil
}

// correctionWriter holds only a bounded response and commits immediately for unbuffered streams or overflow.
type correctionWriter struct {
	gin.ResponseWriter
	header          http.Header
	body            []byte
	status          int
	size            int
	limit           int
	request         *correctionRequestCapture
	wrote           bool
	passthrough     bool
	bufferStreams   bool
	flushed         bool
	streamChecked   bool
	streamRequested bool
}

func (w *correctionWriter) bufferingSSE() bool {
	if !w.bufferStreams || !isSSEResponse(w.header) {
		return false
	}
	if !w.streamChecked {
		w.streamRequested = requestStreams(w.request.Body)
		w.streamChecked = true
	}
	return w.streamRequested
}

func (w *correctionWriter) Header() http.Header {
	if w.passthrough {
		return w.ResponseWriter.Header()
	}
	return w.header
}
func (w *correctionWriter) Status() int   { return w.status }
func (w *correctionWriter) Size() int     { return w.size }
func (w *correctionWriter) Written() bool { return w.wrote }
func (w *correctionWriter) WriteHeader(status int) {
	if w.passthrough {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if !w.wrote && status > 0 {
		w.status = status
	}
	if status < 200 || status >= 300 {
		w.commit(nil)
	}
}
func (w *correctionWriter) WriteHeaderNow() {
	if w.passthrough {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	w.wrote = true
	if w.size < 0 {
		w.size = 0
	}
}
func (w *correctionWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *correctionWriter) Write(p []byte) (int, error) {
	if !w.passthrough && (w.request.Overflow || len(p) > w.limit-len(w.body) || ((strings.HasPrefix(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream") || requestStreams(w.request.Body)) && !w.bufferingSSE())) {
		w.commit(nil)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(p)
	}
	w.WriteHeaderNow()
	w.body = append(w.body, p...)
	w.size += len(p)
	return len(p), nil
}
func (w *correctionWriter) Flush() {
	if !w.passthrough && w.bufferingSSE() {
		w.flushed = true
		return
	}
	w.commit(nil)
	w.ResponseWriter.Flush()
}
func (w *correctionWriter) commit(replacement []byte) {
	if w.passthrough {
		return
	}
	w.passthrough = true
	out := w.ResponseWriter.Header()
	for key := range out {
		delete(out, key)
	}
	for key, values := range w.header {
		out[key] = append([]string(nil), values...)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if w.wrote {
		w.ResponseWriter.WriteHeaderNow()
	}
	if replacement != nil {
		_, _ = w.ResponseWriter.Write(replacement)
	} else if len(w.body) != 0 {
		_, _ = w.ResponseWriter.Write(w.body)
	}
	w.body = nil
	if w.flushed {
		w.ResponseWriter.Flush()
	}
}

func requestStreams(body []byte) bool {
	var input struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &input) == nil && input.Stream
}

// correctionText limits edits to a single completed assistant text field.
func correctionText(surface string, body []byte) (string, func(string) ([]byte, error), bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return "", nil, false
	}
	var container map[string]json.RawMessage
	var content map[string]json.RawMessage
	var items []json.RawMessage
	var field string
	switch surface {
	case cjkevi.SurfaceChat:
		if json.Unmarshal(root["choices"], &items) != nil || len(items) != 1 || json.Unmarshal(items[0], &container) != nil || !jsonStringEquals(container["finish_reason"], "stop") || hasNonNull(container, "logprobs") || json.Unmarshal(container["message"], &content) != nil || !jsonStringEquals(content["role"], "assistant") || hasNonNull(content, "tool_calls", "function_call", "refusal", "reasoning", "reasoning_content", "annotations", "audio") {
			return "", nil, false
		}
		field = "content"
	case cjkevi.SurfaceAnthropic:
		if !jsonStringEquals(root["type"], "message") || !jsonStringEquals(root["role"], "assistant") || !jsonStringEquals(root["stop_reason"], "end_turn") || json.Unmarshal(root["content"], &items) != nil || len(items) != 1 || json.Unmarshal(items[0], &content) != nil || !jsonStringEquals(content["type"], "text") || hasNonNull(content, "citations") {
			return "", nil, false
		}
		field = "text"
	case cjkevi.SurfaceResponses:
		if !jsonStringEquals(root["status"], "completed") || json.Unmarshal(root["output"], &items) != nil || len(items) != 1 || json.Unmarshal(items[0], &container) != nil || !jsonStringEquals(container["type"], "message") || !jsonStringEquals(container["role"], "assistant") || (hasNonNull(container, "status") && !jsonStringEquals(container["status"], "completed")) {
			return "", nil, false
		}
		var blocks []json.RawMessage
		if json.Unmarshal(container["content"], &blocks) != nil || len(blocks) != 1 || json.Unmarshal(blocks[0], &content) != nil || !jsonStringEquals(content["type"], "output_text") || hasNonNull(root, "logprobs", "top_logprobs") || hasNonNull(container, "logprobs", "top_logprobs") || hasNonNull(content, "annotations", "logprobs", "top_logprobs") {
			return "", nil, false
		}
		field = "text"
	default:
		return "", nil, false
	}
	var original string
	if json.Unmarshal(content[field], &original) != nil || strings.TrimSpace(original) == "" {
		return "", nil, false
	}
	rootText := root["output_text"]
	if surface == cjkevi.SurfaceResponses && len(rootText) != 0 && !jsonStringEquals(rootText, original) {
		return "", nil, false
	}
	return original, func(replacement string) ([]byte, error) {
		encoded, err := json.Marshal(replacement)
		if err != nil {
			return nil, err
		}
		content[field] = encoded
		if surface == cjkevi.SurfaceResponses && len(rootText) != 0 {
			root["output_text"] = encoded
		}
		encoded, err = json.Marshal(content)
		if err != nil {
			return nil, err
		}
		switch surface {
		case cjkevi.SurfaceChat:
			container["message"] = encoded
			items[0], err = json.Marshal(container)
			if err == nil {
				root["choices"], err = json.Marshal(items)
			}
		case cjkevi.SurfaceAnthropic:
			items[0] = encoded
			root["content"], err = json.Marshal(items)
		case cjkevi.SurfaceResponses:
			container["content"], err = json.Marshal([]json.RawMessage{encoded})
			if err == nil {
				items[0], err = json.Marshal(container)
			}
			if err == nil {
				root["output"], err = json.Marshal(items)
			}
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(root)
	}, true
}

func jsonStringEquals(raw json.RawMessage, expected string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == expected
}

func hasNonNull(item map[string]json.RawMessage, fields ...string) bool {
	for _, field := range fields {
		if raw := item[field]; len(raw) != 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte("[]")) {
			return true
		}
	}
	return false
}
