package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// unsetProxySpeedEnv 真正删除环境变量（t.Setenv("") 仍算显式设置，会覆盖默认值）。
func unsetProxySpeedEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BABEL_PROXY_VALIDATION_CACHE_TTL",
		"BABEL_PROXY_NEGATIVE_CACHE_TTL",
		"BABEL_PROXY_ASYNC_WARMUP",
		"BABEL_PROXY_MIN_RUNES",
		"BABEL_PROXY_SKIP_NON_PROSE",
	} {
		old, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, old)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// 恢复包级保守默认，避免用例之间互相污染。
func resetProxySpeedVars(t *testing.T) {
	t.Helper()
	proxyValidationCacheTTL = 0
	proxyNegativeCacheTTL = 0
	proxyCompileBudget = 0
	proxyAsyncWarmup = false
	proxySkipCacheControl = false
	proxyCompressDeveloper = false
	proxyWarmupUserMinHits = 1
	proxyMinRunes = 35
	proxySkipNonProse = false
	validationCache.Reset()
	negativeCache.Reset()
	resetWarmupAdmissionCache()
	t.Cleanup(func() {
		proxyValidationCacheTTL = 0
		proxyNegativeCacheTTL = 0
		proxyCompileBudget = 0
		proxyAsyncWarmup = false
		proxySkipCacheControl = false
		proxyWarmupUserMinHits = 1
		proxyMinRunes = 35
		proxySkipNonProse = false
		validationCache.Reset()
		negativeCache.Reset()
		resetWarmupAdmissionCache()
	})
}

// startServer() 的默认值只在环境变量“未设置”时生效；显式 =0/=false 必须覆盖。
func TestProxySpeedProductionDefaultsAndExplicitOverrides(t *testing.T) {
	resetProxySpeedVars(t)

	unsetProxySpeedEnv(t)
	configureProxySpeedDefaults()
	if proxyValidationCacheTTL != 10*time.Minute || proxyNegativeCacheTTL != 10*time.Minute ||
		!proxyAsyncWarmup || proxyMinRunes != 150 || !proxySkipNonProse {
		t.Fatalf("production defaults not applied: ttl=%v/%v warmup=%t minRunes=%d skipNonProse=%t",
			proxyValidationCacheTTL, proxyNegativeCacheTTL, proxyAsyncWarmup, proxyMinRunes, proxySkipNonProse)
	}

	t.Setenv("BABEL_PROXY_VALIDATION_CACHE_TTL", "0")
	t.Setenv("BABEL_PROXY_NEGATIVE_CACHE_TTL", "0")
	t.Setenv("BABEL_PROXY_ASYNC_WARMUP", "false")
	t.Setenv("BABEL_PROXY_MIN_RUNES", "35")
	t.Setenv("BABEL_PROXY_SKIP_NON_PROSE", "false")
	configureProxySpeedDefaults()
	if proxyValidationCacheTTL != 0 || proxyNegativeCacheTTL != 0 ||
		proxyAsyncWarmup || proxyMinRunes != 35 || proxySkipNonProse {
		t.Fatalf("explicit overrides ignored: ttl=%v/%v warmup=%t minRunes=%d skipNonProse=%t",
			proxyValidationCacheTTL, proxyNegativeCacheTTL, proxyAsyncWarmup, proxyMinRunes, proxySkipNonProse)
	}

	t.Setenv("BABEL_PROXY_VALIDATION_CACHE_TTL", "90s")
	configureProxySpeedDefaults()
	if proxyValidationCacheTTL != 90*time.Second {
		t.Fatalf("explicit duration ignored: %v", proxyValidationCacheTTL)
	}
}

