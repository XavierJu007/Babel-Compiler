package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestNumericGuardThresholdGraphReordering(t *testing.T) {
	// The zh-threshold-range evaluation fixture uses a left numeric operand for
	// the lower bound and a right numeric operand for the upper bound.
	const source = "报销金额大于或等于5000元且小于10000元时，必须由部门主管审批；金额等于10000元时由财务总监审批。"
	const candidate = "5000 元 <= 报销金额 < 10000 元：须部门主管审批；报销金额 = 10000 元：须财务总监审批。"
	if failures := checkDeterministicInvariants(source, candidate, nil); len(failures) != 0 {
		t.Fatalf("ordinary graph reordering rejected: %v\nsource numbers: %+v\ncandidate numbers: %+v", failures, numberOccurrences(source), numberOccurrences(candidate))
	}
	for _, mutation := range []string{
		"5000 元 < 报销金额 < 10000 元：须部门主管审批；金额 = 10000 元：须财务总监审批。",
		"5000 千元 <= 报销金额 < 10000 千元：须部门主管审批；金额 = 10000 千元：须财务总监审批。",
	} {
		if failures := checkDeterministicInvariants(source, mutation, nil); len(failures) == 0 {
			t.Fatalf("known boundary/unit mutation accepted locally: %s", mutation)
		}
	}
}

func TestNumericGuardPublicOperandOrders(t *testing.T) {
	for _, tc := range []struct{ source, candidate string }{
		{"count >= 5", "5 <= count"},
		{"count ≥ 5", "5 ≤ count"},
		{"5 < count", "count > 5"},
		{"count <= -5", "-5 >= count"},
		{"count > +5", "+5 < count"},
		{"count != 5", "5 != count"},
		{"duration <= 30 seconds", "30 seconds >= duration"},
		{"amount is greater than or equal to 5", "5 <= amount"},
		{"amount is less than or equal to 5", "5 >= amount"},
		{"-5 <= count < +5", "+5 > count >= -5"},
		// Unit scope and operator phrasing are not determined by this lexer.
		{"超时 30 秒", "超时（秒）: 30"},
		{"count >= 5", "count 的闭区间下界：5"},
		{"count >= 5", "闭区间下界：5 -> count"},
		{"count is not less than 5", "count >= 5"},
		{"金额不能大于5000元", "金额 <= 5000 元"},
		{"not (5 < x)", "x <= 5"},
	} {
		t.Run(tc.source+" to "+tc.candidate, func(t *testing.T) {
			if failures := checkDeterministicInvariants(tc.source, tc.candidate, nil); len(failures) != 0 {
				t.Fatalf("reordered/unknown relationship must reach model review: %v", failures)
			}
		})
	}
}

func TestNumericGuardRetainsExactAdjacentValues(t *testing.T) {
	got := numberOccurrences("5,5,50 -5,+5,-5")
	want := []numberOccurrence{{Value: "5"}, {Value: "5"}, {Value: "50"}, {Value: "-5"}, {Value: "+5"}, {Value: "-5"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	for _, tc := range []struct{ source, candidate string }{
		{"Wait 5 seconds", "Wait 50 seconds"},
		{"states: 5,5,50", "states: 5,5"},
		{"offset >= -5", "-5 > offset"},
		{"offset >= -5", "offset >= 5"},
		{"count >= 5", "5 < count"},
		{"duration <= 30 seconds", "30 minutes >= duration"},
		{"x >= 5; y < 5", "5 <= x; 5 <= y"},
	} {
		t.Run(tc.source+" to "+tc.candidate, func(t *testing.T) {
			if failures := checkDeterministicInvariants(tc.source, tc.candidate, nil); len(failures) == 0 {
				t.Fatal("known numeric mutation was not rejected")
			}
		})
	}
}

func TestNumericGuardUnknownIsStillCompared(t *testing.T) {
	var calls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, `{"pass":false,"missing":["模型仍须检查数值绑定及条件范围"]}`)
	}))
	defer service.Close()
	p := Profile{APIKey: "test", BaseURL: service.URL, Model: "test-model"}
	result := runValidation(p, "count >= 5", "count 的闭区间下界：5", nil)
	if calls.Load() != 1 || result.Method != "llm_compare" || result.Status != "failed" {
		t.Fatalf("unknown numeric relation must reach the model, never pass locally: calls=%d result=%+v", calls.Load(), result)
	}
}

