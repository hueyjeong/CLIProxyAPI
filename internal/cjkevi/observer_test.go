package cjkevi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const observedMessage = `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"中간 상황"}]}`

func TestObserverDisabledDoesNotStartOrCallKev(t *testing.T) {
	var calls atomic.Int32
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"answers":{"intrusion":{"type":"noul","noul":0.8},"language":{"type":"choice","choice":"ko"}}}`))
	}))
	defer kev.Close()
	if got := NewObserver(config.CJKeviShadowConfig{KevURL: kev.URL + "/v1/systemone"}); got != nil {
		got.Close()
		t.Fatal("disabled observer started")
	}
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true})
	if o == nil {
		t.Fatal("enabled observer not created")
	}
	defer o.Close()
	o.observe(job{body: []byte(observedMessage), contentType: "application/json", surface: SurfaceAnthropic})
	if calls.Load() != 0 {
		t.Fatalf("unexpected external calls: %d", calls.Load())
	}
}

func TestObserverLocalStateCacheAndSurfaceIsolation(t *testing.T) {
	var calls atomic.Int32
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected local judge request: method=%s path=%s", r.Method, r.URL.Path)
		}
		var request struct {
			Model     string            `json:"model"`
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type         string            `json:"type"`
				Instructions string            `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("API_KEY_NOT_SENT")) {
			t.Error("raw user text leaked to judge")
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Errorf("decode state: %v", err)
		}
		if request.Model != "jaredpalmer/kev-0.8b" || request.Questions["intrusion"].Type != "noul" || request.Questions["language"].Type != "choice" || len(request.Questions["language"].Criteria) == 0 {
			t.Error("missing required System One model or questions")
		}
		if request.State["span"] != "中" || !strings.Contains(request.State["context"], "<span>") || request.State["target_language"] == "" || request.State["surface"] == "" {
			t.Errorf("unexpected state: %#v", request.State)
		}
		if _, ok := request.State["api_key"]; ok {
			t.Error("auth data leaked to judge")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"intrusion":{"type":"noul","noul":0.24},"language":{"type":"choice","choice":"no_correction"}}}`))
	}))
	defer kev.Close()
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone"})
	if o == nil {
		t.Fatal("observer not initialized")
	}
	defer o.Close()

	j := job{body: []byte(`{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"中"}]}`), request: []byte(`{"messages":[{"role":"user","content":"Tell me more. API_KEY_NOT_SENT"}]}`), contextHash: sha256.Sum256([]byte("context-one")), contentType: "application/json", surface: SurfaceAnthropic}
	o.observe(j)
	for _, entry := range o.cache {
		if entry.noul != 0.24 || entry.choice != "no-correction" {
			t.Fatalf("raw System One verdict not cached: %+v", entry)
		}
	}
	o.observe(j)
	if got := calls.Load(); got != 1 {
		t.Fatalf("same context cache calls = %d, want 1", got)
	}
	j.contextHash = sha256.Sum256([]byte("context-two"))
	o.observe(j)
	j.request = []byte(`{"messages":[{"role":"user","content":"Answer in English."}]}`)
	o.observe(j)
	j.body = []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"中"}}]}`)
	j.surface = SurfaceChat
	o.observe(j)
	if got := calls.Load(); got != 4 {
		t.Fatalf("context/language/surface separation calls = %d, want 4", got)
	}
}

func TestObserverQueueOverflowAndJudgeTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var calls atomic.Int32
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return
		}
		close(second)
		_, _ = w.Write([]byte(`{"answers":{"intrusion":{"type":"noul","noul":0.12},"language":{"type":"choice","choice":"unknown"}}}`))
	}))
	defer kev.Close()
	defer close(release)
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone", JudgeTimeout: "250ms", QueueSize: 1, Workers: 1})
	if o == nil {
		t.Fatal("observer not initialized")
	}
	defer o.Close()
	j := job{body: []byte(observedMessage), request: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), contentType: "application/json", surface: SurfaceAnthropic}
	o.Submit(j.body, j.request, j.contextHash, j.contentType, http.StatusOK, j.surface)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first fake Kev call never started")
	}
	o.Submit(j.body, j.request, sha256.Sum256([]byte("second")), j.contentType, http.StatusOK, j.surface)
	if len(o.jobs) != 1 {
		t.Fatal("expected exactly one queued job while first judge is blocked")
	}
	o.Submit(j.body, j.request, sha256.Sum256([]byte("third")), j.contentType, http.StatusOK, j.surface)
	select {
	case <-second:
		// Processing the queued request proves the blocked first judge timed out.
	case <-time.After(3 * time.Second):
		t.Fatal("queued job was not processed after first judge timeout")
	}
	if calls.Load() != 2 {
		t.Fatalf("queue overflow called Kev %d times, want 2", calls.Load())
	}
}

