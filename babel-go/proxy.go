package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var proxyLogSequence atomic.Uint64

func containsASCIIFold(s, lowerNeedle string) bool {
	n := len(lowerNeedle)
	if n == 0 {
		return true
	}
	if len(s) < n {
		return false
	}
	first := lowerNeedle[0]
	limit := len(s) - n
	for i := 0; i <= limit; i++ {
		b := s[i]
		if b >= 'A' && b <= 'Z' {
			b |= 0x20
		}
		if b != first {
			continue
		}
		matched := true
		for j := 1; j < n; j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c |= 0x20
			}
			if c != lowerNeedle[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

var secretPatterns = []struct {
	re         *regexp.Regexp
	repl       string
	maybeMatch func(string) bool
}{
	{
		re:         regexp.MustCompile(`\bsk-(?:ant-|proj-)?[a-zA-Z0-9_-]{16,}\b`),
		repl:       `sk-***[REDACTED]`,
		maybeMatch: func(s string) bool { return strings.Contains(s, "sk-") },
	},
	{
		re:         regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		repl:       `AKIA***[REDACTED]`,
		maybeMatch: func(s string) bool { return strings.Contains(s, "AKIA") },
	},
	{
		re:         regexp.MustCompile(`\bgh[pousr]_[a-zA-Z0-9]{20,}\b`),
		repl:       `gh*_***[REDACTED]`,
		maybeMatch: func(s string) bool { return strings.Contains(s, "gh") && strings.Contains(s, "_") },
	},
	{
		re:         regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`),
		repl:       `eyJ***[JWT_REDACTED]`,
		maybeMatch: func(s string) bool { return strings.Contains(s, "eyJ") && strings.Contains(s, ".") },
	},
	{
		re:         regexp.MustCompile(`(?i)(\bBearer\s+)[a-zA-Z0-9._\-+/=]{12,}`),
		repl:       `${1}***[REDACTED]`,
		maybeMatch: func(s string) bool { return containsASCIIFold(s, "bearer") },
	},
	{
		re:   regexp.MustCompile(`(?i)((?:api[_-]?key|secret[_-]?key|password|passwd|pwd|access[_-]?token|auth[_-]?token)\s*[:=]\s*["']?)[a-zA-Z0-9._\-+/=!@#$%^&*]{6,}(["']?)`),
		repl: `${1}***[REDACTED]${2}`,
		maybeMatch: func(s string) bool {
			return strings.IndexAny(s, ":=") >= 0 &&
				(containsASCIIFold(s, "key") || containsASCIIFold(s, "pass") || containsASCIIFold(s, "pwd") || containsASCIIFold(s, "token"))
		},
	},
}

func redactSecrets(text string) string {
	res := text
	for _, p := range secretPatterns {
		if p.maybeMatch != nil && !p.maybeMatch(res) {
			continue
		}
		res = p.re.ReplaceAllString(res, p.repl)
	}
	return res
}

// 服务端模式下把 6 条密钥正则的扫描与上游用量回填串行丢到单个后台 goroutine，
// 既避免热路径同步跑正则，又保证用量更新严格排在日志写入之后不发生竞态；
// 单测与 CLI 仍是同步脱敏，日志可见性与既有断言不变。
type proxyLogEventKind int

const (
	proxyLogEventAppend proxyLogEventKind = iota
	proxyLogEventUsage
	proxyLogEventClear
	proxyLogEventBarrier
)

type proxyLogEvent struct {
	kind             proxyLogEventKind
	entry            ProxyLog
	logIDs           []string
	estOrigTotal     int
	estCompTotal     int
	realPromptTokens int
	done             chan struct{}
}

var (
	logRedactAsync atomic.Bool
	logRedactQueue chan proxyLogEvent
	logRedactOnce  sync.Once
)

func startAsyncLogRedaction() {
	logRedactOnce.Do(func() {
		logRedactQueue = make(chan proxyLogEvent, 2048)
		go func() {
			for ev := range logRedactQueue {
				switch ev.kind {
				case proxyLogEventAppend:
					insertProxyLog(redactProxyLog(ev.entry))
				case proxyLogEventUsage:
					applyUpstreamUsageSync(ev.logIDs, ev.estOrigTotal, ev.estCompTotal, ev.realPromptTokens)
				case proxyLogEventClear:
					logsMutex.Lock()
					proxyLogs = nil
					logsMutex.Unlock()
					if ev.done != nil {
						close(ev.done)
					}
				case proxyLogEventBarrier:
					if ev.done != nil {
						close(ev.done)
					}
				}
			}
		}()
	})
	logRedactAsync.Store(true)
}

func clearProxyLogs() {
	if logRedactAsync.Load() && logRedactQueue != nil {
		done := make(chan struct{})
		logRedactQueue <- proxyLogEvent{kind: proxyLogEventClear, done: done}
		<-done
		return
	}
	logsMutex.Lock()
	proxyLogs = nil
	logsMutex.Unlock()
}

func drainAsyncLogRedaction(timeout time.Duration) {
	if !logRedactAsync.Load() || logRedactQueue == nil {
		return
	}
	done := make(chan struct{})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case logRedactQueue <- proxyLogEvent{kind: proxyLogEventBarrier, done: done}:
	case <-timer.C:
		return
	}
	select {
	case <-done:
	case <-timer.C:
	}
}

func redactProxyLog(entry ProxyLog) ProxyLog {
	if !isLogRedactEnabled() {
		return entry
	}
	redactedOrig := redactSecrets(entry.Original)
	redactedComp := redactedOrig
	if entry.Compiled != entry.Original {
		redactedComp = redactSecrets(entry.Compiled)
	}
	if redactedOrig != entry.Original || redactedComp != entry.Compiled {
		entry.Redacted = true
		proxyMetrics.redactMatchedTotal.Add(1)
	}
	entry.Original = redactedOrig
	entry.Compiled = redactedComp
	entry.Reason = redactSecrets(entry.Reason)
	return entry
}

func insertProxyLog(entry ProxyLog) {
	logsMutex.Lock()
	defer logsMutex.Unlock()
	// 原地移位：单次追加不分配新切片，避免高并发下反复复制整段日志。
	if len(proxyLogs) < maxLogs {
		proxyLogs = append(proxyLogs, ProxyLog{})
	}
	copy(proxyLogs[1:], proxyLogs[:len(proxyLogs)-1])
	proxyLogs[0] = entry
}

func appendProxyLog(entry ProxyLog) {
	if logRedactAsync.Load() && logRedactQueue != nil {
		select {
		case logRedactQueue <- proxyLogEvent{kind: proxyLogEventAppend, entry: entry}:
			proxyMetrics.redactAsyncTotal.Add(1)
			return
		default: // 队列满时退回同步路径，绝不阻塞请求
			proxyMetrics.redactSyncFallbackTotal.Add(1)
		}
	}
	insertProxyLog(redactProxyLog(entry))
}

func applyUpstreamUsage(logIDs []string, estOrigTotal, estCompTotal, realPromptTokens int) {
	if realPromptTokens <= 0 || len(logIDs) == 0 {
		return
	}
	if logRedactAsync.Load() && logRedactQueue != nil {
		idsCopy := append([]string(nil), logIDs...)
		select {
		case logRedactQueue <- proxyLogEvent{
			kind:             proxyLogEventUsage,
			logIDs:           idsCopy,
			estOrigTotal:     estOrigTotal,
			estCompTotal:     estCompTotal,
			realPromptTokens: realPromptTokens,
		}:
			return
		default: // 队列满时退回同步路径，绝不阻塞请求
		}
	}
	applyUpstreamUsageSync(logIDs, estOrigTotal, estCompTotal, realPromptTokens)
}

func applyUpstreamUsageSync(logIDs []string, estOrigTotal, estCompTotal, realPromptTokens int) {
	if realPromptTokens <= 0 || len(logIDs) == 0 {
		return
	}

	// The provider reports the whole forwarded request, including tool schemas,
	// message framing and possibly multimodal input. The original request was not
	// sent to the provider, so its cost and actual savings are unknown.
	usageTag := fmt.Sprintf("上游实际输入 %d tok（整次请求）；文本估算 %d→%d tok", realPromptTokens, estOrigTotal, estCompTotal)

	idSet := make(map[string]bool, len(logIDs))
	for _, id := range logIDs {
		idSet[id] = true
	}

	logsMutex.Lock()
	defer logsMutex.Unlock()
	for i := range proxyLogs {
		if idSet[proxyLogs[i].ID] {
			proxyLogs[i].RealPromptTokens = realPromptTokens
			proxyLogs[i].EstOrigTokens = estOrigTotal
			proxyLogs[i].RealBillingRatio = usageTag
		}
	}
}

func isJSONResponseFormat(payload map[string]any) bool {
	rf, ok := payload["response_format"].(map[string]any)
	if !ok {
		return false
	}
	rfType, _ := rf["type"].(string)
	return rfType == "json_object"
}

// Keep every JSON number's original spelling. Passing unrelated fields such as
// tool enum values or integer IDs through float64 can silently change them.
func decodeProxyPayload(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("request must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("request must contain exactly one JSON object")
	}
	return payload, nil
}

// marshalProxyPayload disables Go's default HTML escaping so `<`, `>`, `&` in
// code blocks, tool outputs and user prompts stay untouched in forwarded JSON.
func marshalProxyPayload(payload map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func resolveProxyDialect(r *http.Request) (Dialect, []string, error) {
	name := strings.TrimSpace(r.Header.Get("X-Babel-Dialect"))
	if name == "" {
		name = strings.TrimSpace(r.Header.Get("X-Babel-Mode"))
	}
	mode, err := normalizeDialectMode(name)
	if err != nil {
		return Dialect{}, nil, err
	}
	var domains []string
	if selected := strings.TrimSpace(r.Header.Get("X-Babel-Domains")); selected != "" {
		domains = strings.Split(selected, ",")
	}
	// Validate configuration before forwarding, even when every message is short.
	// Resolve legacy code-audit's default before canonicalizing to mixed. Later
	// block compilation receives explicit domain names, so the default survives.
	packs, err := resolveDomainPacks(mode, domains)
	if err != nil {
		return Dialect{}, nil, err
	}
	domains = nil
	for _, pack := range packs {
		domains = append(domains, pack.Name)
	}
	dialect, err := composeDialect(defaultDialectName, domains, "")
	if err != nil {
		return Dialect{}, nil, err
	}
	return dialect, domains, nil
}

func translateMessageBlock(protocol, role, text string, p Profile, dialect Dialect, reqKeywords, domains []string) (string, string, int, int) {
	return translateMessageBlockContext(context.Background(), protocol, role, text, p, dialect, reqKeywords, domains)
}

func translateMessageBlockContext(ctx context.Context, protocol, role, text string, p Profile, dialect Dialect, reqKeywords, domains []string) (string, string, int, int) {
	return translateMessageBlockWithConfig(ctx, protocol, role, text, p, dialect, reqKeywords, domains, nil)
}

func translateMessageBlockWithConfig(ctx context.Context, protocol, role, text string, p Profile, dialect Dialect, reqKeywords, domains []string, preloadedCfg *Config) (string, string, int, int) {
	return translateMessageBlockWithOptions(ctx, protocol, role, text, p, dialect, reqKeywords, domains, preloadedCfg, false)
}

func translateMessageBlockWithOptions(ctx context.Context, protocol, role, text string, p Profile, dialect Dialect, reqKeywords, domains []string, preloadedCfg *Config, dryRun bool) (string, string, int, int) {
	origTok := estimateTokensBPE(text)
	if ctx.Err() != nil {
		return text, "", origTok, origTok
	}
	policy := snapshotProxyPolicy()
	if utf8.RuneCountInString(strings.TrimSpace(text)) < policy.MinRunes {
		return text, "", origTok, origTok
	}
	// 纯 JSON 或整体代码围栏对模型没有压缩价值，本地直接跳过。
	if policy.SkipNonProse && isNonProsePayload(text) {
		return text, "", origTok, origTok
	}

	var activeKeywords []string
	if len(reqKeywords) > 0 {
		textLower := strings.ToLower(text)
		for _, kw := range reqKeywords {
			if kw != "" && strings.Contains(textLower, strings.ToLower(kw)) {
				activeKeywords = append(activeKeywords, kw)
			}
		}
	}

	// 单块只组合一次 dialect 与缓存键，供快路径、预热判断和 doCompileContext 复用。
	blockDialect, dialectErr := composeDialect(dialect.Name, domains, text)
	if dialectErr != nil {
		return text, "", origTok, origTok
	}
	cacheKey := makeCacheKey(blockDialect, p, text)
	req := CompileReq{
		Profile:               p.Name,
		Text:                  text,
		Mode:                  dialect.Name,
		Domains:               domains,
		StrictGate:            true,
		RequiredKeywords:      activeKeywords,
		APIKey:                p.APIKey,
		BaseURL:               p.BaseURL,
		Model:                 p.Model,
		TokenEncoding:         p.TokenEncoding,
		ValidationMode:        p.ValidationMode,
		ReaderProfile:         &p.ReaderProfile,
		AuditProfile:          &p.AuditProfile,
		SkipAuditIfNotShorter: true,
		proxyPath:             true,
		preparedDialect:       &blockDialect,
		preparedCacheKey:      cacheKey,
	}

	// 纯内存快路径：生成缓存 + 校验缓存双命中时，完全不碰信号量与熔断器。
	if compiled, logID, compTok, hit := proxyCachedAdoptionWithOptions(protocol, role, text, p, blockDialect, domains, activeKeywords, cacheKey, preloadedCfg, dryRun); hit {
		return compiled, logID, origTok, compTok
	}

	// 负缓存命中：这段文本此前确定压不短，直接返回原文，省掉一次模型往返。
	if proxyNegativeCacheTTL > 0 && negativeCache.Get(negativeCacheKey(blockDialect, p, text)) {
		logID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), proxyLogSequence.Add(1))
		appendProxyLog(ProxyLog{
			ID: logID, Timestamp: time.Now().Format("15:04:05"),
			Protocol: protocol, Dialect: dialect.Name, Domains: domains, Role: role,
			Original: text, Compiled: text,
			Decision: "kept_original",
			Reason:   "负缓存命中：该文本此前未产生更短候选，已保留原文",
		})
		return text, logID, origTok, origTok
	}

	// 熔断窗口内直接短路：既不排队占槽位，也不额外记录失败。
	if compilerCB.IsOpen() {
		return text, "", origTok, origTok
	}

	// 预热只覆盖「本地没有候选」的冷启动；已有缓存时走正常路径直接采用。
	if policy.AsyncWarmup && !cacheKeyHasCandidate(cacheKey) {
		reason := "已触发后台异步预热，本次保留原文直通"
		if !shouldWarmupBlock(role, cacheKey) {
			reason = "一次性 user 文本未达预热频次阈值，本次保留原文直通"
		} else {
			status := enqueueShadowWarmup(protocol, role, text, p, blockDialect, activeKeywords, domains)
			switch status {
			case warmupInFlight:
				reason = "该文本已在后台预热，本次保留原文直通"
			case warmupQueueFull:
				reason = "预热队列已满，本次保留原文直通"
			}
		}
		logID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), proxyLogSequence.Add(1))
		appendProxyLog(ProxyLog{
			ID: logID, Timestamp: time.Now().Format("15:04:05"),
			Protocol: protocol, Dialect: dialect.Name, Domains: domains, Role: role,
			Original: text, Compiled: text,
			Decision: "kept_original",
			Reason:   reason,
		})
		return text, logID, origTok, origTok
	}

	// 全局并发闸门：队列超时说明编译侧忙不过来，退回原文而不是拖住上游请求。
	releaseSem, acquired, canceled := proxyCompileSem.Acquire(ctx, proxyCompileQueueTimeout)
	if canceled {
		return text, "", origTok, origTok
	}
	if !acquired {
		logID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), proxyLogSequence.Add(1))
		appendProxyLog(ProxyLog{
			ID: logID, Timestamp: time.Now().Format("15:04:05"),
			Protocol: protocol, Dialect: dialect.Name, Domains: domains, Role: role,
			Original: text, Compiled: text,
			Decision: "kept_original",
			Reason:   "编译并发队列繁忙，已保留原文",
		})
		return text, logID, origTok, origTok
	}
	defer releaseSem()

	allowed, probeID := compilerCB.Acquire()
	if !allowed {
		return text, "", origTok, origTok
	}

	res, err := doCompileContext(ctx, req)
	if ctx.Err() != nil {
		compilerCB.ReleaseProbe(probeID)
		return text, "", origTok, origTok
	}
	// A source-derived cache key is not a request ID: repeated requests must not
	// overwrite one another's provider usage in the log.
	logID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), proxyLogSequence.Add(1))
	entry := ProxyLog{
		ID: logID, Timestamp: time.Now().Format("15:04:05"),
		Protocol: protocol, Dialect: dialect.Name, Domains: domains, Role: role,
		Original: text, Compiled: text, CacheHit: res.CacheHit,
		Decision:            "kept_original",
		MeasurementKind:     res.Stats.MeasurementKind,
		MeasurementEncoding: res.Stats.MeasurementEncoding,
		ValidationMethod:    res.Validation.Method,
	}
	if err != nil {
		var validationErr *ValidationError
		var measurementErr *TokenMeasurementError
		if errors.As(err, &measurementErr) {
			compilerCB.RecordSuccess()
			entry.Reason = "目标文本计数不可用，已保留原文"
		} else if errors.As(err, &validationErr) && validationErr.Validation.Status == "failed" {
			// A rejected candidate proves the compiler is reachable. Release any
			// half-open probe rather than leaving the circuit permanently busy.
			compilerCB.RecordSuccess()
			proxyMetrics.validationRejectedTotal.Add(1)
			entry.ValidationMethod = validationErr.Validation.Method
			entry.Reason = "候选未通过编译检查，已保留原文"
		} else {
			compilerCB.RecordFailure()
			entry.Reason = "编译或审计服务异常，已保留原文"
		}
		appendProxyLog(entry)
		return text, logID, origTok, origTok
	}

	compilerCB.RecordSuccess()
	// Aggregate log totals stay explicitly heuristic. Adoption uses the selected
	// encoding's per-request counts, never an earlier singleflight caller's count.
	compTok := estimateTokensBPE(res.Compiled)
	countLabel := "估算 Token"
	if res.Stats.MeasurementKind == "exact" {
		countLabel = res.Stats.MeasurementEncoding + " 文本 Token"
	}
	if strings.TrimSpace(res.Compiled) == "" || res.Stats.CompiledTokens >= res.Stats.OriginalTokens {
		if proxyNegativeCacheTTL > 0 && res.Stats.OriginalTokens > 0 {
			negativeCache.Put(negativeCacheKey(blockDialect, p, text), proxyNegativeCacheTTL)
		}
		entry.Reason = "候选未减少" + countLabel + "，已保留原文"
		appendProxyLog(entry)
		return text, logID, origTok, origTok
	}
	entry.Compiled = res.Compiled
	entry.Ratio = res.Stats.CompressionRatio
	if dryRun {
		entry.Decision = "dry_run_would_adopt"
		entry.Reason = "Dry-Run 观察模式：本次审计通过，且" + countLabel + "更少，实际已原样转发原文"
	} else {
		entry.Decision = "adopted"
		entry.Reason = "本次审计通过，且" + countLabel + "更少"
	}
	appendProxyLog(entry)
	return res.Compiled, logID, origTok, compTok
}

