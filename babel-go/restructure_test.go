package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const outlineSource = "你是值班助理，负责执行发布。\n一、备份要求\n先备份配置，备份成功后才可部署。\n二、发布要求\n发布前须经负责人批准；发布失败须回滚。\n三、记录要求\n每步记录结果，记录保留180天。"
const outlineDraft = "一、备份要求\n备份成功后才可部署。\n二、发布要求\n负责人批准后发布；失败须回滚。\n三、记录要求\n每步留痕，保留180天。"
const outlineGraph = "role:值班助理→执行发布\n├部署仅当备份配置已成功∧负责人已批准\n│ └发布失败→须回滚\n└∀步记录结果,保留180天"

func TestOutlineRewriteCachesOnlyFinalCandidateAndRevalidates(t *testing.T) {
	isolateDomainIntegration(t)
	var generations, rewrites, audits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Error("invalid compiler request")
			http.Error(w, "invalid request", 400)
			return
		}
		system, input := body.Messages[0].Content, body.Messages[1].Content
		switch {
		case system == verifyGatePrompt:
			audits.Add(1)
			var payload map[string]string
			if json.Unmarshal([]byte(input), &payload) != nil || !reflect.DeepEqual(payload, map[string]string{"original": outlineSource, "candidate": outlineGraph}) {
				t.Error("audit saw a draft instead of the final relation candidate")
			}
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		case strings.HasSuffix(system, outlineRewriteInstruction):
			rewrites.Add(1)
			payload := decodeCompilerInput(t, input)
			if payload.Original != outlineSource || payload.Candidate != outlineDraft {
				t.Error("rewrite lost original or changed its untrusted draft")
			}
			writeModelContent(w, outlineGraph)
		default:
			generations.Add(1)
			if payload := decodeCompilerInput(t, input); payload.Original != outlineSource || payload.Candidate != "" {
				t.Error("initial compilation lost original")
			}
			writeModelContent(w, outlineDraft)
		}
	}))
	defer provider.Close()
	request := CompileReq{Text: outlineSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true}
	profile, err := resolveCompileProfile(loadConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := composeDialect(request.Mode, request.Domains, request.Text)
	if err != nil {
		t.Fatal(err)
	}
	// Even a restored draft under the current identity must not bypass shape
	// checks or be mistaken for the result of the new compilation.
	cacheMutex.Lock()
	diskCache[makeCacheKey(dialect, profile, request.Text)] = CacheEntry{Compiled: outlineDraft}
	cacheMutex.Unlock()
	for i := 0; i < 2; i++ {
		result, err := doCompile(request)
		if err != nil || result.Compiled != outlineGraph || result.Validation.Status != "passed" || result.CacheHit != (i > 0) {
			t.Fatalf("compile %d: %+v error=%v", i, result, err)
		}
	}
	if generations.Load() != 1 || rewrites.Load() != 1 || audits.Load() != 2 {
		t.Fatalf("unexpected calls: generate=%d rewrite=%d audit=%d", generations.Load(), rewrites.Load(), audits.Load())
	}
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	if len(diskCache) != 1 {
		t.Fatalf("expected only the final candidate in cache, got %d", len(diskCache))
	}
	for _, entry := range diskCache {
		if entry.Compiled != outlineGraph {
			t.Fatal("numbered draft was cached")
		}
	}
}

func TestRepeatedOutlineFailsWithoutAuditOrCache(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "without semantic audit", true: "strict"}[strict], func(t *testing.T) {
			isolateDomainIntegration(t)
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
					t.Error("invalid model request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if body.Messages[0].Content == verifyGatePrompt {
					t.Error("failed outline reached semantic audit")
				}
				writeModelContent(w, outlineDraft)
			}))
			defer provider.Close()
			payload, _ := json.Marshal(CompileReq{Text: outlineSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: strict})
			response := httptest.NewRecorder()
			newServerMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/compile", bytes.NewReader(payload)))
			var result struct {
				Validation ValidationResult `json:"validation"`
			}
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != http.StatusUnprocessableEntity || result.Validation.Status != "failed" || !strings.Contains(strings.Join(result.Validation.Issues, " "), "表达结构未达标") {
				t.Fatalf("outline was not reported as an expression failure: %d %s", response.Code, response.Body.String())
			}
			cacheMutex.RLock()
			entries := len(diskCache)
			cacheMutex.RUnlock()
			if calls.Load() != 2 || entries != 0 {
				t.Fatalf("rewrite was unbounded or draft cached: calls=%d cache=%d", calls.Load(), entries)
			}
		})
	}
}

func TestOutlineRewriteDoesNotBypassSemanticRejection(t *testing.T) {
	isolateDomainIntegration(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			writeModelContent(w, outlineDraft)
		case 2:
			writeModelContent(w, strings.Replace(outlineGraph, "∧", "∨", 1))
		default:
			writeModelContent(w, `{"pass":false,"missing":["把前置条件合取改成析取"]}`)
		}
	}))
	defer provider.Close()
	result, err := doCompile(CompileReq{Text: outlineSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true})
	var rejected *ValidationError
	if !errors.As(err, &rejected) || result.Validation.Status != "failed" || result.Validation.Method != "llm_compare" || calls.Load() != 3 {
		t.Fatalf("restructuring implied semantic success: result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}

func TestOutlineRewriteStopsOnUpstreamErrorOrCancellation(t *testing.T) {
	for _, failure := range []string{"generation", "rewrite", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if failure == "cancel" {
					cancel()
					return
				}
				if failure == "generation" || failure == "rewrite" && call == 2 {
					http.Error(w, "fixture failure", http.StatusBadGateway)
					return
				}
				writeModelContent(w, outlineDraft)
			}))
			defer provider.Close()
			_, err := generateBabelCandidate(ctx, Profile{APIKey: "fixture", BaseURL: provider.URL}, Dialect{Prompt: commonCompilerRules}, outlineSource)
			wantCalls := int32(1)
			if failure == "rewrite" {
				wantCalls = 2
			}
			if err == nil || calls.Load() != wantCalls || failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("failure caused extra requests or disappeared: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestOutlineFailureKeepsProxyOriginalWithoutOpeningCircuit(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, outlineDraft)
	}))
	defer provider.Close()
	profile := Profile{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
	text, logID, before, after := translateMessageBlockContext(context.Background(), "openai", "user", outlineSource, profile, getDialect(defaultDialectName), nil, nil)
	if text != outlineSource || logID == "" || before != after || calls.Load() != 2 {
		t.Fatalf("outline failure changed proxy input: text=%q calls=%d", text, calls.Load())
	}
	if status := compilerCB.Status(); status["state"] != "CLOSED" || status["failures"] != 0 {
		t.Fatalf("expression failure counted as an unavailable model: %+v", status)
	}
}
