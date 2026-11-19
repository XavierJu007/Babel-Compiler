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

func blindFixtureReading() BlindReading {
	return BlindReading{
		SchemaVersion: 1,
		Entities:      []string{"系统", "日志"},
		Relations: []BlindRelation{{
			Subject: "系统", Relation: "删除", Object: "日志", Conditions: []string{},
			Modality: "permission", Quantities: []BlindQuantity{}, Evidence: "系统允许删除日志。",
		}},
		Ambiguities: []string{},
	}
}

func blindReadingJSON(t *testing.T, reading BlindReading) string {
	t.Helper()
	data, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBlindRequestsKeepReaderIndependent(t *testing.T) {
	const original = "系统可以删除日志。"
	const candidate = "系统允许删除日志。"
	reading := blindFixtureReading()
	var readerCalls, comparatorCalls atomic.Int32
	readerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readerCalls.Add(1)
		var body struct {
			Model    string `json:"model"`
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		if body.Model != "reader-model" || len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[0].Content != blindReaderPrompt || body.Messages[1].Role != "user" {
			t.Errorf("reader request is not a fresh neutral request: %+v", body)
			http.Error(w, "invalid reader messages", 400)
			return
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &payload); err != nil || !reflect.DeepEqual(payload, map[string]string{"candidate": candidate}) {
			t.Errorf("reader received extra context: %s", body.Messages[1].Content)
		}
		if strings.Contains(body.Messages[0].Content, original) || strings.Contains(body.Messages[1].Content, original) {
			t.Error("original leaked into reader request")
		}
		writeModelContent(w, blindReadingJSON(t, reading))
	}))
	defer readerServer.Close()
	comparatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		comparatorCalls.Add(1)
		var body struct {
			Model    string `json:"model"`
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		if body.Model != "comparator-model" || len(body.Messages) != 2 || body.Messages[0].Content != blindComparatorPrompt {
			t.Errorf("comparator request: %+v", body)
			http.Error(w, "invalid comparator messages", 400)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body.Messages[1].Content), &payload); err != nil || len(payload) != 2 || payload["original"] == nil || payload["reading"] == nil {
			t.Errorf("comparator must only receive original + reading: %s", body.Messages[1].Content)
		}
		var gotOriginal string
		var gotReading BlindReading
		_ = json.Unmarshal(payload["original"], &gotOriginal)
		_ = json.Unmarshal(payload["reading"], &gotReading)
		if gotOriginal != original || !reflect.DeepEqual(gotReading, reading) {
			t.Errorf("comparator lost the independent reading: %s", body.Messages[1].Content)
		}
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer comparatorServer.Close()

	reader := Profile{APIKey: "reader-key", BaseURL: readerServer.URL, Model: "reader-model"}
	comparator := Profile{APIKey: "comparator-key", BaseURL: comparatorServer.URL, Model: "comparator-model"}
	result := runBlindValidation(reader, comparator, original, candidate, []string{"日志"})
	if result.Status != "passed" || result.Method != "blind_read" || result.PolicyVersion != "semantic-blind-v1" || len(result.Issues) != 0 {
		t.Fatalf("result=%+v", result)
	}
	if readerCalls.Load() != 1 || comparatorCalls.Load() != 1 || len(result.Stages) != 3 {
		t.Fatalf("reader=%d comparator=%d stages=%+v", readerCalls.Load(), comparatorCalls.Load(), result.Stages)
	}
	for i, want := range []string{"completed", "completed", "passed"} {
		if result.Stages[i].Status != want || result.Stages[i].Issues == nil {
			t.Errorf("stage %d: %+v", i, result.Stages[i])
		}
	}
	if result.Stages[1].Model != "reader-model" || result.Stages[2].Model != "comparator-model" || !reflect.DeepEqual(result.Stages[1].Reading, &reading) {
		t.Fatalf("missing reader/comparator evidence: %+v", result.Stages)
	}
}