func cacheKeyHasCandidate(cacheKey string) bool {
	cacheMutex.RLock()
	entry, ok := diskCache[cacheKey]
	cacheMutex.RUnlock()
	return ok && entry.Compiled != ""
}

// proxyCachedAdoption 是纯内存快路径：生成缓存命中、且同一策略指纹下的校验
// 结论为 passed 时，直接采用候选，不占信号量、不碰熔断器，也不上报成功探针。
func proxyCachedAdoption(protocol, role, text string, p Profile, dialect Dialect, domains, keywords []string, cacheKey string) (string, string, int, bool) {
	return proxyCachedAdoptionWithOptions(protocol, role, text, p, dialect, domains, keywords, cacheKey, nil, false)
}

func proxyCachedAdoptionWithConfig(protocol, role, text string, p Profile, dialect Dialect, domains, keywords []string, cacheKey string, preloadedCfg *Config) (string, string, int, bool) {
	return proxyCachedAdoptionWithOptions(protocol, role, text, p, dialect, domains, keywords, cacheKey, preloadedCfg, false)
}

func proxyCachedAdoptionWithOptions(protocol, role, text string, p Profile, dialect Dialect, domains, keywords []string, cacheKey string, preloadedCfg *Config, dryRun bool) (string, string, int, bool) {
	if proxyValidationCacheTTL <= 0 {
		return "", "", 0, false
	}
	cacheMutex.RLock()
	entry, ok := diskCache[cacheKey]
	cacheMutex.RUnlock()
	if !ok || entry.Compiled == "" {
		return "", "", 0, false
	}
	if len(babelStructureIssues(text, entry.Compiled)) > 0 {
		return "", "", 0, false
	}

	var cfg Config
	if preloadedCfg != nil {
		cfg = *preloadedCfg
	} else {
		cfg = loadConfig()
	}
	reader := p
	if p.ValidationMode == "blind" {
		resolved, err := resolveReaderProfile(cfg, p)
		if err != nil {
			return "", "", 0, false
		}
		reader = resolved
	}
	auditor, err := resolveAuditProfile(cfg, p, "")
	if err != nil {
		return "", "", 0, false
	}
	lookup := CompileReq{Text: text, RequiredKeywords: keywords}
	if _, cached := validationCache.Get(makeValidationCacheKey(lookup, p.ValidationMode, auditor, reader, entry.Compiled)); !cached {
		return "", "", 0, false
	}

	before, err := measureTokens(text, p.TokenEncoding)
	if err != nil {
		return "", "", 0, false
	}
	after, err := measureTokens(entry.Compiled, p.TokenEncoding)
	if err != nil || after.Tokens >= before.Tokens {
		return "", "", 0, false
	}
	now := time.Now().Unix()
	if now-entry.LastAccess >= 30 {
		cacheMutex.Lock()
		if current, exists := diskCache[cacheKey]; exists && current.Compiled != "" {
			current.LastAccess = now
			diskCache[cacheKey] = current
		}
		cacheMutex.Unlock()
	}
	compTok := estimateTokensBPE(entry.Compiled)
	label := "估算"
	if before.Kind == "exact" {
		label = "所选编码精确计数: " + before.Encoding
	}
	percent := 0.0
	if before.Tokens > 0 {
		percent = (1.0 - float64(after.Tokens)/float64(before.Tokens)) * 100
	}
	proxyMetrics.fastPathHitTotal.Add(1)
	logID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), proxyLogSequence.Add(1))
	decision := "adopted"
	reason := "生成缓存与校验缓存双命中，直接采用"
	if dryRun {
		decision = "dry_run_would_adopt"
		reason = "Dry-Run 观察模式：生成与校验缓存双命中，已测算可省 Token，实际已原样转发原文"
	}
	appendProxyLog(ProxyLog{
		ID: logID, Timestamp: time.Now().Format("15:04:05"),
		Protocol: protocol, Dialect: dialect.Name, Domains: domains, Role: role,
		Original: text, Compiled: entry.Compiled, CacheHit: true,
		Decision:            decision,
		Reason:              reason,
		Ratio:               fmt.Sprintf("文本 Token（%s）%d→%d (减少%.1f%%)", label, before.Tokens, after.Tokens, percent),
		MeasurementKind:     before.Kind,
		MeasurementEncoding: before.Encoding,
		ValidationMethod:    "cached",
	})
	return entry.Compiled, logID, compTok, true
}

