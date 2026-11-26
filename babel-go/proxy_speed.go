package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 包级默认保持保守：全部关闭，保证既有单测与 CLI 行为零变化。
// 生产默认值只在 startServer() 中按环境变量补齐。
var (
	proxyPolicyMu           sync.RWMutex
	proxyDryRun             bool
	proxyValidationCacheTTL time.Duration
	proxyNegativeCacheTTL   time.Duration
	proxyCompileBudget      time.Duration
	proxyAsyncWarmup        bool
	proxySkipCacheControl   bool
	proxyCompressDeveloper  bool
	proxyMinRunes           = 35
	proxySkipNonProse       bool
	proxyLocalFastPath      bool
	proxyWarmupWorkers      = 2
	// proxyWarmupUserMinHits 控制 user 角色文本进入后台异步预热前需被观测到的次数：
	// 包级默认 1 保持单测零变化；服务端生产默认 2，避免一次性 user 提问白烧编译配额与污染 LRU。
	// system 与 tool:* 属于高复用前缀，始终在首次命中即预热；0 表示禁用 user 预热。
	proxyWarmupUserMinHits = 1
)

const (
	proxyValidationCacheCapacity = 4096
	proxyNegativeCacheCapacity   = 2000
	proxyWarmupSeenCapacity      = 4096
	proxyWarmupSeenTTL           = 30 * time.Minute
	shadowWarmupQueueSize        = 256
	shadowWarmupTimeout          = 45 * time.Second
	shadowWarmupWorkers          = 2
)

// parseProxyDuration 接受 "0"/"off" 与 Go duration（10m、30s）。
// 显式写错时按 0 处理（关闭），绝不因为拼写错误悄悄开启一个缓存。
func parseProxyDuration(raw string) time.Duration {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "", "0", "off", "false", "no":
		return 0
	}
	if value, err := time.ParseDuration(raw); err == nil && value > 0 {
		return value
	}
	return 0
}