// 相同的候选与策略只审计一次；换校验模型或加必含关键词必须失效重验。
func TestValidationCacheReusesPassAndInvalidatesOnPolicyOrModelChange(t *testing.T) {
	isolateCompilerState(t)
	resetProxySpeedVars(t)
	proxyValidationCacheTTL = 10 * time.Minute

	var auditCalls atomic.Int32
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditCalls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer auditor.Close()
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeModelContent(w, "状态=待审 ⇒ 主管批准")
	}))
	defer compiler.Close()

	writeAuditorConfig := func(model string) {
		t.Helper()
		if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
			"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen", AuditProfile: "auditor"},
			"auditor":  {Name: "auditor", APIKey: "audit-key", BaseURL: auditor.URL, Model: model},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	compile := func(keywords []string) {
		t.Helper()
		if _, err := doCompileContext(context.Background(), CompileReq{
			Profile:               "compiler",
			Text:                  "订单处于待审核状态时，必须由主管批准后才能继续处理。",
			StrictGate:            true,
			RequiredKeywords:      keywords,
			SkipAuditIfNotShorter: true,
			proxyPath:             true,
		}); err != nil {
			t.Fatalf("compile failed: %v", err)
		}
	}

	writeAuditorConfig("audit-model")
	compile(nil)
	compile(nil)
	if got := auditCalls.Load(); got != 1 {
		t.Fatalf("identical policy audited %d times; want 1", got)
	}

	writeAuditorConfig("audit-model-2")
	compile(nil)
	if got := auditCalls.Load(); got != 2 {
		t.Fatalf("audit model change did not invalidate cache: %d calls", got)
	}

	compile([]string{"主管批准"})
	if got := auditCalls.Load(); got != 3 {
		t.Fatalf("required keywords change did not invalidate cache: %d calls", got)
	}

	// Web/CLI 路径（未标记 proxyPath）永远不得复用代理缓存。
	validationCache.Reset()
	compile(nil) // proxyPath=true 写入缓存
	before := auditCalls.Load()
	if _, err := doCompileContext(context.Background(), CompileReq{
		Profile:    "compiler",
		Text:       "订单处于待审核状态时，必须由主管批准后才能继续处理。",
		StrictGate: true,
	}); err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if got := auditCalls.Load(); got != before+1 {
		t.Fatalf("non-proxy compile reused proxy validation cache: %d -> %d", before, got)
	}
}

// 异步预热：首轮原文直通、相同文本只触发一次后台编译、预热完成后正常压缩。
func TestProxyShadowWarmupNonBlockingAndDeduplicated(t *testing.T) {
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyAsyncWarmup = true

	var genCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		time.Sleep(250 * time.Millisecond)
		writeModelContent(w, "状态=待审 ⇒ 主管批准")
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}
	profile := getActiveProfile(loadConfig(), "compiler")
	dialect := getDialect(defaultDialectName)
	source := "订单处于待审核状态时，必须由主管批准后才能继续处理，并记录审批人与时间。"

	start := time.Now()
	out, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("cold proxy request blocked for %v; want原文直通 < 100ms", elapsed)
	}
	if out != source {
		t.Fatalf("warmup request returned %q; want original", out)
	}

	var wg sync.WaitGroup
	var slowest atomic.Int64
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
			if elapsed := time.Since(started).Nanoseconds(); elapsed > slowest.Load() {
				slowest.Store(elapsed)
			}
		}()
	}
	wg.Wait()
	// 命中「已在预热」也必须直通：任何一次并发请求都不许跌穿到同步编译。
	if elapsed := time.Duration(slowest.Load()); elapsed > 100*time.Millisecond {
		t.Fatalf("deduplicated warmup request blocked for %v; want 原文直通 < 100ms", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if genCalls.Load() > 1 {
			t.Fatalf("identical text triggered %d background compiles; want 1", genCalls.Load())
		}
		compiled, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
		if compiled != source {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background warmup never populated the cache")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if genCalls.Load() != 1 {
		t.Fatalf("generation calls=%d; want exactly 1", genCalls.Load())
	}
}

// 负缓存只记录确定性低收益；502 这类瞬时错误必须允许下次重试。
func TestNegativeCacheSkipsUncompressibleAndNeverCachesTransientErrors(t *testing.T) {
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyNegativeCacheTTL = 10 * time.Minute

	var genCalls atomic.Int32
	failNext := atomic.Bool{}
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		if failNext.Load() {
			http.Error(w, "upstream exploded", http.StatusBadGateway)
			return
		}
		// 故意返回更长的候选：确定性低收益。
		writeModelContent(w, body.Messages[1].Content+"（补充说明：本条候选刻意保持更长，用于验证负缓存与早停路径。）")
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}
	profile := getActiveProfile(loadConfig(), "compiler")
	dialect := getDialect(defaultDialectName)
	source := "订单处于待审核状态时，必须由主管批准后才能继续处理，并记录审批人与时间。"

	translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("first call generation=%d; want 1", got)
	}
	translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("negative cache did not skip the second compile: %d calls", got)
	}

	transient := "另一段需要重新尝试的说明文本，长度超过最小阈值，用于确认上游故障不会被写进负缓存而必须下次重试。"
	failNext.Store(true)
	translateMessageBlock("openai", "user", transient, profile, dialect, nil, nil)
	firstFail := genCalls.Load()
	failNext.Store(false)
	translateMessageBlock("openai", "user", transient, profile, dialect, nil, nil)
	if got := genCalls.Load(); got != firstFail+1 {
		t.Fatalf("transient 502 was cached: generation calls %d -> %d", firstFail, got)
	}
	if strings.Contains(negativeCacheKey(dialect, profile, transient), "502") {
		t.Fatal("negative cache key must not embed error text")
	}
}