// proxyTextBlock 是一个待编译文本槽位。apply 在全部并发结果就绪后才执行，
// 因此所有对请求 payload 的写入都发生在同一 goroutine，不存在并发写 map。
type proxyTextBlock struct {
	role        string
	text        string
	reqKeywords []string
	apply       func(string)
}

type proxyBlockResult struct {
	compiled string
	logID    string
	origTok  int
	compTok  int
}

// appendProxyTextBlock 只把够长的文本加入编译队列；短文本按原文计入统计。
func appendProxyTextBlock(blocks *[]proxyTextBlock, role, text string, reqKeywords []string, apply func(string), estOrig, estComp *int) {
	if utf8.RuneCountInString(strings.TrimSpace(text)) < snapshotProxyPolicy().MinRunes {
		t := estimateTokensBPE(text)
		*estOrig += t
		*estComp += t
		return
	}
	*blocks = append(*blocks, proxyTextBlock{
		role:        role,
		text:        text,
		reqKeywords: reqKeywords,
		apply:       apply,
	})
}

const maxSchemaTraversalDepth = 16

// Only description strings are eligible for compilation. Schema keywords,
// property names, enum values and all other protocol data keep their values.
func collectSchemaBlocks(toolLabel string, node map[string]any, blocks *[]proxyTextBlock, estOrig, estComp *int) {
	collectSchemaBlocksDepth(toolLabel, node, 0, blocks, estOrig, estComp)
}

