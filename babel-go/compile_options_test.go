package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCachedCandidatesAreCountedWithEachRequestsEncoding(t *testing.T) {
	isolateDomainIntegration(t)
	const text = "所有请求必须通过身份验证，不得泄露密钥。"
	var generations atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generations.Add(1)
		writeModelContent(w, text)
	}))
	defer service.Close()
	req := CompileReq{Text: text, Model: "compiler-not-consumer", APIKey: "test", BaseURL: service.URL}
	// Counts are from the independent OpenAI reference fixture.
	for i, tc := range []struct {
		encoding string
		want     int
	}{{"cl100k_base", 20}, {"o200k_base", 13}} {
		req.TokenEncoding = tc.encoding
		out, err := doCompile(req)
		if err != nil {
			t.Fatal(err)
		}
		if out.Stats.OriginalTokens != tc.want || out.Stats.CompiledTokens != tc.want || out.Stats.MeasurementEncoding != tc.encoding || out.Stats.MeasurementKind != "exact" {
			t.Fatalf("wrong request count: %+v", out.Stats)
		}
		if out.CacheHit != (i > 0) {
			t.Fatalf("unexpected cache state: %v", out.CacheHit)
		}
	}
	req.TokenEncoding = "estimate"
	out, err := doCompile(req)
	if err != nil || !out.CacheHit || out.Stats.MeasurementKind != "estimated" {
		t.Fatalf("estimate override: %+v %v", out, err)
	}
	if generations.Load() != 1 {
		t.Fatalf("encoding selection regenerated candidate %d times", generations.Load())
	}
}

func TestSingleflightDoesNotShareTokenMeasurement(t *testing.T) {
	isolateDomainIntegration(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var generations atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if generations.Add(1) == 1 {
			close(started)
		}
		<-release
		writeModelContent(w, "所有请求必须通过身份验证，不得泄露密钥。")
	}))
	defer service.Close()
	defer unblock()
	type result struct {
		out CompileResp
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	req := CompileReq{Text: "所有请求必须通过身份验证，不得泄露密钥。", Model: "test", APIKey: "test", BaseURL: service.URL, TokenEncoding: "cl100k_base"}
	go func() { out, err := doCompile(req); first <- result{out, err} }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("generation did not start")
	}
	follower := req
	follower.TokenEncoding = "o200k_base"
	go func() { out, err := doCompile(follower); second <- result{out, err} }()
	joined := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		compileFlight.mu.Lock()
		for _, call := range compileFlight.calls {
			joined = joined || call.waiters > 0
		}
		compileFlight.mu.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}
	unblock()
	a, b := <-first, <-second
	if !joined || a.err != nil || b.err != nil || generations.Load() != 1 {
		t.Fatalf("generation sharing failed: joined=%v calls=%d errors=%v/%v", joined, generations.Load(), a.err, b.err)
	}
	if a.out.Stats.CompiledTokens != 20 || b.out.Stats.CompiledTokens != 13 {
		t.Fatalf("leader's count leaked: %d / %d", a.out.Stats.CompiledTokens, b.out.Stats.CompiledTokens)
	}
}

func TestExplicitExecutionSettingsFailBeforeModelCalls(t *testing.T) {
	isolateDomainIntegration(t)
	var calls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeModelContent(w, "candidate") }))
	defer service.Close()
	missing := "missing-reader"
	base := CompileReq{Text: "Read this source text.", Model: "test", APIKey: "test", BaseURL: service.URL}
	for _, mutate := range []func(*CompileReq){
		func(r *CompileReq) { r.Profile = "missing-profile" },
		func(r *CompileReq) { r.TokenEncoding = "qwen" },
		func(r *CompileReq) { r.ValidationMode = "trust-cache" },
		func(r *CompileReq) { r.ValidationMode = "blind"; r.ReaderProfile = &missing },
	} {
		req := base
		mutate(&req)
		if _, err := doCompile(req); err == nil {
			t.Fatalf("invalid selection accepted: %+v", req)
		}
		body := map[string]any{"text": req.Text, "candidate": "Read source.", "profile": req.Profile, "token_encoding": req.TokenEncoding, "validation_mode": req.ValidationMode, "reader_profile": req.ReaderProfile, "base_url": service.URL, "api_key": "test"}
		sendDomainAPI(t, http.MethodPost, "/api/validate", body, http.StatusBadRequest)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid settings made %d model calls", calls.Load())
	}
}

