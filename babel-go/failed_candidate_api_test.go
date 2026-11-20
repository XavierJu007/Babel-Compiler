package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

const failedCandidateSource = "请审阅提供的完整规范，汇总其中的运行要求，并用中文 JSON 回答，保留所有适用条件与例外。"
const failedCandidateText = "Review spec→operational requirements; 中文 JSON; 保留条件∧例外."

func TestCompileAPIReturnsCandidateOnStrictValidationFailure(t *testing.T) {
	for _, tc := range []struct {
		name, validationStatus string
		status                 int
		serviceError           bool
	}{
		{name: "semantic rejection", validationStatus: "failed", status: http.StatusUnprocessableEntity},
		{name: "validation service error", validationStatus: "error", status: http.StatusBadGateway, serviceError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			var generations, audits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
					t.Error("invalid compiler fixture request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if request.Messages[0].Content == verifyGatePrompt {
					audits.Add(1)
					if tc.serviceError {
						http.Error(w, "fixture audit unavailable", http.StatusBadGateway)
						return
					}
					writeModelContent(w, `{"pass":false,"missing":["fixture: constraint omitted"]}`)
					return
				}
				generations.Add(1)
				writeModelContent(w, failedCandidateText)
			}))
			defer provider.Close()
			request := CompileReq{Text: failedCandidateSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true}
			for attempt := 0; attempt < 2; attempt++ {
				body := sendDomainAPI(t, http.MethodPost, "/api/compile", request, tc.status)
				for _, field := range []string{"compiled", "dialect", "cache_hit", "stats", "validation", "detail"} {
					if _, exists := body[field]; !exists {
						t.Fatalf("failure response omitted %q: %v", field, body)
					}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					CompileResp
					Detail string `json:"detail"`
				}
				if err := json.Unmarshal(encoded, &result); err != nil {
					t.Fatal(err)
				}
				if result.Compiled != failedCandidateText || result.Dialect != defaultDialectName || result.CacheHit != (attempt > 0) {
					t.Fatalf("failure lost the current candidate or its provenance: %+v", result)
				}
				if result.Validation.Status != tc.validationStatus || result.Validation.Method != "llm_compare" || len(result.Validation.Issues) == 0 || result.Detail == "" {
					t.Fatalf("candidate was returned without its actual failed check: %+v", result)
				}
				if result.Stats.OriginalChars != utf8.RuneCountInString(failedCandidateSource) || result.Stats.CompiledChars != utf8.RuneCountInString(failedCandidateText) || result.Stats.OriginalTokens <= 0 || result.Stats.CompiledTokens <= 0 {
					t.Fatalf("failure omitted candidate measurements: %+v", result.Stats)
				}
			}
			if generations.Load() != 1 || audits.Load() != 2 {
				t.Fatalf("cache hit skipped fresh validation: generations=%d audits=%d", generations.Load(), audits.Load())
			}
		})
	}
}

func TestFailedOutlineRewriteRetainsLatestCandidateWithoutCaching(t *testing.T) {
	const secondDraft = outlineDraft + "\n执行过程保留记录。"
	for _, tc := range []struct {
		name, rewrite, want string
		serviceError        bool
	}{
		{name: "repeated outline", rewrite: secondDraft, want: secondDraft},
		{name: "empty rewrite", want: outlineDraft},
		{name: "rewrite service error", want: outlineDraft, serviceError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			var generations, rewrites, audits atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
					t.Error("invalid compiler fixture request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				switch system := request.Messages[0].Content; {
				case system == verifyGatePrompt:
					audits.Add(1)
					t.Error("failed expression rewrite reached semantic validation")
					writeModelContent(w, `{"pass":true,"missing":[]}`)
				case strings.HasSuffix(system, outlineRewriteInstruction):
					rewrites.Add(1)
					if tc.serviceError {
						http.Error(w, "fixture rewrite unavailable", http.StatusBadGateway)
						return
					}
					writeModelContent(w, tc.rewrite)
				default:
					generations.Add(1)
					writeModelContent(w, outlineDraft)
				}
			}))
			defer provider.Close()
			request := CompileReq{Text: outlineSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := doCompile(request)
				if err == nil || result.Compiled != tc.want || result.CacheHit || result.Validation.Status == "passed" {
					t.Fatalf("failed rewrite lost or accepted its latest candidate: result=%+v error=%v", result, err)
				}
				cacheMutex.RLock()
				entries := len(diskCache)
				cacheMutex.RUnlock()
				if entries != 0 {
					t.Fatalf("failed expression candidate entered cache: %d entries", entries)
				}
			}
			if generations.Load() != 2 || rewrites.Load() != 2 || audits.Load() != 0 {
				t.Fatalf("failed rewrite was reused or retried without bound: generation=%d rewrite=%d audit=%d", generations.Load(), rewrites.Load(), audits.Load())
			}
		})
	}
}

func TestCompileAPIGenerationFailureDoesNotInventCandidate(t *testing.T) {
	for _, serviceError := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty generation", true: "generation service error"}[serviceError], func(t *testing.T) {
			isolateDomainIntegration(t)
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if serviceError {
					http.Error(w, "fixture generation unavailable", http.StatusBadGateway)
					return
				}
				writeModelContent(w, "")
			}))
			defer provider.Close()
			payload, err := json.Marshal(CompileReq{Text: failedCandidateSource, APIKey: "fixture", BaseURL: provider.URL, StrictGate: true})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newServerMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/compile", bytes.NewReader(payload)))
			var result struct {
				Compiled   string           `json:"compiled"`
				Detail     string           `json:"detail"`
				Validation ValidationResult `json:"validation"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code < http.StatusBadRequest || result.Compiled != "" || result.Detail == "" || result.Validation.Status == "passed" {
				t.Fatalf("generation failure invented a candidate or success: %d %s", response.Code, response.Body.String())
			}
			cacheMutex.RLock()
			entries := len(diskCache)
			cacheMutex.RUnlock()
			if calls.Load() != 1 || entries != 0 {
				t.Fatalf("generation failure retried or cached a candidate: calls=%d cache=%d", calls.Load(), entries)
			}
		})
	}
}