func collectSchemaBlocksDepth(toolLabel string, node map[string]any, depth int, blocks *[]proxyTextBlock, estOrig, estComp *int) {
	if node == nil || depth > maxSchemaTraversalDepth {
		return
	}
	if desc, ok := node["description"].(string); ok {
		target := node
		appendProxyTextBlock(blocks, toolLabel, desc, nil, func(v string) { target["description"] = v }, estOrig, estComp)
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for propName, propVal := range props {
			if propMap, ok := propVal.(map[string]any); ok {
				collectSchemaBlocksDepth(toolLabel+"."+propName, propMap, depth+1, blocks, estOrig, estComp)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		collectSchemaBlocksDepth(toolLabel+"[]", items, depth+1, blocks, estOrig, estComp)
	}
	if prefixItems, ok := node["prefixItems"].([]any); ok {
		for i, item := range prefixItems {
			if itemMap, ok := item.(map[string]any); ok {
				collectSchemaBlocksDepth(fmt.Sprintf("%s[%d]", toolLabel, i), itemMap, depth+1, blocks, estOrig, estComp)
			}
		}
	}
	if addProps, ok := node["additionalProperties"].(map[string]any); ok {
		collectSchemaBlocksDepth(toolLabel+".*", addProps, depth+1, blocks, estOrig, estComp)
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := node[keyword].([]any); ok {
			for i, v := range variants {
				if vMap, ok := v.(map[string]any); ok {
					collectSchemaBlocksDepth(fmt.Sprintf("%s.%s[%d]", toolLabel, keyword, i), vMap, depth+1, blocks, estOrig, estComp)
				}
			}
		}
	}
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]any); ok {
			for defName, defVal := range defs {
				if defMap, ok := defVal.(map[string]any); ok {
					collectSchemaBlocksDepth(toolLabel+"."+defsKey+"."+defName, defMap, depth+1, blocks, estOrig, estComp)
				}
			}
		}
	}
}

func hasCacheControl(m map[string]any) bool {
	if m == nil {
		return false
	}
	v, ok := m["cache_control"]
	return ok && v != nil
}

func messageHasCacheControl(msgMap map[string]any) bool {
	if hasCacheControl(msgMap) {
		return true
	}
	if parts, ok := msgMap["content"].([]any); ok {
		for _, part := range parts {
			if partMap, ok := part.(map[string]any); ok && hasCacheControl(partMap) {
				return true
			}
		}
	}
	return false
}

func collectToolsBlocksWithOptions(tools []any, skipCacheControl bool, blocks *[]proxyTextBlock, estOrig, estComp *int) {
	lastCachedIdx := -1
	if skipCacheControl {
		// In Anthropic prompt caching, placing cache_control on tool[k] caches the
		// entire tools[0..k] prefix; mutating any earlier tool busts that prefix.
		for i, item := range tools {
			if toolMap, ok := item.(map[string]any); ok && hasCacheControl(toolMap) {
				lastCachedIdx = i
			}
		}
	}
	for i, item := range tools {
		if i <= lastCachedIdx {
			continue
		}
		toolMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fnMap, ok := toolMap["function"].(map[string]any); ok {
			fnName, _ := fnMap["name"].(string)
			if fnName == "" {
				fnName = "unknown"
			}
			label := "tool:" + fnName
			if desc, ok := fnMap["description"].(string); ok {
				target := fnMap
				appendProxyTextBlock(blocks, label, desc, nil, func(v string) { target["description"] = v }, estOrig, estComp)
			}
			if params, ok := fnMap["parameters"].(map[string]any); ok {
				collectSchemaBlocks(label, params, blocks, estOrig, estComp)
			}
			continue
		}

		toolName, _ := toolMap["name"].(string)
		if toolName == "" {
			toolName = "unknown"
		}
		label := "tool:" + toolName
		if desc, ok := toolMap["description"].(string); ok {
			target := toolMap
			appendProxyTextBlock(blocks, label, desc, nil, func(v string) { target["description"] = v }, estOrig, estComp)
		}
		if schema, ok := toolMap["input_schema"].(map[string]any); ok {
			collectSchemaBlocks(label, schema, blocks, estOrig, estComp)
		}
		// OpenAI Responses API uses flat function tools with top-level "parameters".
		if params, ok := toolMap["parameters"].(map[string]any); ok {
			collectSchemaBlocks(label, params, blocks, estOrig, estComp)
		}
	}
}

func collectToolsBlocks(tools []any, blocks *[]proxyTextBlock, estOrig, estComp *int) {
	collectToolsBlocksWithOptions(tools, false, blocks, estOrig, estComp)
}

func runProxyBlockTranslations(ctx context.Context, protocol string, blocks []proxyTextBlock, p Profile, dialect Dialect, domains []string, preloadedCfg *Config, dryRun bool) []proxyBlockResult {
	results := make([]proxyBlockResult, len(blocks))
	if len(blocks) == 1 {
		b := blocks[0]
		comp, id, oTok, cTok := translateMessageBlockWithOptions(ctx, protocol, b.role, b.text, p, dialect, b.reqKeywords, domains, preloadedCfg, dryRun)
		results[0] = proxyBlockResult{compiled: comp, logID: id, origTok: oTok, compTok: cTok}
		return results
	}
	workers := proxyBlockConcurrency
	if workers <= 0 {
		workers = 1
	}
	if workers > len(blocks) {
		workers = len(blocks)
	}
	var nextIdx atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				idx := int(nextIdx.Add(1) - 1)
				if idx >= len(blocks) {
					return
				}
				b := blocks[idx]
				comp, id, oTok, cTok := translateMessageBlockWithOptions(ctx, protocol, b.role, b.text, p, dialect, b.reqKeywords, domains, preloadedCfg, dryRun)
				results[idx] = proxyBlockResult{compiled: comp, logID: id, origTok: oTok, compTok: cTok}
			}
		}()
	}
	wg.Wait()
	return results
}

