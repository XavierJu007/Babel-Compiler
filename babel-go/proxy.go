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
)

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
	}

	logsMutex.Lock()
	defer logsMutex.Unlock()
	proxyLogs = append([]ProxyLog{entry}, proxyLogs...)
	if len(proxyLogs) > maxLogs {
		proxyLogs = proxyLogs[:maxLogs]
	}
}

func applyRealBillingUsage(logIDs []string, estOrigTotal, estCompTotal, realPromptTokens int, hasTools bool) {
	if realPromptTokens <= 0 || estOrigTotal <= 0 {
		return
	}

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

func isJSONResponseFormat(payload map[string]any) bool {
	rf, ok := payload["response_format"].(map[string]any)
	if !ok {
		return false
	}
	rfType, _ := rf["type"].(string)
	return rfType == "json_object"
}

func resolveProxyDialect(r *http.Request) Dialect {
	name := strings.TrimSpace(r.Header.Get("X-Babel-Dialect"))
	if name == "" {
		name = strings.TrimSpace(r.Header.Get("X-Babel-Mode"))
	}
	if name == "" {
		name = "token"
	}
	return getDialect(name)
}

func translateMessageBlock(protocol, role, text string, p Profile, dialect Dialect, reqKeywords []string) (string, string, int, int) {
	origTok := estimateTokensBPE(text)
	if len([]rune(strings.TrimSpace(text))) < 35 {
		return text, "", origTok, origTok
	}

	if !compilerCB.Allow() {
		return text, "", origTok, origTok
	}

	var activeKeywords []string
	textLower := strings.ToLower(text)
	for _, kw := range reqKeywords {
		if kw != "" && strings.Contains(textLower, strings.ToLower(kw)) {
			activeKeywords = append(activeKeywords, kw)
		}
	}

	res, err := doCompile(CompileReq{
		Text:             text,
		Mode:             dialect.Name,
		StrictGate:       true,
		RequiredKeywords: activeKeywords,
		APIKey:           p.APIKey,
		BaseURL:          p.BaseURL,
		Model:            p.Model,
	})
	if err != nil {
		if !strings.Contains(err.Error(), "门禁拦截") {
			compilerCB.RecordFailure()
		}
		return text, "", origTok, origTok
	}

	compilerCB.RecordSuccess()
	if res.Compiled == "" {
		return text, "", origTok, origTok
	}

	key := makeCacheKey(dialect, p.Model, text)
	logID := key[:8]
	compTok := estimateTokensBPE(res.Compiled)

	appendProxyLog(ProxyLog{
		ID:        logID,
		Timestamp: time.Now().Format("15:04:05"),
		Protocol:  protocol,
		Dialect:   dialect.Name,
		Role:      role,
		Original:  text,
		Compiled:  res.Compiled,
		Ratio:     res.Stats.CompressionRatio,
		CacheHit:  res.CacheHit,
	})
	return res.Compiled, logID, origTok, compTok
}

func compressSchemaNode(protocol, toolLabel string, node map[string]any, p Profile, dialect Dialect, logIDs *[]string, estOrig, estComp *int) {
	if desc, ok := node["description"].(string); ok {
		comp, id, oTok, cTok := translateMessageBlock(protocol, toolLabel, desc, p, dialect, nil)
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
				compressSchemaNode(protocol, toolLabel+"."+propName, propMap, p, dialect, logIDs, estOrig, estComp)
			}
		}
	}

	if items, ok := node["items"].(map[string]any); ok {
		compressSchemaNode(protocol, toolLabel+"[]", items, p, dialect, logIDs, estOrig, estComp)
	}
}

func compressToolsArray(protocol string, tools []any, p Profile, dialect Dialect, logIDs *[]string, estOrig, estComp *int) {
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
				comp, id, oTok, cTok := translateMessageBlock(protocol, label, desc, p, dialect, nil)
				fnMap["description"] = comp
				*estOrig += oTok
				*estComp += cTok
				if id != "" {
					*logIDs = append(*logIDs, id)
				}
			}
			if params, ok := fnMap["parameters"].(map[string]any); ok {
				compressSchemaNode(protocol, label, params, p, dialect, logIDs, estOrig, estComp)
			}
			continue
		}

		toolName, _ := toolMap["name"].(string)
		if toolName == "" {
			toolName = "unknown"
		}
		label := "tool:" + toolName

		if desc, ok := toolMap["description"].(string); ok {
			comp, id, oTok, cTok := translateMessageBlock(protocol, label, desc, p, dialect, nil)
			toolMap["description"] = comp
			*estOrig += oTok
			*estComp += cTok
			if id != "" {
				*logIDs = append(*logIDs, id)
			}
		}
		if schema, ok := toolMap["input_schema"].(map[string]any); ok {
			compressSchemaNode(protocol, label, schema, p, dialect, logIDs, estOrig, estComp)
		}
	}
}

