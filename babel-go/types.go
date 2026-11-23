package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Profile struct {
	Name           string `json:"name"`
	APIKey         string `json:"api_key"`
	BaseURL        string `json:"base_url"` // Exact compiler/validation POST URL; legacy field name retained.
	Model          string `json:"model"`
	UpstreamURL    string `json:"upstream_url"`
	TokenEncoding  string `json:"token_encoding,omitempty"`
	ValidationMode string `json:"validation_mode,omitempty"`
	ReaderProfile  string `json:"reader_profile,omitempty"`
	// AuditProfile 指定 compare 校验与 blind 对照使用的方案；留空沿用编译方案。
	AuditProfile string `json:"audit_profile,omitempty"`
	// DisableThinking / ReasoningEffort 只在显式配置时注入请求体，
	// 默认保持字节级不变的请求，避免非标准字段被严格端点拒绝。
	DisableThinking bool   `json:"disable_thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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
	Guidance   string `json:"-"` // Untrusted domain data; never part of the system message.
	PromptHash string `json:"prompt_hash"`
	Source     string `json:"source"`
	FilePath   string `json:"file_path"`
}

type CompileReq struct {
	Text             string   `json:"text"`
	Mode             string   `json:"mode,omitempty"` // Deprecated: legacy builtin aliases resolve to mixed.
	Domains          []string `json:"domains,omitempty"`
	Verify           bool     `json:"verify"`
	StrictGate       bool     `json:"strict_gate"`
	NoCache          bool     `json:"no_cache"`
	RequiredKeywords []string `json:"required_keywords"`
	Profile          string   `json:"profile,omitempty"`
	APIKey           string   `json:"api_key,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"` // Exact POST URL override; never an inferred route.
	Model            string   `json:"model,omitempty"`
	TokenEncoding    string   `json:"token_encoding,omitempty"`
	ValidationMode   string   `json:"validation_mode,omitempty"`
	ReaderProfile    *string  `json:"reader_profile,omitempty"`
	AuditProfile     *string  `json:"audit_profile,omitempty"`
	// SkipAuditIfNotShorter 仅供代理路径：候选不短于原文时无需再跑模型审计。
	SkipAuditIfNotShorter bool `json:"-"`
}

// ValidationResult describes this request's checks, never a cached assertion.
// A passed LLM comparison is evidence from that check, not a proof of equivalence.
type ValidationResult struct {
	Status        string            `json:"status"`
	Method        string            `json:"method"`
	PolicyVersion string            `json:"policy_version"`
	Issues        []string          `json:"issues"`
	Stages        []ValidationStage `json:"stages,omitempty"`
}

type ValidationError struct {
	Validation ValidationResult
}

type CompileResp struct {
	Compiled   string           `json:"compiled"`
	Dialect    string           `json:"dialect"`
	Audit      string           `json:"audit,omitempty"`
	CacheHit   bool             `json:"cache_hit"`
	Validation ValidationResult `json:"validation"`
	Stats      struct {
		OriginalChars       int    `json:"original_chars"`
		CompiledChars       int    `json:"compiled_chars"`
		OriginalTokens      int    `json:"original_tokens"`
		CompiledTokens      int    `json:"compiled_tokens"`
		CompressionRatio    string `json:"compression_ratio"`
		MeasurementKind     string `json:"measurement_kind"`
		MeasurementSource   string `json:"measurement_source"`
		MeasurementScope    string `json:"measurement_scope"`
		MeasurementEncoding string `json:"measurement_encoding"`
		MeasurementVersion  string `json:"measurement_version"`
	} `json:"stats"`
}

type CacheEntry struct {
	Compiled string `json:"compiled"`
	// Audit is read only for compatibility with old cache files. It is never trusted.
	Audit      string `json:"audit,omitempty"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	CreatedAt  string `json:"created_at"`
	LastAccess int64  `json:"last_access"`
}

type ProxyLog struct {
	ID                  string   `json:"id"`
	Timestamp           string   `json:"timestamp"`
	Protocol            string   `json:"protocol"`
	Dialect             string   `json:"dialect"`
	Domains             []string `json:"domains,omitempty"`
	Decision            string   `json:"decision,omitempty"`
	Reason              string   `json:"reason,omitempty"`
	Role                string   `json:"role"`
	Original            string   `json:"original"`
	Compiled            string   `json:"compiled"`
	Ratio               string   `json:"ratio"`
	CacheHit            bool     `json:"cache_hit"`
	Redacted            bool     `json:"redacted"`
	RealPromptTokens    int      `json:"real_prompt_tokens,omitempty"`
	EstOrigTokens       int      `json:"est_orig_tokens,omitempty"`
	RealBillingRatio    string   `json:"real_billing_ratio,omitempty"`
	MeasurementKind     string   `json:"measurement_kind,omitempty"`
	MeasurementEncoding string   `json:"measurement_encoding,omitempty"`
	ValidationMethod    string   `json:"validation_method,omitempty"`
}

// ==================== 编译器故障熔断器 ====================

type CircuitBreaker struct {
	mu           sync.RWMutex
	failures     int
	threshold    int
	cooldown     time.Duration
	openedAt     time.Time
	halfOpenBusy bool
	probeID      uint64
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown}
}

func (cb *CircuitBreaker) Allow() bool {
	allowed, _ := cb.Acquire()
	return allowed
}

