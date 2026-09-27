package management

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func correctionSettings(cfg config.CJKeviCorrectionConfig) gin.H {
	return gin.H{
		"enabled": cfg.Enabled, "buffer-streams": cfg.BufferStreams,
		"default-language": cfg.DefaultLanguage, "max-buffer-bytes": cfg.MaxBufferBytes,
		"jev-url": cfg.JevURL, "jev-model": cfg.JevModel, "jev-key-env": cfg.JevKeyEnv,
		"llm-url": cfg.LLMURL, "llm-model": cfg.LLMModel, "llm-protocol": cfg.LLMProtocol,
		"llm-key-env": cfg.LLMKeyEnv, "intent-threshold": cfg.IntentThreshold,
		"intrusion-threshold": cfg.IntrusionThreshold,
		"has-jev-api-key":     cfg.JevAPIKey != "" || os.Getenv(cfg.JevKeyEnv) != "",
		"has-llm-api-key":     cfg.LLMAPIKey != "" || os.Getenv(cfg.LLMKeyEnv) != "",
	}
}

func (h *Handler) GetCJKeviSettings(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg.CJKeviCorrection.WithDefaults()
	h.mu.Unlock()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, correctionSettings(cfg))
}

// PutCJKeviSettings merges supplied fields, validates the full configuration, and saves it.
// Empty secret values are never interpreted as deletion.
func (h *Handler) PutCJKeviSettings(c *gin.Context) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings"})
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings"})
		return
	}
	h.mu.Lock()
	candidate := h.cfg.CJKeviCorrection
	for name, raw := range fields {
		var err error
		switch name {
		case "enabled":
			err = json.Unmarshal(raw, &candidate.Enabled)
		case "buffer-streams":
			err = json.Unmarshal(raw, &candidate.BufferStreams)
		case "default-language":
			err = json.Unmarshal(raw, &candidate.DefaultLanguage)
		case "max-buffer-bytes":
			err = json.Unmarshal(raw, &candidate.MaxBufferBytes)
		case "jev-url":
			err = json.Unmarshal(raw, &candidate.JevURL)
		case "jev-model":
			err = json.Unmarshal(raw, &candidate.JevModel)
		case "jev-key-env":
			err = json.Unmarshal(raw, &candidate.JevKeyEnv)
		case "llm-url":
			err = json.Unmarshal(raw, &candidate.LLMURL)
		case "llm-model":
			err = json.Unmarshal(raw, &candidate.LLMModel)
		case "llm-protocol":
			err = json.Unmarshal(raw, &candidate.LLMProtocol)
		case "llm-key-env":
			err = json.Unmarshal(raw, &candidate.LLMKeyEnv)
		case "intent-threshold":
			err = json.Unmarshal(raw, &candidate.IntentThreshold)
		case "intrusion-threshold":
			err = json.Unmarshal(raw, &candidate.IntrusionThreshold)
		case "jev-api-key", "llm-api-key":
			var value string
			err = json.Unmarshal(raw, &value)
			if err == nil && strings.TrimSpace(value) == "" {
				err = io.ErrUnexpectedEOF
			}
			if err == nil {
				if name == "jev-api-key" {
					candidate.JevAPIKey = value
				} else {
					candidate.LLMAPIKey = value
				}
			}
		default:
			err = io.ErrUnexpectedEOF
		}
		if err != nil || string(raw) == "null" {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings field"})
			return
		}
	}
	full := *h.cfg
	full.CJKeviCorrection = candidate
	if err := full.ValidateCJKeviCorrection(); err != nil {
		h.mu.Unlock()
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid correction configuration"})
		return
	}
	previous := h.cfg.CJKeviCorrection
	h.cfg.CJKeviCorrection = candidate
	snapshot, ok := h.saveConfigAndSnapshotLocked(c)
	if !ok {
		h.cfg.CJKeviCorrection = previous
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, correctionSettings(candidate.WithDefaults()))
}
