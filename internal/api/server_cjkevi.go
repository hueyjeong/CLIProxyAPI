package api

import (
	"crypto/sha256"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	log "github.com/sirupsen/logrus"
)

// observeShadow wraps only explicitly registered response routes after authentication.
func (s *Server) observeShadow(surface string) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.cfgMu.RLock()
		observer := s.shadow
		s.cfgMu.RUnlock()
		if observer == nil {
			c.Next()
			return
		}
		if !observer.TryCapture() {
			log.WithFields(log.Fields{"surface": surface, "reason": "capture_slots_full"}).Debug("CJKevi shadow skipped")
			c.Next()
			return
		}
		defer observer.ReleaseCapture()

		var requestCapture cjkevi.RequestCapture
		if original := c.Request.Body; original != nil {
			c.Request.Body = struct {
				io.Reader
				io.Closer
			}{Reader: io.TeeReader(original, &requestCapture), Closer: original}
		}
		writer := cjkevi.NewResponseTee(c.Writer, observer.MaxCaptureBytes())
		c.Writer = writer
		c.Next()

		if c.Request.Context().Err() != nil {
			return
		}
		if requestCapture.Overflow {
			log.WithFields(log.Fields{"surface": surface, "reason": "request_too_large"}).Debug("CJKevi shadow skipped")
			return
		}
		body, overflow := writer.Snapshot()
		if overflow {
			log.WithFields(log.Fields{"surface": surface, "reason": "response_too_large"}).Debug("CJKevi shadow skipped")
			return
		}
		fingerprint := sha256.Sum256(requestCapture.Body)
		observer.Submit(body, requestCapture.Body, fingerprint, c.Writer.Header().Get("Content-Type"), c.Writer.Status(), surface)
	}
}