func TestBlindReadingStrictSchema(t *testing.T) {
	raw := blindReadingJSON(t, blindFixtureReading())
	if _, err := parseBlindReading(" \n" + raw + "\n "); err != nil {
		t.Fatalf("valid reading rejected: %v", err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"plain text", "NOT PASS"},
		{"null object", "null"},
		{"missing version", strings.Replace(raw, `"schema_version":1,`, "", 1)},
		{"unsupported version", strings.Replace(raw, `"schema_version":1`, `"schema_version":2`, 1)},
		{"float version", strings.Replace(raw, `"schema_version":1`, `"schema_version":1.0`, 1)},
		{"null entities", strings.Replace(raw, `"entities":["系统","日志"]`, `"entities":null`, 1)},
		{"null entity", strings.Replace(raw, `"entities":["系统","日志"]`, `"entities":[null]`, 1)},
		{"null relations", `{"schema_version":1,"entities":[],"relations":null,"ambiguities":[]}`},
		{"null relation", `{"schema_version":1,"entities":[],"relations":[null],"ambiguities":[]}`},
		{"null ambiguity", strings.Replace(raw, `"ambiguities":[]`, `"ambiguities":[null]`, 1)},
		{"null subject", strings.Replace(raw, `"subject":"系统"`, `"subject":null`, 1)},
		{"missing boolean", strings.Replace(raw, `"negated":false,`, "", 1)},
		{"string boolean", strings.Replace(raw, `"negated":false`, `"negated":"false"`, 1)},
		{"null boolean", strings.Replace(raw, `"negated":false`, `"negated":null`, 1)},
		{"null quantities", strings.Replace(raw, `"quantities":[]`, `"quantities":null`, 1)},
		{"empty evidence", strings.Replace(raw, `"evidence":"系统允许删除日志。"`, `"evidence":""`, 1)},
		{"unknown modality", strings.Replace(raw, `"modality":"permission"`, `"modality":"PASS"`, 1)},
		{"duplicate root", strings.Replace(raw, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)},
		{"duplicate nested", strings.Replace(raw, `"modality":"permission"`, `"modality":"prohibition","modality":"permission"`, 1)},
		{"unknown nested", strings.Replace(raw, `"modality":"permission"`, `"modality":"permission","private_map":{}`, 1)},
		{"case folded key", strings.Replace(raw, `"modality":`, `"Modality":`, 1)},
		{"misplaced known key", strings.Replace(raw, `"object":"日志"`, `"entities":["日志"]`, 1)},
		{"trailing text", raw + " NOT PASS"},
		{"second object", raw + raw},
		{"fenced", "```json\n" + raw + "\n```"},
		{"over limit", strings.Repeat(" ", 256*1024) + raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBlindReading(tc.raw); err == nil {
				t.Fatalf("malformed extraction accepted: %s", tc.raw)
			}
		})
	}

	quantified := blindFixtureReading()
	quantified.Relations[0].Quantities = []BlindQuantity{{Value: "30", Unit: "秒", Comparison: "至多", AppliesTo: "等待时间"}}
	quantityJSON := blindReadingJSON(t, quantified)
	for _, mutation := range []string{
		strings.Replace(quantityJSON, `"value":"30"`, `"value":30`, 1),
		strings.Replace(quantityJSON, `"unit":"秒",`, "", 1),
		strings.Replace(quantityJSON, `"comparison":"至多"`, `"comparison":null`, 1),
		strings.Replace(quantityJSON, `"applies_to":"等待时间"`, `"applies_to":""`, 1),
		strings.Replace(quantityJSON, `"value":"30"`, `"value":"30","value":"40"`, 1),
	} {
		if _, err := parseBlindReading(mutation); err == nil {
			t.Fatalf("malformed quantity accepted: %s", mutation)
		}
	}
}

