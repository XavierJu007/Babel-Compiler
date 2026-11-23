package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamHooks 让生成流水线把进度推给 SSE 连接，同时保持与
// generateBabelCandidate 完全一致的语义与结构校验。
type streamHooks struct {
	onStage func(stage string)
	onDelta func(chunk string)
	onReset func()
}

// callLLMStreamContext 以 SSE 方式请求模型并逐帧回调增量文本。
// 返回累计正文与 finish_reason；非 stop 的结束原因沿用既有防截断规则。
func callLLMStreamContext(ctx context.Context, p Profile, sysPrompt, userPrompt string, temp float64, maxTokens int, onDelta func(string)) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if p.APIKey == "" {
		return "", "", fmt.Errorf("当前配置方案 [%s] 未设置 API Key", p.Name)
	}
	p = effectiveCompilerProfile(p)

	reqBody := map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": temp,
		"stream":      true,
	}
	if maxTokens > 0 {
		reqBody["max_tokens"] = maxTokens
	}
	if effort := normalizeReasoningEffort(p.ReasoningEffort); effort != "" {
		reqBody["reasoning_effort"] = effort
	}
	if p.DisableThinking {
		reqBody["enable_thinking"] = false
		reqBody["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	jsonBytes, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", compilerRequestURL(p.BaseURL), strings.NewReader(string(jsonBytes)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := newOutboundHTTPClient(120 * time.Second).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", "", fmt.Errorf("上游 API 错误 (%d)\n请求地址: %s\n响应: %s",
			resp.StatusCode, compilerRequestURL(p.BaseURL), strings.TrimSpace(string(raw)))
	}

	var content strings.Builder
	finishReason := ""
	total := 0
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, readErr := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
				if delta := chunk.Choices[0].Delta.Content; delta != "" {
					total += len(delta)
					if total > maxModelResponseBytes {
						return content.String(), finishReason, fmt.Errorf("模型响应超过 %d 字节上限，未解析或采用", maxModelResponseBytes)
					}
					content.WriteString(delta)
					if onDelta != nil {
						onDelta(delta)
					}
				}
				if finish := chunk.Choices[0].FinishReason; finish != "" {
					finishReason = finish
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return content.String(), finishReason, readErr
		}
	}
	if finishReason != "" && finishReason != "stop" {
		return content.String(), finishReason, fmt.Errorf("模型响应未完整结束: finish_reason=%s", finishReason)
	}
	return content.String(), finishReason, nil
}

// generateBabelCandidateStream 与 generateBabelCandidate 同构：
// 主生成流式推送；需要一次有界重组时先发 reset，再推送最终候选。
func generateBabelCandidateStream(ctx context.Context, p Profile, dialect Dialect, original string, hooks streamHooks) (string, error) {
	data, _ := json.Marshal(babelCompilerInput{Original: original, DomainGuidance: dialect.Guidance})
	raw, _, err := callLLMStreamContext(ctx, p, dialect.Prompt, string(data), 0.1, 0, func(delta string) {
		if hooks.onDelta != nil {
			hooks.onDelta(delta)
		}
	})
	if err != nil {
		return compactDSL(raw), err
	}
	candidate := compactDSL(raw)
	if strings.TrimSpace(candidate) == "" {
		return "", fmt.Errorf("编译服务返回空候选")
	}
	if len(babelStructureIssues(original, candidate)) == 0 {
		return candidate, nil
	}
	if err := ctx.Err(); err != nil {
		return candidate, err
	}

	if hooks.onReset != nil {
		hooks.onReset()
	}
	if hooks.onStage != nil {
		hooks.onStage("rewriting")
	}
	data, _ = json.Marshal(babelCompilerInput{Original: original, DomainGuidance: dialect.Guidance, Candidate: candidate})
	raw, _, err = callLLMStreamContext(ctx, p, dialect.Prompt+"\n\n"+outlineRewriteInstruction, string(data), 0.1, 0, func(delta string) {
		if hooks.onDelta != nil {
			hooks.onDelta(delta)
		}
	})
	if err != nil {
		if strings.TrimSpace(raw) != "" {
			candidate = compactDSL(raw)
		}
		return candidate, err
	}
	rewritten := compactDSL(raw)
	if strings.TrimSpace(rewritten) == "" {
		return candidate, fmt.Errorf("重组服务返回空候选")
	}
	candidate = rewritten
	if hooks.onDelta != nil {
		hooks.onDelta(candidate)
	}
	if issues := babelStructureIssues(original, candidate); len(issues) > 0 {
		return candidate, &ValidationError{Validation: newValidation("failed", "local_checks",
			"表达结构未达标："+strings.Join(issues, "；")+"；已尝试重组 1 次，未作为巴别语接受。")}
	}
	return candidate, nil
}

type compileStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	failed  bool
}

func newCompileStreamWriter(w http.ResponseWriter) *compileStreamWriter {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	return &compileStreamWriter{w: w, flusher: flusher}
}

func (s *compileStreamWriter) emit(event string, payload any) bool {
	if s.failed {
		return false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.failed = true
		return false
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		s.failed = true
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

// compileStreamHandler 是 POST /api/compile/stream：
// 生成阶段流式输出，校验阶段只推送 stage，最后给出完整 CompileResp 或错误。
func compileStreamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"detail": "仅支持 POST"})
		return
	}
	var req CompileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"detail": "无效请求"})
		return
	}

	stream := newCompileStreamWriter(w)
	fail := func(message string, candidate string, validation ValidationResult) {
		payload := map[string]any{"error": message}
		if candidate != "" {
			payload["candidate"] = candidate
		}
		if validation.Status != "" {
			payload["validation"] = validation
		}
		stream.emit("error", payload)
	}

	if strings.TrimSpace(req.Text) == "" {
		fail("编译文本不能为空", "", ValidationResult{})
		return
	}
	cfg := loadConfig()
	p, err := resolveCompileProfile(cfg, req)
	if err != nil {
		fail(err.Error(), "", ValidationResult{})
		return
	}
	reader := p
	if p.ValidationMode == "blind" {
		reader, err = resolveReaderProfile(cfg, p)
		if err != nil {
			fail(err.Error(), "", ValidationResult{})
			return
		}
	}
	auditor, err := resolveAuditProfile(cfg, p, "")
	if err != nil {
		fail(err.Error(), "", ValidationResult{})
		return
	}
	dialect, err := composeDialect(req.Mode, req.Domains, req.Text)
	if err != nil {
		fail(err.Error(), "", ValidationResult{})
		return
	}

	// 生成缓存命中时不做流式（没有可流的内容），直接进入校验并给结果。
	cacheKey := makeCacheKey(dialect, p, req.Text)
	candidate := ""
	cacheHit := false
	if !req.NoCache && cacheKeyHasCandidate(cacheKey) {
		cacheMutex.RLock()
		candidate = diskCache[cacheKey].Compiled
		cacheMutex.RUnlock()
		cacheHit = candidate != "" && len(babelStructureIssues(req.Text, candidate)) == 0
	}
	var genErr error
	if cacheHit {
		stream.emit("stage", map[string]string{"stage": "validating"})
	} else {
		candidate = ""
		stream.emit("stage", map[string]string{"stage": "generating"})
		if shouldCompileInChunks(req) {
			candidate, genErr = generateBabelCandidateParallel(r.Context(), p, dialect, req.Text)
			if genErr == nil {
				stream.emit("delta", map[string]any{"chunk": candidate, "reset": false})
			}
		} else {
			candidate, genErr = generateBabelCandidateStream(r.Context(), p, dialect, req.Text, streamHooks{
				onStage: func(stage string) { stream.emit("stage", map[string]string{"stage": stage}) },
				onDelta: func(chunk string) { stream.emit("delta", map[string]any{"chunk": chunk, "reset": false}) },
				onReset: func() { stream.emit("delta", map[string]any{"chunk": "", "reset": true}) },
			})
		}
		if genErr == nil && !req.NoCache && strings.TrimSpace(candidate) != "" {
			now := time.Now()
			cacheMutex.Lock()
			diskCache[cacheKey] = CacheEntry{Compiled: candidate, Model: p.Model, Mode: dialect.Name,
				CreatedAt: now.Format(time.RFC3339), LastAccess: now.Unix()}
			evictCacheIfNeededLocked()
			cacheMutex.Unlock()
			scheduleDiskCacheSave()
		}
	}

	out := CompileResp{Compiled: candidate, Dialect: dialect.Name, CacheHit: cacheHit}
	if genErr != nil {
		out.Validation = newValidation("error", "none", genErr.Error())
		var validationErr *ValidationError
		if errors.As(genErr, &validationErr) {
			out.Validation = validationErr.Validation
		}
		out.Audit = validationSummary(out.Validation)
		_ = fillCompileStats(&out, req.Text, p.TokenEncoding)
		fail(genErr.Error(), candidate, out.Validation)
		return
	}

	stream.emit("stage", map[string]string{"stage": "validating"})
	out, err = validateCompileResponse(r.Context(), req, p, auditor, reader, out)
	if err != nil {
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			fail(err.Error(), out.Compiled, validationErr.Validation)
			return
		}
		fail(err.Error(), out.Compiled, out.Validation)
		return
	}
	stream.emit("result", out)
}
