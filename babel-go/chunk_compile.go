package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const (
	parallelChunkThreshold     = 2000
	chunkTargetRunes           = 1000
	chunkMinRunes              = 600
	autoSectionChunkThreshold  = 900
	autoSectionTargetRunes     = 420
	autoSectionMinRunes        = 240
	parallelChunkWorkers       = 6
	defaultChunkMaxConcurrency = 16
)

// chunkCompileSem isolates chunked sub-tasks from proxyCompileSem so a caller
// already holding a proxyCompileSem slot can never self-deadlock on nested acquire.
var chunkCompileSem = newCompileSemaphore(envPositiveInt("BABEL_CHUNK_MAX_CONCURRENCY", defaultChunkMaxConcurrency))

func enableAutoParallelChunks(req CompileReq) CompileReq {
	switch normalizeCompileStrategy(req.Strategy) {
	case "auto", "deep":
		runes := utf8.RuneCountInString(req.Text)
		if runes > parallelChunkThreshold || (runes > autoSectionChunkThreshold && len(splitSectionChunks(req.Text, autoSectionTargetRunes, autoSectionMinRunes)) >= 2) {
			req.ParallelChunks = true
		}
	}
	return req
}

// shouldCompileInChunks 只在显式开启或 auto/deep 策略遇到超长文本/可安全分段文本时走分块并行；
// 传统空策略仍保留显式开关语义，保证既有编译行为与测试完全兼容。
func shouldCompileInChunks(req CompileReq) bool {
	s := normalizeCompileStrategy(req.Strategy)
	if s == "local" {
		return false
	}
	runes := utf8.RuneCountInString(req.Text)
	if s == "auto" || s == "deep" {
		if !req.ParallelChunks {
			return false
		}
		if runes > parallelChunkThreshold {
			return true
		}
		return runes > autoSectionChunkThreshold && len(splitSectionChunks(req.Text, autoSectionTargetRunes, autoSectionMinRunes)) >= 2
	}
	return req.ParallelChunks && runes > parallelChunkThreshold
}

// splitSectionChunks 仅在代码围栏/引文之外的空行边界(\n\n)按目标尺寸切分，
// 用于中等长度多章节文本并行提速且绝不把同一章节或列表从中间劈开。
func splitSectionChunks(text string, target, minTail int) []string {
	if target <= 0 {
		target = autoSectionTargetRunes
	}
	if minTail <= 0 {
		minTail = autoSectionMinRunes
	}
	if utf8.RuneCountInString(text) <= target {
		return []string{text}
	}
	mask := outlineLiteralMask(text)
	var chunks []string
	chunkStart := 0
	runes := 0
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		last := i + size - 1
		runes++
		if size == 1 && text[i] == '\n' && (i >= len(mask) || !mask[i]) {
			// Match blank line ("\n\n" or "\r\n\r\n") and advance break past consecutive blank lines.
			nextIdx := i + 1
			isBlankSep := false
			if nextIdx < len(text) && text[nextIdx] == '\n' && (nextIdx >= len(mask) || !mask[nextIdx]) {
				isBlankSep = true
			} else if nextIdx+1 < len(text) && text[nextIdx] == '\r' && text[nextIdx+1] == '\n' && (nextIdx >= len(mask) || !mask[nextIdx]) {
				isBlankSep = true
			}
			if isBlankSep && runes >= target {
				breakAt := nextIdx
				for breakAt < len(text) && (text[breakAt] == '\n' || text[breakAt] == '\r') && (breakAt >= len(mask) || !mask[breakAt]) {
					breakAt++
				}
				if breakAt < len(text) {
					chunks = append(chunks, text[chunkStart:breakAt])
					chunkStart = breakAt
					runes = 0
					i = breakAt
					continue
				}
			}
		}
		i = last + 1
	}
	if chunkStart < len(text) {
		chunks = append(chunks, text[chunkStart:])
	}
	if len(chunks) < 2 {
		return []string{text}
	}
	if utf8.RuneCountInString(chunks[len(chunks)-1]) < minTail {
		chunks[len(chunks)-2] += chunks[len(chunks)-1]
		chunks = chunks[:len(chunks)-1]
	}
	if len(chunks) < 2 {
		return []string{text}
	}
	return chunks
}