func compileProxyBlocks(ctx context.Context, protocol string, blocks []proxyTextBlock, p Profile, dialect Dialect, domains []string, logIDs *[]string, estOrig, estComp *int) bool {
	mutated, _, _ := compileProxyBlocksWithOptions(ctx, protocol, blocks, p, dialect, domains, nil, logIDs, estOrig, estComp, false)
	return mutated
}

func compileProxyBlocksWithConfig(ctx context.Context, protocol string, blocks []proxyTextBlock, p Profile, dialect Dialect, domains []string, preloadedCfg *Config, logIDs *[]string, estOrig, estComp *int) bool {
	mutated, _, _ := compileProxyBlocksWithOptions(ctx, protocol, blocks, p, dialect, domains, preloadedCfg, logIDs, estOrig, estComp, false)
	return mutated
}

// compileProxyBlocksWithOptions 把一次请求里的文本块做有界并行编译，然后按原顺序写回。
// 启用代理缓存或预热时先对相同文本做请求内去重，避免重复校验与绕过预热频次阈值。
// Dry-Run 模式下照常测算可省 Token 与写入观察日志，但不修改请求体，保证上游收到原文。
func compileProxyBlocksWithOptions(ctx context.Context, protocol string, blocks []proxyTextBlock, p Profile, dialect Dialect, domains []string, preloadedCfg *Config, logIDs *[]string, estOrig, estComp *int, dryRun bool) (bool, int, int) {
	if len(blocks) == 0 {
		return false, 0, 0
	}
	policy := snapshotProxyPolicy()
	applyResult := func(b proxyTextBlock, r proxyBlockResult, mutated *bool, origTokens, savedTokens *int) {
		*estOrig += r.origTok
		if dryRun {
			*estComp += r.origTok
		} else {
			*estComp += r.compTok
		}
		if r.compiled != b.text {
			if saved := r.origTok - r.compTok; saved > 0 {
				*origTokens += r.origTok
				*savedTokens += saved
			}
			if !dryRun {
				b.apply(r.compiled)
				*mutated = true
			}
		}
	}
	if (proxyValidationCacheTTL > 0 || policy.AsyncWarmup) && len(blocks) > 1 {
		type blockGroup struct {
			rep     proxyTextBlock
			indices []int
		}
		var groups []blockGroup
		groupIdxByKey := make(map[string]int, len(blocks))
		for i, b := range blocks {
			var kwKey string
			if len(b.reqKeywords) > 0 {
				kws := append([]string(nil), b.reqKeywords...)
				sort.Strings(kws)
				kwKey = strings.Join(kws, "\x00")
			}
			key := b.text + "\x00" + kwKey
			if gIdx, exists := groupIdxByKey[key]; exists {
				if groups[gIdx].rep.role == "user" && b.role != "user" {
					groups[gIdx].rep.role = b.role
				}
				groups[gIdx].indices = append(groups[gIdx].indices, i)
			} else {
				groupIdxByKey[key] = len(groups)
				groups = append(groups, blockGroup{rep: b, indices: []int{i}})
			}
		}
		if len(groups) < len(blocks) {
			reps := make([]proxyTextBlock, len(groups))
			for i, g := range groups {
				reps[i] = g.rep
			}
			repResults := runProxyBlockTranslations(ctx, protocol, reps, p, dialect, domains, preloadedCfg, dryRun)
			mutated := false
			origTokens := 0
			savedTokens := 0
			for gIdx, g := range groups {
				r := repResults[gIdx]
				for _, idx := range g.indices {
					applyResult(blocks[idx], r, &mutated, &origTokens, &savedTokens)
				}
				if r.logID != "" {
					*logIDs = append(*logIDs, r.logID)
				}
			}
			return mutated, origTokens, savedTokens
		}
	}
	results := runProxyBlockTranslations(ctx, protocol, blocks, p, dialect, domains, preloadedCfg, dryRun)
	mutated := false
	origTokens := 0
	savedTokens := 0
	for i, b := range blocks {
		r := results[i]
		applyResult(b, r, &mutated, &origTokens, &savedTokens)
		if r.logID != "" {
			*logIDs = append(*logIDs, r.logID)
		}
	}
	return mutated, origTokens, savedTokens
}

func extractUsageFromOpenAISSETail(tail string) int {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonStr == "" || jsonStr == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(jsonStr), &chunk) == nil && chunk.Usage.PromptTokens > 0 {
			return chunk.Usage.PromptTokens
		}
	}
	return 0
}

func extractUsageFromAnthropicSSE(buf string) int {
	lines := strings.Split(buf, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonStr == "" {
			continue
		}
		var evt struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(jsonStr), &evt) == nil && evt.Type == "message_start" {
			total := evt.Message.Usage.InputTokens + evt.Message.Usage.CacheCreationInputTokens + evt.Message.Usage.CacheReadInputTokens
			if total > 0 {
				return total
			}
		}
	}
	return 0
}

type proxyCollectOptions struct {
	skipCacheControl  bool
	compressDeveloper bool
}

type proxyProtocolAdapter struct {
	protocol           string
	missingAuthMessage string
	hasCallerAuth      func(r *http.Request) bool
	resolveTargetURL   func(r *http.Request, p Profile) (string, error)
	collectBlocks      func(payload map[string]any, opts proxyCollectOptions, blocks *[]proxyTextBlock, estOrig, estComp *int)
	prepareHeaders     func(dst, src http.Header)
	extractUsage       func(respBytes []byte) int
	newSSESampler      func() *sseUsageSampler
}

func resolveRequestCompileBudget(r *http.Request) time.Duration {
	if raw := strings.TrimSpace(r.Header.Get("X-Babel-Compile-Budget")); raw != "" {
		return parseProxyDuration(raw)
	}
	return snapshotProxyPolicy().CompileBudget
}

func resolveRequestSkipCacheControl(r *http.Request) bool {
	defaultSkip := snapshotProxyPolicy().SkipCacheControl
	if raw := strings.TrimSpace(r.Header.Get("X-Babel-Skip-Cache-Control")); raw != "" {
		return parseProxyBool(raw, defaultSkip)
	}
	return defaultSkip
}

func resolveRequestCompressDeveloper(r *http.Request) bool {
	defaultCompress := snapshotProxyPolicy().CompressDeveloper
	if raw := strings.TrimSpace(r.Header.Get("X-Babel-Compress-Developer")); raw != "" {
		return parseProxyBool(raw, defaultCompress)
	}
	return defaultCompress
}

func resolveRequestDryRun(r *http.Request) bool {
	defaultDryRun := snapshotProxyPolicy().DryRun
	if raw := strings.TrimSpace(r.Header.Get("X-Babel-Dry-Run")); raw != "" {
		return parseProxyBool(raw, defaultDryRun)
	}
	return defaultDryRun
}

func isResponsesJSONFormat(payload map[string]any) bool {
	textCfg, ok := payload["text"].(map[string]any)
	if !ok {
		return false
	}
	formatCfg, ok := textCfg["format"].(map[string]any)
	if !ok {
		return false
	}
	formatType, _ := formatCfg["type"].(string)
	return formatType == "json_object"
}

func isProxyBypassRequested(r *http.Request) bool {
	return parseProxyBool(r.Header.Get("X-Babel-Bypass"), false)
}

