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

const babelGraphSource = "你是发布协调员。在生产环境中，发布前必须检查 userId，并记录该字段的来源。在生产环境中，只有审批通过并且错误率小于5%时，才允许发布。生产环境全程禁止修改用户数据。示例命令 `printf 'A  B'` 必须原样保留。最终用中文列出检查结果。"

const babelGraphCandidate = "角色: 发布协调员\n" +
	"生产环境:\n" +
	"├发布前须: 检查 userId + 记录该字段来源\n" +
	"├允许发布仅当: 审批通过 ∧ 错误率 < 5%\n" +
	"╰全程禁止: 修改用户数据\n" +
	"须原样保留: `printf 'A  B'`\n" +
	"输出: 中文检查结果"

func babelGraphFixtureReading() BlindReading {
	const role = "角色: 发布协调员"
	const checks = role + "\n生产环境:\n├发布前须: 检查 userId + 记录该字段来源"
	const permission = checks + "\n├允许发布仅当: 审批通过 ∧ 错误率 < 5%"
	const prohibition = permission + "\n╰全程禁止: 修改用户数据"
	return BlindReading{
		SchemaVersion: 1,
		Entities:      []string{"发布协调员", "生产环境", "userId", "错误率", "用户数据"},
		Ambiguities:   []string{},
		Relations: []BlindRelation{
			{Relation: "角色", Object: "发布协调员", Conditions: []string{}, Modality: "fact", Quantities: []BlindQuantity{}, Evidence: role},
			{Subject: "发布协调员", Relation: "检查", Object: "userId", Conditions: []string{"发布前"}, Scope: "生产环境", Modality: "obligation", Quantities: []BlindQuantity{}, Evidence: checks},
			{Subject: "发布协调员", Relation: "记录来源", Object: "userId", Conditions: []string{"发布前"}, Scope: "生产环境", Modality: "obligation", Quantities: []BlindQuantity{}, Evidence: checks},
			{Subject: "发布协调员", Relation: "发布", Conditions: []string{"仅当审批通过 ∧ 错误率 < 5%"}, Scope: "生产环境", Modality: "permission",
				Quantities: []BlindQuantity{{Value: "5%", Comparison: "<", AppliesTo: "错误率"}}, Evidence: permission},
			{Subject: "发布协调员", Relation: "修改", Object: "用户数据", Conditions: []string{}, Scope: "生产环境全程", Modality: "prohibition", Quantities: []BlindQuantity{}, Evidence: prohibition},
			{Relation: "原样保留", Object: "printf 'A  B'", Conditions: []string{}, Modality: "obligation", Quantities: []BlindQuantity{}, Evidence: "须原样保留: `printf 'A  B'`"},
			{Relation: "列出", Object: "检查结果", Conditions: []string{}, Scope: "输出语言: 中文", Modality: "obligation", Quantities: []BlindQuantity{}, Evidence: "输出: 中文检查结果"},
		},
	}
}

