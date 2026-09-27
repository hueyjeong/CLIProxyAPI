package cjkevi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	SurfaceAnthropic = "anthropic-messages"
	SurfaceChat      = "openai-chat-completions"
	SurfaceResponses = "openai-responses"
	MaxCaptureSlots  = 8
	policyVersion    = "shadow-v1"
)

type job struct {
	body        []byte
	request     []byte
	contextHash [32]byte
	contentType string
	surface     string
}

type cacheEntry struct {
	noul   float64
	choice string
	expiry time.Time
}

// Budget bounds queued and in-flight shadow memory across observer reloads.
type Budget struct {
	slots       chan struct{}
	mu          sync.Mutex
	queuedBytes int64
	limit       int64
}

func NewBudget() *Budget {
	return &Budget{slots: make(chan struct{}, MaxCaptureSlots), limit: config.CJKeviQueueByteLimit}
}

func (b *Budget) reserve(size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queuedBytes+size > b.limit {
		return false
	}
	b.queuedBytes += size
	return true
}

func (b *Budget) release(size int64) {
	b.mu.Lock()
	b.queuedBytes -= size
	b.mu.Unlock()
}

// Observer owns a bounded queue and caches only hashes, never response text.
type Observer struct {
	cfg     config.CJKeviShadowConfig
	jobs    chan job
	budget  *Budget
	queueMu sync.Mutex
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	cacheMu sync.Mutex
	cache   map[[32]byte]cacheEntry
}

func NewObserver(cfg config.CJKeviShadowConfig, shared ...*Budget) *Observer {
	if !cfg.Enabled || cfg.Validate() != nil {
		return nil
	}
	cfg = cfg.WithDefaults()
	var budget *Budget
	if len(shared) > 0 && shared[0] != nil {
		budget = shared[0]
	} else {
		budget = NewBudget()
	}
	ctx, cancel := context.WithCancel(context.Background())
	o := &Observer{
		cfg:    cfg,
		jobs:   make(chan job, cfg.QueueSize),
		budget: budget,
		client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		ctx:    ctx,
		cancel: cancel,
		cache:  make(map[[32]byte]cacheEntry),
	}
	for range cfg.Workers {
		go o.run()
	}
	return o
}

func (o *Observer) MaxCaptureBytes() int {
	return o.cfg.MaxCaptureBytes
}