// 双缓存命中必须绕过信号量与熔断器：两者都被占满时仍要亚毫秒返回。
func TestProxyCachedAdoptionBypassesSemaphoreAndBreaker(t *testing.T) {
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyValidationCacheTTL = 10 * time.Minute

	var genCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		writeModelContent(w, "状态=待审 ⇒ 主管批准")
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}
	profile := getActiveProfile(loadConfig(), "compiler")
	dialect := getDialect(defaultDialectName)
	source := "订单处于待审核状态时，必须由主管批准后才能继续处理，并记录审批人与时间。"

	first, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if first == source || genCalls.Load() != 1 {
		t.Fatalf("fixture did not warm the caches: out=%q gen=%d", first, genCalls.Load())
	}

	// 占满唯一槽位并打开熔断器：纯缓存命中不应受任何影响。
	oldSem := proxyCompileSem
	proxyCompileSem = newCompileSemaphore(1)
	proxyCompileSem.slots <- struct{}{}
	oldBreaker := compilerCB
	compilerCB = newCircuitBreaker(1, time.Minute)
	compilerCB.RecordFailure()
	t.Cleanup(func() {
		<-proxyCompileSem.slots
		proxyCompileSem = oldSem
		compilerCB = oldBreaker
	})

	started := time.Now()
	out, logID, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("cached adoption waited on semaphore/breaker: %v", elapsed)
	}
	if out != first || logID == "" || genCalls.Load() != 1 {
		t.Fatalf("cached adoption did not reuse the candidate: out=%q gen=%d", out, genCalls.Load())
	}
	if status := compilerCB.Status(); status["state"] == "CLOSED" {
		t.Fatal("cached adoption wrongly reset an open circuit breaker")
	}
}

// 负缓存键必须带上目标编码：切换精确计数后应允许重新尝试。
func TestNegativeCacheKeySeparatesTokenEncodings(t *testing.T) {
	p := Profile{Name: "x", BaseURL: "https://example.com/v1/chat/completions", Model: "m"}
	dialect := getDialect(defaultDialectName)
	text := "同一段文本在两种计数口径下的负缓存必须互相独立。"
	estimated := p
	estimated.TokenEncoding = "estimate"
	exact := p
	exact.TokenEncoding = "cl100k_base"
	if negativeCacheKey(dialect, estimated, text) == negativeCacheKey(dialect, exact, text) {
		t.Fatal("negative cache key ignored token encoding")
	}
}