func handleStructuredProxy(w http.ResponseWriter, r *http.Request, adapter proxyProtocolAdapter) {
	proxyMetrics.requestsTotal.Add(1)
	if r.Method != http.MethodPost {
		writeProxyError(w, adapter.protocol, http.StatusMethodNotAllowed, "invalid_request_error", "Method Not Allowed")
		return
	}

	forwardDirect := func(status string, body io.Reader, contentLength int64) {
		if !adapter.hasCallerAuth(r) {
			writeProxyError(w, adapter.protocol, http.StatusUnauthorized, "authentication_error", adapter.missingAuthMessage)
			return
		}
		cfg := loadConfig()
		p, err := resolveProxyProfile(r, cfg)
		if err != nil {
			writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		targetURL, err := adapter.resolveTargetURL(r, p)
		if err != nil {
			writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if status == "bypass" {
			proxyMetrics.bypassTotal.Add(1)
		} else {
			proxyMetrics.oversizeTotal.Add(1)
		}
		var extra func(http.Header)
		if adapter.protocol == "anthropic" {
			extra = func(h http.Header) {
				if h.Get("anthropic-version") == "" {
					h.Set("anthropic-version", "2023-06-01")
				}
			}
		}
		streamPassthroughBodyWithStatus(w, r, adapter.protocol, targetURL, status, body, contentLength, extra)
	}

	compileLimit := proxyMaxCompileBodyLimit()
	if isProxyBypassRequested(r) {
		forwardDirect("bypass", r.Body, r.ContentLength)
		return
	}
	if r.ContentLength > compileLimit {
		forwardDirect("oversize", r.Body, r.ContentLength)
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, compileLimit+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProxyError(w, adapter.protocol, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("请求正文超过 %d 字节上限", maxErr.Limit))
			return
		}
		writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", "读取请求失败")
		return
	}
	if int64(len(rawBody)) > compileLimit {
		// Chunked or unknown-length body exceeded compileLimit: stream the already-read
		// prefix plus the remaining request body directly upstream without buffering all of it.
		forwardDirect("oversize", io.MultiReader(bytes.NewReader(rawBody), r.Body), -1)
		return
	}

	payload, err := decodeProxyPayload(rawBody)
	if err != nil {
		writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体")
		return
	}
	if !adapter.hasCallerAuth(r) {
		writeProxyError(w, adapter.protocol, http.StatusUnauthorized, "authentication_error", adapter.missingAuthMessage)
		return
	}

	cfg := loadConfig()
	p, err := resolveProxyProfile(r, cfg)
	if err != nil {
		writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	dialect, domains, err := resolveProxyDialect(r)
	if err != nil {
		writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Validate the final destination before any generation or audit request.
	targetURL, err := adapter.resolveTargetURL(r, p)
	if err != nil {
		writeProxyError(w, adapter.protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	var (
		logIDs       []string
		estOrigTotal int
		estCompTotal int
		blocks       []proxyTextBlock
	)
	adapter.collectBlocks(payload, proxyCollectOptions{
		skipCacheControl:  resolveRequestSkipCacheControl(r),
		compressDeveloper: resolveRequestCompressDeveloper(r),
	}, &blocks, &estOrigTotal, &estCompTotal)

	dryRun := resolveRequestDryRun(r)
	compileStart := time.Now()
	compileCtx := r.Context()
	var cancelCompile context.CancelFunc
	if budget := resolveRequestCompileBudget(r); budget > 0 {
		compileCtx, cancelCompile = context.WithTimeout(r.Context(), budget)
	}
	mutated, origTokens, savedTokens := compileProxyBlocksWithOptions(compileCtx, adapter.protocol, blocks, p, dialect, domains, &cfg, &logIDs, &estOrigTotal, &estCompTotal, dryRun)
	budgetExceeded := cancelCompile != nil && compileCtx.Err() != nil && r.Context().Err() == nil
	if cancelCompile != nil {
		cancelCompile()
	}
	compileElapsedMS := time.Since(compileStart).Milliseconds()
	if r.Context().Err() != nil {
		return
	}

	babelStatus := "passthrough"
	switch {
	case dryRun:
		proxyMetrics.dryRunTotal.Add(1)
		proxyMetrics.passthroughTotal.Add(1)
		if savedTokens > 0 {
			babelStatus = "dry-run-hit"
			proxyMetrics.dryRunWouldAdoptTotal.Add(1)
			proxyMetrics.dryRunOrigTokensTotal.Add(uint64(origTokens))
			proxyMetrics.dryRunPotentialSavedTokensTotal.Add(uint64(savedTokens))
		} else {
			babelStatus = "dry-run"
		}
	case mutated:
		babelStatus = "adopted"
		proxyMetrics.adoptedTotal.Add(1)
		if savedTokens > 0 {
			proxyMetrics.adoptedOrigTokensTotal.Add(uint64(origTokens))
			proxyMetrics.adoptedSavedTokensTotal.Add(uint64(savedTokens))
		}
	case budgetExceeded:
		babelStatus = "budget_timeout"
		proxyMetrics.budgetTimeoutTotal.Add(1)
		proxyMetrics.passthroughTotal.Add(1)
	case len(blocks) > 0:
		babelStatus = "fallback"
		proxyMetrics.passthroughTotal.Add(1)
	default:
		proxyMetrics.passthroughTotal.Add(1)
	}

	isStream, _ := payload["stream"].(bool)
	// Preserve caller-selected streaming options. Compatible providers may reject
	// optional fields; usage is recorded only when it appears in their response.
	// 未替换任何文本时直接复用原始字节，避免对上 MB 的工具输出重新序列化。
	modifiedBody := rawBody
	if mutated && !dryRun {
		if encoded, err := marshalProxyPayload(payload); err == nil {
			modifiedBody = encoded
		}
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewBuffer(modifiedBody))
	if err != nil {
		writeProxyError(w, adapter.protocol, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	adapter.prepareHeaders(proxyReq.Header, r.Header)

	resp, err := newProxyForwardHTTPClient().Do(proxyReq)
	if err != nil {
		proxyMetrics.upstreamErrorTotal.Add(1)
		writeProxyError(w, adapter.protocol, http.StatusBadGateway, "api_error", fmt.Sprintf("上游转发失败: %v", err))
		return
	}
	defer resp.Body.Close()
	if rejectProxyRedirectForProtocol(w, adapter.protocol, resp) {
		proxyMetrics.upstreamErrorTotal.Add(1)
		return
	}

	copyProxyResponseHeaders(w.Header(), resp.Header)
	setDiagnosticHeader(w.Header(), "X-Babel-Status", babelStatus)
	setDiagnosticHeader(w.Header(), "X-Babel-Compile-Ms", fmt.Sprintf("%d", compileElapsedMS))
	if dryRun && savedTokens > 0 {
		setDiagnosticHeader(w.Header(), "X-Babel-Dry-Run-Saved-Tokens", strconv.Itoa(savedTokens))
	}
	w.WriteHeader(resp.StatusCode)

	hasEncodedBody := strings.TrimSpace(resp.Header.Get("Content-Encoding")) != ""
	useSSE := isEventStreamResponse(resp) || (isStream && resp.StatusCode >= 200 && resp.StatusCode < 300)
	if !useSSE {
		if hasEncodedBody {
			if _, copyErr := io.Copy(w, resp.Body); copyErr != nil && r.Context().Err() == nil {
				panic(http.ErrAbortHandler)
			}
			return
		}
		respBytes, copyErr := forwardProxyResponseBody(w, resp.Body)
		if copyErr != nil {
			if r.Context().Err() == nil {
				panic(http.ErrAbortHandler)
			}
			return
		}
		if realTokens := adapter.extractUsage(respBytes); realTokens > 0 {
			applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, realTokens)
		}
		return
	}

	flusher, canFlush := w.(http.Flusher)
	bufPtr := proxyStreamBufPool.Get().(*[]byte)
	defer proxyStreamBufPool.Put(bufPtr)
	buf := *bufPtr
	var sampler *sseUsageSampler
	if !hasEncodedBody {
		sampler = adapter.newSSESampler()
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
			if sampler != nil {
				sampler.Write(chunk)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && r.Context().Err() == nil {
				panic(http.ErrAbortHandler)
			}
			break
		}
	}

	if sampler != nil {
		if realTokens := sampler.Tokens(); realTokens > 0 {
			applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, realTokens)
		}
	}
}

var openAIProxyAdapter = proxyProtocolAdapter{
	protocol:           "openai",
	missingAuthMessage: "代理请求必须携带最终服务的 Authorization 或 api-key；不会使用已保存的编译密钥",
	hasCallerAuth: func(r *http.Request) bool {
		return strings.TrimSpace(r.Header.Get("Authorization")) != "" || strings.TrimSpace(r.Header.Get("api-key")) != ""
	},
	resolveTargetURL: func(r *http.Request, p Profile) (string, error) {
		upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
		if upstreamBase == "" {
			upstreamBase = strings.TrimSpace(p.UpstreamURL)
		}
		targetURL := compilerRequestURL(p.BaseURL)
		if upstreamBase != "" {
			targetURL = buildOpenAICompletionsURL(upstreamBase)
		}
		targetURL = mergeRequestQuery(targetURL, r.URL.RawQuery)
		if err := validateOutboundURL(targetURL); err != nil {
			return "", fmt.Errorf("上游地址无效: %v", err)
		}
		return targetURL, nil
	},
	collectBlocks: func(payload map[string]any, opts proxyCollectOptions, blocks *[]proxyTextBlock, estOrig, estComp *int) {
		var reqKeywords []string
		if isJSONResponseFormat(payload) {
			reqKeywords = append(reqKeywords, "json")
		}
		if msgs, ok := payload["messages"].([]any); ok {
			lastCachedMsgIdx := -1
			if opts.skipCacheControl {
				for i, m := range msgs {
					if msgMap, ok := m.(map[string]any); ok && messageHasCacheControl(msgMap) {
						lastCachedMsgIdx = i
					}
				}
			}
			for i, m := range msgs {
				msgMap, ok := m.(map[string]any)
				if !ok {
					continue
				}
				role, _ := msgMap["role"].(string)
				// Compile only system/user by default; developer is opt-in via
				// BABEL_PROXY_COMPRESS_DEVELOPER / X-Babel-Compress-Developer.
				eligible := role == "system" || role == "user" || (opts.compressDeveloper && role == "developer")
				if !eligible {
					if contentStr, ok := msgMap["content"].(string); ok {
						t := estimateTokensBPE(contentStr)
						*estOrig += t
						*estComp += t
					}
					continue
				}
				switch content := msgMap["content"].(type) {
				case string:
					if i <= lastCachedMsgIdx {
						t := estimateTokensBPE(content)
						*estOrig += t
						*estComp += t
						continue
					}
					target := msgMap
					appendProxyTextBlock(blocks, role, content, reqKeywords, func(v string) { target["content"] = v }, estOrig, estComp)
				case []any:
					lastCachedPartIdx := -1
					if opts.skipCacheControl {
						for j, part := range content {
							if partMap, ok := part.(map[string]any); ok && hasCacheControl(partMap) {
								lastCachedPartIdx = j
							}
						}
					}
					for j, part := range content {
						if partMap, ok := part.(map[string]any); ok {
							if partMap["type"] == "text" {
								if txt, ok := partMap["text"].(string); ok {
									if i < lastCachedMsgIdx || j <= lastCachedPartIdx {
										t := estimateTokensBPE(txt)
										*estOrig += t
										*estComp += t
										continue
									}
									target := partMap
									appendProxyTextBlock(blocks, role, txt, reqKeywords, func(v string) { target["text"] = v }, estOrig, estComp)
								}
							}
						}
					}
				}
			}
		}
		if tools, ok := payload["tools"].([]any); ok {
			collectToolsBlocksWithOptions(tools, opts.skipCacheControl, blocks, estOrig, estComp)
		}
	},
	prepareHeaders: func(dst, src http.Header) {
		copyProxyRequestHeaders(dst, src)
		dst.Set("Content-Type", "application/json")
	},
	extractUsage: func(respBytes []byte) int {
		var nonStreamResp struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil && nonStreamResp.Usage.PromptTokens > 0 {
			return nonStreamResp.Usage.PromptTokens
		}
		return 0
	},
	newSSESampler: newOpenAISSEUsageSampler,
}

var anthropicProxyAdapter = proxyProtocolAdapter{
	protocol:           "anthropic",
	missingAuthMessage: "代理请求必须携带最终服务的 x-api-key 或 Authorization；不会使用已保存的编译密钥",
	hasCallerAuth: func(r *http.Request) bool {
		return strings.TrimSpace(r.Header.Get("x-api-key")) != "" || strings.TrimSpace(r.Header.Get("Authorization")) != ""
	},
	resolveTargetURL: func(r *http.Request, p Profile) (string, error) {
		upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
		if upstreamBase == "" {
			upstreamBase = strings.TrimSpace(p.UpstreamURL)
		}
		if upstreamBase == "" {
			return "", fmt.Errorf("Anthropic 代理需要 upstream_url 或 X-Upstream-BaseURL；无法从编译请求 URL 推导 Messages 地址")
		}
		targetURL := mergeRequestQuery(buildAnthropicMessagesURL(upstreamBase), r.URL.RawQuery)
		if err := validateOutboundURL(targetURL); err != nil {
			return "", fmt.Errorf("上游地址无效: %v", err)
		}
		return targetURL, nil
	},
	collectBlocks: func(payload map[string]any, opts proxyCollectOptions, blocks *[]proxyTextBlock, estOrig, estComp *int) {
		switch sysVal := payload["system"].(type) {
		case string:
			target := payload
			appendProxyTextBlock(blocks, "system", sysVal, nil, func(v string) { target["system"] = v }, estOrig, estComp)
		case []any:
			lastCachedSysIdx := -1
			if opts.skipCacheControl {
				for i, block := range sysVal {
					if blockMap, ok := block.(map[string]any); ok && hasCacheControl(blockMap) {
						lastCachedSysIdx = i
					}
				}
			}
			for i, block := range sysVal {
				if blockMap, ok := block.(map[string]any); ok {
					if blockMap["type"] == "text" {
						if txt, ok := blockMap["text"].(string); ok {
							if i <= lastCachedSysIdx {
								t := estimateTokensBPE(txt)
								*estOrig += t
								*estComp += t
								continue
							}
							target := blockMap
							appendProxyTextBlock(blocks, "system", txt, nil, func(v string) { target["text"] = v }, estOrig, estComp)
						}
					}
				}
			}
		}

		if msgs, ok := payload["messages"].([]any); ok {
			lastCachedMsgIdx := -1
			if opts.skipCacheControl {
				for i, m := range msgs {
					if msgMap, ok := m.(map[string]any); ok && messageHasCacheControl(msgMap) {
						lastCachedMsgIdx = i
					}
				}
			}
			for i, m := range msgs {
				msgMap, ok := m.(map[string]any)
				if !ok {
					continue
				}
				role, _ := msgMap["role"].(string)
				if role != "user" {
					if contentStr, ok := msgMap["content"].(string); ok {
						t := estimateTokensBPE(contentStr)
						*estOrig += t
						*estComp += t
					}
					continue
				}
				switch content := msgMap["content"].(type) {
				case string:
					if i <= lastCachedMsgIdx {
						t := estimateTokensBPE(content)
						*estOrig += t
						*estComp += t
						continue
					}
					target := msgMap
					appendProxyTextBlock(blocks, "user", content, nil, func(v string) { target["content"] = v }, estOrig, estComp)
				case []any:
					lastCachedPartIdx := -1
					if opts.skipCacheControl {
						for j, part := range content {
							if partMap, ok := part.(map[string]any); ok && hasCacheControl(partMap) {
								lastCachedPartIdx = j
							}
						}
					}
					for j, part := range content {
						if partMap, ok := part.(map[string]any); ok {
							if partMap["type"] == "text" {
								if txt, ok := partMap["text"].(string); ok {
									if i < lastCachedMsgIdx || j <= lastCachedPartIdx {
										t := estimateTokensBPE(txt)
										*estOrig += t
										*estComp += t
										continue
									}
									target := partMap
									appendProxyTextBlock(blocks, "user", txt, nil, func(v string) { target["text"] = v }, estOrig, estComp)
								}
							}
						}
					}
				}
			}
		}

		if tools, ok := payload["tools"].([]any); ok {
			collectToolsBlocksWithOptions(tools, opts.skipCacheControl, blocks, estOrig, estComp)
		}
	},
	prepareHeaders: func(dst, src http.Header) {
		copyProxyRequestHeaders(dst, src)
		dst.Set("Content-Type", "application/json")
		if dst.Get("anthropic-version") == "" {
			dst.Set("anthropic-version", "2023-06-01")
		}
	},
	extractUsage: func(respBytes []byte) int {
		var nonStreamResp struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil {
			return nonStreamResp.Usage.InputTokens + nonStreamResp.Usage.CacheCreationInputTokens + nonStreamResp.Usage.CacheReadInputTokens
		}
		return 0
	},
	newSSESampler: newAnthropicSSEUsageSampler,
}

var openAIResponsesProxyAdapter = proxyProtocolAdapter{
	protocol:           "openai",
	missingAuthMessage: "代理请求必须携带最终服务的 Authorization 或 api-key；不会使用已保存的编译密钥",
	hasCallerAuth: func(r *http.Request) bool {
		return strings.TrimSpace(r.Header.Get("Authorization")) != "" || strings.TrimSpace(r.Header.Get("api-key")) != ""
	},
	resolveTargetURL: func(r *http.Request, p Profile) (string, error) {
		upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
		if upstreamBase == "" {
			upstreamBase = strings.TrimSpace(p.UpstreamURL)
		}
		if upstreamBase == "" {
			upstreamBase = inferOpenAIBaseFromCompilerURL(p.BaseURL)
		}
		if upstreamBase == "" {
			return "", fmt.Errorf("OpenAI Responses 代理需要 upstream_url 或 X-Upstream-BaseURL")
		}
		targetURL := mergeRequestQuery(buildOpenAIResponsesURL(upstreamBase), r.URL.RawQuery)
		if err := validateOutboundURL(targetURL); err != nil {
			return "", fmt.Errorf("上游地址无效: %v", err)
		}
		return targetURL, nil
	},
	collectBlocks: func(payload map[string]any, opts proxyCollectOptions, blocks *[]proxyTextBlock, estOrig, estComp *int) {
		var reqKeywords []string
		if isResponsesJSONFormat(payload) {
			reqKeywords = append(reqKeywords, "json")
		}
		if instr, ok := payload["instructions"].(string); ok {
			target := payload
			appendProxyTextBlock(blocks, "system", instr, reqKeywords, func(v string) { target["instructions"] = v }, estOrig, estComp)
		}
		switch inputVal := payload["input"].(type) {
		case string:
			target := payload
			appendProxyTextBlock(blocks, "user", inputVal, reqKeywords, func(v string) { target["input"] = v }, estOrig, estComp)
		case []any:
			lastCachedInputIdx := -1
			if opts.skipCacheControl {
				for i, item := range inputVal {
					if itemMap, ok := item.(map[string]any); ok && messageHasCacheControl(itemMap) {
						lastCachedInputIdx = i
					}
				}
			}
			for i, item := range inputVal {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				// Only compress message items; skip function_call / function_call_output / reasoning items.
				if itemType, _ := itemMap["type"].(string); itemType != "" && itemType != "message" {
					continue
				}
				role, _ := itemMap["role"].(string)
				eligible := role == "system" || role == "user" || (opts.compressDeveloper && role == "developer")
				if !eligible {
					if contentStr, ok := itemMap["content"].(string); ok {
						t := estimateTokensBPE(contentStr)
						*estOrig += t
						*estComp += t
					}
					continue
				}
				switch content := itemMap["content"].(type) {
				case string:
					if i <= lastCachedInputIdx {
						t := estimateTokensBPE(content)
						*estOrig += t
						*estComp += t
						continue
					}
					target := itemMap
					appendProxyTextBlock(blocks, role, content, reqKeywords, func(v string) { target["content"] = v }, estOrig, estComp)
				case []any:
					lastCachedPartIdx := -1
					if opts.skipCacheControl {
						for j, part := range content {
							if partMap, ok := part.(map[string]any); ok && hasCacheControl(partMap) {
								lastCachedPartIdx = j
							}
						}
					}
					for j, part := range content {
						partMap, ok := part.(map[string]any)
						if !ok {
							continue
						}
						partType, _ := partMap["type"].(string)
						if partType == "input_text" || partType == "text" {
							if txt, ok := partMap["text"].(string); ok {
								if i < lastCachedInputIdx || j <= lastCachedPartIdx {
									t := estimateTokensBPE(txt)
									*estOrig += t
									*estComp += t
									continue
								}
								target := partMap
								appendProxyTextBlock(blocks, role, txt, reqKeywords, func(v string) { target["text"] = v }, estOrig, estComp)
							}
						}
					}
				}
			}
		}
		if tools, ok := payload["tools"].([]any); ok {
			collectToolsBlocksWithOptions(tools, opts.skipCacheControl, blocks, estOrig, estComp)
		}
	},
	prepareHeaders: func(dst, src http.Header) {
		copyProxyRequestHeaders(dst, src)
		dst.Set("Content-Type", "application/json")
	},
	extractUsage: func(respBytes []byte) int {
		var nonStreamResp struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil && nonStreamResp.Usage.InputTokens > 0 {
			return nonStreamResp.Usage.InputTokens
		}
		return 0
	},
	newSSESampler: newOpenAIResponsesSSEUsageSampler,
}

func openAIProxyHandler(w http.ResponseWriter, r *http.Request) {
	handleStructuredProxy(w, r, openAIProxyAdapter)
}

func openAIResponsesProxyHandler(w http.ResponseWriter, r *http.Request) {
	handleStructuredProxy(w, r, openAIResponsesProxyAdapter)
}

func anthropicProxyHandler(w http.ResponseWriter, r *http.Request) {
	handleStructuredProxy(w, r, anthropicProxyAdapter)
}

const maxProxyUsageCaptureBytes = 1 << 20

type proxyUsageCapture struct {
	data     []byte
	exceeded bool
}

func (capture *proxyUsageCapture) Write(p []byte) (int, error) {
	n := len(p)
	if capture.exceeded {
		return n, nil
	}
	if n > maxProxyUsageCaptureBytes-len(capture.data) {
		capture.data = nil
		capture.exceeded = true
		return n, nil
	}
	needed := len(capture.data) + n
	if needed > cap(capture.data) {
		capacity := 2 * cap(capture.data)
		if capacity < needed {
			capacity = needed
		}
		if capacity > maxProxyUsageCaptureBytes {
			capacity = maxProxyUsageCaptureBytes
		}
		grown := make([]byte, len(capture.data), capacity)
		copy(grown, capture.data)
		capture.data = grown
	}
	capture.data = append(capture.data, p...)
	return n, nil
}

// Response bytes always flow to the caller; the optional usage copy is bounded.
// Once a response exceeds the limit, discard its capture instead of truncating
// the response or interpreting a partial JSON document as complete usage.
func forwardProxyResponseBody(dst io.Writer, src io.Reader) ([]byte, error) {
	capture := &proxyUsageCapture{}
	if _, err := io.Copy(io.MultiWriter(dst, capture), src); err != nil {
		return nil, err
	}
	return capture.data, nil
}

func rejectProxyRedirect(w http.ResponseWriter, resp *http.Response) bool {
	return rejectProxyRedirectForProtocol(w, "openai", resp)
}

func rejectProxyRedirectForProtocol(w http.ResponseWriter, protocol string, resp *http.Response) bool {
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return false
	}
	// Relaying Location would let an auto-following SDK perform the same unsafe
	// replay. Report the error without that header or the provider's response body.
	writeProxyError(w, protocol, http.StatusBadGateway, "api_error", fmt.Sprintf("上游重定向已拒绝 (HTTP %d)，请直接配置最终请求地址", resp.StatusCode))
	return true
}
