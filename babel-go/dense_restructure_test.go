package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Route the full user-reported failure through the real compiler/cache path.
// The provider returns fixed fixtures: this tests the contract, not model quality.
func TestReportedRefundRewritesAndCachesOnlyTree(t *testing.T) {
	source, tree := refundSOPFixture(t)
	reported := readRefundFixture(t, "refund-sop.reported.txt")
	isolateDomainIntegration(t)
	if len(babelStructureIssues(source, reported)) == 0 || len(babelStructureIssues(source, tree)) != 0 {
		t.Fatal("full refund fixtures no longer distinguish the reported failure from the target tree")
	}
	var generations, rewrites, audits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Error("invalid compiler request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		system, input := body.Messages[0].Content, body.Messages[1].Content
		switch {
		case system == verifyGatePrompt:
			audits.Add(1)
			var payload map[string]string
			if json.Unmarshal([]byte(input), &payload) != nil || !reflect.DeepEqual(payload, map[string]string{"original": source, "candidate": tree}) {
				t.Error("semantic review did not receive the original and final tree")
			}
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		case strings.HasSuffix(system, outlineRewriteInstruction):
			rewrites.Add(1)
			payload := decodeCompilerInput(t, input)
			if payload.Original != source || payload.Candidate != reported {
				t.Error("rewrite lost the source or changed the user-reported draft")
			}
			writeModelContent(w, tree)
		default:
			generations.Add(1)
			payload := decodeCompilerInput(t, input)
			if payload.Original != source || payload.Candidate != "" || !strings.Contains(system, mixedCompilerStyle) || !strings.Contains(system, mixedCompilerExamples) {
				t.Error("generation lost the original or the fixed tree preset")
			}
			writeModelContent(w, reported)
		}
	}))
	defer provider.Close()
	request := CompileReq{Text: source, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true}
	profile, err := resolveCompileProfile(loadConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := composeDialect(request.Mode, request.Domains, request.Text)
	if err != nil {
		t.Fatal(err)
	}
	key := makeCacheKey(dialect, profile, source)
	cacheMutex.Lock()
	diskCache[key] = CacheEntry{Compiled: reported}
	cacheMutex.Unlock()
	for i := 0; i < 2; i++ {
		result, err := doCompile(request)
		if err != nil || result.Compiled != tree || result.Validation.Status != "passed" || result.CacheHit != (i > 0) {
			t.Fatalf("compile %d: result=%+v error=%v", i, result, err)
		}
	}
	if generations.Load() != 1 || rewrites.Load() != 1 || audits.Load() != 2 {
		t.Fatalf("unexpected calls: generate=%d rewrite=%d audit=%d", generations.Load(), rewrites.Load(), audits.Load())
	}
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	if len(diskCache) != 1 || diskCache[key].Compiled != tree {
		t.Fatal("cache retained the flat reported output or did not store the final tree")
	}
}

func TestReportedRefundRepeatedFailureRemainsVisible(t *testing.T) {
	source, _ := refundSOPFixture(t)
	reported := readRefundFixture(t, "refund-sop.reported.txt")
	for _, test := range []struct{ name, candidate string }{
		{"original flat headings", reported},
		{"renamed but still flat", strings.NewReplacer(
			"受理资格:", "输入条件:", "金额分级与审批链:", "金额门槛:",
			"执行流程:", "动作:", "异常与并发:", "分支:",
			"记录与质检:", "记录:", "职责边界:", "范围:",
		).Replace(reported)},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
					t.Error("invalid compiler request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if body.Messages[0].Content == verifyGatePrompt {
					t.Error("structurally failed candidate reached semantic review")
				}
				writeModelContent(w, test.candidate)
			}))
			defer provider.Close()
			payload, _ := json.Marshal(CompileReq{Text: source, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true})
			response := httptest.NewRecorder()
			newServerMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/compile", bytes.NewReader(payload)))
			var result struct {
				Compiled   string           `json:"compiled"`
				Validation ValidationResult `json:"validation"`
			}
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != http.StatusUnprocessableEntity || result.Compiled != test.candidate || result.Validation.Status != "failed" || !strings.Contains(strings.Join(result.Validation.Issues, " "), "多分支正文未组织为树") {
				t.Fatalf("failed tree contract hid or adopted the candidate: %d %s", response.Code, response.Body.String())
			}
			cacheMutex.RLock()
			entries := len(diskCache)
			cacheMutex.RUnlock()
			if calls.Load() != 2 || entries != 0 {
				t.Fatalf("rewrite was unbounded or cached failure: calls=%d cache=%d", calls.Load(), entries)
			}
		})
	}
}