// parseProxyBool 只认明确的真假写法；无法识别时回退 fallback。
func parseProxyBool(raw string, fallback bool) bool {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

// configureProxySpeedDefaults 在服务端模式应用生产默认值：
// 环境变量"未设置"才取默认，"显式 =0/=false"必须覆盖默认。
func configureProxySpeedDefaults() {
	proxyPolicyMu.Lock()
	defer proxyPolicyMu.Unlock()

	if raw, ok := os.LookupEnv("BABEL_PROXY_DRY_RUN"); ok {
		proxyDryRun = parseProxyBool(raw, false)
	} else {
		proxyDryRun = false
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_VALIDATION_CACHE_TTL"); ok {
		proxyValidationCacheTTL = parseProxyDuration(raw)
	} else {
		proxyValidationCacheTTL = 10 * time.Minute
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_NEGATIVE_CACHE_TTL"); ok {
		proxyNegativeCacheTTL = parseProxyDuration(raw)
	} else {
		proxyNegativeCacheTTL = 10 * time.Minute
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_ASYNC_WARMUP"); ok {
		proxyAsyncWarmup = parseProxyBool(raw, false)
	} else {
		proxyAsyncWarmup = true
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_MIN_RUNES"); ok {
		if value, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && value > 0 {
			proxyMinRunes = value
		} else {
			proxyMinRunes = 35
		}
	} else {
		proxyMinRunes = 150
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_SKIP_NON_PROSE"); ok {
		proxySkipNonProse = parseProxyBool(raw, false)
	} else {
		proxySkipNonProse = true
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_LOCAL_FAST"); ok {
		proxyLocalFastPath = parseProxyBool(raw, false)
	} else {
		proxyLocalFastPath = true
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_COMPILE_BUDGET"); ok {
		proxyCompileBudget = parseProxyDuration(raw)
	} else {
		proxyCompileBudget = 5 * time.Second
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_SKIP_CACHE_CONTROL"); ok {
		proxySkipCacheControl = parseProxyBool(raw, false)
	} else {
		proxySkipCacheControl = false
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_COMPRESS_DEVELOPER"); ok {
		proxyCompressDeveloper = parseProxyBool(raw, false)
	} else {
		proxyCompressDeveloper = false
	}
	if raw, ok := os.LookupEnv("BABEL_PROXY_WARMUP_USER_MIN_HITS"); ok {
		if value, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && value >= 0 {
			proxyWarmupUserMinHits = value
		} else {
			proxyWarmupUserMinHits = 2
		}
	} else {
		proxyWarmupUserMinHits = 2
	}
	// 预热 worker 数量：默认 min(8, max(2, 全局槽位/2))，可按环境变量覆盖。
	if raw, ok := os.LookupEnv("BABEL_PROXY_WARMUP_WORKERS"); ok {
		if value, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && value > 0 {
			proxyWarmupWorkers = value
		}
	} else {
		workers := proxyCompileSem.Status()["limit"] / 2
		if workers < 2 {
			workers = 2
		}
		if workers > 8 {
			workers = 8
		}
		proxyWarmupWorkers = workers
	}
}

type proxyPolicySnapshot struct {
	DryRun            bool
	SkipCacheControl  bool
	CompressDeveloper bool
	AsyncWarmup       bool
	SkipNonProse      bool
	LocalFastPath     bool
	CompileBudget     time.Duration
	WarmupUserMinHits int
	MinRunes          int
}

func snapshotProxyPolicy() proxyPolicySnapshot {
	proxyPolicyMu.RLock()
	defer proxyPolicyMu.RUnlock()
	return proxyPolicySnapshot{
		DryRun:            proxyDryRun,
		SkipCacheControl:  proxySkipCacheControl,
		CompressDeveloper: proxyCompressDeveloper,
		AsyncWarmup:       proxyAsyncWarmup,
		SkipNonProse:      proxySkipNonProse,
		LocalFastPath:     proxyLocalFastPath,
		CompileBudget:     proxyCompileBudget,
		WarmupUserMinHits: proxyWarmupUserMinHits,
		MinRunes:          proxyMinRunes,
	}
}

type proxyPolicyPatch struct {
	DryRun            *bool
	SkipCacheControl  *bool
	CompressDeveloper *bool
	AsyncWarmup       *bool
	LocalFastPath     *bool
	CompileBudget     *string
	WarmupUserMinHits *int
}

func parseStrictProxyDuration(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	switch trimmed {
	case "", "0", "off", "false", "no":
		return 0, nil
	}
	value, err := time.ParseDuration(trimmed)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("无效的编译预算时长: %q（请使用如 5s、10s 或 off）", raw)
	}
	return value, nil
}

func updateProxyPolicy(patch proxyPolicyPatch) error {
	var parsedBudget time.Duration
	if patch.CompileBudget != nil {
		var err error
		parsedBudget, err = parseStrictProxyDuration(*patch.CompileBudget)
		if err != nil {
			return err
		}
	}
	if patch.WarmupUserMinHits != nil && *patch.WarmupUserMinHits < 0 {
		return fmt.Errorf("user 预热频次阈值不能为负数")
	}
	proxyPolicyMu.Lock()
	defer proxyPolicyMu.Unlock()
	if patch.DryRun != nil {
		proxyDryRun = *patch.DryRun
	}
	if patch.SkipCacheControl != nil {
		proxySkipCacheControl = *patch.SkipCacheControl
	}
	if patch.CompressDeveloper != nil {
		proxyCompressDeveloper = *patch.CompressDeveloper
	}
	if patch.AsyncWarmup != nil {
		proxyAsyncWarmup = *patch.AsyncWarmup
	}
	if patch.LocalFastPath != nil {
		proxyLocalFastPath = *patch.LocalFastPath
	}
	if patch.CompileBudget != nil {
		proxyCompileBudget = parsedBudget
	}
	if patch.WarmupUserMinHits != nil {
		proxyWarmupUserMinHits = *patch.WarmupUserMinHits
	}
	return nil
}

