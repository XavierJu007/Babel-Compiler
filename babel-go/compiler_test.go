package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditJSONRejectsAmbiguousApproval(t *testing.T) {
	for _, tc := range []struct {
		name, raw     string
		pass, invalid bool
	}{
		{"valid pass", `{"pass":true,"missing":[]}`, true, false},
		{"valid fail", `{"pass":false,"missing":["lost scope"]}`, false, false},
		{"false without reason", `{"pass":false,"missing":[]}`, false, false},
		{"inconsistent", `{"pass":true,"missing":["lost scope"]}`, false, false},
		{"plain pass", `PASS`, false, true},
		{"not pass", `NOT PASS`, false, true},
		{"missing pass", `{"missing":[]}`, false, true},
		{"missing issues", `{"pass":true}`, false, true},
		{"null pass", `{"pass":null,"missing":[]}`, false, true},
		{"string pass", `{"pass":"true","missing":[]}`, false, true},
		{"null issues", `{"pass":true,"missing":null}`, false, true},
		{"null issue", `{"pass":true,"missing":[null]}`, false, true},
		{"empty issue", `{"pass":true,"missing":[""]}`, false, true},
		{"duplicate key", `{"pass":false,"pass":true,"missing":[]}`, false, true},
		{"unknown field", `{"pass":true,"missing":[],"note":"ok"}`, false, true},
		{"trailing explanation", `{"pass":true,"missing":[]} NOT PASS`, false, true},
		{"two objects", `{"pass":true,"missing":[]}{"pass":true,"missing":[]}`, false, true},
		{"fenced", "```json\n{\"pass\":true,\"missing\":[]}\n```", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass, _, err := parseAuditResult(tc.raw)
			if (err != nil) != tc.invalid || pass != tc.pass {
				t.Fatalf("pass=%v err=%v", pass, err)
			}
		})
	}
}

func TestNumericGuardsUseExactUnconsumedBoundaries(t *testing.T) {
	got := extractStandaloneNumbers("1,2,3 5% 30s -8 127.0.0.1 RS256 H5256 HTTP2 item42")
	want := []string{"1", "2", "3", "5%", "30", "-8", "127.0.0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	for _, tc := range []struct{ name, original, candidate string }{
		{"substring", "超时 5 秒", "超时 50 秒"},
		{"adjacent", "合法状态 1,2,3", "合法状态 1,3"},
		{"identifier", "状态码 5", "状态码 status5"},
		{"percent", "阈值 5%", "阈值 5"},
		{"unit", "超时 30s", "超时 30min"},
		{"operator", "count >= 5", "count > 5"},
		{"negative", "offset -8", "offset 8"},
		{"ip", "host 127.0.0.1", "host 127.0.0.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if failures := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(failures) == 0 {
				t.Fatal("numeric mutation was not rejected")
			}
		})
	}
	if got := checkDeterministicInvariants("count 至少 5，超时 30 秒", "count >= 5；超时 30s", nil); len(got) > 0 {
		t.Fatalf("equivalent numeric expressions rejected: %v", got)
	}
}

func TestModalGuardsRejectMatchedActionConflicts(t *testing.T) {
	for _, tc := range []struct{ name, original, candidate string }{
		{"second prohibition", "禁止删除日志；禁止外发数据", "禁止删除日志；允许外发数据"},
		{"permission to obligation", "允许删除日志", "必须删除日志"},
		{"fact to prohibition", "尚未删除日志", "禁止删除日志"},
		{"recommendation", "建议保存日志", "必须保存日志"},
		{"english", "MUST NOT delete logs; MUST NOT export data", "MUST NOT delete logs; MAY export data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if failures := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(failures) == 0 {
				t.Fatal("explicit modality conflict was not rejected")
			}
		})
	}
	if got := checkDeterministicInvariants("禁止删除日志", "不得删除日志", nil); len(got) > 0 {
		t.Fatalf("same prohibition rejected: %v", got)
	}
	if got := checkDeterministicInvariants("必须在请求成功后保存审计记录", "请求成功后: 须保存审计记录", nil); len(got) > 0 {
		t.Fatalf("reordered unknown relation must proceed to model review: %v", got)
	}
}

func TestCompactDSLPreservesTechnicalContent(t *testing.T) {
	for _, input := range []string{
		"    code  \n\n├─ literal  \n\tvalue\n",
		"```json\n{\"payload\":\"├─ x  \"}\n```",
		"```python\n  print('x')\n\n```",
		"prefix\n```text\n  literal  \n```\nsuffix",
	} {
		if got := compactDSL(input); got != input {
			t.Fatalf("content changed:\n%q\n=>%q", input, got)
		}
	}
	wrapped := "```text\n  first  \n\n├─ second  \n```"
	if got, want := compactDSL(wrapped), "  first  \n\n├─ second  "; got != want {
		t.Fatalf("wrapper result %q; want %q", got, want)
	}
}

