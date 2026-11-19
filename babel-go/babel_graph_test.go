package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	fixtures "github.com/XavierJu007/Babel-Compiler/eval"
)

// These handwritten candidates exercise compiler plumbing and local guards.
// They do not claim that a live model generated or understood the notation.
func TestBabelGraphEvaluationCandidatesSurviveLocalChecks(t *testing.T) {
	decoder := json.NewDecoder(bytes.NewReader(fixtures.Cases))
	checked := 0
	for {
		var row struct {
			ID             string   `json:"id"`
			Tags           []string `json:"tags"`
			Source         string   `json:"source"`
			ValidCandidate string   `json:"valid_candidate"`
		}
		if err := decoder.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(row.Tags, "babel-graph") {
			continue
		}
		checked++
		t.Run(row.ID, func(t *testing.T) {
			if issues := checkDeterministicInvariants(row.Source, row.ValidCandidate, nil); len(issues) != 0 {
				t.Fatalf("faithful graph candidate blocked before semantic comparison: %v", issues)
			}
			if got := compactDSL(row.ValidCandidate); got != row.ValidCandidate {
				t.Fatal("postprocessing changed graph structure or literal content")
			}
		})
	}
	if checked == 0 {
		t.Fatal("no graph evaluation fixtures exercised")
	}
}

const babelWorkflowSource = "你是发布审查员，负责检查订单服务的部署计划。请先核对 HTTP API 契约：POST /api/v1/orders 必须携带 Idempotency-Key；金额 amount 必须大于或等于0元，相同 Idempotency-Key 的重放必须返回同一订单。随后检查任务依赖：fetch_users 和 fetch_invoices 可以并行执行，只有两项都成功后才能启动 build_report；任意一项失败都禁止启动 build_report。最后核对发布与证据：只有系统负责人书面批准且回滚演练成功后，运维人员才允许发布；未经批准禁止写生产数据。每条结论都必须附可重复的请求与响应原文，凭据一律脱敏；未验证的猜测单列为待确认，不能放入已确认结论。最终用中文输出 Markdown 表格，列名为检查项、证据、结论。"

const babelWorkflowCandidate = `发布审查员: 审查订单服务部署计划
顺序: HTTP API契约→任务依赖→发布与证据
契约: POST /api/v1/orders MUST携带Idempotency-Key; amount≥0元; 相同Idempotency-Key重放→MUST返回同一订单
依赖: MAY并行(fetch_users,fetch_invoices); build_report仅当两项均成功后; 任一失败→禁止启动build_report
发布: 运维人员允许发布仅当系统负责人已书面批准∧回滚演练已成功; 未经批准→禁止写生产数据
证据: ∀结论 MUST附可重复请求+响应原文; 凭据一律脱敏; 未验证猜测→待确认,禁止混入已确认结论
输出: 中文Markdown表格[检查项|证据|结论]`

func TestBabelWorkflowCompressionAndLiteralGuards(t *testing.T) {
	if issues := checkDeterministicInvariants(babelWorkflowSource, babelWorkflowCandidate, nil); len(issues) != 0 {
		t.Fatalf("workflow graph blocked before semantic comparison: %v", issues)
	}
	for _, encoding := range []string{"cl100k_base", "o200k_base"} {
		t.Run(encoding, func(t *testing.T) {
			original, err := measureTokens(babelWorkflowSource, encoding)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := measureTokens(babelWorkflowCandidate, encoding)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("handwritten workflow fixture, %s: %d → %d text tokens", encoding, original.Tokens, candidate.Tokens)
			if candidate.Tokens >= original.Tokens {
				t.Fatal("workflow example did not reduce tokens in this encoding")
			}
		})
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"identifier", "fetch_users", "fetch_user"},
		{"endpoint", "/api/v1/orders", "/api/v1/order"},
		{"numeric boundary", "amount≥0元", "amount>0元"},
		{"unit", "amount≥0元", "amount≥0千元"},
		{"production prohibition", "禁止写生产数据", "允许写生产数据"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := strings.Replace(babelWorkflowCandidate, mutation.before, mutation.after, 1)
			if issues := checkDeterministicInvariants(babelWorkflowSource, candidate, nil); len(issues) == 0 {
				t.Fatal("known literal or prohibition mutation was not rejected")
			}
		})
	}
}

func TestBabelGraphConditionLossStillRequiresSemanticComparison(t *testing.T) {
	// The lexer cannot prove conjunction equivalence. A compact surface form
	// must reach the semantic checker, whose rejection must remain a failure.
	candidate := strings.Replace(babelWorkflowCandidate, "书面批准∧回滚演练", "书面批准∨回滚演练", 1)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
			t.Error("invalid semantic comparison request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &payload); err != nil || payload["original"] != babelWorkflowSource || payload["candidate"] != candidate {
			t.Error("semantic checker did not receive original and exact graph candidate")
		}
		writeModelContent(w, `{"pass":false,"missing":["发布需要书面批准与回滚演练均成立，候选将且改为或"]}`)
	}))
	defer provider.Close()
	profile := Profile{APIKey: "fixture-key", BaseURL: provider.URL, Model: "fixture-model"}
	result := runValidation(profile, babelWorkflowSource, candidate, nil)
	if calls.Load() != 1 || result.Method != "llm_compare" || result.Status != "failed" {
		t.Fatalf("graph condition loss bypassed semantic comparison: calls=%d result=%+v", calls.Load(), result)
	}
}
