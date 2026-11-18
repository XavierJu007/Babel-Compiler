package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed index.html
var indexHTML string

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

type CompileReq struct {
	Text       string `json:"text"`
	Mode       string `json:"mode"`        // extreme | standard | token
	Verify     bool   `json:"verify"`      // 开启语义审计
	StrictGate bool   `json:"strict_gate"` // 硬门禁：缺失数值/阈值/否定约束时直接报错失败
	NoCache    bool   `json:"no_cache"`    // 强制跳过缓存重新编译
	Profile    string `json:"profile,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Model      string `json:"model,omitempty"`
}

type CompileResp struct {
	Compiled string `json:"compiled"`
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
	Role             string `json:"role"`
	Original         string `json:"original"`
	Compiled         string `json:"compiled"`
	Ratio            string `json:"ratio"`
	CacheHit         bool   `json:"cache_hit"`
	RealPromptTokens int    `json:"real_prompt_tokens,omitempty"`
	EstOrigTokens    int    `json:"est_orig_tokens,omitempty"`
	RealBillingRatio string `json:"real_billing_ratio,omitempty"`
}

// 零外部依赖 Singleflight 防缓存击穿
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
	maxLogs         = 40
)

var (
	cacheMutex    sync.RWMutex
	diskCache     = make(map[string]CacheEntry)
	compileFlight flightGroup

	calibMutex          sync.RWMutex
	bpeCalibrationScale = 1.0 // 基于上游真实 usage.prompt_tokens 动态校准的缩放系数

	logsMutex sync.RWMutex
	proxyLogs []ProxyLog

	numRegex = regexp.MustCompile(`\d+(?:\.\d+)?`)
)

var prompts = map[string]string{
	"standard": `你是一个「高密度提示词编译器」。将自然语言无损编译为结构清晰的树状符号指令（目标长度为原文的 55%~70%）。
规范：
1. 使用 ASCII 树形线（├ │ ╰）划分多级缩进，每个独立检查项独占一行。
2. 剔除虚词，严禁编造多余小标题。
3. 符号替换：因果→ | 并列+ | 枚举| | 否定¬ | 遍历∀ | 变更↔。
4. 自动修正技术术语中的明显 OCR 错别字（如 userld→userId, bp//filter→php://filter, CRLE→CRLF）。
5. 零空行，仅输出纯文本，不要 Markdown 代码块包裹。`,

	"extreme": `你是一个「极限数理逻辑编译器（Babel-DSL）」。将自然语言无损编译为高密度的数理逻辑 AST 树（目标长度为原文的 50%~65%）。
规范：
1. 首行格式固定为：Role:=<角色> | Target:=<目标> ∧ <核心范围>
2. 必须使用 ├ │ ╰ 构建多级缩进树（主阶段用 ├1 / ├2 / ╰N，子项用 │ ├ / │ ╰），每个独立条件独占一行。
3. 强制使用数理逻辑算子：定义:= | 因果→ | 与∧ | 或| | 非¬ | 遍历∀ | 命中∃ | 属于∈ | 篡改↔ | 探测? | 红线⚠。
4. 严禁丢失任何数字、阈值、Payload 与否定约束（如“不批量”、“不过期”、“不验签”必须显式保留 ¬ 约束）。
5. 零空行，仅输出编译后的纯文本，不要 Markdown 代码块包裹。`,

	"token": `你是一个「BPE Tokenizer 极致压缩编译器」。为了避免 UTF-8 多字节符号（如 ├ ∀ ∃）触发分词器 Byte-Fallback 消耗 2~3 个 Token，所有结构与逻辑符号必须严格使用单字节 ASCII 字符！
规范：
1. 首行格式：Role:=<角色> | Target:=<目标> & <范围>
2. 树形缩进仅使用单字节 ASCII：|- 1. / |- / '- /   |- （禁止使用 ├ │ ╰）。
3. 逻辑算子仅使用单字节 ASCII：:= (定义) | -> (因果) | & (与) | | (或) | ! (非/禁止) | ALL (遍历) | HAS (命中) | IN (属于) | <-> (篡改) | ? (探测)。
4. 严禁丢失任何数字、阈值、Payload 与否定约束。
5. 零空行，仅输出纯文本，不要 Markdown 代码块包裹。`,
}

const verifyGatePrompt = `你是一个严苛的「语义无损门禁审计器」。对比【原文】与【编译后DSL】，逐项核查以下三类关键不变量是否 100% 保留：
1. 数值与阈值（所有数字、状态码、IP、端口、倍率、长度限制）；
2. 否定与禁止约束（如“不写”、“不批量”、“不过期”、“不验签”、“不混入”等是否已用 ¬ / ! / 禁 准确表达）；
3. 核心技术实体与 Payload（参数名、漏洞类型、协议字段）。

请严格输出合法的 JSON（不要 Markdown 代码块包裹）：
{"pass": true, "missing": []}
或
{"pass": false, "missing": ["缺失项1", "缺失项2"]}`

func configPath() string {
	localCfg := "babel.config.json"
	if _, err := os.Stat(localCfg); err == nil {
		return localCfg
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return localCfg
	}
	return filepath.Join(home, ".babel_config.json")
}

func cacheFilePath() string {
	localCache := "babel.cache.json"
	if _, err := os.Stat("babel.config.json"); err == nil {
		return localCache
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return localCache
	}
	return filepath.Join(home, ".babel_cache.json")
}

func initDiskCache() {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return
	}
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	_ = json.Unmarshal(data, &diskCache)
}

func evictCacheIfNeededLocked() {
	for len(diskCache) > maxCacheEntries {
		var oldestKey string
		var oldestTime int64 = math.MaxInt64
		for k, v := range diskCache {
			if v.LastAccess < oldestTime {
				oldestTime = v.LastAccess
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(diskCache, oldestKey)
		} else {
			break
		}
	}
}

func saveDiskCache() {
	cacheMutex.Lock()
	evictCacheIfNeededLocked()
	data, err := json.MarshalIndent(diskCache, "", "  ")
	cacheMutex.Unlock()
	if err == nil {
		_ = os.WriteFile(cacheFilePath(), data, 0600)
	}
}

func defaultProfile() Profile {
	base := os.Getenv("BABEL_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("BABEL_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return Profile{
		Name:        "default",
		APIKey:      os.Getenv("BABEL_API_KEY"),
		BaseURL:     base,
		Model:       model,
		UpstreamURL: os.Getenv("BABEL_UPSTREAM_URL"),
	}
}

func loadConfig() Config {
	cfg := Config{
		ActiveProfile: "default",
		Profiles:      map[string]Profile{"default": defaultProfile()},
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved Config
	if json.Unmarshal(data, &saved) != nil {
		return cfg
	}
	if saved.Profiles == nil {
		saved.Profiles = make(map[string]Profile)
	}
	if len(saved.Profiles) == 0 && (saved.APIKey != "" || saved.BaseURL != "" || saved.Model != "") {
		p := defaultProfile()
		if saved.APIKey != "" {
			p.APIKey = saved.APIKey
		}
		if saved.BaseURL != "" {
			p.BaseURL = saved.BaseURL
		}
		if saved.Model != "" {
			p.Model = saved.Model
		}
		if saved.UpstreamURL != "" {
			p.UpstreamURL = saved.UpstreamURL
		}
		saved.Profiles["default"] = p
	}
	if len(saved.Profiles) > 0 {
		cfg.Profiles = saved.Profiles
	}
	if saved.ActiveProfile != "" {
		if _, exists := cfg.Profiles[saved.ActiveProfile]; exists {
			cfg.ActiveProfile = saved.ActiveProfile
		}
	}
	return cfg
}

func getActiveProfile(cfg Config, overrideName string) Profile {
	name := cfg.ActiveProfile
	if overrideName != "" {
		name = overrideName
	}
	if p, ok := cfg.Profiles[name]; ok {
		return p
	}
	for _, p := range cfg.Profiles {
		return p
	}
	return defaultProfile()
}

func writeConfigFile(cfg Config) error {
	active := getActiveProfile(cfg, "")
	cfg.APIKey = active.APIKey
	cfg.BaseURL = active.BaseURL
	cfg.Model = active.Model
	cfg.UpstreamURL = active.UpstreamURL
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func maskKey(k string) string {
	if len(k) <= 6 {
		if k == "" {
			return ""
		}
		return "***"
	}
	return k[:6] + "***"
}

func appendProxyLog(entry ProxyLog) {
	logsMutex.Lock()
	defer logsMutex.Unlock()
	proxyLogs = append([]ProxyLog{entry}, proxyLogs...)
	if len(proxyLogs) > maxLogs {
		proxyLogs = proxyLogs[:maxLogs]
	}
}

// 当上游返回真实 usage.prompt_tokens 时，动态校准本地估算因子并回写代理日志
func applyRealBillingUsage(logIDs []string, estOrigTotal, estCompTotal, realPromptTokens int, hasTools bool) {
	if realPromptTokens <= 0 || estOrigTotal <= 0 {
		return
	}

	// 仅在无额外 tools Schema 干扰时，用真实账单值动态平滑校准 BPE 估算权重
	if !hasTools && estCompTotal > 20 {
		observedRatio := float64(realPromptTokens) / float64(estCompTotal)
		if observedRatio >= 0.65 && observedRatio <= 1.65 {
			calibMutex.Lock()
			bpeCalibrationScale = bpeCalibrationScale*0.8 + observedRatio*0.2
			calibMutex.Unlock()
		}
	}

	if len(logIDs) == 0 {
		return
	}

	// 按当前校准系数还原原始自然语言的等效真实 Token 数
	calibMutex.RLock()
	scale := bpeCalibrationScale
	calibMutex.RUnlock()

	calibratedOrig := int(float64(estOrigTotal)*scale + 0.5)
	if calibratedOrig < realPromptTokens {
		calibratedOrig = estOrigTotal
	}
	realSavedPct := 0.0
	if calibratedOrig > 0 {
		realSavedPct = (1.0 - float64(realPromptTokens)/float64(calibratedOrig)) * 100
	}
	billingTag := fmt.Sprintf("实测 %d→%d tok (省%.1f%%)", calibratedOrig, realPromptTokens, realSavedPct)

	idSet := make(map[string]bool, len(logIDs))
	for _, id := range logIDs {
		idSet[id] = true
	}

	logsMutex.Lock()
	defer logsMutex.Unlock()
	for i := range proxyLogs {
		if idSet[proxyLogs[i].ID] {
			proxyLogs[i].RealPromptTokens = realPromptTokens
			proxyLogs[i].EstOrigTokens = calibratedOrig
			proxyLogs[i].RealBillingRatio = billingTag
		}
	}
}

func buildCompletionsURL(baseURL string) string {
	clean := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(clean, "/chat/completions") {
		return clean
	}
	return clean + "/chat/completions"
}

func callLLM(p Profile, sysPrompt, userPrompt string, temp float64) (string, error) {
	if p.APIKey == "" {
		return "", fmt.Errorf("当前配置方案 [%s] 未设置 API Key", p.Name)
	}
	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := p.Model
	if model == "" {
		model = "gpt-4o-mini"
	}

	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": temp,
	}
	jsonBytes, _ := json.Marshal(reqBody)
	targetURL := buildCompletionsURL(baseURL)

	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("上游 API 错误 (%d)\n请求地址: %s\n响应: %s", resp.StatusCode, targetURL, string(bodyBytes))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil || len(result.Choices) == 0 {
		return "", fmt.Errorf("解析响应失败: %s", string(bodyBytes))
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func compactDSL(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"```json", "```text", "```dsl", "```"} {
		raw = strings.TrimPrefix(raw, prefix)
	}
	raw = strings.TrimSuffix(raw, "```")

	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		trimmed = strings.ReplaceAll(trimmed, "├─ ", "├")
		trimmed = strings.ReplaceAll(trimmed, "├─", "├")
		trimmed = strings.ReplaceAll(trimmed, "└─ ", "╰")
		trimmed = strings.ReplaceAll(trimmed, "└─", "╰")
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}

