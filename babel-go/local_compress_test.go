package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalCompressDeterministicJSONAndLiterals(t *testing.T) {
	prettyJSON := "{\n  \"service\": \"refund-engine\",\n  \"max_amount\": 500,\n  \"strict\": true,\n  \"rules\": [\n    \"never_delete_ledger\",\n    \"require_audit_id\"\n  ]\n}"
	comp, origTok, compTok, changed := localCompressDeterministic(prettyJSON, "o200k_base", []string{"refund-engine"})
	if !changed {
		t.Fatalf("expected pretty JSON to compress cleanly")
	}
	if compTok >= origTok {
		t.Fatalf("expected compressed JSON tokens (%d) < original (%d)", compTok, origTok)
	}
	if !json.Valid([]byte(comp)) {
		t.Fatalf("compressed JSON is invalid: %q", comp)
	}
	if strings.Contains(comp, "\n  ") {
		t.Fatalf("expected minified single-line JSON, got %q", comp)
	}
}

func TestLocalCompressDeterministicPreservesProtectedRegions(t *testing.T) {
	input := strings.Join([]string{
		"---",
		"name:   keep-yaml-spacing",
		"version: 1",
		"---",
		"",
		"Setext Heading",
		"---",
		"",
		"Please note that   every request must never skip `checkOrder(id)`   or  \"quoted   literal\".",
		"",
		"---",
		"",
		"",
		"In order to see docs visit https://example.com/a/b?x=1&y=2   now.",
		"",
		"| Column A   |   Column B   |",
		"| :--------- | -----------: |",
		"| `val_1`    |        >= 50 |",
		"",
		"```go",
		"// Keep   internal   spacing & --- untouched",
		"const limit = 100",
		"```",
	}, "\n")

	comp, origTok, compTok, changed := localCompressDeterministic(input, "o200k_base", nil)
	if !changed {
		t.Fatalf("expected markdown/whitespace sample to compress")
	}
	if compTok >= origTok {
		t.Fatalf("expected token reduction: %d -> %d", origTok, compTok)
	}
	for _, mustKeep := range []string{
		"name:   keep-yaml-spacing",
		"Setext Heading\n---",
		"`checkOrder(id)`",
		"\"quoted   literal\"",
		"https://example.com/a/b?x=1&y=2",
		"|`val_1`|>= 50|",
		"|:---|---:|",
		"// Keep   internal   spacing & --- untouched",
		"const limit = 100",
		"must never skip",
	} {
		if !strings.Contains(comp, mustKeep) {
			t.Fatalf("missing protected fragment %q in %q", mustKeep, comp)
		}
	}
	if strings.Contains(comp, "Please note that") {
		t.Fatalf("expected conservative boilerplate prefix to be stripped: %q", comp)
	}
	if !strings.Contains(comp, "To see docs visit") {
		t.Fatalf("expected 'In order to ' -> 'To ': %q", comp)
	}
}

