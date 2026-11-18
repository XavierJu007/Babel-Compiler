package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const verifyGatePrompt = `你是一个严苛的「语义无损与零码本门禁审计器」。对比【原文】与【编译后DSL】，逐项核查以下四项指标是否 100% 合格：
1. 数值与阈值（所有数字、状态码、IP、端口、倍率、长度限制）；
2. 否定与禁止约束（如“不写”、“不批量”、“不过期”、“不验签”、“不混入”等是否已用 ¬ / ! / 禁 准确表达）；
3. 核心技术实体、枚举值与 Payload（参数名、漏洞类型、协议字段）；
4. 零私有码本与自解释性（严禁出现未定义的单字母代号如 π、α、A 指代人或段落；任意大模型不看图例必须能无歧义还原原意）。

请严格输出合法的 JSON（不要 Markdown 代码块包裹）：
{"pass": true, "missing": []}
或
{"pass": false, "missing": ["问题1", "问题2"]}`

// 仅提取独立数值、IP、版本号或百分比（避免误提取 RS256 / H5256 / HTTP2 中的内嵌数字）
var standaloneNumRegex = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_])(\d+(?:\.\d+)*%?)(?:$|[^a-zA-Z0-9_])`)

func extractStandaloneNumbers(s string) []string {
	matches := standaloneNumRegex.FindAllStringSubmatch(s, -1)
	var nums []string
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) > 1 && !seen[m[1]] {
			seen[m[1]] = true
			nums = append(nums, m[1])
		}
	}
	return nums
}

func buildOpenAICompletionsURL(baseURL string) string {
	clean := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(clean, "/chat/completions") {
		return clean
	}
	return clean + "/chat/completions"
}

func buildAnthropicMessagesURL(baseURL string) string {
	clean := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(clean, "/v1/messages") || strings.HasSuffix(clean, "/messages") {
		return clean
	}
	if strings.HasSuffix(clean, "/v1") {
		return clean + "/messages"
	}
	return clean + "/v1/messages"
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
	targetURL := buildOpenAICompletionsURL(baseURL)

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

func checkDeterministicInvariants(orig, comp string, requiredKeywords []string) []string {
	var failures []string

	for _, num := range extractStandaloneNumbers(orig) {
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

	compLower := strings.ToLower(comp)
	for _, kw := range requiredKeywords {
		if kw == "" {
			continue
		}
		if !strings.Contains(compLower, strings.ToLower(kw)) {
			failures = append(failures, fmt.Sprintf("缺失协议保留词: %s", kw))
		}
	}

	for _, sym := range []string{"π@", "α@", "β@"} {
		if !strings.Contains(orig, sym) && strings.Contains(comp, sym) {
			failures = append(failures, fmt.Sprintf("触发私有码本拦截: 检测到未定义符号代号 %s", sym))
		}
	}

	return failures
}

func runAuditGate(p Profile, orig, comp string, requiredKeywords []string) (bool, string) {
	localFails := checkDeterministicInvariants(orig, comp, requiredKeywords)
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
			return true, "[PASS] 数值、否定约束与零码本自解释 100% 合格"
		}
		return false, "[语义门禁拦截] 缺失/歧义: " + strings.Join(gateRes.Missing, "; ")
	}

	if strings.Contains(strings.ToUpper(cleaned), "PASS") {
		return true, cleaned
	}
	return false, "[语义门禁拦截] " + cleaned
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

	dialect := getDialect(req.Mode)
	cacheKey := makeCacheKey(dialect, p.Model, req.Text)

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
			out.Dialect = dialect.Name
			out.CacheHit = true
			out.Audit = cached.Audit

			if req.StrictGate {
				if fails := checkDeterministicInvariants(req.Text, out.Compiled, req.RequiredKeywords); len(fails) > 0 {
					return CompileResp{}, fmt.Errorf("[本地硬门禁拦截] %s", strings.Join(fails, "; "))
				}
			}

			if (req.Verify || req.StrictGate) && out.Audit == "" {
				passed, report := runAuditGate(p, req.Text, out.Compiled, req.RequiredKeywords)
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

	flightKey := cacheKey
	if req.NoCache {
		flightKey = fmt.Sprintf("%s_force_%d", cacheKey, time.Now().UnixNano())
	}

	res, shared, err := compileFlight.Do(flightKey, func() (CompileResp, error) {
		compiled, err := callLLM(p, dialect.Prompt, req.Text, 0.1)
		if err != nil {
			return CompileResp{}, err
		}
		compiled = compactDSL(compiled)

		var auditReport string
		if req.Verify || req.StrictGate {
			passed, report := runAuditGate(p, req.Text, compiled, req.RequiredKeywords)
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
			Mode:       dialect.Name,
			CreatedAt:  now.Format(time.RFC3339),
			LastAccess: now.Unix(),
		}
		cacheMutex.Unlock()
		go saveDiskCache()

		var out CompileResp
		out.Compiled = compiled
		out.Dialect = dialect.Name
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
