package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const quotedRefundSource = "可自助受理：「已签收未超过 7 天」且商品未拆封；参与「满 3 件 8 折」且已拆单的订单不支持自助退款。"
const quotedRefundCandidate = "自助许可: 签收≤7天∧未拆封; 参与(满3件8折)∧已拆单→禁止自助退款"

func quotedRefundReading(candidate string) BlindReading {
	clauses := strings.SplitN(candidate, "; ", 2)
	return BlindReading{
		SchemaVersion: 1, Entities: []string{"自助退款"}, Ambiguities: []string{},
		Relations: []BlindRelation{
			{Relation: "自助退款", Conditions: []string{strings.TrimPrefix(clauses[0], "自助许可: ")}, Modality: "permission",
				Quantities: []BlindQuantity{{Value: "7", Unit: "天", Comparison: "≤", AppliesTo: "签收时间"}}, Evidence: clauses[0]},
			{Relation: "自助退款", Conditions: []string{"参与满3件8折", "已拆单"}, Modality: "prohibition",
				Quantities: []BlindQuantity{{Value: "3", Unit: "件", AppliesTo: "活动门槛"}, {Value: "8", Unit: "折", AppliesTo: "活动折扣"}}, Evidence: clauses[1]},
		},
	}
}

// Model responses below are fixtures for validation routing and fail-closed
// behavior. They do not measure a model's actual semantic understanding.
func runQuotedSemanticFixture(t *testing.T, mode, original, candidate string, reading BlindReading, pass bool) {
	t.Helper()
	var readerCalls, comparisons atomic.Int32
	readerJSON := blindReadingJSON(t, reading)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Error("invalid validation request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch body.Messages[0].Content {
		case blindReaderPrompt:
			readerCalls.Add(1)
			var input map[string]string
			if json.Unmarshal([]byte(body.Messages[1].Content), &input) != nil || !reflect.DeepEqual(input, map[string]string{"candidate": candidate}) {
				t.Error("blind reader did not receive only the exact rewritten candidate")
			}
			writeModelContent(w, readerJSON)
			return
		case verifyGatePrompt:
			comparisons.Add(1)
			var input map[string]string
			if json.Unmarshal([]byte(body.Messages[1].Content), &input) != nil || !reflect.DeepEqual(input, map[string]string{"original": original, "candidate": candidate}) {
				t.Error("comparison lost original or modified the rewritten candidate")
			}
		case blindComparatorPrompt:
			comparisons.Add(1)
			var input map[string]json.RawMessage
			if json.Unmarshal([]byte(body.Messages[1].Content), &input) != nil || len(input) != 2 || input["original"] == nil || input["reading"] == nil {
				t.Error("blind comparison did not receive exactly original and reading")
			}
			var gotOriginal string
			var gotReading BlindReading
			if json.Unmarshal(input["original"], &gotOriginal) != nil || gotOriginal != original || json.Unmarshal(input["reading"], &gotReading) != nil || !reflect.DeepEqual(gotReading, reading) {
				t.Error("blind comparison changed the original or independent reading")
			}
		default:
			t.Error("unexpected model request during validation")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if pass {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		} else {
			writeModelContent(w, `{"pass":false,"missing":["候选把自助受理的共同条件改为任选其一"]}`)
		}
	}))
	defer provider.Close()
	profile := Profile{APIKey: "quoted-fixture", BaseURL: provider.URL, Model: "quoted-fixture"}
	var result ValidationResult
	wantReader := int32(0)
	if mode == "blind" {
		wantReader = 1
		result = runBlindValidation(profile, profile, original, candidate, nil)
	} else {
		result = runValidation(profile, original, candidate, nil)
	}
	wantStatus := "failed"
	if pass {
		wantStatus = "passed"
	}
	if result.Status != wantStatus || readerCalls.Load() != wantReader || comparisons.Load() != 1 {
		t.Fatalf("quoted prose bypassed or failed to reach semantic validation: reader=%d comparator=%d result=%+v", readerCalls.Load(), comparisons.Load(), result)
	}
	if !pass && !strings.Contains(strings.Join(result.Issues, "; "), "共同条件") {
		t.Fatalf("model rejection was replaced by another failure: %+v", result)
	}
}

