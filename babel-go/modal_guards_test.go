package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestModalGuardAmbiguitiesReachComparison(t *testing.T) {
	for _, tc := range []struct {
		source, candidate, modality, action, object string
		negated                                     bool
	}{
		{"The system cannot delete logs.", "The system can not delete logs.", "capability", "delete", "logs", true},
		{"The system may not delete logs.", "The system might not delete logs.", "uncertain", "delete", "logs", true},
		{"If either fails, do not start build_report.", "either fails → MUST NOT start build_report.", "prohibition", "start", "build_report", false},
		{"MUST NOT delete logs.", "Do not delete logs.", "prohibition", "delete", "logs", false},
		{"只展示，不执行", "仅展示; 禁止执行", "prohibition", "执行", "", false},
		{"禁止执行", "不执行", "prohibition", "执行", "", false},
		{"The system does not delete logs.", "The system MUST NOT delete logs.", "prohibition", "delete", "logs", false},
	} {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(mode+" "+tc.source, func(t *testing.T) {
				var readerCalls, comparatorCalls atomic.Int32
				reading := BlindReading{SchemaVersion: 1, Entities: []string{}, Ambiguities: []string{}, Relations: []BlindRelation{{
					Relation: tc.action, Object: tc.object, Conditions: []string{}, Modality: tc.modality, Negated: tc.negated, Quantities: []BlindQuantity{}, Evidence: tc.candidate,
				}}}
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body compilerRequestBody
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
						t.Error("invalid validation request")
						http.Error(w, "invalid request", 400)
						return
					}
					if body.Messages[0].Content == blindReaderPrompt {
						readerCalls.Add(1)
						writeModelContent(w, blindReadingJSON(t, reading))
						return
					}
					comparatorCalls.Add(1)
					// A model rejection must remain authoritative: a locally unknown
					// modal expression is never a successful semantic validation.
					writeModelContent(w, `{"pass":false,"missing":["需要依据上下文判断能力、可能性或禁止"]}`)
				}))
				defer service.Close()
				p := Profile{APIKey: "test", BaseURL: service.URL, Model: "test-model"}
				var result ValidationResult
				wantReader := int32(0)
				if mode == "blind" {
					wantReader = 1
					result = runBlindValidation(p, p, tc.source, tc.candidate, nil)
				} else {
					result = runValidation(p, tc.source, tc.candidate, nil)
				}
				if readerCalls.Load() != wantReader || comparatorCalls.Load() != 1 || result.Status != "failed" {
					t.Fatalf("uncertain modality must reach comparison: reader=%d comparator=%d result=%+v", readerCalls.Load(), comparatorCalls.Load(), result)
				}
			})
		}
	}
}

func TestModalGuardUncertainCandidateDoesNotBecomeKnownConflict(t *testing.T) {
	for _, candidate := range []string{"The system cannot delete logs.", "The system may not delete logs."} {
		if issues := checkDeterministicInvariants("The system must not delete logs.", candidate, nil); len(issues) != 0 {
			t.Fatalf("ambiguous candidate requires model review: %v", issues)
		}
	}
	if issues := checkDeterministicInvariants("The system MUST NOT delete logs.", "The system MAY delete logs.", nil); len(issues) == 0 {
		t.Fatal("explicit prohibition-to-permission conflict was not rejected")
	}
	if issues := checkDeterministicInvariants("禁止删除日志", "允许删除日志", nil); len(issues) == 0 {
		t.Fatal("explicit Chinese prohibition-to-permission conflict was not rejected")
	}
	if issues := checkDeterministicInvariants("尚未删除日志", "禁止删除日志", nil); len(issues) == 0 {
		t.Fatal("explicit factual-negation-to-prohibition conflict was not rejected")
	}
}

func TestConditionalNegationDoesNotConflictWithAnotherScope(t *testing.T) {
	original := "单笔不超过99元时无需审批；未经审批不得导出原始明细。"
	candidate := "单笔≤99元: 无需审批; 未经审批→禁止导出原始明细。"
	if issues := checkDeterministicInvariants(original, candidate, nil); len(issues) != 0 {
		t.Fatalf("approval prerequisite was confused with an exemption in another scope: %v", issues)
	}
}