// proxySpeedDurationLabel 让关掉的缓存显示为 off，而不是 0s。
func proxySpeedDurationLabel(value time.Duration) string {
	if value <= 0 {
		return "off"
	}
	return value.String()
}

func proxySpeedSummary() map[string]any {
	snap := snapshotProxyPolicy()
	return map[string]any{
		"dry_run":              snap.DryRun,
		"validation_cache_ttl": proxyValidationCacheTTL.String(),
		"negative_cache_ttl":   proxyNegativeCacheTTL.String(),
		"compile_budget":       proxySpeedDurationLabel(snap.CompileBudget),
		"async_warmup":         snap.AsyncWarmup,
		"skip_cache_control":   snap.SkipCacheControl,
		"compress_developer":   snap.CompressDeveloper,
		"warmup_user_min_hits": snap.WarmupUserMinHits,
		"min_runes":            snap.MinRunes,
		"skip_non_prose":       snap.SkipNonProse,
		"local_fast_path":      snap.LocalFastPath,
		"warmup_workers":       proxyWarmupWorkers,
	}
}

// isNonProsePayload 识别纯 JSON 载荷与整体被单个代码围栏包裹的文本：
// 这类内容编译收益极低，开启 skip_non_prose 后本地直接跳过。
func isNonProsePayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 {
		return false
	}
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		lits := codeLiterals(trimmed)
		if len(lits) == 1 && lits[0].start == 0 && lits[0].end == len(trimmed) {
			return true
		}
	}
	first, last := trimmed[0], trimmed[len(trimmed)-1]
	if (first == '{' && last == '}') || (first == '[' && last == ']') {
		return json.Valid([]byte(trimmed))
	}
	return false
}

// ==================== Shadow 预热 ====================

type shadowWarmupJob struct {
	protocol string
	role     string
	text     string
	profile  Profile
	dialect  Dialect
	keywords []string
	domains  []string
}

var (
	shadowWarmupOnce    sync.Once
	shadowWarmupQueue   chan shadowWarmupJob
	shadowWarmupFlight  sync.Map
	shadowWarmupPending sync.WaitGroup

	warmupSeenMu    sync.Mutex
	warmupSeenCache = newTTLLRU[string, int](proxyWarmupSeenCapacity)
)

// shouldWarmupBlock implements a frequency admission filter for async warmup:
// high-reuse prefixes (system, tool:*) always warm immediately, while user text
// must be seen at least proxyWarmupUserMinHits times to avoid burning compiler
// quota and evicting useful LRU entries on one-off prompts.
func shouldWarmupBlock(role, cacheKey string) bool {
	if role != "user" {
		return true
	}
	minHits := snapshotProxyPolicy().WarmupUserMinHits
	if minHits <= 0 {
		return false
	}
	if minHits == 1 {
		return true
	}
	warmupSeenMu.Lock()
	defer warmupSeenMu.Unlock()
	hits, _ := warmupSeenCache.Get(cacheKey)
	hits++
	warmupSeenCache.Put(cacheKey, hits, proxyWarmupSeenTTL)
	return hits >= minHits
}

func resetWarmupAdmissionCache() {
	warmupSeenMu.Lock()
	warmupSeenCache.Reset()
	warmupSeenMu.Unlock()
}

// warmupStatus 区分「新入队 / 已在预热 / 队列满」：三者都意味着不能同步等待。
type warmupStatus int

const (
	warmupEnqueued warmupStatus = iota
	warmupInFlight
	warmupQueueFull
)

func startShadowWarmupWorkers() {
	workers := proxyWarmupWorkers
	if workers < 1 {
		workers = 1
	}
	shadowWarmupQueue = make(chan shadowWarmupJob, shadowWarmupQueueSize)
	for i := 0; i < workers; i++ {
		go func() {
			for job := range shadowWarmupQueue {
				runShadowWarmup(job)
			}
		}()
	}
}