func TestQuotedNaturalLanguageRewritesReachBothValidationModes(t *testing.T) {
	const whitespaceSource = "参与「满 3 件 8 折」且已拆单的订单不支持自助退款。"
	const whitespaceCandidate = "参与「满3件8折」∧已拆单→禁止自助退款"
	const narrativeCandidate = "记录方式: 优先可追溯∧易复核"
	whitespaceReading := BlindReading{
		SchemaVersion: 1, Entities: []string{"自助退款"}, Ambiguities: []string{},
		Relations: []BlindRelation{{Relation: "自助退款", Conditions: []string{"参与满3件8折", "已拆单"}, Modality: "prohibition",
			Quantities: []BlindQuantity{{Value: "3", Unit: "件", AppliesTo: "活动门槛"}, {Value: "8", Unit: "折", AppliesTo: "活动折扣"}}, Evidence: whitespaceCandidate}},
	}
	narrativeReading := BlindReading{
		SchemaVersion: 1, Entities: []string{"记录方式"}, Ambiguities: []string{},
		Relations: []BlindRelation{{Relation: "优先考虑", Object: "可追溯且易于复核的记录方式", Conditions: []string{}, Modality: "recommendation", Quantities: []BlindQuantity{}, Evidence: narrativeCandidate}},
	}
	for _, test := range []struct {
		name, original, candidate string
		reading                   BlindReading
	}{
		{"refund conditions", quotedRefundSource, quotedRefundCandidate, quotedRefundReading(quotedRefundCandidate)},
		{"quoted prose whitespace", whitespaceSource, whitespaceCandidate, whitespaceReading},
		{"corner prose", "请优先考虑「可追溯并且易于复核」的记录方式。", narrativeCandidate, narrativeReading},
		{"curly prose", "请优先考虑“可追溯并且易于复核”的记录方式。", narrativeCandidate, narrativeReading},
		{"double corner prose", "请优先考虑『可追溯并且易于复核』的记录方式。", narrativeCandidate, narrativeReading},
	} {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				runQuotedSemanticFixture(t, mode, test.original, test.candidate, test.reading, true)
			})
		}
	}
}

func TestQuotedConditionRewriteStillRespectsSemanticRejection(t *testing.T) {
	broken := strings.Replace(quotedRefundCandidate, "∧", "∨", 1)
	for _, mode := range []string{"compare", "blind"} {
		t.Run(mode, func(t *testing.T) {
			runQuotedSemanticFixture(t, mode, quotedRefundSource, broken, quotedRefundReading(broken), false)
		})
	}
}

func TestExplicitQuotedLiteralsRemainProtectedBeforeModelCalls(t *testing.T) {
	for _, test := range []struct{ name, original, candidate string }{
		{"explicit exact condition", "字符串必须逐字保留为「已签收未超过 7 天」。", "字符串: 「已签收≤7天」。"},
		{"explicit exact spacing", "输出字符串需原样保留为「满 3 件 8 折」。", "输出字符串: 「满3件8折」。"},
		{"inline code", "保留代码 `已签收未超过 7 天`。", "代码: `已签收≤7天`。"},
		{"protocol string", `JSON 字段 status 必须返回 "PENDING"。`, `JSON 字段 status 必须返回 "DONE"。`},
	} {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					writeModelContent(w, `{"pass":true,"missing":[]}`)
				}))
				defer provider.Close()
				profile := Profile{APIKey: "quoted-fixture", BaseURL: provider.URL, Model: "quoted-fixture"}
				var result ValidationResult
				if mode == "blind" {
					result = runBlindValidation(profile, profile, test.original, test.candidate, nil)
				} else {
					result = runValidation(profile, test.original, test.candidate, nil)
				}
				if result.Status != "failed" || calls.Load() != 0 || !strings.Contains(strings.Join(result.Issues, "; "), "技术字面量") {
					t.Fatalf("explicit literal damage reached a model or passed: calls=%d result=%+v", calls.Load(), result)
				}
			})
		}
	}
}

func TestQuotedLiteralContextDoesNotFreezeIndependentProse(t *testing.T) {
	for _, test := range []struct{ name, original, candidate string }{
		{"English sentence boundary", `Keep "READY" verbatim. Prefer "traceable and easy to review" records.`, `Keep "READY" verbatim; records: prioritize traceability∧reviewability.`},
		{"Chinese clause boundary", "原样保留「READY」，请优先考虑「可追溯并且易于复核」的记录方式。", "原样保留「READY」; 记录方式: 优先可追溯∧易复核"},
		{"identifier inside prose", "请考虑「检查 user_id 后再更新记录」的顺序。", "先校验 user_id→再更新记录"},
		{"French prose", `Préférer "traçables et faciles à vérifier" pour les écritures.`, "Écritures: priorité traçabilité∧vérifiabilité."},
		{"short English prose", `Prefer "clear" records.`, "Records: prioritize clarity."},
		{"decorative fixed output quotes", "渠道超时则返回「处理中」。", `渠道超时→返回"处理中"。`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if issues := checkLiteralInvariants(test.original, test.candidate); len(issues) != 0 {
				t.Fatalf("independent prose or decorative quotation marks were treated as literal content: %v", issues)
			}
		})
	}
}

func TestQuotedStructuredValuesAndEmbeddedIdentifiersRemainProtected(t *testing.T) {
	for _, test := range []struct{ name, original, candidate string }{
		{"exact wording suffix", `Keep "READY" exactly as written.`, `Keep "DONE" exactly as written.`},
		{"JSON object value", `JSON: {"state":"PENDING"}`, `JSON: {"state":"DONE"}`},
		{"JSON array after null", `JSON: ["READY", null, "DONE"]`, `JSON: ["READY", null, "FAIL"]`},
		{"assignment", `status = "PENDING"`, `status = "DONE"`},
		{"fixed output", "渠道超时则返回「处理中」。", "渠道超时→返回「已完成」。"},
		{"identifier inside prose", "请考虑「检查 user_id 后再更新记录」的顺序。", "先校验 user_ids→再更新记录"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if issues := checkLiteralInvariants(test.original, test.candidate); len(issues) == 0 {
				t.Fatal("changed structured string or embedded technical identifier escaped local protection")
			}
		})
	}
}
