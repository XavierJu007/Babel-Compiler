package main

import (
	"sync"
	"time"
)

type Profile struct {
	Name        string `json:"name"`
	APIKey      string `json:"api_key"`
	BaseURL     string `json:"base_url"`
	Model       string `json:"model"`
	UpstreamURL string `json:"upstream_url"`
}

type Config struct {
	ActiveProfile string             `json:"active_profile"`
	Profiles      map[string]Profile `json:"profiles"`
	APIKey        string             `json:"api_key,omitempty"`
	BaseURL       string             `json:"base_url,omitempty"`
	Model         string             `json:"model,omitempty"`
	UpstreamURL   string             `json:"upstream_url,omitempty"`
}

type Dialect struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Prompt     string `json:"prompt"`
	PromptHash string `json:"prompt_hash"`
	Source     string `json:"source"`
	FilePath   string `json:"file_path"`
}

type CompileReq struct {
	Text             string   `json:"text"`
	Mode             string   `json:"mode"`
	Verify           bool     `json:"verify"`
	StrictGate       bool     `json:"strict_gate"`
	NoCache          bool     `json:"no_cache"`
	RequiredKeywords []string `json:"required_keywords"`
	Profile          string   `json:"profile,omitempty"`
	APIKey           string   `json:"api_key,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"`
	Model            string   `json:"model,omitempty"`
}

type CompileResp struct {
	Compiled string `json:"compiled"`
	Dialect  string `json:"dialect"`
	Audit    string `json:"audit,omitempty"`
	CacheHit bool   `json:"cache_hit"`
	Stats    struct {
		OriginalChars    int    `json:"original_chars"`
		CompiledChars    int    `json:"compiled_chars"`
		OriginalTokens   int    `json:"original_tokens"`
		CompiledTokens   int    `json:"compiled_tokens"`
		CompressionRatio string `json:"compression_ratio"`
	} `json:"stats"`
}

type CacheEntry struct {
	Compiled   string `json:"compiled"`
	Audit      string `json:"audit,omitempty"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	CreatedAt  string `json:"created_at"`
	LastAccess int64  `json:"last_access"`
}

type ProxyLog struct {
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp"`
	Protocol         string `json:"protocol"`
	Dialect          string `json:"dialect"`
	Role             string `json:"role"`
	Original         string `json:"original"`
	Compiled         string `json:"compiled"`
	Ratio            string `json:"ratio"`
	CacheHit         bool   `json:"cache_hit"`
	Redacted         bool   `json:"redacted"`
	RealPromptTokens int    `json:"real_prompt_tokens,omitempty"`
	EstOrigTokens    int    `json:"est_orig_tokens,omitempty"`
	RealBillingRatio string `json:"real_billing_ratio,omitempty"`
}

// ==================== 编译器故障熔断器 ====================

type CircuitBreaker struct {
	mu           sync.RWMutex
	failures     int
	threshold    int
	cooldown     time.Duration
	openedAt     time.Time
	halfOpenBusy bool
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown}
}

func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.failures < cb.threshold {
		return true
	}
	if time.Since(cb.openedAt) < cb.cooldown {
		return false
	}
	if !cb.halfOpenBusy {
		cb.halfOpenBusy = true
		return true
	}
	return false
}

func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.halfOpenBusy = false
	cb.openedAt = time.Time{}
}

func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.halfOpenBusy = false
	cb.failures++
	if cb.failures >= cb.threshold {
		cb.openedAt = time.Now()
	}
}

func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.halfOpenBusy = false
	cb.openedAt = time.Time{}
}

func (cb *CircuitBreaker) Status() map[string]any {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	state := "CLOSED"
	remainingSec := 0
	if cb.failures >= cb.threshold {
		elapsed := time.Since(cb.openedAt)
		if elapsed < cb.cooldown {
			state = "OPEN"
			remainingSec = int((cb.cooldown - elapsed).Seconds()) + 1
		} else {
			state = "HALF-OPEN"
		}
	}
	return map[string]any{
		"state":         state,
		"failures":      cb.failures,
		"threshold":     cb.threshold,
		"remaining_sec": remainingSec,
	}
}

// ==================== Singleflight 防缓存击穿 ====================

type flightCall struct {
	wg  sync.WaitGroup
	res CompileResp
	err error
}

type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

func (g *flightGroup) Do(key string, fn func() (CompileResp, error)) (CompileResp, bool, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*flightCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.res, true, c.err
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.res, c.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()

	return c.res, false, c.err
}

const (
	maxCacheEntries = 2000
	maxLogs         = 50
)

var (
	cacheMutex    sync.RWMutex
	diskCache     = make(map[string]CacheEntry)
	compileFlight flightGroup

	dialectsMutex  sync.RWMutex
	loadedDialects = make(map[string]Dialect)

	calibMutex          sync.RWMutex
	bpeCalibrationScale = 1.0

	compilerCB = newCircuitBreaker(3, 60*time.Second)

	redactMutex      sync.RWMutex
	logRedactEnabled = true

	logsMutex sync.RWMutex
	proxyLogs []ProxyLog
)

func isLogRedactEnabled() bool {
	redactMutex.RLock()
	defer redactMutex.RUnlock()
	return logRedactEnabled
}

func setLogRedactEnabled(v bool) {
	redactMutex.Lock()
	defer redactMutex.Unlock()
	logRedactEnabled = v
}
