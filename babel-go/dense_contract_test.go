package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type denseNegationCase struct {
	name, original, candidate, comparison string
	equivalent, ambiguous                 bool
}

func denseNegationCases() []denseNegationCase {
	return []denseNegationCase{
		{"lower bound", "count >= 5", "¬(count < 5)", "<", true, false},
		{"upper bound", "金额不超过99元", "¬(金额>99元)", ">", true, false},
		{"reversed operands", "count <= 5", "¬(5 < count)", "<", true, false},
		{"multiline negation", "count >= 5", "¬(\ncount < 5\n)", "<", true, false},
		{"negated disjunction", "x >= 5; y >= 10", "¬(x < 5 ∨ y < 10)", "<", true, false},
		{"multiline negated disjunction", "x >= 5; y >= 10", "¬(x < 5 ∨\ny < 10)", "<", true, false},
		{"wrong comparison", "count >= 5", "¬(count > 5)", ">", false, false},
		{"wrong connective", "x >= 5; y >= 10", "¬(x < 5 ∧ y < 10)", "<", false, false},
		{"unclosed scope", "count >= 5", "¬(count < 5", "<", false, true},
	}
}

func TestDenseContractNegatedComparisonsReachModel(t *testing.T) {
	// Public propositional negation changes the meaning of the inner numeric
	// comparison. The lexical guard must defer instead of asserting a conflict.
	for _, tc := range denseNegationCases() {
		t.Run(tc.name, func(t *testing.T) {
			if issues := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(issues) > 0 {
				t.Fatalf("negated comparison was treated as its positive inner relation: %v", issues)
			}
		})
	}
}

func TestDenseContractNegationInBothValidationModes(t *testing.T) {
	// These fixtures exercise control flow and grounding, not model accuracy.
	// The reader preserves the literal inner comparison plus negation instead
	// of inventing a comparison token that is absent from its evidence.
	for _, tc := range denseNegationCases() {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(mode+" "+tc.name, func(t *testing.T) {
				quantities := []BlindQuantity{}
				for _, number := range numberOccurrences(tc.candidate) {
					quantities = append(quantities, BlindQuantity{Value: number.Value, Unit: number.Unit,
						Comparison: tc.comparison, AppliesTo: "候选所引数值条件"})
				}
				reading := BlindReading{SchemaVersion: 1, Entities: []string{}, Ambiguities: []string{},
					Relations: []BlindRelation{{Relation: "候选所引条件成立", Conditions: []string{},
						Modality: "fact", Negated: true, Quantities: quantities, Evidence: tc.candidate}}}
				if tc.ambiguous {
					reading.Ambiguities = []string{"否定括号未闭合，作用域不确定"}
				}
				var readerCalls, comparatorCalls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request compilerRequestBody
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
						t.Error("invalid validation fixture request")
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					if request.Messages[0].Content == blindReaderPrompt {
						readerCalls.Add(1)
						writeModelContent(w, blindReadingJSON(t, reading))
						return
					}
					comparatorCalls.Add(1)
					if tc.equivalent {
						writeModelContent(w, `{"pass":true,"missing":[]}`)
					} else {
						writeModelContent(w, `{"pass":false,"missing":["fixture: numeric negation or scope does not preserve the source"]}`)
					}
				}))
				defer provider.Close()
				profile := Profile{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
				var result ValidationResult
				wantReader, wantComparator := int32(0), int32(1)
				if mode == "blind" {
					wantReader = 1
					if tc.ambiguous {
						wantComparator = 0
					}
					result = runBlindValidation(profile, profile, tc.original, tc.candidate, nil)
				} else {
					result = runValidation(profile, tc.original, tc.candidate, nil)
				}
				wantStatus := "failed"
				if tc.equivalent {
					wantStatus = "passed"
				}
				if readerCalls.Load() != wantReader || comparatorCalls.Load() != wantComparator || result.Status != wantStatus {
					t.Fatalf("negation did not reach the required semantic decision: reader=%d comparator=%d result=%+v",
						readerCalls.Load(), comparatorCalls.Load(), result)
				}
			})
		}
	}
}

func TestDenseContractNegationStillProtectsValuesAndUnits(t *testing.T) {
	for _, tc := range []struct{ original, candidate string }{
		{"count >= 5", "¬(count < 6)"},
		{"count >= 5", "¬(\ncount < 6\n)"},
		{"duration <= 30 seconds", "¬(duration > 30 minutes)"},
		{"duration <= 30 seconds", "¬(\nduration > 30 minutes\n)"},
	} {
		if issues := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(issues) == 0 {
			t.Fatalf("propositional negation hid a changed number or unit: %q -> %q", tc.original, tc.candidate)
		}
	}
}

func TestDenseContractClosedNegationDoesNotHideOtherComparisonConflicts(t *testing.T) {
	for _, tc := range []struct{ original, candidate string }{
		{"¬(flag) ∧ count >= 5", "¬(flag) ∧ count < 5"},
		{"¬(flag)\ncount >= 5", "¬(flag)\ncount < 5"},
	} {
		if issues := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(issues) == 0 {
			t.Fatalf("closed unrelated negation hid a known comparison conflict: %q -> %q", tc.original, tc.candidate)
		}
	}
}

func TestDenseContractMixedFormsRemainModelDecisions(t *testing.T) {
	// The fixture always rejects. These cases prove only that compact syntax
	// reaches semantic review and a rejection cannot turn into a local pass.
	// They include both plausible equivalents and lossy/ambiguous rewrites.
	for _, tc := range []struct{ name, original, candidate string }{
		{"mixed scope", "你是售后助理。金额不超过99元时无需审批。", "role:售后助理\nif 金额<=99元: No审批"},
		{"permission and obligation", "可以退款；必须记录审计。", "may退款; must记录审计"},
		{"Chinese short permission", "允许主管审批。", "可主管审批"},
		{"exemption is not prohibition", "禁止审批。", "No审批"},
		{"permission is not automatic execution", "审核通过后可以退款。", "审核通过→退款"},
		{"cause is not condition", "因为网络超时，所以请求失败。", "if 网络超时: 请求失败"},
		{"public reason symbol", "因为网络超时，所以请求失败。", "∵网络超时, 请求失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeModelContent(w, `{"pass":false,"missing":["fixture: compare modality, conditions and causal force"]}`)
			}))
			defer provider.Close()
			profile := Profile{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
			result := runValidation(profile, tc.original, tc.candidate, nil)
			if calls.Load() != 1 || result.Method != "llm_compare" || result.Status != "failed" {
				t.Fatalf("compact/uncertain relation bypassed semantic comparison: calls=%d result=%+v", calls.Load(), result)
			}
		})
	}
}

func TestDenseContractExplicitModalConflictsStillFailLocally(t *testing.T) {
	for _, tc := range []struct{ original, candidate string }{
		{"允许审批", "must审批"},
		{"禁止审批", "may审批"},
		{"无需审批", "须审批"},
	} {
		if issues := checkDeterministicInvariants(tc.original, tc.candidate, nil); len(issues) == 0 {
			t.Fatalf("explicit modal conflict was not rejected: %q -> %q", tc.original, tc.candidate)
		}
	}
}