func TestBlindReaderFailuresNeverReachComparator(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, want, issue string
		mutate                       func(*BlindReading)
		raw                          string
		status                       int
		finishReason                 string
	}{
		{name: "ambiguity", want: "failed", issue: "未决歧义", mutate: func(r *BlindReading) { r.Ambiguities = []string{"主体所指不明"} }},
		{name: "empty reading", want: "failed", issue: "未提取", mutate: func(r *BlindReading) { r.Relations = []BlindRelation{} }},
		{name: "invented entity", want: "failed", issue: "实体名称", mutate: func(r *BlindReading) { r.Entities = []string{"A17 隐藏含义"} }},
		{name: "invented evidence", want: "failed", issue: "引文不在", mutate: func(r *BlindReading) { r.Relations[0].Evidence = "系统禁止删除日志。" }},
		{name: "hallucinated prohibition", want: "failed", issue: "模态/否定", mutate: func(r *BlindReading) { r.Relations[0].Modality = "prohibition" }},
		{name: "factual negation removed", candidate: "系统尚未删除日志。", want: "failed", issue: "模态/否定", mutate: func(r *BlindReading) {
			r.Relations[0].Modality = "fact"
			r.Relations[0].Evidence = "系统尚未删除日志。"
		}},
		{name: "unknown modality", want: "failed", issue: "尚未确定", mutate: func(r *BlindReading) { r.Relations[0].Modality = "unknown" }},
		{name: "numeric substring hallucination", candidate: "超时 50 秒", want: "failed", issue: "数值", mutate: func(r *BlindReading) {
			r.Entities = []string{}
			r.Relations = []BlindRelation{{Subject: "", Relation: "超时", Object: "", Conditions: []string{}, Scope: "", Modality: "fact", Quantities: []BlindQuantity{{Value: "5", Unit: "秒", AppliesTo: "超时"}}, Evidence: "超时 50 秒"}}
		}},
		{name: "bad JSON", raw: "NOT PASS", want: "error", issue: "格式错误"},
		{name: "service failure", status: 503, raw: "temporarily unavailable", want: "error", issue: "服务异常"},
		{name: "truncated completion", finishReason: "length", want: "error", issue: "未完整结束"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := tc.candidate
			if candidate == "" {
				candidate = "系统允许删除日志。"
			}
			reading := blindFixtureReading()
			if tc.mutate != nil {
				tc.mutate(&reading)
			}
			raw := tc.raw
			if raw == "" {
				raw = blindReadingJSON(t, reading)
			}
			var readerCalls, comparatorCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				_ = json.NewDecoder(r.Body).Decode(&body)
				if len(body.Messages) == 2 && body.Messages[0].Content == blindReaderPrompt {
					readerCalls.Add(1)
					if tc.status != 0 {
						http.Error(w, raw, tc.status)
						return
					}
					if tc.finishReason != "" {
						_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": tc.finishReason, "message": map[string]string{"content": raw}}}})
						return
					}
					writeModelContent(w, raw)
					return
				}
				comparatorCalls.Add(1)
				writeModelContent(w, `{"pass":true,"missing":[]}`)
			}))
			defer server.Close()
			p := Profile{APIKey: "test-key", BaseURL: server.URL, Model: "test-model"}
			result := runBlindValidation(p, p, candidate, candidate, nil)
			if result.Status != tc.want || readerCalls.Load() != 1 || comparatorCalls.Load() != 0 || len(result.Stages) != 3 || result.Stages[1].Status != tc.want || result.Stages[2].Status != "not_run" || !strings.Contains(strings.Join(result.Issues, " "), tc.issue) {
				t.Fatalf("reader=%d comparator=%d result=%+v", readerCalls.Load(), comparatorCalls.Load(), result)
			}
		})
	}
}

func TestBlindComparatorRejectsFailuresAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		status          int
	}{
		{"explicit rejection", `{"pass":false,"missing":["遗漏条件"]}`, "failed", 0},
		{"inconsistent approval", `{"pass":true,"missing":["原文的禁止条件未保留"]}`, "failed", 0},
		{"unexplained rejection", `{"pass":false,"missing":[]}`, "failed", 0},
		{"plain NOT PASS", "NOT PASS", "error", 0},
		{"missing pass", `{"missing":[]}`, "error", 0},
		{"duplicate pass", `{"pass":false,"pass":true,"missing":[]}`, "error", 0},
		{"upstream failure", "temporarily unavailable", "error", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var comparatorCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				_ = json.NewDecoder(r.Body).Decode(&body)
				if len(body.Messages) == 2 && body.Messages[0].Content == blindReaderPrompt {
					writeModelContent(w, blindReadingJSON(t, blindFixtureReading()))
					return
				}
				comparatorCalls.Add(1)
				if tc.status != 0 {
					http.Error(w, tc.raw, tc.status)
					return
				}
				writeModelContent(w, tc.raw)
			}))
			defer server.Close()
			p := Profile{APIKey: "test-key", BaseURL: server.URL, Model: "test-model"}
			result := runBlindValidation(p, p, "系统可以删除日志。", "系统允许删除日志。", nil)
			if result.Status != tc.want || comparatorCalls.Load() != 1 || result.Stages[1].Status != "completed" || result.Stages[2].Status != tc.want || len(result.Issues) == 0 {
				t.Fatalf("comparator=%d result=%+v", comparatorCalls.Load(), result)
			}
		})
	}
}

func TestBlindLocalFailuresSkipBothRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer server.Close()
	p := Profile{APIKey: "test-key", BaseURL: server.URL, Model: "test-model"}
	for _, tc := range []struct {
		original, candidate string
		required            []string
	}{
		{"超时 5 秒", "超时 50 秒", nil},
		{"返回 JSON", "返回文本", []string{"JSON"}},
		{"允许删除日志", "必须删除日志", nil},
		{"原文", "", nil},
	} {
		result := runBlindValidation(p, p, tc.original, tc.candidate, tc.required)
		if result.Status != "failed" || result.Method != "blind_read" || result.Stages[0].Status != "failed" || result.Stages[1].Status != "not_run" || result.Stages[2].Status != "not_run" {
			t.Errorf("result=%+v", result)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("locally rejected input used %d model calls", calls.Load())
	}
}

func TestBlindReorderedConditionStillReachesComparator(t *testing.T) {
	const original = "必须在请求成功后保存审计记录。"
	const candidate = "请求成功后: 须保存审计记录。"
	reading := BlindReading{SchemaVersion: 1, Entities: []string{"请求", "审计记录"}, Relations: []BlindRelation{{
		Relation: "保存", Object: "审计记录", Conditions: []string{"请求成功后"}, Modality: "obligation", Quantities: []BlindQuantity{}, Evidence: candidate,
	}}, Ambiguities: []string{}}
	var comparatorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) == 2 && body.Messages[0].Content == blindReaderPrompt {
			writeModelContent(w, blindReadingJSON(t, reading))
			return
		}
		comparatorCalls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer server.Close()
	p := Profile{APIKey: "test-key", BaseURL: server.URL, Model: "test-model"}
	result := runBlindValidation(p, p, original, candidate, nil)
	if result.Status != "passed" || comparatorCalls.Load() != 1 {
		t.Fatalf("valid reordering was blocked: %+v; comparator=%d", result, comparatorCalls.Load())
	}
}

func TestBlindCapabilityIsNotAutomaticallyReadAsProhibition(t *testing.T) {
	const candidate = "The system cannot delete logs."
	reading := BlindReading{SchemaVersion: 1, Entities: []string{"system", "logs"}, Relations: []BlindRelation{{
		Subject: "system", Relation: "delete", Object: "logs", Conditions: []string{}, Modality: "capability", Negated: true, Quantities: []BlindQuantity{}, Evidence: candidate,
	}}, Ambiguities: []string{}}
	if issues := checkBlindReadingGrounding(reading, candidate); len(issues) > 0 {
		t.Fatalf("capability must be reviewed in context, not rejected as a definite prohibition conflict: %v", issues)
	}
}