// splitCompileChunks 只在「代码围栏/引文之外」的换行处切分，
// 目标大小约 1000 runes，绝不把受保护片段切开。
func splitCompileChunks(text string, target int) []string {
	if target <= 0 {
		target = chunkTargetRunes
	}
	if utf8.RuneCountInString(text) <= target {
		return []string{text}
	}
	mask := outlineLiteralMask(text)

	// 增量累加 rune 数，避免在每个换行上重扫整段前缀（原实现是 O(N²)）。
	// 优先在空行段落边界切分（runes >= target*3/4），无空行时在 runes >= target 回退切分。
	var chunks []string
	chunkStart := 0
	runes := 0
	softTarget := target * 3 / 4
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		last := i + size - 1
		runes++
		if size == 1 && text[i] == '\n' && (i >= len(mask) || !mask[i]) {
			nextIsBlank := i+1 < len(text) && (text[i+1] == '\n' || (text[i+1] == '\r' && i+2 < len(text) && text[i+2] == '\n'))
			if (nextIsBlank && runes >= softTarget) || runes >= target {
				breakAt := i + 1
				chunks = append(chunks, text[chunkStart:breakAt])
				chunkStart = breakAt
				runes = 0
			}
		}
		i = last + 1
	}
	if chunkStart < len(text) {
		chunks = append(chunks, text[chunkStart:])
	}
	if len(chunks) < 2 {
		return []string{text}
	}
	// 尾块过短时并回前一块：省掉一次带完整系统提示词的 LLM 往返。
	if len(chunks) >= 2 && utf8.RuneCountInString(chunks[len(chunks)-1]) < chunkMinRunes {
		chunks[len(chunks)-2] += chunks[len(chunks)-1]
		chunks = chunks[:len(chunks)-1]
	}
	return chunks
}

// generateBabelCandidateParallel 对超长文档做 Map-Reduce：
// 分块并行生成，再按原顺序拼接；整体语义/字面量校验仍由调用方对全文执行一次。
func generateBabelCandidateParallel(ctx context.Context, p Profile, dialect Dialect, original string) (string, error) {
	chunks := splitCompileChunks(original, chunkTargetRunes)
	return runParallelChunkGeneration(ctx, p, dialect, original, chunks, false)
}

func selectChunksForReq(req CompileReq) []string {
	s := normalizeCompileStrategy(req.Strategy)
	if (s == "auto" || s == "deep") && utf8.RuneCountInString(req.Text) <= parallelChunkThreshold {
		if sec := splitSectionChunks(req.Text, autoSectionTargetRunes, autoSectionMinRunes); len(sec) >= 2 {
			return sec
		}
	}
	return splitCompileChunks(req.Text, chunkTargetRunes)
}

func generateBabelCandidateParallelForReq(ctx context.Context, req CompileReq, p Profile, dialect Dialect, original string) (string, error) {
	chunks := selectChunksForReq(req)
	return runParallelChunkGeneration(ctx, p, dialect, original, chunks, true)
}

func runParallelChunkGeneration(ctx context.Context, p Profile, dialect Dialect, original string, chunks []string, continuationHint bool) (string, error) {
	if len(chunks) < 2 {
		return generateBabelCandidate(ctx, p, dialect, original)
	}
	results := make([]string, len(chunks))
	errs := make([]error, len(chunks))
	// 任意一块失败立即取消其余块，避免继续烧上游配额。
	chunkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := parallelChunkWorkers
	if workers > len(chunks) {
		workers = len(chunks)
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for {
				idx := int(next.Add(1) - 1)
				if idx >= len(chunks) {
					return
				}
				if err := chunkCtx.Err(); err != nil {
					errs[idx] = err
					return
				}
				chunkDialect := dialect
				if continuationHint && idx > 0 {
					chunkDialect.Prompt = dialect.Prompt + fmt.Sprintf("\n\n【分块续写】本段是长文档第 %d/%d 部分：仅精炼本段规则并用 `├` / `╰` 分支承接上文，禁止重复整篇总标题或前文共同主体。", idx+1, len(chunks))
				}
				// 使用独立的分块信号量，避免与外层 proxyCompileSem 形成嵌套死锁。
				release, acquired, canceled := chunkCompileSem.Acquire(chunkCtx, proxyCompileQueueTimeout)
				if canceled {
					errs[idx] = chunkCtx.Err()
					if errs[idx] == nil {
						errs[idx] = ctx.Err()
					}
					return
				}
				if !acquired {
					errs[idx] = fmt.Errorf("编译并发队列繁忙，分块编译已中止")
					return
				}
				results[idx], errs[idx] = generateBabelCandidate(chunkCtx, p, chunkDialect, chunks[idx])
				release()
				if errs[idx] != nil {
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	for idx, err := range errs {
		if err != nil {
			return "", fmt.Errorf("第 %d 块编译失败: %w", idx+1, err)
		}
	}
	return strings.Join(results, "\n"), nil
}
