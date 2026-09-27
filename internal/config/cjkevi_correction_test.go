package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCJKeviCorrectionDefaultsAndValidation(t *testing.T) {
	var disabled CJKeviCorrectionConfig
	if disabled.Enabled || disabled.BufferStreams || disabled.WithDefaults().BufferStreams || disabled.Validate() != nil {
		t.Fatal("correction and SSE buffering must remain disabled by default")
	}
	t.Setenv("CJKEVI_OPENROUTER_API_KEY", "jev-secret-value")
	t.Setenv("CJKEVI_OPENCODE_API_KEY", "llm-secret-value")
	cfg := CJKeviCorrectionConfig{Enabled: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	defaults := cfg.WithDefaults()
	if defaults.DefaultLanguage != "ko-KR" || defaults.MaxBufferBytes != 64<<10 || defaults.IntentThreshold != .8 || defaults.IntrusionThreshold != .55 || defaults.JevKeyEnv != "CJKEVI_OPENROUTER_API_KEY" || defaults.LLMKeyEnv != "CJKEVI_OPENCODE_API_KEY" || defaults.JevURL != "https://openrouter.ai/api/v1/systemone" || defaults.JevModel != "typesafe/jev-1.13" || defaults.LLMURL != "https://opencode.ai/zen/go/v1/responses" || defaults.LLMModel != "muse-spark-1.3-contributor" || defaults.LLMProtocol != "responses" {
		t.Fatalf("unexpected correction defaults: %+v", defaults)
	}
	for _, option := range []struct{ protocol, url, model string }{
		{"chat", "https://opencode.ai/zen/go/v1/chat/completions", "glm-5.3-flash"},
		{"responses", "https://opencode.ai/zen/go/v1/responses", "muse-spark-1.3-contributor"},
		{"messages", "https://opencode.ai/zen/go/v1/messages", "qwen3.8-flash"},
	} {
		configured := CJKeviCorrectionConfig{Enabled: true, LLMProtocol: option.protocol}
		if err := configured.Validate(); err != nil {
			t.Fatalf("%s defaults rejected: %v", option.protocol, err)
		}
		filled := configured.WithDefaults()
		if filled.LLMURL != option.url || filled.LLMModel != option.model {
			t.Fatalf("incorrect %s defaults: %s %s", option.protocol, filled.LLMURL, filled.LLMModel)
		}
	}
	for _, test := range []struct {
		name   string
		change func(*CJKeviCorrectionConfig)
	}{
		{"language", func(c *CJKeviCorrectionConfig) { c.DefaultLanguage = "xx-XX" }},
		{"buffer", func(c *CJKeviCorrectionConfig) { c.MaxBufferBytes = CJKeviCorrectionMaxBufferBytes + 1 }},
		{"protocol", func(c *CJKeviCorrectionConfig) { c.LLMProtocol = "other" }},
		{"env name", func(c *CJKeviCorrectionConfig) { c.JevKeyEnv = "INVALID=NAME" }},
		{"missing env", func(c *CJKeviCorrectionConfig) { c.JevKeyEnv = "CJKEVI_TEST_MISSING_KEY" }},
		{"external http", func(c *CJKeviCorrectionConfig) { c.JevURL = "http://example.com/v1/systemone" }},
		{"userinfo", func(c *CJKeviCorrectionConfig) { c.LLMURL = "https://secret@example.com/v1/chat/completions" }},
		{"protocol path", func(c *CJKeviCorrectionConfig) {
			c.LLMProtocol, c.LLMURL = "messages", "https://opencode.ai/zen/go/v1/chat/completions"
		}},
		{"intrusion threshold", func(c *CJKeviCorrectionConfig) { c.IntrusionThreshold = 1.1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := cfg
			test.change(&invalid)
			if err := invalid.Validate(); err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected safe validation error, got %v", err)
			}
		})
	}
	t.Setenv("CJKEVI_TEST_JEV", "other-secret")
	t.Setenv("CJKEVI_TEST_LLM", "other-secret")
	cfg.JevKeyEnv, cfg.LLMKeyEnv = "CJKEVI_TEST_JEV", "CJKEVI_TEST_LLM"
	cfg.JevURL = "http://127.0.0.1:9876/v1/systemone"
	cfg.LLMProtocol = "messages"
	cfg.LLMURL = "http://localhost:9877/v1/messages"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid loopback targets rejected: %v", err)
	}
}

