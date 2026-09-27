package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const CJKeviCorrectionMaxBufferBytes = 1 << 20

var correctionEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CJKeviCorrectionConfig enables bounded assistant text correction.
type CJKeviCorrectionConfig struct {
	Enabled            bool    `yaml:"enabled" json:"enabled"`
	BufferStreams      bool    `yaml:"buffer-streams" json:"buffer-streams"`
	DefaultLanguage    string  `yaml:"default-language" json:"default-language"`
	MaxBufferBytes     int     `yaml:"max-buffer-bytes" json:"max-buffer-bytes"`
	JevURL             string  `yaml:"jev-url" json:"jev-url"`
	JevModel           string  `yaml:"jev-model" json:"jev-model"`
	JevAPIKey          string  `yaml:"jev-api-key" json:"-"`
	JevKeyEnv          string  `yaml:"jev-key-env" json:"jev-key-env"`
	LLMURL             string  `yaml:"llm-url" json:"llm-url"`
	LLMModel           string  `yaml:"llm-model" json:"llm-model"`
	LLMProtocol        string  `yaml:"llm-protocol" json:"llm-protocol"`
	LLMAPIKey          string  `yaml:"llm-api-key" json:"-"`
	LLMKeyEnv          string  `yaml:"llm-key-env" json:"llm-key-env"`
	IntentThreshold    float64 `yaml:"intent-threshold" json:"intent-threshold"`
	IntrusionThreshold float64 `yaml:"intrusion-threshold" json:"intrusion-threshold"`
}

func (c CJKeviCorrectionConfig) WithDefaults() CJKeviCorrectionConfig {
	if c.DefaultLanguage == "" {
		c.DefaultLanguage = "ko-KR"
	}
	if c.MaxBufferBytes == 0 {
		c.MaxBufferBytes = 64 << 10
	}
	if c.JevKeyEnv == "" {
		c.JevKeyEnv = "CJKEVI_OPENROUTER_API_KEY"
	}
	if c.LLMKeyEnv == "" {
		c.LLMKeyEnv = "CJKEVI_OPENCODE_API_KEY"
	}
	if c.JevURL == "" {
		c.JevURL = "https://openrouter.ai/api/v1/systemone"
	}
	if c.JevModel == "" {
		c.JevModel = "typesafe/jev-1.13"
	}
	if c.LLMProtocol == "" {
		switch {
		case strings.HasSuffix(c.LLMURL, "/chat/completions"):
			c.LLMProtocol = "chat"
		case strings.HasSuffix(c.LLMURL, "/messages"):
			c.LLMProtocol = "messages"
		default:
			c.LLMProtocol = "responses"
		}
	}
	if c.LLMURL == "" || c.LLMModel == "" {
		endpoint, model := "chat/completions", "glm-5.3-flash"
		switch c.LLMProtocol {
		case "responses":
			endpoint, model = "responses", "muse-spark-1.3-contributor"
		case "messages":
			endpoint, model = "messages", "qwen3.8-flash"
		}
		if c.LLMURL == "" {
			c.LLMURL = "https://opencode.ai/zen/go/v1/" + endpoint
		}
		if c.LLMModel == "" {
			c.LLMModel = model
		}
	}
	if c.IntentThreshold == 0 {
		c.IntentThreshold = .8
	}
	if c.IntrusionThreshold == 0 {
		c.IntrusionThreshold = .55
	}
	return c
}

func (c SDKConfig) ValidateCJKeviCorrection() error {
	if c.CJKeviShadow.Enabled && c.CJKeviCorrection.Enabled {
		return fmt.Errorf("cjkevi-shadow and cjkevi-correction cannot both be enabled")
	}
	return c.CJKeviCorrection.Validate()
}

func (c CJKeviCorrectionConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.WithDefaults()
	if !ValidCJKeviLanguage(c.DefaultLanguage) {
		return fmt.Errorf("cjkevi-correction.default-language must be an allowed language")
	}
	if c.MaxBufferBytes < 1 || c.MaxBufferBytes > CJKeviCorrectionMaxBufferBytes {
		return fmt.Errorf("cjkevi-correction.max-buffer-bytes must be between 1 and %d", CJKeviCorrectionMaxBufferBytes)
	}
	if c.LLMProtocol != "chat" && c.LLMProtocol != "responses" && c.LLMProtocol != "messages" {
		return fmt.Errorf("cjkevi-correction.llm-protocol must be chat, responses, or messages")
	}
	if c.JevModel == "" || c.LLMModel == "" || len(c.JevModel) > 128 || len(c.LLMModel) > 128 {
		return fmt.Errorf("cjkevi-correction model names must be between 1 and 128 characters")
	}
	if math.IsNaN(c.IntentThreshold) || math.IsInf(c.IntentThreshold, 0) || c.IntentThreshold <= 0 || c.IntentThreshold > 1 || math.IsNaN(c.IntrusionThreshold) || math.IsInf(c.IntrusionThreshold, 0) || c.IntrusionThreshold <= 0 || c.IntrusionThreshold > 1 {
		return fmt.Errorf("cjkevi-correction thresholds must be greater than 0 and at most 1")
	}
	for _, credential := range []struct {
		keyName string
		key     string
		envName string
		env     string
	}{
		{keyName: "jev-api-key", key: c.JevAPIKey, envName: "jev-key-env", env: c.JevKeyEnv},
		{keyName: "llm-api-key", key: c.LLMAPIKey, envName: "llm-key-env", env: c.LLMKeyEnv},
	} {
		if credential.key != "" {
			continue
		}
		if !correctionEnvName.MatchString(credential.env) {
			return fmt.Errorf("cjkevi-correction.%s must name an environment variable", credential.envName)
		}
		if os.Getenv(credential.env) == "" {
			return fmt.Errorf("cjkevi-correction.%s is required (environment variable %s is empty)", credential.keyName, credential.envName)
		}
	}
	if err := validateCorrectionURL(c.JevURL, "/systemone"); err != nil {
		return fmt.Errorf("cjkevi-correction.jev-url: %w", err)
	}
	path := map[string]string{"chat": "/chat/completions", "responses": "/responses", "messages": "/messages"}[c.LLMProtocol]
	if err := validateCorrectionURL(c.LLMURL, path); err != nil {
		return fmt.Errorf("cjkevi-correction.llm-url: %w", err)
	}
	return nil
}

func validateCorrectionURL(raw, suffix string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !strings.HasSuffix(u.Path, suffix) || u.Path == "" {
		return fmt.Errorf("invalid transport target")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if (ip != nil && ip.IsLoopback() || u.Hostname() == "localhost") && u.Port() != "" {
			return nil
		}
	}
	return fmt.Errorf("transport target must use HTTPS or loopback HTTP with an explicit port")
}