func TestNumericGuardReorderingReachesBlindReaderAndComparator(t *testing.T) {
	const source = "报销金额大于或等于5000元且小于10000元时，必须由部门主管审批；金额等于10000元时由财务总监审批。"
	const lowerRange = "5000 元 <= 报销金额 < 10000 元：须部门主管审批"
	const equality = "报销金额 = 10000 元：须财务总监审批。"
	const candidate = lowerRange + "；" + equality
	reading := BlindReading{
		SchemaVersion: 1, Entities: []string{"报销金额", "部门主管", "财务总监"}, Ambiguities: []string{},
		Relations: []BlindRelation{
			{Subject: "部门主管", Relation: "审批", Object: "报销金额", Conditions: []string{"5000 元 <= 报销金额 < 10000 元"}, Modality: "obligation", Evidence: lowerRange,
				Quantities: []BlindQuantity{{Value: "5000", Unit: "元", Comparison: "<=", AppliesTo: "报销金额"}, {Value: "10000", Unit: "元", Comparison: "<", AppliesTo: "报销金额"}}},
			{Subject: "财务总监", Relation: "审批", Object: "报销金额", Conditions: []string{"报销金额 = 10000 元"}, Modality: "obligation", Evidence: equality,
				Quantities: []BlindQuantity{{Value: "10000", Unit: "元", Comparison: "=", AppliesTo: "报销金额"}}},
		},
	}
	var readerCalls, comparatorCalls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Error("invalid validation request")
			http.Error(w, "invalid request", 400)
			return
		}
		switch body.Messages[0].Content {
		case blindReaderPrompt:
			readerCalls.Add(1)
			writeModelContent(w, blindReadingJSON(t, reading))
		case blindComparatorPrompt:
			comparatorCalls.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		default:
			t.Error("unexpected validation stage")
			http.Error(w, "unexpected stage", 400)
		}
	}))
	defer service.Close()
	p := Profile{APIKey: "test", BaseURL: service.URL, Model: "test-model"}
	result := runBlindValidation(p, p, source, candidate, nil)
	if readerCalls.Load() != 1 || comparatorCalls.Load() != 1 || result.Status != "passed" {
		t.Fatalf("numeric helper broke blind grounding: reader=%d comparator=%d result=%+v", readerCalls.Load(), comparatorCalls.Load(), result)
	}
}

func TestNumericGuardRateUnits(t *testing.T) {
	source := "已知的系统限流阈值为每秒 20 次请求，超过按指数退避，最长等待 120 秒。"
	for _, equiv := range []string{
		"限流阈值20req/s，超限指数退避≤120s。",
		"限流阈值 20 qps，超限指数退避<=120秒。",
		"限流阈值 20次/s，超限指数退避<=120s。",
		"限流阈值 20次/秒，超限指数退避<=120s。",
	} {
		if failures := checkDeterministicInvariants(source, equiv, nil); len(failures) != 0 {
			t.Fatalf("equivalent rate expression rejected: %q -> %v\nsource: %+v\ncand: %+v",
				equiv, failures, numberOccurrences(source), numberOccurrences(equiv))
		}
	}
	for _, mutated := range []string{
		"限流阈值10req/s，超限指数退避≤120s。",
		"限流阈值20分钟，超限指数退避≤120s。",
	} {
		if failures := checkDeterministicInvariants(source, mutated, nil); len(failures) == 0 {
			t.Fatalf("mutated rate/unit unexpectedly passed: %q", mutated)
		}
	}
}