func ensureJSONKeywordInOpenAIMessages(msgs []any) {
	for _, m := range msgs {
		msgMap, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch c := msgMap["content"].(type) {
		case string:
			if strings.Contains(strings.ToLower(c), "json") {
				return
			}
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if txt, ok := pm["text"].(string); ok && strings.Contains(strings.ToLower(txt), "json") {
						return
					}
				}
			}
		}
	}

	for _, m := range msgs {
		msgMap, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		if role == "system" || role == "user" {
			if c, ok := msgMap["content"].(string); ok {
				msgMap["content"] = c + " (JSON)"
				return
			}
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

	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		http.Error(w, "无效的 JSON 请求体", 400)
		return
	}

	cfg := loadConfig()
	p := getActiveProfile(cfg, r.Header.Get("X-Babel-Profile"))
	dialect := resolveProxyDialect(r)

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
			if role != "system" && role != "user" {
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
				comp, id, oTok, cTok := translateMessageBlock("openai", role, content, p, dialect, reqKeywords)
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
								comp, id, oTok, cTok := translateMessageBlock("openai", role, txt, p, dialect, reqKeywords)
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

		if requireJSON {
			ensureJSONKeywordInOpenAIMessages(msgs)
		}
	}

	toolsList, hasTools := payload["tools"].([]any)
	if hasTools && len(toolsList) > 0 {
		compressToolsArray("openai", toolsList, p, dialect, &logIDs, &estOrigTotal, &estCompTotal)
	}

	isStream, _ := payload["stream"].(bool)
	if isStream && payload["stream_options"] == nil {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	modifiedBody, _ := json.Marshal(payload)

	upstreamBase := r.Header.Get("X-Upstream-BaseURL")
	if upstreamBase == "" {
		upstreamBase = p.UpstreamURL
	}
	if upstreamBase == "" {
		upstreamBase = p.BaseURL
	}
	targetURL := buildOpenAICompletionsURL(upstreamBase)

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
		go applyRealBillingUsage(logIDs, estOrigTotal, estCompTotal, realTokens, hasTools)
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

	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		http.Error(w, "无效的 JSON 请求体", 400)
		return
	}

	cfg := loadConfig()
	p := getActiveProfile(cfg, r.Header.Get("X-Babel-Profile"))
	dialect := resolveProxyDialect(r)

	var (
		logIDs       []string
		estOrigTotal int
		estCompTotal int
	)

	switch sysVal := payload["system"].(type) {
	case string:
		comp, id, oTok, cTok := translateMessageBlock("anthropic", "system", sysVal, p, dialect, nil)
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
						comp, id, oTok, cTok := translateMessageBlock("anthropic", "system", txt, p, dialect, nil)
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
				comp, id, oTok, cTok := translateMessageBlock("anthropic", "user", content, p, dialect, nil)
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
								comp, id, oTok, cTok := translateMessageBlock("anthropic", "user", txt, p, dialect, nil)
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

	toolsList, hasTools := payload["tools"].([]any)
	if hasTools && len(toolsList) > 0 {
		compressToolsArray("anthropic", toolsList, p, dialect, &logIDs, &estOrigTotal, &estCompTotal)
	}

	isStream, _ := payload["stream"].(bool)
	modifiedBody, _ := json.Marshal(payload)

	upstreamBase := r.Header.Get("X-Upstream-BaseURL")
	if upstreamBase == "" {
		upstreamBase = p.UpstreamURL
	}
	if upstreamBase == "" {
		upstreamBase = p.BaseURL
	}
	targetURL := buildAnthropicMessagesURL(upstreamBase)

	proxyReq, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(modifiedBody))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	if apiKey := r.Header.Get("x-api-key"); apiKey != "" {
		proxyReq.Header.Set("x-api-key", apiKey)
	} else if auth := r.Header.Get("Authorization"); auth != "" {
		proxyReq.Header.Set("Authorization", auth)
	} else if p.APIKey != "" {
		proxyReq.Header.Set("x-api-key", p.APIKey)
	}

	if ver := r.Header.Get("anthropic-version"); ver != "" {
		proxyReq.Header.Set("anthropic-version", ver)
	} else {
		proxyReq.Header.Set("anthropic-version", "2023-06-01")
	}
	if beta := r.Header.Get("anthropic-beta"); beta != "" {
		proxyReq.Header.Set("anthropic-beta", beta)
	}

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Anthropic 上游转发失败: %v", err), 502)
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
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(respBytes, &nonStreamResp) == nil {
			realIn := nonStreamResp.Usage.InputTokens + nonStreamResp.Usage.CacheCreationInputTokens + nonStreamResp.Usage.CacheReadInputTokens
			if realIn > 0 {
				go applyRealBillingUsage(logIDs, estOrigTotal, estCompTotal, realIn, hasTools)
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
		go applyRealBillingUsage(logIDs, estOrigTotal, estCompTotal, realTokens, hasTools)
	}
}