// These tests do not run in parallel: config/cache paths are process-relative.
// A task-private working directory prevents reading or writing real settings.
func isolateCompilerState(t *testing.T) string {
	t.Helper()
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("babel.config.json", []byte(`{"active_profile":"test","profiles":{"test":{"name":"test"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cacheMutex.Lock()
	previousCache := diskCache
	diskCache = make(map[string]CacheEntry)
	cacheMutex.Unlock()
	compileFlight = flightGroup{}
	t.Cleanup(func() {
		cacheMutex.Lock()
		diskCache = previousCache
		cacheMutex.Unlock()
		compileFlight = flightGroup{}
		if err := os.Chdir(previousDir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

type compilerRequestBody struct {
	Messages []struct{ Role, Content string } `json:"messages"`
}

func writeModelContent(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
}

func TestCacheHitRevalidatesAndIgnoresLegacyAudit(t *testing.T) {
	isolateCompilerState(t)
	var generationCalls, auditCalls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if body.Messages[0].Content == verifyGatePrompt {
			auditCalls.Add(1)
			writeModelContent(w, `{"pass":false,"missing":["removed a constraint"]}`)
		} else {
			generationCalls.Add(1)
			writeModelContent(w, "Review the specification.")
		}
	}))
	defer service.Close()
	req := CompileReq{Text: "Please review the specification.", Mode: "token", APIKey: "test", BaseURL: service.URL, Model: "test-model"}
	first, err := doCompile(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Validation.Status != "not_run" || first.Audit != "" {
		t.Fatalf("unverified result claimed validation: %+v", first.Validation)
	}
	cacheMutex.Lock()
	for key, entry := range diskCache {
		entry.Audit = "[PASS] old proof"
		diskCache[key] = entry
	}
	cacheMutex.Unlock()
	req.StrictGate = true
	second, err := doCompile(req)
	var gateErr *ValidationError
	if !errors.As(err, &gateErr) || gateErr.Validation.Status != "failed" || !second.CacheHit {
		t.Fatalf("cached legacy PASS bypassed new check: %+v err=%v", second, err)
	}
	if generationCalls.Load() != 1 || auditCalls.Load() != 1 {
		t.Fatalf("calls generation=%d audit=%d", generationCalls.Load(), auditCalls.Load())
	}
	req.RequiredKeywords = []string{"json"}
	third, err := doCompile(req)
	if !errors.As(err, &gateErr) || third.Validation.Method != "local_checks" {
		t.Fatalf("request-specific keyword did not fail: %+v %v", third.Validation, err)
	}
	if auditCalls.Load() != 1 {
		t.Fatal("missing keyword should be caught before model review")
	}
}

func TestEveryVerifiedCacheHitRunsFreshAudit(t *testing.T) {
	isolateCompilerState(t)
	var generations, audits atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Messages[0].Content == verifyGatePrompt {
			audits.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		} else {
			generations.Add(1)
			writeModelContent(w, "Read the specification.")
		}
	}))
	defer service.Close()
	req := CompileReq{Text: "Please read the specification.", StrictGate: true, APIKey: "test", BaseURL: service.URL, Model: "test"}
	for i := 0; i < 2; i++ {
		out, err := doCompile(req)
		if err != nil || out.Validation.Status != "passed" {
			t.Fatalf("validation %d: %+v %v", i, out.Validation, err)
		}
	}
	if generations.Load() != 1 || audits.Load() != 2 {
		t.Fatalf("generation=%d audits=%d", generations.Load(), audits.Load())
	}
}

func TestSingleflightSharesCandidateButNotValidation(t *testing.T) {
	isolateCompilerState(t)
	started, release := make(chan struct{}), make(chan struct{})
	var generations, audits atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Messages[0].Content == verifyGatePrompt {
			audits.Add(1)
			writeModelContent(w, `{"pass":false,"missing":["missing exception"]}`)
			return
		}
		if generations.Add(1) == 1 {
			close(started)
		}
		<-release
		writeModelContent(w, "Read the specification.")
	}))
	defer service.Close()
	req := CompileReq{Text: "Please read the specification.", APIKey: "test", BaseURL: service.URL, Model: "test"}
	type result struct {
		out CompileResp
		err error
	}
	looseDone, strictDone := make(chan result, 1), make(chan result, 1)
	go func() { out, err := doCompile(req); looseDone <- result{out, err} }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("generation did not start")
	}
	strictReq := req
	strictReq.StrictGate = true
	go func() { out, err := doCompile(strictReq); strictDone <- result{out, err} }()
	// Wait until the strict request is actually a follower, not a later cache hit.
	deadline := time.Now().Add(2 * time.Second)
	joined := false
	for time.Now().Before(deadline) {
		compileFlight.mu.Lock()
		for _, call := range compileFlight.calls {
			if call.waiters > 0 {
				joined = true
			}
		}
		compileFlight.mu.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}
	close(release)
	loose, strict := <-looseDone, <-strictDone
	if !joined {
		t.Fatal("strict request never joined singleflight")
	}
	if loose.err != nil || loose.out.Validation.Status != "not_run" {
		t.Fatalf("loose request inherited strict result: %+v %v", loose.out.Validation, loose.err)
	}
	var gateErr *ValidationError
	if !errors.As(strict.err, &gateErr) || strict.out.Validation.Status != "failed" {
		t.Fatalf("strict follower inherited loose validation: %+v %v", strict.out.Validation, strict.err)
	}
	if generations.Load() != 1 || audits.Load() != 1 {
		t.Fatalf("generation=%d audits=%d", generations.Load(), audits.Load())
	}
}

func TestReorderedModalClauseIsReviewedByModel(t *testing.T) {
	isolateCompilerState(t)
	var audits atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Messages[0].Content == verifyGatePrompt {
			audits.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		} else {
			writeModelContent(w, "请求成功后: 须保存审计记录")
		}
	}))
	defer service.Close()
	out, err := doCompile(CompileReq{Text: "必须在请求成功后保存审计记录", StrictGate: true, APIKey: "test", BaseURL: service.URL, Model: "test"})
	if err != nil || audits.Load() != 1 || out.Validation.Method != "llm_compare" {
		t.Fatalf("legitimate reordering blocked before review: %+v audits=%d err=%v", out.Validation, audits.Load(), err)
	}
}

func TestAuditErrorsNeverApproveStrictRequest(t *testing.T) {
	for _, tc := range []struct {
		name, audit string
		status      int
	}{
		{"plain NOT PASS", "NOT PASS", 200},
		{"missing pass", `{"missing":[]}`, 200},
		{"inconsistent true", `{"pass":true,"missing":["missing rule"]}`, 200},
		{"service error", "unavailable", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCompilerState(t)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Messages[0].Content == verifyGatePrompt {
					if tc.status != 200 {
						http.Error(w, tc.audit, tc.status)
					} else {
						writeModelContent(w, tc.audit)
					}
				} else {
					writeModelContent(w, "Read the specification.")
				}
			}))
			defer service.Close()
			req := CompileReq{Text: "Please read the specification.", Verify: true, APIKey: "test", BaseURL: service.URL, Model: "test"}
			advisory, err := doCompile(req)
			if err != nil || advisory.Validation.Status == "passed" || advisory.Validation.Status == "not_run" {
				t.Fatalf("advisory result %+v err=%v", advisory.Validation, err)
			}
			req.StrictGate = true
			strict, err := doCompile(req)
			var gateErr *ValidationError
			if !errors.As(err, &gateErr) || strict.Validation.Status == "passed" {
				t.Fatalf("strict result %+v err=%v", strict.Validation, err)
			}
		})
	}
}

func TestNoCacheBypassesReadWriteAndStatsAreEstimated(t *testing.T) {
	dir := isolateCompilerState(t)
	var generations atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeModelContent(w, fmt.Sprintf("candidate-%d", generations.Add(1)))
	}))
	defer service.Close()
	req := CompileReq{Text: "Please review this specification.", APIKey: "test", BaseURL: service.URL, Model: "test"}
	first, err := doCompile(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "babel.cache.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req.NoCache = true
	second, err := doCompile(req)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHit || second.Compiled == first.Compiled || string(before) != string(after) {
		t.Fatal("no-cache reused or changed stored candidate")
	}
	req.NoCache = false
	third, err := doCompile(req)
	if err != nil {
		t.Fatal(err)
	}
	if third.Compiled != first.Compiled || generations.Load() != 2 {
		t.Fatal("no-cache replaced the original cache entry")
	}
	if second.Stats.MeasurementKind != "estimated" || second.Stats.MeasurementSource != "heuristic-v2" || second.Stats.MeasurementScope != "text" || !strings.Contains(second.Stats.CompressionRatio, "估算") {
		t.Fatalf("missing estimate metadata: %+v", second.Stats)
	}
}

func TestCallLLMRejectsExplicitlyIncompleteResponses(t *testing.T) {
	for _, reason := range []string{"", "stop", "length", "content_filter", "tool_calls"} {
		t.Run("reason="+reason, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": reason, "message": map[string]string{"content": "partial or complete"}}}})
			}))
			defer service.Close()
			_, err := callLLM(Profile{APIKey: "test", BaseURL: service.URL, Model: "test"}, "system", "input", 0)
			if wantError := reason != "" && reason != "stop"; (err != nil) != wantError {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
		})
	}
}
