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
	"strings"
	"sync/atomic"
	"time"
)

var proxyLogSequence atomic.Uint64

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\bsk-(?:ant-|proj-)?[a-zA-Z0-9_-]{16,}\b`), `sk-***[REDACTED]`},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), `AKIA***[REDACTED]`},
	{regexp.MustCompile(`\bgh[pousr]_[a-zA-Z0-9]{20,}\b`), `gh*_***[REDACTED]`},
	{regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`), `eyJ***[JWT_REDACTED]`},
	{regexp.MustCompile(`(?i)(\bBearer\s+)[a-zA-Z0-9._\-+/=]{12,}`), `${1}***[REDACTED]`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|secret[_-]?key|password|passwd|pwd|access[_-]?token|auth[_-]?token)\s*[:=]\s*["']?)[a-zA-Z0-9._\-+/=!@#$%^&*]{6,}(["']?)`), `${1}***[REDACTED]${2}`},
}

func redactSecrets(text string) string {
	res := text
	for _, p := range secretPatterns {
		res = p.re.ReplaceAllString(res, p.repl)
	}
	return res
}

func appendProxyLog(entry ProxyLog) {
	if isLogRedactEnabled() {
		redactedOrig := redactSecrets(entry.Original)
		redactedComp := redactSecrets(entry.Compiled)
		if redactedOrig != entry.Original || redactedComp != entry.Compiled {
			entry.Redacted = true
		}
		entry.Original = redactedOrig
		entry.Compiled = redactedComp
		entry.Reason = redactSecrets(entry.Reason)
	}

	logsMutex.Lock()
	defer logsMutex.Unlock()
	proxyLogs = append([]ProxyLog{entry}, proxyLogs...)
	if len(proxyLogs) > maxLogs {
		proxyLogs = proxyLogs[:maxLogs]
	}
}

func applyUpstreamUsage(logIDs []string, estOrigTotal, estCompTotal, realPromptTokens int) {
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
	origTok := estimateTokensBPE(text)
	if ctx.Err() != nil {
		return text, "", origTok, origTok
	}
	if len([]rune(strings.TrimSpace(text))) < 35 {
		return text, "", origTok, origTok
	}

	allowed, probeID := compilerCB.Acquire()
	if !allowed {
		return text, "", origTok, origTok
	}

	var activeKeywords []string
	textLower := strings.ToLower(text)
	for _, kw := range reqKeywords {
		if kw != "" && strings.Contains(textLower, strings.ToLower(kw)) {
			activeKeywords = append(activeKeywords, kw)
		}
	}

	res, err := doCompileContext(ctx, CompileReq{
		Profile:          p.Name,
		Text:             text,
		Mode:             dialect.Name,
		Domains:          domains,
		StrictGate:       true,
		RequiredKeywords: activeKeywords,
		APIKey:           p.APIKey,
		BaseURL:          p.BaseURL,
		Model:            p.Model,
		TokenEncoding:    p.TokenEncoding,
		ValidationMode:   p.ValidationMode,
		ReaderProfile:    &p.ReaderProfile,
	})
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
		entry.Reason = "候选未减少" + countLabel + "，已保留原文"
		appendProxyLog(entry)
		return text, logID, origTok, origTok
	}
	entry.Compiled = res.Compiled
	entry.Ratio = res.Stats.CompressionRatio
	entry.Decision = "adopted"
	entry.Reason = "本次审计通过，且" + countLabel + "更少"
	appendProxyLog(entry)
	return res.Compiled, logID, origTok, compTok
}