// 感知 UTF-8 Byte-Fallback 与动态校准系数的 BPE Token 估算器
func estimateTokensBPE(s string) int {
	var total float64
	asciiWordRun := 0

	flushWord := func() {
		if asciiWordRun > 0 {
			total += float64(asciiWordRun) / 3.8
			if asciiWordRun < 3 {
				total += 0.35
			}
			asciiWordRun = 0
		}
	}

	for _, r := range s {
		bLen := utf8.RuneLen(r)
		if bLen == 1 {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
				asciiWordRun++
			} else {
				flushWord()
				if r == ' ' || r == '\n' || r == '\t' {
					total += 0.35
				} else {
					total += 1.0
				}
			}
		} else {
			flushWord()
			if r >= 0x4E00 && r <= 0x9FFF {
				total += 1.45
			} else if r >= 0x2500 && r <= 0x257F {
				total += 2.5
			} else if r >= 0x2190 && r <= 0x22FF {
				total += 2.0
			} else {
				total += float64(bLen) * 0.75
			}
		}
	}
	flushWord()

	calibMutex.RLock()
	scale := bpeCalibrationScale
	calibMutex.RUnlock()

	return int(total*scale + 0.5)
}

func formatStats(orig, comp string) (int, int, int, int, string) {
	oChars := len([]rune(orig))
	cChars := len([]rune(comp))
	oTok := estimateTokensBPE(orig)
	cTok := estimateTokensBPE(comp)

	charPct := 0.0
	if oChars > 0 {
		charPct = (1.0 - float64(cChars)/float64(oChars)) * 100
	}
	tokPct := 0.0
	if oTok > 0 {
		tokPct = (1.0 - float64(cTok)/float64(oTok)) * 100
	}
	summary := fmt.Sprintf("Token %d→%d (省%.1f%%) | 字符 %.1f%%", oTok, cTok, tokPct, charPct)
	return oChars, cChars, oTok, cTok, summary
}

