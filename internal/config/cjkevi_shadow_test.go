package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCJKeviShadowDefaultsAndValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CJKeviShadow.Enabled || cfg.CJKeviShadow.KevURL != "" {
		t.Fatalf("shadow unexpectedly enabled: %+v", cfg.CJKeviShadow)
	}
	defaults := cfg.CJKeviShadow.WithDefaults()
	if defaults.DefaultLanguage != "ko-KR" || defaults.MaxCaptureBytes != 64<<10 || defaults.QueueSize != 32 || defaults.Workers != 2 || defaults.KevModel != "jaredpalmer/kev-0.8b" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	for _, test := range []struct {
		name string
		cfg  CJKeviShadowConfig
		bad  bool
	}{
		{"loopback", CJKeviShadowConfig{Enabled: true, KevURL: "http://127.0.0.1:9000/v1/systemone"}, false},
		{"ipv6", CJKeviShadowConfig{Enabled: true, KevURL: "http://[::1]:9000/v1/systemone"}, false},
		{"remote", CJKeviShadowConfig{Enabled: true, KevURL: "http://192.0.2.5:9000/v1/systemone"}, true},
		{"dns", CJKeviShadowConfig{Enabled: true, KevURL: "http://localhost:9000/v1/systemone"}, true},
		{"redirect-path", CJKeviShadowConfig{Enabled: true, KevURL: "http://127.0.0.1:9000/else"}, true},
		{"invalid-language", CJKeviShadowConfig{Enabled: true, DefaultLanguage: "xx"}, true},
		{"unbounded", CJKeviShadowConfig{Enabled: true, MaxCaptureBytes: 1 << 21}, true},
		{"queue-byte-budget", CJKeviShadowConfig{Enabled: true, MaxCaptureBytes: 1 << 20, QueueSize: 512}, true},
		{"bounded-queue", CJKeviShadowConfig{Enabled: true, MaxCaptureBytes: 1 << 20, QueueSize: 15}, false},
		{"timeout", CJKeviShadowConfig{Enabled: true, JudgeTimeout: "20s"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); (err != nil) != test.bad {
				t.Fatalf("Validate() = %v; want error %v", err, test.bad)
			}
		})
	}
}

func TestCJKeviShadowConfigLoadAndParseUseSameValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	for _, test := range []struct {
		name string
		yaml string
		bad  bool
	}{
		{"valid", "cjkevi-shadow:\n  enabled: true\n  kev-url: http://127.0.0.1:8008/v1/systemone\n", false},
		{"remote", "cjkevi-shadow:\n  enabled: true\n  kev-url: http://192.0.2.1:8008/v1/systemone\n", true},
		{"queue-budget", "cjkevi-shadow:\n  enabled: true\n  max-capture-bytes: 1048576\n  queue-size: 512\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(test.yaml)
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, errLoad := LoadConfig(file)
			_, errParse := ParseConfigBytes(data)
			if (errLoad != nil) != test.bad || (errParse != nil) != test.bad {
				t.Fatalf("LoadConfig=%v ParseConfigBytes=%v; want both rejected=%v", errLoad, errParse, test.bad)
			}
		})
	}
}