// IsOpen 只读快判：熔断器正处于冷却窗口内、会直接拒绝请求时返回 true。
// 它不占用半开探针，因此可以在排队/建连之前提前短路。
func (cb *CircuitBreaker) IsOpen() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.failures >= cb.threshold && time.Since(cb.openedAt) < cb.cooldown
}

func (cb *CircuitBreaker) Acquire() (bool, uint64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.failures < cb.threshold {
		return true, 0
	}
	if time.Since(cb.openedAt) < cb.cooldown {
		return false, 0
	}
	if !cb.halfOpenBusy {
		cb.halfOpenBusy = true
		cb.probeID++
		return true, cb.probeID
	}
	return false, 0
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

// A caller cancellation says nothing about service health. Release a possible
// half-open probe without counting a failure or clearing previous failures.
func (cb *CircuitBreaker) ReleaseProbe(probeID uint64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if probeID != 0 && cb.halfOpenBusy && probeID == cb.probeID {
		cb.halfOpenBusy = false
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
	done    chan struct{}
	res     CompileResp
	err     error
	waiters int // Joined followers, protected by flightGroup.mu.
	active  int // Callers still waiting, protected by flightGroup.mu.
	cancel  context.CancelFunc
}

type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

func (g *flightGroup) Do(ctx context.Context, key string, fn func(context.Context) (CompileResp, error)) (CompileResp, bool, error) {
	if err := ctx.Err(); err != nil {
		return CompileResp{}, false, err
	}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return CompileResp{}, false, err
	}
	if g.calls == nil {
		g.calls = make(map[string]*flightCall)
	}
	c, shared := g.calls[key]
	if shared {
		c.waiters++
		c.active++
	} else {
		generationCtx, cancel := context.WithCancel(context.Background())
		c = &flightCall{done: make(chan struct{}), active: 1, cancel: cancel}
		g.calls[key] = c
		go func() {
			c.res, c.err = fn(generationCtx)
			g.mu.Lock()
			// A canceled flight may already have been replaced under this key.
			if g.calls[key] == c {
				delete(g.calls, key)
			}
			g.mu.Unlock()
			close(c.done)
		}()
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		c.active--
		if c.active == 0 {
			// An individual caller cannot cancel work another caller still
			// needs. Once nobody is waiting, stop generation and any repair.
			c.cancel()
			if g.calls[key] == c {
				delete(g.calls, key)
			}
		}
		g.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return CompileResp{}, shared, ctx.Err()
	case <-c.done:
		return c.res, shared, c.err
	}
}

// ==================== 全局编译并发信号量 ====================

// compileSemaphore 限制同时进行「编译 + 审计」的文本块数量。
// 队列满或等待超时的调用方会退回原文，而不是无限排队拖垮上游配额。
type compileSemaphore struct {
	slots chan struct{}
}

func newCompileSemaphore(limit int) *compileSemaphore {
	if limit <= 0 {
		limit = 16
	}
	return &compileSemaphore{slots: make(chan struct{}, limit)}
}

// Acquire 在 queueTimeout 内尝试获取一个槽位，或直到 ctx 结束。
// 返回 (release, acquired, canceled)：canceled 表示被调用方取消，acquired=false
// 且 canceled=false 表示队列超时，调用方应保留原文继续。
func (s *compileSemaphore) Acquire(ctx context.Context, queueTimeout time.Duration) (func(), bool, bool) {
	if ctx.Err() != nil {
		return nil, false, true
	}
	if s == nil || cap(s.slots) == 0 {
		return func() {}, true, false
	}
	select {
	case s.slots <- struct{}{}:
		return s.releaseFunc(), true, false
	default:
	}
	if queueTimeout <= 0 {
		return nil, false, false
	}
	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return s.releaseFunc(), true, false
	case <-ctx.Done():
		return nil, false, true
	case <-timer.C:
		return nil, false, false
	}
}

func (s *compileSemaphore) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-s.slots
		})
	}
}

func (s *compileSemaphore) Status() map[string]int {
	if s == nil {
		return map[string]int{"in_flight": 0, "limit": 0}
	}
	return map[string]int{"in_flight": len(s.slots), "limit": cap(s.slots)}
}

// envPositiveInt 读取正整数环境变量；未设置或非法时回退默认值。
func envPositiveInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

const (
	maxCacheEntries = 2000
	// 高并发网关下 50 条会把一次多块编译的日志互相挤掉，保留 500 条便于回溯。
	maxLogs = 500

	defaultProxyMaxConcurrency   = 16
	defaultProxyBlockConcurrency = 8
	defaultProxyQueueTimeoutMS   = 5000
)

var (
	cacheMutex    sync.RWMutex
	diskCache     = make(map[string]CacheEntry)
	compileFlight flightGroup

	compilerCB = newCircuitBreaker(3, 60*time.Second)

	proxyCompileSem          = newCompileSemaphore(envPositiveInt("BABEL_PROXY_MAX_CONCURRENCY", defaultProxyMaxConcurrency))
	proxyBlockConcurrency    = envPositiveInt("BABEL_PROXY_BLOCK_CONCURRENCY", defaultProxyBlockConcurrency)
	proxyCompileQueueTimeout = time.Duration(envPositiveInt("BABEL_PROXY_QUEUE_TIMEOUT_MS", defaultProxyQueueTimeoutMS)) * time.Millisecond

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