func checkDeterministicInvariants(orig, comp string) []string {
	var failures []string

	origNums := numRegex.FindAllString(orig, -1)
	seen := make(map[string]bool)
	for _, num := range origNums {
		if seen[num] {
			continue
		}
		seen[num] = true
		if !strings.Contains(comp, num) {
			failures = append(failures, fmt.Sprintf("缺失数值/阈值: %s", num))
		}
	}

	negKeywords := []string{"不批量", "不过期", "不验签", "不得", "禁止", "严禁", "绝不", "不能", "不可", "只读不写"}
	hasOrigNeg := false
	for _, kw := range negKeywords {
		if strings.Contains(orig, kw) {
			hasOrigNeg = true
			break
		}
	}
	if hasOrigNeg {
		negMarkers := []string{"¬", "!", "不", "禁", "无", "非", "免"}
		hasCompNeg := false
		for _, m := range negMarkers {
			if strings.Contains(comp, m) {
				hasCompNeg = true
				break
			}
		}
		if !hasCompNeg {
			failures = append(failures, "缺失否定/禁止约束算子 (¬ / !)")
		}
	}

	return failures
}

func runAuditGate(p Profile, orig, comp string) (bool, string) {
	localFails := checkDeterministicInvariants(orig, comp)
	if len(localFails) > 0 {
		return false, "[本地硬门禁拦截] " + strings.Join(localFails, "; ")
	}

	rawAudit, err := callLLM(p, verifyGatePrompt, fmt.Sprintf("【原文】:\n%s\n\n【编译后DSL】:\n%s", orig, comp), 0.0)
	if err != nil {
		return false, fmt.Sprintf("[审计服务异常] %v", err)
	}

	cleaned := compactDSL(rawAudit)
	var gateRes struct {
		Pass    bool     `json:"pass"`
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(cleaned), &gateRes); err == nil {
		if gateRes.Pass {
			return true, "[PASS] 数值、阈值与否定约束 100% 无损"
		}
		return false, "[语义门禁拦截] 缺失: " + strings.Join(gateRes.Missing, "; ")
	}

	if strings.Contains(strings.ToUpper(cleaned), "PASS") {
		return true, cleaned
	}
	return false, "[语义门禁拦截] " + cleaned
}

