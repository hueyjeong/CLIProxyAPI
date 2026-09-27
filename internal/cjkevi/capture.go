package cjkevi

import "github.com/gin-gonic/gin"

// ResponseTee forwards writes before retaining at most limit bytes for observation.
// It never owns the client response or alters its status, headers, or flushes.
type ResponseTee struct {
	gin.ResponseWriter
	body     []byte
	limit    int
	overflow bool
}

func NewResponseTee(writer gin.ResponseWriter, limit int) *ResponseTee {
	return &ResponseTee{ResponseWriter: writer, limit: limit}
}

func (w *ResponseTee) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture(p[:n])
	return n, err
}

func (w *ResponseTee) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.capture([]byte(s[:n]))
	return n, err
}

func (w *ResponseTee) Flush() {
	w.ResponseWriter.Flush()
}

func (w *ResponseTee) capture(p []byte) {
	remaining := w.limit - len(w.body)
	if len(p) > remaining {
		w.overflow = true
		p = p[:max(remaining, 0)]
	}
	w.body = append(w.body, p...)
}

func (w *ResponseTee) Snapshot() ([]byte, bool) {
	return append([]byte(nil), w.body...), w.overflow
}