func TestTryFastCompileStrategyShortPromptSkipsLLM(t *testing.T) {
	isolateCompilerState(t)

	var llmCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		http.Error(w, "unexpected remote call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := Profile{Name: "test", BaseURL: srv.URL, APIKey: "k", Model: "m", TokenEncoding: "o200k_base"}
	dialect, err := composeDialect(defaultDialectName, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	shortPrompt := "禁止删除线上订单，金额 >= 500 元必须由财务复核。"
	resp, handled, err := tryFastCompileStrategy(CompileReq{
		Text:       shortPrompt,
		Strategy:   "auto",
		StrictGate: true,
	}, p, dialect)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatalf("expected short prompt in auto mode to be handled locally without LLM")
	}
	if resp.Strategy != "passthrough" || resp.Compiled != shortPrompt {
		t.Fatalf("expected passthrough of short prompt, got strategy=%q compiled=%q", resp.Strategy, resp.Compiled)
	}
	if resp.Validation.Status != "passed" || resp.Validation.Method != "local_fast" {
		t.Fatalf("unexpected validation result: %+v", resp.Validation)
	}
	if llmCalls.Load() != 0 {
		t.Fatalf("expected 0 remote LLM calls, got %d", llmCalls.Load())
	}

	// Same short prompt in "deep" mode must NOT short-circuit.
	_, handledDeep, err := tryFastCompileStrategy(CompileReq{
		Text:     shortPrompt,
		Strategy: "deep",
	}, p, dialect)
	if err != nil || handledDeep {
		t.Fatalf("expected deep strategy not to short-circuit, got handled=%t err=%v", handledDeep, err)
	}

	// Legacy empty strategy also stays unchanged.
	_, handledLegacy, err := tryFastCompileStrategy(CompileReq{
		Text: shortPrompt,
	}, p, dialect)
	if err != nil || handledLegacy {
		t.Fatalf("expected empty strategy not to short-circuit, got handled=%t err=%v", handledLegacy, err)
	}
}

func TestDoCompileAutoStrategyFallsBackWhenLLMExpands(t *testing.T) {
	isolateCompilerState(t)

	// Build a prompt above autoMinLLMRunes & autoMinLLMTokens that has extra whitespace
	// so LLM expansion triggers automatic fallback to shorter local/original output.
	longCompact := "System rules for automated release pipelines:\n\n" +
		strings.Repeat("- Always check checksum SHA256 before deploying artifact build_2026.\n", 6)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message": map[string]string{
					"content": "expanded_long_output_header\n" + strings.Repeat("├─ verbose unnecessary explanatory expansion line with extra tokens\n", 14),
				},
			}},
		})
	}))
	defer srv.Close()

	out, err := doCompileContext(context.Background(), CompileReq{
		Text:          longCompact,
		Strategy:      "auto",
		BaseURL:       srv.URL,
		APIKey:        "test-key",
		Model:         "test-model",
		TokenEncoding: "o200k_base",
		StrictGate:    true,
		NoCache:       true,
	})
	if err != nil {
		t.Fatalf("expected auto fallback rather than error when LLM expands, got %v", err)
	}
	if out.Stats.CompiledTokens > out.Stats.OriginalTokens {
		t.Fatalf("auto strategy expanded tokens: %d -> %d", out.Stats.OriginalTokens, out.Stats.CompiledTokens)
	}
	if out.Strategy != "passthrough" && out.Strategy != "local_fast" {
		t.Fatalf("expected passthrough or local_fast fallback, got %q", out.Strategy)
	}
	if out.Validation.Status != "passed" || out.Validation.Method != "local_fast" {
		t.Fatalf("unexpected fallback validation: %+v", out.Validation)
	}
}

func TestProxyLocalFastPathColdStartAndJSON(t *testing.T) {
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	configureProxySpeedDefaults()

	proxyPolicyMu.Lock()
	proxyAsyncWarmup = true
	proxyLocalFastPath = true
	proxySkipNonProse = true
	proxyMinRunes = 40
	proxyPolicyMu.Unlock()

	var compilerHits atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compilerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"∀order: check auth"}}]}`))
	}))
	defer compiler.Close()

	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		_, _ = json.NewDecoder(r.Body).Token()
		_ = buf
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10}}`))
	}))
	defer upstream.Close()
	_ = upstreamBody

	p := Profile{Name: "test", BaseURL: compiler.URL, APIKey: "k", Model: "m", UpstreamURL: upstream.URL, TokenEncoding: "o200k_base"}
	dialect, err := composeDialect(defaultDialectName, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	prettyJSON := "{\n  \"service\": \"orders\",\n  \"max_retries\": 3,\n  \"strict\": true,\n  \"description\": \"Never skip signature verification for any incoming webhook event payload.\"\n}"
	compJSON, _, _, _ := translateMessageBlockWithOptions(context.Background(), "openai", "user", prettyJSON, p, dialect, nil, nil, nil, false)
	if !strings.HasPrefix(compJSON, "{\"") || strings.Contains(compJSON, "\n  ") {
		t.Fatalf("expected SkipNonProse + LocalFastPath to compact JSON, got %q", compJSON)
	}

	proseCold := "Please note that   every request must check `verifySig(req)`   before touching ledger rows.\n\n---\n\n\n| Field   | Rule   |\n| :------ | :----- |\n| `id`    | >= 1   |"
	compProse, _, origTok, compTok := translateMessageBlockWithOptions(context.Background(), "openai", "user", proseCold, p, dialect, nil, nil, nil, false)
	drainShadowWarmup(2 * time.Second)
	if compTok >= origTok || compProse == proseCold {
		t.Fatalf("expected cold-start LocalFastPath compression (%d -> %d): %q", origTok, compTok, compProse)
	}
	if !strings.Contains(compProse, "`verifySig(req)`") || !strings.Contains(compProse, "|`id`|>= 1|") {
		t.Fatalf("protected tokens lost in proxy fast path: %q", compProse)
	}

	logsMutex.RLock()
	if len(proxyLogs) < 2 || proxyLogs[0].Decision != "local_fast" || proxyLogs[1].Decision != "local_fast" {
		logsMutex.RUnlock()
		t.Fatalf("expected local_fast proxy decisions, got %+v", proxyLogs)
	}
	logsMutex.RUnlock()
}

