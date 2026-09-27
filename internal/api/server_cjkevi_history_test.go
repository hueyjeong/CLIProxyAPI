package api

import (
	"context"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCJKeviManagementHistoryAndSettings(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "management-test-secret"
	server := newTestServerWithConfig(t, cfg, WithLocalManagementPassword("management-test-secret"))
	defer server.Stop(context.Background())
	initial, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.configFilePath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		if key != "" {
			r.Header.Set("X-Management-Key", key)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/v0/management/cjkevi/history", "/v0/management/cjkevi/settings"} {
		if w := request(http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s: %d", path, w.Code)
		}
	}
	if w := request(http.MethodPut, "/v0/management/cjkevi/settings", `{ "enabled":true }`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT: %d", w.Code)
	}
	key := "management-test-secret"
	if w := request(http.MethodGet, "/v0/management/cjkevi/history", "", key); w.Code != 200 || w.Body.String() != `{"events":[]}` {
		t.Fatalf("empty history: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, "/v0/management/cjkevi/settings", `{"jev-api-key":"jev-private","llm-api-key":"llm-private","enabled":true,"default-language":"invalid"}`, key); w.Code != 422 {
		t.Fatalf("invalid settings accepted: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, "/v0/management/cjkevi/settings", `{"jev-api-key":"jev-private","llm-api-key":"llm-private","enabled":true}`, key); w.Code != 200 {
		t.Fatalf("settings PUT: %d %s", w.Code, w.Body.String())
	}
	if server.correction == nil {
		t.Fatal("settings did not hot reload correction")
	}
	if w := request(http.MethodPut, "/v0/management/cjkevi/settings", `{"buffer-streams":true,"jev-api-key":""}`, key); w.Code != 400 {
		t.Fatalf("empty secret accepted: %d", w.Code)
	}
	if w := request(http.MethodPut, "/v0/management/cjkevi/settings", `{"buffer-streams":true}`, key); w.Code != 200 {
		t.Fatalf("partial PUT: %d %s", w.Code, w.Body.String())
	}
	w := request(http.MethodGet, "/v0/management/cjkevi/settings", "", key)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("GET leaked secret: %d %s", w.Code, w.Body.String())
	}
	var settings map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil || settings["has-jev-api-key"] != true || settings["has-llm-api-key"] != true || settings["buffer-streams"] != true {
		t.Fatalf("settings GET: %v %v", settings, err)
	}
	data, err := os.ReadFile(server.configFilePath)
	if err != nil || !strings.Contains(string(data), "jev-private") || !strings.Contains(string(data), "llm-private") {
		t.Fatalf("settings not saved: %v", err)
	}
	server.correctionHistory.Add(cjkevi.HistoryEvent{Outcome: "corrected", Reason: "replacement_applied", Surface: cjkevi.SurfaceChat})
	server.UpdateClientsContext(context.Background(), server.getConfig().CloneForRuntime())
	w = request(http.MethodGet, "/v0/management/cjkevi/history", "", key)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "replacement_applied") {
		t.Fatalf("history after reload: %d %s", w.Code, w.Body.String())
	}
}