func TestProfileOptionsPersistAndLegacySavesPreserveThem(t *testing.T) {
	isolateDomainIntegration(t)
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{"action": "save", "profile_name": "test", "token_encoding": "o200k_base", "validation_mode": "blind", "reader_profile": "test"}, http.StatusOK)
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{"action": "save", "profile_name": "test", "model": "updated-model"}, http.StatusOK)
	got := loadConfig().Profiles["test"]
	if got.TokenEncoding != "o200k_base" || got.ValidationMode != "blind" || got.ReaderProfile != "test" {
		t.Fatalf("legacy save erased execution choices: %+v", got)
	}
	listed := sendDomainAPI(t, http.MethodGet, "/api/config", nil, http.StatusOK)
	var profiles map[string]map[string]any
	json.Unmarshal(listed["profiles"], &profiles)
	if profiles["test"]["token_encoding"] != "o200k_base" || profiles["test"]["validation_mode"] != "blind" {
		t.Fatal("saved options missing from config response")
	}
	self := ""
	resolved, err := resolveCompileProfile(loadConfig(), CompileReq{TokenEncoding: "estimate", ValidationMode: "compare", ReaderProfile: &self})
	if err != nil || resolved.TokenEncoding != "estimate" || resolved.ValidationMode != "compare" || resolved.ReaderProfile != "" {
		t.Fatalf("explicit override failed: %+v %v", resolved, err)
	}
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{"action": "save", "profile_name": "test", "token_encoding": "auto"}, http.StatusBadRequest)
	if loadConfig().Profiles["test"].TokenEncoding != "o200k_base" {
		t.Fatal("invalid options changed persisted profile")
	}
}

func TestProxyAdoptionUsesSelectedEncodingInsteadOfHeuristic(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	original := strings.Repeat("Hello, world!", 4)
	// An intentionally bad mock candidate separates the two counters. The mock
	// auditor is forced to approve so this test isolates the adoption decision.
	candidate := "燚龘麤靐齉爩鱻灥麤龘"
	before, _ := measureTokens(original, "cl100k_base")
	after, _ := measureTokens(candidate, "cl100k_base")
	if estimateTokensBPE(candidate) >= estimateTokensBPE(original) || after.Tokens <= before.Tokens {
		t.Fatal("fixture no longer distinguishes heuristic and exact counts")
	}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		if body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		writeModelContent(w, candidate)
	}))
	defer service.Close()
	p := Profile{Model: "test", APIKey: "test", BaseURL: service.URL, TokenEncoding: "cl100k_base"}
	got, _, _, _ := translateMessageBlock("openai", "user", original, p, getDialect("token"), nil, nil)
	if got != original {
		t.Fatal("proxy adopted a larger exact-token candidate because heuristic was smaller")
	}
	logsMutex.RLock()
	entry := proxyLogs[0]
	logsMutex.RUnlock()
	if entry.MeasurementEncoding != "cl100k_base" || entry.MeasurementKind != "exact" || !strings.Contains(entry.Reason, "cl100k_base") {
		t.Fatalf("missing decision provenance: %+v", entry)
	}
}

func TestReferencedReaderProfileCannotBeDeleted(t *testing.T) {
	isolateDomainIntegration(t)
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
		"test":   {ValidationMode: "blind", ReaderProfile: "reader"},
		"reader": {Model: "reader-model"},
	}}); err != nil {
		t.Fatal(err)
	}
	response := sendDomainAPI(t, http.MethodPost, "/api/config", map[string]string{"action": "delete", "profile_name": "reader"}, http.StatusBadRequest)
	if !strings.Contains(string(response["detail"]), "test") {
		t.Fatal("delete error did not identify the referencing profile")
	}
	if len(loadConfig().Profiles) != 2 {
		t.Fatal("rejected deletion changed the config")
	}
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]string{"action": "save", "profile_name": "test", "reader_profile": ""}, http.StatusOK)
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]string{"action": "delete", "profile_name": "reader"}, http.StatusOK)
	if cfg := loadConfig(); len(cfg.Profiles) != 1 || cfg.Profiles["test"].ReaderProfile != "" {
		t.Fatal("explicitly clearing the reader did not allow deletion")
	}
}