func TestCJKeviCorrectionConfigCredentialsFromYAML(t *testing.T) {
	t.Setenv("CJKEVI_OPENROUTER_API_KEY", "")
	t.Setenv("CJKEVI_OPENCODE_API_KEY", "")
	body := []byte("cjkevi-correction:\n  enabled: true\n  jev-api-key: jev-yaml-secret\n  llm-api-key: llm-yaml-secret\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config file credentials should validate without environment variables: %v", err)
	}
	parsed, err := ParseConfigBytes(body)
	if err != nil {
		t.Fatalf("YAML credentials should validate without environment variables: %v", err)
	}
	if loaded.CJKeviCorrection.JevAPIKey != "jev-yaml-secret" || loaded.CJKeviCorrection.LLMAPIKey != "llm-yaml-secret" || parsed.CJKeviCorrection.JevAPIKey != loaded.CJKeviCorrection.JevAPIKey || parsed.CJKeviCorrection.LLMAPIKey != loaded.CJKeviCorrection.LLMAPIKey {
		t.Fatal("YAML credentials were not loaded consistently")
	}
	jsonConfig, err := json.Marshal(loaded.CJKeviCorrection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonConfig), "jev-yaml-secret") || strings.Contains(string(jsonConfig), "llm-yaml-secret") {
		t.Fatalf("credentials leaked through config JSON: %s", jsonConfig)
	}
	yamlConfig, err := yaml.Marshal(loaded.CJKeviCorrection)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(yamlConfig), "jev-yaml-secret") || !strings.Contains(string(yamlConfig), "llm-yaml-secret") {
		t.Fatalf("YAML credentials did not persist: %s", yamlConfig)
	}
	missing := CJKeviCorrectionConfig{Enabled: true}
	if err := missing.Validate(); err == nil {
		t.Fatal("enabled correction accepted missing provider credentials")
	}
}

func TestCJKeviCorrectionConfigLoadParseAndSecrets(t *testing.T) {
	t.Setenv("CJKEVI_OPENROUTER_API_KEY", "jev-secret-value")
	t.Setenv("CJKEVI_OPENCODE_API_KEY", "llm-secret-value")
	for _, body := range []string{
		"cjkevi-correction:\n  enabled: true\n",
		"cjkevi-correction:\n  enabled: true\n  buffer-streams: true\n",
		"cjkevi-correction:\n  enabled: true\n  max-buffer-bytes: 1048577\n",
		"cjkevi-shadow:\n  enabled: true\ncjkevi-correction:\n  enabled: true\n",
	} {
		file := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, loadErr := LoadConfig(file)
		parsed, parseErr := ParseConfigBytes([]byte(body))
		if (loadErr != nil) != (parseErr != nil) {
			t.Fatalf("load/parse validation mismatch: load=%v parse=%v", loadErr, parseErr)
		}
		if strings.Contains(body, "1048577") || strings.Contains(body, "cjkevi-shadow") {
			if loadErr == nil {
				t.Fatal("invalid correction config accepted")
			}
			continue
		}
		if loaded == nil || parsed == nil || !loaded.CJKeviCorrection.Enabled || !parsed.CJKeviCorrection.Enabled || loaded.CJKeviCorrection.BufferStreams != strings.Contains(body, "buffer-streams: true") || parsed.CJKeviCorrection.BufferStreams != strings.Contains(body, "buffer-streams: true") {
			t.Fatal("correction config or buffer-streams flag not loaded")
		}
		for _, secret := range []string{"jev-secret-value", "llm-secret-value"} {
			if strings.Contains(body, secret) || strings.Contains(loaded.CJKeviCorrection.JevKeyEnv, secret) || strings.Contains(loaded.CJKeviCorrection.LLMKeyEnv, secret) {
				t.Fatal("secret value leaked into persisted configuration")
			}
		}
	}
}