// TryCapture reserves one of the bounded in-flight response capture slots.
func (o *Observer) TryCapture() bool {
	if o == nil || o.ctx.Err() != nil {
		return false
	}
	select {
	case o.budget.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (o *Observer) ReleaseCapture() {
	<-o.budget.slots
}

func (o *Observer) Close() {
	if o == nil {
		return
	}
	o.cancel()
	o.queueMu.Lock()
	for {
		select {
		case j := <-o.jobs:
			o.budget.release(int64(len(j.body) + len(j.request)))
		default:
			o.queueMu.Unlock()
			o.client.CloseIdleConnections()
			return
		}
	}
}

// Submit never waits for a worker. Request data must contain no credentials or headers.
func (o *Observer) Submit(body, request []byte, hash [32]byte, contentType string, status int, surface string) {
	if o == nil || o.ctx.Err() != nil {
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return
	}
	if len(body) > o.cfg.MaxCaptureBytes || len(request) > MaxRequestBytes {
		log.WithFields(log.Fields{"surface": surface, "reason": "capture_too_large"}).Debug("CJKevi shadow skipped")
		return
	}
	o.queueMu.Lock()
	defer o.queueMu.Unlock()
	if o.ctx.Err() != nil {
		return
	}
	size := int64(len(body) + len(request))
	if !o.budget.reserve(size) {
		log.WithFields(log.Fields{"surface": surface, "reason": "queue_byte_budget"}).Debug("CJKevi shadow skipped")
		return
	}
	j := job{body: body, request: request, contextHash: hash, contentType: contentType, surface: surface}
	select {
	case o.jobs <- j:
	default:
		o.budget.release(size)
		log.WithFields(log.Fields{"surface": surface, "reason": "queue_full"}).Debug("CJKevi shadow skipped")
	}
}

func (o *Observer) run() {
	for {
		select {
		case <-o.ctx.Done():
			return
		case j := <-o.jobs:
			if o.ctx.Err() == nil {
				o.observe(j)
			}
			o.budget.release(int64(len(j.body) + len(j.request)))
		}
	}
}

func (o *Observer) observe(j job) {
	language, userLanguage, locked, conflict := requestLanguage(j.request, o.cfg.DefaultLanguage)
	if conflict {
		log.WithFields(log.Fields{"surface": j.surface, "reason": "language_conflict"}).Debug("CJKevi shadow skipped")
		return
	}
	for _, text := range finalText(j.body, j.contentType, j.surface) {
		for _, candidate := range candidates(text, language) {
			if o.ctx.Err() != nil {
				return
			}
			fields := log.Fields{"surface": j.surface, "language": language, "script": candidate.Script, "offset": candidate.Offset}
			if o.cfg.KevURL == "" {
				fields["reason"] = "judge_unconfigured"
				log.WithFields(fields).Debug("CJKevi shadow skipped")
				continue
			}
			key := o.cacheKey(j.contextHash, j.surface, language, candidate.Text, candidate.Context)
			o.cacheMu.Lock()
			entry, hit := o.cache[key]
			if hit && time.Now().Before(entry.expiry) {
				o.cacheMu.Unlock()
				fields["noul"] = entry.noul
				fields["choice"] = entry.choice
				fields["cached"] = true
				log.WithFields(fields).Debug("CJKevi shadow judged")
				continue
			}
			o.cacheMu.Unlock()
			start := time.Now()
			noul, choice, ok := o.judge(j.surface, language, userLanguage, locked, candidate)
			fields["latency_ms"] = time.Since(start).Milliseconds()
			if !ok {
				fields["reason"] = "judge_unavailable"
				log.WithFields(fields).Debug("CJKevi shadow skipped")
				continue
			}
			o.cacheMu.Lock()
			if len(o.cache) >= 512 {
				clear(o.cache)
			}
			o.cache[key] = cacheEntry{noul: noul, choice: choice, expiry: time.Now().Add(5 * time.Minute)}
			o.cacheMu.Unlock()
			fields["noul"] = noul
			fields["choice"] = choice
			log.WithFields(fields).Debug("CJKevi shadow judged")
		}
	}
}

func (o *Observer) cacheKey(contextHash [32]byte, surface, language, text, surrounding string) [32]byte {
	h := sha256.New()
	_, _ = h.Write(contextHash[:])
	for _, value := range []string{language, surface, policyVersion, o.cfg.ModelVersion, o.cfg.KevModel, o.cfg.KevURL, text, surrounding} {
		_, _ = io.WriteString(h, value)
		_, _ = h.Write([]byte{0})
	}
	var key [32]byte
	_ = h.Sum(key[:0]) // The digest is written only into this fresh, private array.
	return key
}

func (o *Observer) judge(surface, language, userLanguage string, locked bool, span candidate) (float64, string, bool) {
	if userLanguage == "" {
		userLanguage = "unknown"
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model": o.cfg.KevModel,
		"state": map[string]string{
			"default_language":     o.cfg.DefaultLanguage,
			"latest_user_language": userLanguage,
			"target_language":      language,
			"surface":              surface,
			"span":                 span.Text,
			"context":              span.Context,
		},
		"questions": map[string]any{
			"intrusion": map[string]string{
				"type":         "noul",
				"instructions": "Is the highlighted span an unnecessary language intrusion into the intended natural-language response, rather than an intentional multilingual term?",
			},
			"language": map[string]any{
				"type":         "choice",
				"instructions": "Which language should the surrounding natural-language response use? Choose unknown if the context is insufficient; do not override an explicit language instruction.",
				"criteria": map[string]string{
					"ko":            "Korean is the intended response language.",
					"en":            "English is the intended response language.",
					"ja":            "Japanese is the intended response language.",
					"zh_cn":         "Simplified Chinese is the intended response language.",
					"zh_tw":         "Traditional Chinese is the intended response language.",
					"no_correction": "The span is intentional; no language correction is warranted.",
					"unknown":       "The intended language cannot be inferred reliably.",
				},
			},
		},
	})
	if errMarshal != nil {
		return 0, "unknown", false
	}
	timeout, _ := time.ParseDuration(o.cfg.JudgeTimeout)
	ctx, cancel := context.WithTimeout(o.ctx, timeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.KevURL, bytes.NewReader(payload))
	if errRequest != nil {
		return 0, "unknown", false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, errDo := o.client.Do(req)
	if errDo != nil {
		return 0, "unknown", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "unknown", false
	}
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if errRead != nil || len(data) > 4096 {
		return 0, "unknown", false
	}
	var result struct {
		Answers struct {
			Intrusion struct {
				Type string   `json:"type"`
				Noul *float64 `json:"noul"`
			} `json:"intrusion"`
			Language struct {
				Type   string `json:"type"`
				Choice string `json:"choice"`
			} `json:"language"`
		} `json:"answers"`
	}
	if json.Unmarshal(data, &result) != nil || result.Answers.Intrusion.Type != "noul" || result.Answers.Intrusion.Noul == nil || *result.Answers.Intrusion.Noul < 0 || *result.Answers.Intrusion.Noul > 1 {
		return 0, "unknown", false
	}
	choice := map[string]string{
		"ko": "ko-KR", "en": "en-US", "ja": "ja-JP", "zh_cn": "zh-CN", "zh_tw": "zh-TW",
		"no_correction": "no-correction", "unknown": "unknown",
	}[result.Answers.Language.Choice]
	if result.Answers.Language.Type != "choice" || (locked && choice != language && choice != "no-correction") || choice == "" {
		choice = "unknown"
	}
	return *result.Answers.Intrusion.Noul, choice, true
}