func makeCacheKey(mode, model, text string) string {
	h := sha256.Sum256([]byte(mode + "|" + model + "|" + strings.TrimSpace(text)))
	return hex.EncodeToString(h[:])
}

func doCompile(req CompileReq) (CompileResp, error) {
	cfg := loadConfig()
	p := getActiveProfile(cfg, req.Profile)

	if req.APIKey != "" && !strings.Contains(req.APIKey, "***") {
		p.APIKey = req.APIKey
	}
	if req.BaseURL != "" {
		p.BaseURL = req.BaseURL
	}
	if req.Model != "" {
		p.Model = req.Model
	}
	if req.Mode == "" {
		req.Mode = "extreme"
	}
	sys, ok := prompts[req.Mode]
	if !ok {
		sys = prompts["extreme"]
	}

	cacheKey := makeCacheKey(req.Mode, p.Model, req.Text)

	// 1. 查询 LRU 持久化缓存
	if !req.NoCache {
		cacheMutex.Lock()
		cached, hit := diskCache[cacheKey]
		if hit && cached.Compiled != "" {
			cached.LastAccess = time.Now().Unix()
			diskCache[cacheKey] = cached
		}
		cacheMutex.Unlock()

		if hit && cached.Compiled != "" {
			var out CompileResp
			out.Compiled = cached.Compiled
			out.CacheHit = true
			out.Audit = cached.Audit

			if (req.Verify || req.StrictGate) && out.Audit == "" {
				passed, report := runAuditGate(p, req.Text, out.Compiled)
				out.Audit = report
				if req.StrictGate && !passed {
					return CompileResp{}, fmt.Errorf("%s", report)
				}
			} else if req.StrictGate && strings.Contains(out.Audit, "拦截") {
				return CompileResp{}, fmt.Errorf("%s", out.Audit)
			}

			out.Stats.OriginalChars, out.Stats.CompiledChars, out.Stats.OriginalTokens, out.Stats.CompiledTokens, out.Stats.CompressionRatio = formatStats(req.Text, out.Compiled)
			return out, nil
		}
	}

	// 2. Singleflight 并发合并：同一 CacheKey 并发请求只发起一次上游编译
	flightKey := cacheKey
	if req.NoCache {
		flightKey = fmt.Sprintf("%s_force_%d", cacheKey, time.Now().UnixNano())
	}

	res, shared, err := compileFlight.Do(flightKey, func() (CompileResp, error) {
		compiled, err := callLLM(p, sys, req.Text, 0.1)
		if err != nil {
			return CompileResp{}, err
		}
		compiled = compactDSL(compiled)

		var auditReport string
		if req.Verify || req.StrictGate {
			passed, report := runAuditGate(p, req.Text, compiled)
			auditReport = report
			if req.StrictGate && !passed {
				return CompileResp{}, fmt.Errorf("%s", report)
			}
		}

		now := time.Now()
		cacheMutex.Lock()
		diskCache[cacheKey] = CacheEntry{
			Compiled:   compiled,
			Audit:      auditReport,
			Model:      p.Model,
			Mode:       req.Mode,
			CreatedAt:  now.Format(time.RFC3339),
			LastAccess: now.Unix(),
		}
		cacheMutex.Unlock()
		go saveDiskCache()

		var out CompileResp
		out.Compiled = compiled
		out.Audit = auditReport
		out.CacheHit = false
		out.Stats.OriginalChars, out.Stats.CompiledChars, out.Stats.OriginalTokens, out.Stats.CompiledTokens, out.Stats.CompressionRatio = formatStats(req.Text, compiled)
		return out, nil
	})

	if err != nil {
		return CompileResp{}, err
	}
	if shared {
		res.CacheHit = true
	}
	return res, nil
}