// enqueueShadowWarmup 去重后投递后台预热任务；队列满则丢弃，绝不阻塞请求。
func enqueueShadowWarmup(protocol, role, text string, p Profile, dialect Dialect, keywords, domains []string) warmupStatus {
	key := makeCacheKey(dialect, p, text)
	if _, exists := shadowWarmupFlight.LoadOrStore(key, struct{}{}); exists {
		// 同一文本已在预热：调用方必须继续直通，绝不能跌穿到同步编译。
		return warmupInFlight
	}
	shadowWarmupOnce.Do(startShadowWarmupWorkers)
	job := shadowWarmupJob{protocol: protocol, role: role, text: text, profile: p,
		dialect: dialect, keywords: keywords, domains: domains}
	shadowWarmupPending.Add(1)
	select {
	case shadowWarmupQueue <- job:
		return warmupEnqueued
	default:
		shadowWarmupPending.Done()
		shadowWarmupFlight.Delete(key)
		return warmupQueueFull
	}
}

func drainShadowWarmup(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		shadowWarmupPending.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func runShadowWarmup(job shadowWarmupJob) {
	defer shadowWarmupPending.Done()
	key := makeCacheKey(job.dialect, job.profile, job.text)
	defer shadowWarmupFlight.Delete(key)

	// 后台任务脱离请求生命周期，避免调用方断开就丢掉预热。
	ctx, cancel := context.WithTimeout(context.Background(), shadowWarmupTimeout)
	defer cancel()

	release, acquired, canceled := proxyCompileSem.Acquire(ctx, proxyCompileQueueTimeout)
	if canceled || !acquired {
		return
	}
	defer release()
	if compilerCB.IsOpen() {
		return
	}
	allowed, _ := compilerCB.Acquire()
	if !allowed {
		return
	}
	res, err := doCompileContext(ctx, CompileReq{
		Profile:               job.profile.Name,
		Text:                  job.text,
		Mode:                  job.dialect.Name,
		Domains:               job.domains,
		StrictGate:            true,
		RequiredKeywords:      job.keywords,
		APIKey:                job.profile.APIKey,
		BaseURL:               job.profile.BaseURL,
		Model:                 job.profile.Model,
		ParallelChunks:        true,
		TokenEncoding:         job.profile.TokenEncoding,
		ValidationMode:        job.profile.ValidationMode,
		ReaderProfile:         &job.profile.ReaderProfile,
		AuditProfile:          &job.profile.AuditProfile,
		SkipAuditIfNotShorter: true,
		proxyPath:             true,
		preparedDialect:       &job.dialect,
		preparedCacheKey:      key,
	})
	if ctx.Err() != nil {
		// shadowWarmupTimeout is an internal compiler timeout, not a caller
		// cancellation. Record failure so a stalled compiler still trips the breaker.
		compilerCB.RecordFailure()
		return
	}
	storeNegative := func() {
		if res.Stats.OriginalTokens > 0 && res.Stats.CompiledTokens >= res.Stats.OriginalTokens {
			negativeCache.Put(negativeCacheKey(job.dialect, job.profile, job.text), proxyNegativeCacheTTL)
		}
	}
	if err != nil {
		var measurementErr *TokenMeasurementError
		var validationErr *ValidationError
		switch {
		case errors.As(err, &measurementErr):
			// 计数不可用与上游健康无关。
			compilerCB.RecordSuccess()
		case errors.As(err, &validationErr) && validationErr.Validation.Status == "failed":
			// 模型可达但候选未通过检查：不计失败，只有确定性低收益才写负缓存。
			compilerCB.RecordSuccess()
			storeNegative()
		default:
			compilerCB.RecordFailure()
		}
		return
	}
	compilerCB.RecordSuccess()
	storeNegative()
}
