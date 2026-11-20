package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompileAPIPreservesTruncatedCandidateWithoutAcceptingOrCaching(t *testing.T) {
	for _, stage := range []string{"generation", "rewrite"} {
		t.Run(stage, func(t *testing.T) {
			isolateDomainIntegration(t)
			const partial = "值班助理→执行发布; 部署仅当备份配置已成功∧"
			var generations, rewrites, audits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input compilerRequestBody
				if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Messages) != 2 {
					t.Error("invalid compiler request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				system := input.Messages[0].Content
				if system == verifyGatePrompt {
					audits.Add(1)
					t.Error("truncated output reached semantic validation")
					writeModelContent(w, `{"pass":true,"missing":[]}`)
					return
				}
				if strings.HasSuffix(system, outlineRewriteInstruction) {
					rewrites.Add(1)
				} else {
					generations.Add(1)
					if stage == "rewrite" {
						writeModelContent(w, outlineDraft)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": "length", "message": map[string]string{"content": partial},
				}}})
			}))
			defer provider.Close()
			for _, strict := range []bool{true, false} {
				payload, err := json.Marshal(CompileReq{Text: outlineSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: strict})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				newServerMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/compile", bytes.NewReader(payload)))
				var result struct {
					CompileResp
					Detail string `json:"detail"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusBadGateway || result.Compiled != partial || result.CacheHit || result.Validation.Status != "error" || !strings.Contains(result.Detail, "finish_reason=length") {
					t.Fatalf("truncated candidate disappeared or became usable: HTTP %d, %+v", response.Code, result)
				}
				if result.Stats.CompiledTokens <= 0 {
					t.Fatal("truncated candidate was not measured for inspection")
				}
				cacheMutex.RLock()
				cached := len(diskCache)
				cacheMutex.RUnlock()
				if cached != 0 {
					t.Fatal("truncated candidate entered the generation cache")
				}
			}
			wantRewrites := int32(0)
			if stage == "rewrite" {
				wantRewrites = 2
			}
			if generations.Load() != 2 || rewrites.Load() != wantRewrites || audits.Load() != 0 {
				t.Fatalf("unexpected calls: generation=%d rewrite=%d audit=%d", generations.Load(), rewrites.Load(), audits.Load())
			}
		})
	}
}

func TestTruncatedAuditCannotPassWithCompleteJSON(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "length", "message": map[string]string{"content": `{"pass":true,"missing":[]}`},
		}}})
	}))
	defer provider.Close()
	profile := Profile{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
	result := runValidation(profile, "请用中文概括这份说明。", "概括说明→中文", nil)
	if result.Status != "error" || !strings.Contains(strings.Join(result.Issues, "; "), "finish_reason=length") {
		t.Fatalf("a parseable partial audit bypassed the completion error: %+v", result)
	}
}