func TestObserverQueueRemainsBoundedUnderConcurrentSubmit(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			entered <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte(`{"answers":{"intrusion":{"type":"noul","noul":0.5}}}`))
	}))
	defer kev.Close()
	defer close(release)
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone", QueueSize: 4, Workers: 2, JudgeTimeout: "5s"})
	if o == nil {
		t.Fatal("observer not initialized")
	}
	defer o.Close()
	request := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	for i := 0; i < 2; i++ {
		o.Submit([]byte(observedMessage), request, sha256.Sum256([]byte{byte(i)}), "application/json", http.StatusOK, SurfaceAnthropic)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not enter blocked fake judge")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			o.Submit([]byte(observedMessage), request, sha256.Sum256([]byte{byte(index + 2)}), "application/json", http.StatusOK, SurfaceAnthropic)
		}(i)
	}
	wg.Wait()
	if len(o.jobs) != 4 || len(o.budget.slots) != 0 {
		t.Fatalf("unexpected retained work after queue saturation: queued=%d capture_slots=%d", len(o.jobs), len(o.budget.slots))
	}
}

func TestObserverBudgetIsSharedAcrossReload(t *testing.T) {
	budget := NewBudget()
	cfg := config.CJKeviShadowConfig{Enabled: true}
	oldObserver := NewObserver(cfg, budget)
	newObserver := NewObserver(cfg, budget)
	defer oldObserver.Close()
	defer newObserver.Close()
	for range MaxCaptureSlots {
		if !oldObserver.TryCapture() {
			t.Fatal("old observer failed to reserve a capture slot")
		}
	}
	oldObserver.Close()
	if newObserver.TryCapture() {
		t.Fatal("reload reset the shared in-flight capture cap")
	}
	for range MaxCaptureSlots {
		oldObserver.ReleaseCapture()
	}
	if !newObserver.TryCapture() {
		t.Fatal("released shared capture slot did not become available")
	}
	newObserver.ReleaseCapture()
	if !budget.reserve(budget.limit) || budget.reserve(1) {
		t.Fatal("shared byte budget did not enforce its limit")
	}
	budget.release(budget.limit)
	if !budget.reserve(1) {
		t.Fatal("shared byte budget was not released")
	}
	budget.release(1)
}

func TestObserverCloseDrainsQueuedJobs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	defer kev.Close()
	defer close(release)
	budget := NewBudget()
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone", QueueSize: 1, Workers: 1, JudgeTimeout: "5s"}, budget)
	defer o.Close()
	body := []byte(observedMessage)
	request := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	o.Submit(body, request, [32]byte{}, "application/json", http.StatusOK, SurfaceAnthropic)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first judge never started")
	}
	o.Submit(body, request, sha256.Sum256([]byte("second")), "application/json", http.StatusOK, SurfaceAnthropic)
	if len(o.jobs) != 1 {
		t.Fatal("expected pending job before observer close")
	}
	o.Close()
	budget.mu.Lock()
	retained := budget.queuedBytes
	budget.mu.Unlock()
	if len(o.jobs) != 0 || retained > int64(len(body)+len(request)) {
		t.Fatalf("observer close retained pending jobs: queued=%d bytes=%d", len(o.jobs), retained)
	}
	o.Submit(body, request, sha256.Sum256([]byte("third")), "application/json", http.StatusOK, SurfaceAnthropic)
	if len(o.jobs) != 0 {
		t.Fatal("observer accepted a job after close")
	}
}

func TestObserverRejectsRedirectAndOversizedReply(t *testing.T) {
	var redirected atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
	}))
	defer remote.Close()
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL+"/v1/systemone", http.StatusFound)
	}))
	defer kev.Close()
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone"})
	defer o.Close()
	if _, _, ok := o.judge(SurfaceAnthropic, "ko-KR", "", false, candidate{Text: "中", Context: "<span>간"}); ok || redirected.Load() {
		t.Fatal("local judge followed a redirect")
	}

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
	}))
	defer oversized.Close()
	o2 := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: oversized.URL + "/v1/systemone"})
	defer o2.Close()
	if _, _, ok := o2.judge(SurfaceAnthropic, "ko-KR", "", false, candidate{Text: "中"}); ok {
		t.Fatal("oversized judge response was accepted")
	}
}

func TestObserverCloseCancelsInFlightJudge(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	judgeDone := make(chan struct{})
	kev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer kev.Close()
	defer close(release)
	o := NewObserver(config.CJKeviShadowConfig{Enabled: true, KevURL: kev.URL + "/v1/systemone", JudgeTimeout: "5s"})
	go func() {
		_, _, _ = o.judge(SurfaceAnthropic, "ko-KR", "", false, candidate{Text: "中"})
		close(judgeDone)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("judge did not start")
	}
	o.Close()
	select {
	case <-judgeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("judge request did not exit after observer cancellation")
	}
	if o.ctx.Err() != context.Canceled {
		t.Fatal("observer context remains active")
	}
}