// Only description strings are eligible for compilation. Schema keywords,
// property names, enum values and all other protocol data keep their values.
func compressSchemaNode(ctx context.Context, protocol, toolLabel string, node map[string]any, p Profile, dialect Dialect, domains []string, logIDs *[]string, estOrig, estComp *int) {
	if desc, ok := node["description"].(string); ok {
		comp, id, oTok, cTok := translateMessageBlockContext(ctx, protocol, toolLabel, desc, p, dialect, nil, domains)
		node["description"] = comp
		*estOrig += oTok
		*estComp += cTok
		if id != "" {
			*logIDs = append(*logIDs, id)
		}
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for propName, propVal := range props {
			if propMap, ok := propVal.(map[string]any); ok {
				compressSchemaNode(ctx, protocol, toolLabel+"."+propName, propMap, p, dialect, domains, logIDs, estOrig, estComp)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		compressSchemaNode(ctx, protocol, toolLabel+"[]", items, p, dialect, domains, logIDs, estOrig, estComp)
	}
}

func compressToolsArray(ctx context.Context, protocol string, tools []any, p Profile, dialect Dialect, domains []string, logIDs *[]string, estOrig, estComp *int) {
	for _, item := range tools {
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
				comp, id, oTok, cTok := translateMessageBlockContext(ctx, protocol, label, desc, p, dialect, nil, domains)
				fnMap["description"] = comp
				*estOrig += oTok
				*estComp += cTok
				if id != "" {
					*logIDs = append(*logIDs, id)
				}
			}
			if params, ok := fnMap["parameters"].(map[string]any); ok {
				compressSchemaNode(ctx, protocol, label, params, p, dialect, domains, logIDs, estOrig, estComp)
			}
			continue
		}

		toolName, _ := toolMap["name"].(string)
		if toolName == "" {
			toolName = "unknown"
		}
		label := "tool:" + toolName
		if desc, ok := toolMap["description"].(string); ok {
			comp, id, oTok, cTok := translateMessageBlockContext(ctx, protocol, label, desc, p, dialect, nil, domains)
			toolMap["description"] = comp
			*estOrig += oTok
			*estComp += cTok
			if id != "" {
				*logIDs = append(*logIDs, id)
			}
		}
		if schema, ok := toolMap["input_schema"].(map[string]any); ok {
			compressSchemaNode(ctx, protocol, label, schema, p, dialect, domains, logIDs, estOrig, estComp)
		}
	}
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

func openAIProxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求失败", 400)
		return
	}

	payload, err := decodeProxyPayload(rawBody)
	if err != nil {
		http.Error(w, "无效的 JSON 请求体", 400)
		return
	}
	callerAuth := strings.TrimSpace(r.Header.Get("Authorization"))
	if callerAuth == "" {
		http.Error(w, "代理请求必须携带最终服务的 Authorization；不会使用已保存的编译密钥", http.StatusUnauthorized)
		return
	}

	cfg := loadConfig()
	p, err := resolveProxyProfile(r, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dialect, domains, err := resolveProxyDialect(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
	if upstreamBase == "" {
		upstreamBase = strings.TrimSpace(p.UpstreamURL)
	}
	// Validate the final destination before any generation or audit request.
	targetURL := compilerRequestURL(p.BaseURL)
	if upstreamBase != "" {
		targetURL = buildOpenAICompletionsURL(upstreamBase)
	}
	if err := validateOutboundURL(targetURL); err != nil {
		http.Error(w, fmt.Sprintf("上游地址无效: %v", err), http.StatusBadRequest)
		return
	}

	var reqKeywords []string
	requireJSON := isJSONResponseFormat(payload)
	if requireJSON {
		reqKeywords = append(reqKeywords, "json")
	}

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
			// Compile only the original system/user text slots. Developer policy,
			// assistant history and tool results are outside this transform.
			if role != "system" && role != "user" {
				if contentStr, ok := msgMap["content"].(string); ok {
					t := estimateTokensBPE(contentStr)
					estOrigTotal += t
					estCompTotal += t
				}
				continue
			}

			switch content := msgMap["content"].(type) {
			case string:
				comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "openai", role, content, p, dialect, reqKeywords, domains)
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
								comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "openai", role, txt, p, dialect, reqKeywords, domains)
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

	if tools, ok := payload["tools"].([]any); ok {
		compressToolsArray(r.Context(), "openai", tools, p, dialect, domains, &logIDs, &estOrigTotal, &estCompTotal)
	}

	isStream, _ := payload["stream"].(bool)
	// Preserve caller-selected streaming options. Compatible providers may reject
	// optional fields; usage is recorded only when it appears in their response.
	modifiedBody, _ := json.Marshal(payload)

	proxyReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewBuffer(modifiedBody))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Authorization", callerAuth)

	client := newOutboundHTTPClient(300 * time.Second)
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("上游转发失败: %v", err), 502)
		return
	}
	defer resp.Body.Close()
	if rejectProxyRedirect(w, resp) {
		return
	}

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
		respBytes, copyErr := forwardProxyResponseBody(w, resp.Body)
		if copyErr != nil {
			return
		}

		var nonStreamResp struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil && nonStreamResp.Usage.PromptTokens > 0 {
			go applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, nonStreamResp.Usage.PromptTokens)
		}
		return
	}

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

	if realTokens := extractUsageFromOpenAISSETail(tailRing); realTokens > 0 {
		go applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, realTokens)
	}
}

func anthropicProxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求失败", 400)
		return
	}

	payload, err := decodeProxyPayload(rawBody)
	if err != nil {
		http.Error(w, "无效的 JSON 请求体", 400)
		return
	}
	callerAPIKey := strings.TrimSpace(r.Header.Get("x-api-key"))
	callerAuth := strings.TrimSpace(r.Header.Get("Authorization"))
	if callerAPIKey == "" && callerAuth == "" {
		http.Error(w, "代理请求必须携带最终服务的 x-api-key 或 Authorization；不会使用已保存的编译密钥", http.StatusUnauthorized)
		return
	}

	cfg := loadConfig()
	p, err := resolveProxyProfile(r, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dialect, domains, err := resolveProxyDialect(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
	if upstreamBase == "" {
		upstreamBase = strings.TrimSpace(p.UpstreamURL)
	}
	if upstreamBase == "" {
		http.Error(w, "Anthropic 代理需要 upstream_url 或 X-Upstream-BaseURL；无法从编译请求 URL 推导 Messages 地址", http.StatusBadRequest)
		return
	}
	targetURL := buildAnthropicMessagesURL(upstreamBase)
	if err := validateOutboundURL(targetURL); err != nil {
		http.Error(w, fmt.Sprintf("上游地址无效: %v", err), http.StatusBadRequest)
		return
	}

	var (
		logIDs       []string
		estOrigTotal int
		estCompTotal int
	)

	switch sysVal := payload["system"].(type) {
	case string:
		comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "anthropic", "system", sysVal, p, dialect, nil, domains)
		payload["system"] = comp
		estOrigTotal += oTok
		estCompTotal += cTok
		if id != "" {
			logIDs = append(logIDs, id)
		}
	case []any:
		for _, block := range sysVal {
			if blockMap, ok := block.(map[string]any); ok {
				if blockMap["type"] == "text" {
					if txt, ok := blockMap["text"].(string); ok {
						comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "anthropic", "system", txt, p, dialect, nil, domains)
						blockMap["text"] = comp
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

	if msgs, ok := payload["messages"].([]any); ok {
		for _, m := range msgs {
			msgMap, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msgMap["role"].(string)
			if role != "user" {
				if contentStr, ok := msgMap["content"].(string); ok {
					t := estimateTokensBPE(contentStr)
					estOrigTotal += t
					estCompTotal += t
				}
				continue
			}

			switch content := msgMap["content"].(type) {
			case string:
				comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "anthropic", "user", content, p, dialect, nil, domains)
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
								comp, id, oTok, cTok := translateMessageBlockContext(r.Context(), "anthropic", "user", txt, p, dialect, nil, domains)
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

	if tools, ok := payload["tools"].([]any); ok {
		compressToolsArray(r.Context(), "anthropic", tools, p, dialect, domains, &logIDs, &estOrigTotal, &estCompTotal)
	}

	isStream, _ := payload["stream"].(bool)
	modifiedBody, _ := json.Marshal(payload)

	proxyReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewBuffer(modifiedBody))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	if callerAPIKey != "" {
		proxyReq.Header.Set("x-api-key", callerAPIKey)
	} else {
		proxyReq.Header.Set("Authorization", callerAuth)
	}

	if ver := r.Header.Get("anthropic-version"); ver != "" {
		proxyReq.Header.Set("anthropic-version", ver)
	} else {
		proxyReq.Header.Set("anthropic-version", "2023-06-01")
	}
	if beta := r.Header.Get("anthropic-beta"); beta != "" {
		proxyReq.Header.Set("anthropic-beta", beta)
	}

	client := newOutboundHTTPClient(300 * time.Second)
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Anthropic 上游转发失败: %v", err), 502)
		return
	}
	defer resp.Body.Close()
	if rejectProxyRedirect(w, resp) {
		return
	}

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
		respBytes, copyErr := forwardProxyResponseBody(w, resp.Body)
		if copyErr != nil {
			return
		}

		var nonStreamResp struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil {
			realIn := nonStreamResp.Usage.InputTokens + nonStreamResp.Usage.CacheCreationInputTokens + nonStreamResp.Usage.CacheReadInputTokens
			if realIn > 0 {
				go applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, realIn)
			}
		}
		return
	}

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var headCapture string
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			w.Write(chunk)
			if canFlush {
				flusher.Flush()
			}
			if len(headCapture) < 8192 {
				headCapture += string(chunk)
			}
		}
		if readErr != nil {
			break
		}
	}

	if realTokens := extractUsageFromAnthropicSSE(headCapture); realTokens > 0 {
		go applyUpstreamUsage(logIDs, estOrigTotal, estCompTotal, realTokens)
	}
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
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return false
	}
	// Relaying Location would let an auto-following SDK perform the same unsafe
	// replay. Report the error without that header or the provider's response body.
	http.Error(w, fmt.Sprintf("上游重定向已拒绝 (HTTP %d)，请直接配置最终请求地址", resp.StatusCode), http.StatusBadGateway)
	return true
}
