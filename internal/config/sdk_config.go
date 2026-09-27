// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"fmt"
	"net"
	"net/url"
	"time"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// CodexResponseSteering mirrors the provider-wide runtime setting for API handlers.
	CodexResponseSteering bool `yaml:"-" json:"-"`

	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOptimizeMultiAgentV2 mirrors the provider-wide runtime setting for API handlers.
	CodexOptimizeMultiAgentV2 bool `yaml:"-" json:"-"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// CJKeviShadow observes completed Messages, Chat, and Responses text without changing it.
	CJKeviShadow CJKeviShadowConfig `yaml:"cjkevi-shadow" json:"cjkevi-shadow"`

	// CJKeviCorrection rewrites eligible completed assistant text when enabled.
	CJKeviCorrection CJKeviCorrectionConfig `yaml:"cjkevi-correction" json:"cjkevi-correction"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

const CJKeviRequestCaptureLimit = 16 << 10

const CJKeviQueueByteLimit = 16 << 20

// CJKeviShadowConfig controls optional, local-only shadow observation.
type CJKeviShadowConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	DefaultLanguage string `yaml:"default-language" json:"default-language"`
	MaxCaptureBytes int    `yaml:"max-capture-bytes" json:"max-capture-bytes"`
	QueueSize       int    `yaml:"queue-size" json:"queue-size"`
	Workers         int    `yaml:"workers" json:"workers"`
	JudgeTimeout    string `yaml:"judge-timeout" json:"judge-timeout"`
	KevURL          string `yaml:"kev-url" json:"kev-url"`
	KevModel        string `yaml:"kev-model" json:"kev-model"`
	ModelVersion    string `yaml:"model-version" json:"model-version"`
}

// WithDefaults supplies bounded limits without enabling observation or a judge.
func (c CJKeviShadowConfig) WithDefaults() CJKeviShadowConfig {
	if c.DefaultLanguage == "" {
		c.DefaultLanguage = "ko-KR"
	}
	if c.MaxCaptureBytes == 0 {
		c.MaxCaptureBytes = 64 << 10
	}
	if c.QueueSize == 0 {
		c.QueueSize = 32
	}
	if c.Workers == 0 {
		c.Workers = 2
	}
	if c.JudgeTimeout == "" {
		c.JudgeTimeout = "2s"
	}
	if c.KevModel == "" {
		c.KevModel = "jaredpalmer/kev-0.8b"
	}
	if c.ModelVersion == "" {
		c.ModelVersion = "local-v1"
	}
	return c
}

func ValidCJKeviLanguage(language string) bool {
	switch language {
	case "ko-KR", "ja-JP", "zh-CN", "zh-TW", "en-US":
		return true
	default:
		return false
	}
}

// Validate refuses unbounded collection and any non-loopback judge endpoint.
func (c CJKeviShadowConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.WithDefaults()
	if !ValidCJKeviLanguage(c.DefaultLanguage) {
		return fmt.Errorf("cjkevi-shadow.default-language must be an allowed language")
	}
	if c.MaxCaptureBytes < 1 || c.MaxCaptureBytes > 1<<20 || c.QueueSize < 1 || c.QueueSize > 512 || c.Workers < 1 || c.Workers > 8 {
		return fmt.Errorf("cjkevi-shadow capture, queue, or worker limit is out of range")
	}
	if int64(c.QueueSize)*int64(c.MaxCaptureBytes+CJKeviRequestCaptureLimit) > CJKeviQueueByteLimit {
		return fmt.Errorf("cjkevi-shadow queued response/request bytes exceed 16 MiB")
	}
	timeout, errTimeout := time.ParseDuration(c.JudgeTimeout)
	if errTimeout != nil || timeout <= 0 || timeout > 10*time.Second {
		return fmt.Errorf("cjkevi-shadow.judge-timeout must be between 0 and 10s")
	}
	if len(c.ModelVersion) > 128 || len(c.KevModel) > 128 {
		return fmt.Errorf("cjkevi-shadow model name or version is too long")
	}
	if c.KevURL != "" {
		u, errURL := url.Parse(c.KevURL)
		if errURL != nil || u.Scheme != "http" || u.User != nil || u.Path != "/v1/systemone" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return fmt.Errorf("cjkevi-shadow.kev-url must be a local HTTP /v1/systemone URL")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || u.Port() == "" {
			return fmt.Errorf("cjkevi-shadow.kev-url must use a loopback IP and explicit port")
		}
	}
	return nil
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