// 透明代理单条文本编译，返回 (编译后文本, 日志ID, 原始估算Token, 编译后估算Token)
func translateMessageBlock(role, text string, p Profile) (string, string, int, int) {
	origTok := estimateTokensBPE(text)
	if len([]rune(strings.TrimSpace(text))) < 35 {
		return text, "", origTok, origTok
	}

	res, err := doCompile(CompileReq{
		Text:       text,
		Mode:       "token",
		StrictGate: true,
		APIKey:     p.APIKey,
		BaseURL:    p.BaseURL,
		Model:      p.Model,
	})
	if err != nil || res.Compiled == "" {
		return text, "", origTok, origTok
	}

	key := makeCacheKey("token", p.Model, text)
	logID := key[:8]
	compTok := estimateTokensBPE(res.Compiled)

	appendProxyLog(ProxyLog{
		ID:        logID,
		Timestamp: time.Now().Format("15:04:05"),
		Role:      role,
		Original:  text,
		Compiled:  res.Compiled,
		Ratio:     res.Stats.CompressionRatio,
		CacheHit:  res.CacheHit,
	})
	return res.Compiled, logID, origTok, compTok
}

// 从 SSE 尾部文本中提取上游真实返回的 usage.prompt_tokens
func extractUsageFromSSETail(tail string) int {
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

func transparentProxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求失败", 400)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		http.Error(w, "无效的 JSON 请求体", 400)
		return
	}

	cfg := loadConfig()
	p := getActiveProfile(cfg, r.Header.Get("X-Babel-Profile"))

	var (
		logIDs       []string
		estOrigTotal int
		estCompTotal int
	)

	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			msgMap, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msgMap["role"].(string)
			if role != "system" && role != "user" {
				// 统计 assistant/tool 历史消息的基础 Token，对齐上游总 prompt_tokens
				if contentStr, ok := msgMap["content"].(string); ok {
					t := estimateTokensBPE(contentStr) + 4
					estOrigTotal += t
					estCompTotal += t
				}
				continue
			}

			estOrigTotal += 4
			estCompTotal += 4

			switch content := msgMap["content"].(type) {
			case string:
				comp, id, oTok, cTok := translateMessageBlock(role, content, p)
				msgMap["content"] = comp
				estOrigTotal += oTok
				estCompTotal += cTok
				if id != "" {
					logIDs = append(logIDs, id)
				}
			case []any:
				for _, part := range content {
					if partMap, ok := part.(map[string]any); ok {
						if partMap["type"] == "text" {
							if txt, ok := partMap["text"].(string); ok {
								comp, id, oTok, cTok := translateMessageBlock(role, txt, p)
								partMap["text"] = comp
								estOrigTotal += oTok
								estCompTotal += cTok
								if id != "" {
									logIDs = append(logIDs, id)
								}
							}
						}
					}
				}
			}
		}
	}

	// 若为流式请求且未显式关闭 stream_options，则自动注入 include_usage 以捕获真实账单 Token
	isStream, _ := payload["stream"].(bool)
	if isStream && payload["stream_options"] == nil {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	_, hasTools := payload["tools"]
	modifiedBody, _ := json.Marshal(payload)

	upstreamBase := r.Header.Get("X-Upstream-BaseURL")
	if upstreamBase == "" {
		upstreamBase = p.UpstreamURL
	}
	if upstreamBase == "" {
		upstreamBase = p.BaseURL
	}
	targetURL := buildCompletionsURL(upstreamBase)

	proxyReq, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(modifiedBody))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		proxyReq.Header.Set("Authorization", auth)
	} else if p.APIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("上游转发失败: %v", err), 502)
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if !isStream {
		respBytes, _ := io.ReadAll(resp.Body)
		w.Write(respBytes)

		var nonStreamResp struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil && nonStreamResp.Usage.PromptTokens > 0 {
			go applyRealBillingUsage(logIDs, estOrigTotal, estCompTotal, nonStreamResp.Usage.PromptTokens, hasTools)
		}
		return
	}

	// 流式透传并截取最后 4KB 缓冲以解析 usage chunk
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var tailRing string
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			w.Write(chunk)
			if canFlush {
				flusher.Flush()
			}
			tailRing += string(chunk)
			if len(tailRing) > 4096 {
				tailRing = tailRing[len(tailRing)-4096:]
			}
		}
		if readErr != nil {
			break
		}
	}

	if realTokens := extractUsageFromSSETail(tailRing); realTokens > 0 {
		go applyRealBillingUsage(logIDs, estOrigTotal, estCompTotal, realTokens, hasTools)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func bindAvailableListener(preferredAddr string) (net.Listener, string, error) {
	if preferredAddr != "" {
		ln, err := net.Listen("tcp", preferredAddr)
		return ln, preferredAddr, err
	}
	for port := 8080; port <= 8099; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if ln, err := net.Listen("tcp", addr); err == nil {
			return ln, addr, nil
		}
	}
	return nil, "", fmt.Errorf("8080-8099 端口均被占用，请通过 -serve 127.0.0.1:<端口> 手动指定")
}

