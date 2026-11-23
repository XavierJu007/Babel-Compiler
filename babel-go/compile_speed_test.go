package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 默认请求体必须与改造前一致：只有显式开启时才注入思考控制字段。
func TestCallLLMInjectsThinkingControlsOnlyWhenConfigured(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		writeModelContent(w, "ok")
	}))
	defer server.Close()

	defaultProfile := Profile{Name: "plain", APIKey: "k", BaseURL: server.URL, Model: "m"}
	if _, err := callLLMContext(context.Background(), defaultProfile, "sys", "user", 0); err != nil {
		t.Fatal(err)
	}
	tuned := defaultProfile
	tuned.DisableThinking = true
	tuned.ReasoningEffort = "low"
	if _, err := callLLMContext(context.Background(), tuned, "sys", "user", 0); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(bodies))
	}
	for _, forbidden := range []string{"reasoning_effort", "enable_thinking", "chat_template_kwargs", "max_tokens"} {
		if _, exists := bodies[0][forbidden]; exists {
			t.Fatalf("default request injected %q: %+v", forbidden, bodies[0])
		}
	}
	if got := bodies[1]["reasoning_effort"]; got != "low" {
		t.Errorf("reasoning_effort=%v; want low", got)
	}
	if got, _ := bodies[1]["enable_thinking"].(bool); got {
		t.Errorf("enable_thinking=%v; want false", bodies[1]["enable_thinking"])
	}
	kwargs, _ := bodies[1]["chat_template_kwargs"].(map[string]any)
	if enabled, _ := kwargs["enable_thinking"].(bool); enabled {
		t.Errorf("chat_template_kwargs=%v; want enable_thinking=false", kwargs)
	}
	for _, unsupported := range []string{"high", "medium", "bogus"} {
		if err := validateReasoningEffort(unsupported); err == nil {
			t.Errorf("validateReasoningEffort(%q) accepted an unsupported value", unsupported)
		}
	}
}

// 生成打向编译端点，compare 校验打向独立校验端点并限制 max_tokens；
// 被引用的校验方案不允许删除。
func TestAuditProfileSeparatesValidationFromCompilerAndBlocksDelete(t *testing.T) {
	isolateCompilerState(t)

	var genCalls, auditCalls atomic.Int32
	var auditBody map[string]any
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		genCalls.Add(1)
		writeModelContent(w, "状态=待审 ⇒ 主管批准")
	}))
	defer compiler.Close()
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		auditCalls.Add(1)
		auditBody = body
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer auditor.Close()

	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen-model", AuditProfile: "auditor"},
		"auditor":  {Name: "auditor", APIKey: "audit-key", BaseURL: auditor.URL, Model: "audit-model"},
	}}); err != nil {
		t.Fatal(err)
	}

	out, err := doCompileContext(context.Background(), CompileReq{
		Profile:    "compiler",
		Text:       "订单处于待审核状态时，必须由主管批准后才能继续处理。",
		StrictGate: true,
	})
	if err != nil {
		t.Fatalf("compile failed: %v (compiled=%q)", err, out.Compiled)
	}
	if genCalls.Load() != 1 {
		t.Errorf("generation calls=%d; want 1", genCalls.Load())
	}
	if auditCalls.Load() != 1 {
		t.Fatalf("audit calls=%d; want 1 on the dedicated audit endpoint", auditCalls.Load())
	}
	if got := auditBody["model"]; got != "audit-model" {
		t.Errorf("audit model=%v; want audit-model", got)
	}
	if got, _ := auditBody["max_tokens"].(float64); got != 512 {
		t.Errorf("audit max_tokens=%v; want 512", auditBody["max_tokens"])
	}

	// 被引用的校验方案不可删除，且错误信息要指出是「校验方案」引用。
	payload, _ := json.Marshal(map[string]string{"action": "delete", "profile_name": "auditor"})
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "校验方案") {
		t.Fatalf("delete guard failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 候选不比原文短时，代理直接保留原文，不再调用校验端点。
func TestProxyEarlySkipsAuditWhenCandidateNotShorter(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	source := "请审阅这段操作说明并概括其中的审批要求与例外条件，确保下游模型能够正确执行。"
	longer := "请审阅这段操作说明并概括其中的审批要求与例外条件，确保下游模型能够正确执行，同时保留所有细节与原始表述以便逐字复核。"
	var auditCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			http.Error(w, "audit must not run", http.StatusTeapot)
			return
		}
		writeModelContent(w, longer)
	}))
	defer compiler.Close()
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditCalls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer auditor.Close()

	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen", AuditProfile: "auditor"},
		"auditor":  {Name: "auditor", APIKey: "audit-key", BaseURL: auditor.URL, Model: "audit"},
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig()
	profile := getActiveProfile(cfg, "compiler")
	out, logID, before, after := translateMessageBlock("openai", "user", source, profile, getDialect(defaultDialectName), nil, nil)
	if out != source || before != after {
		t.Fatalf("non-shorter candidate was adopted: out=%q before=%d after=%d", out, before, after)
	}
	if auditCalls.Load() != 0 {
		t.Fatalf("audit ran %d times even though the candidate was not shorter", auditCalls.Load())
	}
	logsMutex.RLock()
	var entry ProxyLog
	for _, candidate := range proxyLogs {
		if candidate.ID == logID {
			entry = candidate
			break
		}
	}
	logsMutex.RUnlock()
	if entry.Decision != "kept_original" || !strings.Contains(entry.Reason, "候选未减少") {
		t.Fatalf("unexpected fallback log: %+v", entry)
	}
}
