package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 单请求内的多个工具描述必须并行编译：串行 6×120ms 会超过 600ms 预算。
func TestProxyCompilesMultipleToolsConcurrentlyWithinSingleRequest(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	var inFlight, maxInFlight atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		inFlight.Add(-1)
		writeModelContent(w, "Short tool summary.")
	}))
	defer compiler.Close()

	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&forwarded)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {APIKey: "compiler-key", BaseURL: compiler.URL, Model: "fixture", UpstreamURL: upstream.URL},
	}}); err != nil {
		t.Fatal(err)
	}

	tools := make([]any, 6)
	for i := range tools {
		tools[i] = map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fmt.Sprintf("tool_%d", i),
				"description": fmt.Sprintf("This is a detailed operational description for tool number %d that clearly explains all required parameters and preconditions.", i),
			},
		}
	}
	payload := map[string]any{
		"model":    "consumer",
		"messages": []any{map[string]any{"role": "user", "content": "Run."}},
		"tools":    tools,
	}
	raw, _ := json.Marshal(payload)

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer consumer-key")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("expected parallel tool compilation, got maxInFlight=%d", maxInFlight.Load())
	}
	if elapsed >= 600*time.Millisecond {
		t.Fatalf("parallel compilation took too long (%v), expected < 600ms", elapsed)
	}
	gotTools, _ := forwarded["tools"].([]any)
	if len(gotTools) != 6 {
		t.Fatalf("unexpected tools count: %d", len(gotTools))
	}
}

// 队列超时是本地背压，不是上游故障：必须保留原文、写清原因，且不触发熔断。
func TestProxyQueueOverflowFallsBackWithoutTrippingBreaker(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	oldSem := proxyCompileSem
	oldTimeout := proxyCompileQueueTimeout
	proxyCompileSem = newCompileSemaphore(1)
	proxyCompileQueueTimeout = 40 * time.Millisecond
	t.Cleanup(func() {
		proxyCompileSem = oldSem
		proxyCompileQueueTimeout = oldTimeout
	})

	started := make(chan struct{})
	release := make(chan struct{})
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		writeModelContent(w, "Short summary.")
	}))
	defer compiler.Close()

	profile := Profile{APIKey: "test", BaseURL: compiler.URL, Model: "test"}
	dialect := getDialect(defaultDialectName)

	doneFirst := make(chan struct{})
	// 释放并等待占槽的那个请求结束，否则它会在用例收尾后继续写全局缓存/日志。
	defer func() {
		close(release)
		<-doneFirst
	}()
	go func() {
		defer close(doneFirst)
		translateMessageBlock("openai", "user",
			"First long prompt that occupies the single global compilation slot while waiting for the upstream compiler model.",
			profile, dialect, nil, nil)
	}()
	<-started

	secondOrig := "Second long prompt that should gracefully bypass compilation when the semaphore queue is busy."
	out, logID, before, after := translateMessageBlock("openai", "user", secondOrig, profile, dialect, nil, nil)
	if out != secondOrig || before != after || logID == "" {
		t.Fatalf("overflow did not keep original cleanly: out=%q before=%d after=%d logID=%q", out, before, after, logID)
	}
	if status := compilerCB.Status(); status["state"] != "CLOSED" || status["failures"] != 0 {
		t.Fatalf("local queue backpressure tripped circuit breaker: %+v", status)
	}
	logsMutex.RLock()
	foundBusy := false
	for _, entry := range proxyLogs {
		if entry.ID == logID && entry.Decision == "kept_original" && strings.Contains(entry.Reason, "队列繁忙") {
			foundBusy = true
			break
		}
	}
	logsMutex.RUnlock()
	if !foundBusy {
		t.Fatal("missing queue-busy fallback log entry")
	}
}

// 异步防抖落盘 + 并发配置读写：停止时必须把全部条目刷到磁盘。
func TestAsyncCacheFlusherAndConcurrentConfigSafety(t *testing.T) {
	dir := isolateCompilerState(t)
	stopFlush := startAsyncCacheFlusher(30 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", idx)
			cacheMutex.Lock()
			diskCache[key] = CacheEntry{Compiled: "ok", LastAccess: time.Now().Unix()}
			cacheMutex.Unlock()
			scheduleDiskCacheSave()

			if idx%4 == 0 {
				_ = writeConfigFile(Config{
					ActiveProfile: "default",
					Profiles: map[string]Profile{
						"default": {Name: "default", Model: fmt.Sprintf("m-%d", idx), BaseURL: "https://api.openai.com/v1"},
					},
				})
			} else {
				cfg := loadConfig()
				if cfg.ActiveProfile == "" {
					t.Errorf("observed empty active profile during concurrent write")
				}
			}
		}(i)
	}
	wg.Wait()
	stopFlush()

	raw, err := os.ReadFile(filepath.Join(dir, "babel.cache.json"))
	if err != nil {
		t.Fatalf("async cache file was not flushed on stop: %v", err)
	}
	var restored map[string]CacheEntry
	if err := json.Unmarshal(raw, &restored); err != nil || len(restored) != 16 {
		t.Fatalf("flushed cache incomplete: count=%d err=%v", len(restored), err)
	}
}
