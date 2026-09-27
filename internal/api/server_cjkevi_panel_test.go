package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCJKeviLocalManagementPanel(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.html")
	const page = "<!doctype html><title>CJKevi management</title>"
	if err := os.WriteFile(file, []byte(page), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.PanelLocalFile = file
	server := newTestServerWithConfig(t, cfg)
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/management.html", nil))
		return w
	}
	w := request()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), page) {
		t.Fatalf("local panel: %d %q", w.Code, w.Body.String())
	}
	cfg.RemoteManagement.DisableControlPanel = true
	w = request()
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled panel: %d", w.Code)
	}
}
