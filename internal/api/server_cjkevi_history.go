package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
)

func (s *Server) getCJKeviHistory(c *gin.Context) {
	if s.correctionHistory == nil {
		c.JSON(http.StatusOK, gin.H{"events": []cjkevi.HistoryEvent{}})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"events": s.correctionHistory.Snapshot()})
}