// These local model fixtures verify HTTP routing, preservation and validation
// boundaries. They do not measure whether a real model generates this graph or
// independently judges its semantics correctly.
func TestBabelGraphCompileHTTPPreservesScopeAndLiteralGuards(t *testing.T) {
	for _, mode := range []string{"default compare", "blind"} {
		for _, tc := range []struct {
			name, before, after, issue string
		}{
			{name: "scoped graph"},
			{name: "field case changed", before: "userId", after: "UserId", issue: "技术字面量"},
			{name: "numeric boundary changed", before: "< 5%", after: "<= 5%", issue: "数值"},
			{name: "code spacing changed", before: "A  B", after: "A B", issue: "技术字面量"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				isolateDomainIntegration(t)
				candidate := babelGraphCandidate
				if tc.before != "" {
					candidate = strings.Replace(candidate, tc.before, tc.after, 1)
				}
				reading := babelGraphFixtureReading()
				readerJSON := blindReadingJSON(t, reading)
				var generations, comparisons, reads, blindComparisons atomic.Int32
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body compilerRequestBody
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 || r.Method != http.MethodPost {
						t.Errorf("invalid model request: %+v; error=%v", body, err)
						http.Error(w, "invalid model request", http.StatusBadRequest)
						return
					}
					if body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
						t.Error("model request roles changed")
					}
					switch body.Messages[0].Content {
					case verifyGatePrompt:
						comparisons.Add(1)
						var input map[string]string
						if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil || !reflect.DeepEqual(input, map[string]string{"original": babelGraphSource, "candidate": candidate}) {
							t.Errorf("comparison changed graph structure or input: %s", body.Messages[1].Content)
						}
						writeModelContent(w, `{"pass":true,"missing":[]}`)
					case blindReaderPrompt:
						reads.Add(1)
						var input map[string]string
						if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil || !reflect.DeepEqual(input, map[string]string{"candidate": candidate}) {
							t.Errorf("blind reader received altered graph or extra context: %s", body.Messages[1].Content)
						}
						if strings.Contains(body.Messages[0].Content, babelGraphSource) || strings.Contains(body.Messages[0].Content, mixedCompilerStyle) {
							t.Error("source or compiler strategy leaked to blind reader")
						}
						writeModelContent(w, readerJSON)
					case blindComparatorPrompt:
						blindComparisons.Add(1)
						var input map[string]json.RawMessage
						if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil || len(input) != 2 || input["original"] == nil || input["reading"] == nil {
							t.Errorf("blind comparator received extra/missing context: %s", body.Messages[1].Content)
						}
						var original string
						var extracted BlindReading
						if err := json.Unmarshal(input["original"], &original); err != nil || original != babelGraphSource {
							t.Error("blind comparator lost original")
						}
						if err := json.Unmarshal(input["reading"], &extracted); err != nil || !reflect.DeepEqual(extracted, reading) {
							t.Errorf("parent-to-child evidence changed before comparison: %+v", extracted)
						}
						writeModelContent(w, `{"pass":true,"missing":[]}`)
					default:
						generations.Add(1)
						input := decodeCompilerInput(t, body.Messages[1].Content)
						if !strings.HasPrefix(body.Messages[0].Content, commonCompilerRules) || !strings.Contains(body.Messages[0].Content, mixedCompilerStyle) || input.Original != babelGraphSource || input.Candidate != "" {
							t.Error("default compile API omitted the fixed graph strategy or changed source")
						}
						writeModelContent(w, candidate)
					}
				}))
				defer model.Close()
				server := httptest.NewServer(newServerMux())
				defer server.Close()
				request := CompileReq{
					Text: babelGraphSource, StrictGate: true, NoCache: true,
					APIKey: "graph-fixture", BaseURL: model.URL, Model: "graph-fixture",
				}
				if mode == "blind" {
					request.ValidationMode = "blind"
				}
				encoded, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				response, err := server.Client().Post(server.URL+"/api/compile", "application/json", bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				var result CompileResp
				if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
					t.Fatal(err)
				}
				if generations.Load() != 1 {
					t.Fatalf("generation calls=%d; want exactly one", generations.Load())
				}
				if tc.issue != "" {
					if response.StatusCode != http.StatusUnprocessableEntity || result.Validation.Status != "failed" || !strings.Contains(strings.Join(result.Validation.Issues, "; "), tc.issue) {
						t.Fatalf("literal/constraint mutation not rejected: status=%d validation=%+v", response.StatusCode, result.Validation)
					}
					if comparisons.Load() != 0 || reads.Load() != 0 || blindComparisons.Load() != 0 {
						t.Fatalf("local rejection reached model validation: compare=%d reader=%d comparator=%d", comparisons.Load(), reads.Load(), blindComparisons.Load())
					}
					return
				}
				if response.StatusCode != http.StatusOK || result.Compiled != candidate || result.Dialect != defaultDialectName || result.Validation.Status != "passed" {
					t.Fatalf("graph did not survive compilation and validation: status=%d result=%+v", response.StatusCode, result)
				}
				if mode == "blind" {
					if comparisons.Load() != 0 || reads.Load() != 1 || blindComparisons.Load() != 1 || result.Validation.Method != "blind_read" || len(result.Validation.Stages) != 3 {
						t.Fatalf("unexpected blind stages/calls: %+v; compare=%d reader=%d comparator=%d", result.Validation, comparisons.Load(), reads.Load(), blindComparisons.Load())
					}
					for i, status := range []string{"completed", "completed", "passed"} {
						if result.Validation.Stages[i].Status != status {
							t.Errorf("stage %d: %+v; want %s", i, result.Validation.Stages[i], status)
						}
					}
					if !reflect.DeepEqual(result.Validation.Stages[1].Reading, &reading) {
						t.Error("compile response lost grounded shared context")
					}
				} else if comparisons.Load() != 1 || reads.Load() != 0 || blindComparisons.Load() != 0 || result.Validation.Method != "llm_compare" {
					t.Fatalf("default comparison stages/calls changed: %+v; compare=%d reader=%d comparator=%d", result.Validation, comparisons.Load(), reads.Load(), blindComparisons.Load())
				}
			})
		}
	}
}
