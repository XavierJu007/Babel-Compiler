package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 包级默认保持保守：全部关闭，保证既有单测与 CLI 行为零变化。
// 生产默认值只在 startServer() 中按环境变量补齐。
var (
	proxyValidationCacheTTL time.Duration
	proxyNegativeCacheTTL   time.Duration
	proxyAsyncWarmup        bool
	proxyMinRunes           = 35
	proxySkipNonProse       bool
)

const (
	proxyValidationCacheCapacity = 4096
	proxyNegativeCacheCapacity   = 2000
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
}

// proxySpeedDurationLabel 让关掉的缓存显示为 off，而不是 0s。
func proxySpeedDurationLabel(value time.Duration) string {
	if value <= 0 {
		return "off"
	}
	return value.String()
}

func proxySpeedSummary() map[string]any {
	return map[string]any{
		"validation_cache_ttl": proxyValidationCacheTTL.String(),
		"negative_cache_ttl":   proxyNegativeCacheTTL.String(),
		"async_warmup":         proxyAsyncWarmup,
		"min_runes":            proxyMinRunes,
		"skip_non_prose":       proxySkipNonProse,
	}
}

// isNonProsePayload 识别纯 JSON 载荷与整体被代码围栏包裹的文本：
// 这类内容编译收益极低，开启 skip_non_prose 后本地直接跳过。
func isNonProsePayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") && strings.HasSuffix(trimmed, "```") && strings.Count(trimmed, "```") >= 2 {
		return true
	}
	if len(trimmed) < 2 {
		return false
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
	shadowWarmupOnce   sync.Once
	shadowWarmupQueue  chan shadowWarmupJob
	shadowWarmupFlight sync.Map
)

func startShadowWarmupWorkers() {
	shadowWarmupQueue = make(chan shadowWarmupJob, shadowWarmupQueueSize)
	for i := 0; i < shadowWarmupWorkers; i++ {
		go func() {
			for job := range shadowWarmupQueue {
				runShadowWarmup(job)
			}
		}()
	}
}

// enqueueShadowWarmup 去重后投递后台预热任务；队列满则丢弃，绝不阻塞请求。
func enqueueShadowWarmup(protocol, role, text string, p Profile, dialect Dialect, keywords, domains []string) bool {
	key := makeCacheKey(dialect, p, text)
	if _, exists := shadowWarmupFlight.LoadOrStore(key, struct{}{}); exists {
		return false
	}
	shadowWarmupOnce.Do(startShadowWarmupWorkers)
	job := shadowWarmupJob{protocol: protocol, role: role, text: text, profile: p,
		dialect: dialect, keywords: keywords, domains: domains}
	select {
	case shadowWarmupQueue <- job:
		return true
	default:
		shadowWarmupFlight.Delete(key)
		return false
	}
}

func runShadowWarmup(job shadowWarmupJob) {
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
	allowed, probeID := compilerCB.Acquire()
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
		TokenEncoding:         job.profile.TokenEncoding,
		ValidationMode:        job.profile.ValidationMode,
		ReaderProfile:         &job.profile.ReaderProfile,
		AuditProfile:          &job.profile.AuditProfile,
		SkipAuditIfNotShorter: true,
		proxyPath:             true,
	})
	if ctx.Err() != nil {
		compilerCB.ReleaseProbe(probeID)
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
		case errors.As(err, &validationErr):
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