func startServer(preferredAddr string, autoOpen bool) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			cfg := loadConfig()
			safeProfiles := make(map[string]map[string]any)
			for name, prof := range cfg.Profiles {
				safeProfiles[name] = map[string]any{
					"name":           name,
					"api_key_masked": maskKey(prof.APIKey),
					"has_api_key":    prof.APIKey != "",
					"base_url":       prof.BaseURL,
					"model":          prof.Model,
					"upstream_url":   prof.UpstreamURL,
				}
			}
			cacheMutex.RLock()
			cacheCount := len(diskCache)
			cacheMutex.RUnlock()

			json.NewEncoder(w).Encode(map[string]any{
				"active_profile": cfg.ActiveProfile,
				"config_path":    configPath(),
				"cache_path":     cacheFilePath(),
				"cache_count":    cacheCount,
				"profiles":       safeProfiles,
			})
			return
		}

		if r.Method == "POST" {
			var req struct {
				Action      string `json:"action"`
				ProfileName string `json:"profile_name"`
				APIKey      string `json:"api_key"`
				BaseURL     string `json:"base_url"`
				Model       string `json:"model"`
				UpstreamURL string `json:"upstream_url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
				return
			}

			cfg := loadConfig()
			name := strings.TrimSpace(req.ProfileName)
			if name == "" {
				name = cfg.ActiveProfile
			}
			if name == "" {
				name = "default"
			}

			switch req.Action {
			case "delete":
				if len(cfg.Profiles) <= 1 {
					w.WriteHeader(400)
					json.NewEncoder(w).Encode(map[string]string{"detail": "至少保留一个配置方案"})
					return
				}
				delete(cfg.Profiles, name)
				if cfg.ActiveProfile == name {
					for k := range cfg.Profiles {
						cfg.ActiveProfile = k
						break
					}
				}
			case "switch":
				if _, ok := cfg.Profiles[name]; !ok {
					w.WriteHeader(404)
					json.NewEncoder(w).Encode(map[string]string{"detail": "配置方案不存在"})
					return
				}
				cfg.ActiveProfile = name
			default:
				existing := cfg.Profiles[name]
				existing.Name = name
				if req.APIKey != "" && !strings.Contains(req.APIKey, "***") {
					existing.APIKey = req.APIKey
				}
				if req.BaseURL != "" {
					existing.BaseURL = req.BaseURL
				}
				if req.Model != "" {
					existing.Model = req.Model
				}
				existing.UpstreamURL = req.UpstreamURL
				cfg.Profiles[name] = existing
				cfg.ActiveProfile = name
			}

			if err := writeConfigFile(cfg); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"detail": "写入配置文件失败: " + err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "config_path": configPath()})
		}
	})

	http.HandleFunc("/api/compile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CompileReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"detail": "无效JSON"})
			return
		}
		res, err := doCompile(req)
		if err != nil {
			status := 400
			if strings.Contains(err.Error(), "门禁拦截") {
				status = 422
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			logsMutex.Lock()
			proxyLogs = nil
			logsMutex.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
			return
		}
		logsMutex.RLock()
		defer logsMutex.RUnlock()
		if proxyLogs == nil {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode(proxyLogs)
	})

	http.HandleFunc("/v1/chat/completions", transparentProxyHandler)

	ln, actualAddr, err := bindAvailableListener(preferredAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 启动失败: %v\n", err)
		os.Exit(1)
	}

	webURL := "http://" + actualAddr
	fmt.Printf("[+] Babel-DSL 服务已就绪:\n")
	fmt.Printf("    - 配置文件:    %s\n", configPath())
	fmt.Printf("    - LRU 缓存池:  %s (上限 %d 条)\n", cacheFilePath(), maxCacheEntries)
	fmt.Printf("    - Web 控制台:  %s\n", webURL)
	fmt.Printf("    - OpenAI 代理: %s/v1/chat/completions\n", webURL)

	if autoOpen {
		openBrowser(webURL)
	}

	if err := http.Serve(ln, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[Error] 服务异常退出: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	initDiskCache()

	serveAddr := flag.String("serve", "", "指定 Web/API/代理服务地址 (留空则自动探测可用端口)")
	profileName := flag.String("profile", "", "指定使用的已保存配置方案名称")
	mode := flag.String("m", "extreme", "压缩模式: extreme | standard | token (纯ASCII省Token)")
	verify := flag.Bool("v", false, "开启往返语义审计")
	strictGate := flag.Bool("gate", false, "开启硬门禁：缺失数值、阈值或否定约束时直接报错失败")
	noCache := flag.Bool("no-cache", false, "跳过本地确定性缓存强制重新编译")
	quiet := flag.Bool("q", false, "静默模式，仅输出纯文本")
	file := flag.String("f", "", "从文件读取输入")
	apiKey := flag.String("api-key", "", "指定 API Key")
	baseURL := flag.String("base-url", "", "指定编译器 Base URL")
	model := flag.String("model", "", "指定编译器模型名称")
	saveCfg := flag.Bool("save-config", false, "保存当前参数为配置方案")
	flag.Parse()

	if *saveCfg {
		cfg := loadConfig()
		name := *profileName
		if name == "" {
			name = cfg.ActiveProfile
		}
		p := cfg.Profiles[name]
		p.Name = name
		if *apiKey != "" {
			p.APIKey = *apiKey
		}
		if *baseURL != "" {
			p.BaseURL = *baseURL
		}
		if *model != "" {
			p.Model = *model
		}
		cfg.Profiles[name] = p
		cfg.ActiveProfile = name
		if err := writeConfigFile(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[Error] 保存失败: %v\n", err)
			os.Exit(1)
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[+] 配置方案 [%s] 已保存至 %s\n", name, configPath())
		}
		if *serveAddr == "" && *file == "" && flag.NArg() == 0 {
			return
		}
	}

	if *serveAddr != "" {
		startServer(*serveAddr, false)
		return
	}

	var input string
	if flag.NArg() > 0 {
		input = strings.Join(flag.Args(), " ")
	} else if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Error] 读取文件失败: %v\n", err)
			os.Exit(1)
		}
		input = string(b)
	} else {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			input = string(b)
		} else {
			startServer("", true)
			return
		}
	}

	res, err := doCompile(CompileReq{
		Text:       strings.TrimSpace(input),
		Mode:       *mode,
		Verify:     *verify,
		StrictGate: *strictGate,
		NoCache:    *noCache,
		Profile:    *profileName,
		APIKey:     *apiKey,
		BaseURL:    *baseURL,
		Model:      *model,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] %v\n", err)
		os.Exit(1)
	}

	fmt.Println(res.Compiled)
	if !*quiet {
		cacheTag := "MISS"
		if res.CacheHit {
			cacheTag = "HIT"
		}
		fmt.Fprintf(os.Stderr, "\n--- 计量: %s [Cache: %s] ---\n", res.Stats.CompressionRatio, cacheTag)
		if res.Audit != "" {
			fmt.Fprintf(os.Stderr, "--- 门禁: %s ---\n", res.Audit)
		}
	}
}
