package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// An unavailable model fixture proves routing without treating a lexical pass
// as semantic approval. Both comparison modes must stop at the local guard only
// when the source actually supplies a literal or numeric contract.
func checkQuoteReviewBoundary(t *testing.T, mode, original, candidate string, wantLocal bool) {
	t.Helper()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture semantic service unavailable", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	profile := Profile{APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
	var result ValidationResult
	if mode == "blind" {
		result = runBlindValidation(profile, profile, original, candidate, nil)
	} else {
		result = runValidation(profile, original, candidate, nil)
	}
	if wantLocal {
		if result.Status != "failed" || calls.Load() != 0 {
			t.Fatalf("explicit literal or number damage escaped local checks: calls=%d result=%+v", calls.Load(), result)
		}
	} else if result.Status != "error" || calls.Load() != 1 || !strings.Contains(strings.Join(result.Issues, " "), "fixture semantic service unavailable") {
		t.Fatalf("ordinary prose was locked or semantic checking was bypassed: calls=%d result=%+v", calls.Load(), result)
	}
}

func TestQuoteReviewProseRewritesReachSemanticChecks(t *testing.T) {
	for _, tc := range []struct{ name, original, candidate string }{
		{
			"independent paragraph after fixed output",
			"渠道超时则返回「处理中」\n\n「已签收未超过 7 天」且商品未拆封的订单可自助退款。",
			"渠道超时→返回「处理中」\n签收≤7天∧商品未拆封→可自助退款",
		},
		{
			"ordinary quoted heading",
			`"记录要求": 优先考虑可追溯并且易于复核的记录方式。`,
			"记录: 优先可追溯∧易复核",
		},
		{
			"explicitly optional exact wording",
			"无需逐字保留「可追溯并且易于复核」这句话；请表达相同记录要求。",
			"记录要求: 可追溯∧易复核；措辞可改",
		},
		{
			"explicitly optional original wording",
			"无需原样保留「可追溯并且易于复核」这句话；请表达相同记录要求。",
			"记录要求: 可追溯∧易复核；措辞可改",
		},
	} {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				checkQuoteReviewBoundary(t, mode, tc.original, tc.candidate, false)
			})
		}
	}
}

func TestQuoteReviewExplicitStringsAndNumbersStayLocal(t *testing.T) {
	for _, tc := range []struct{ name, original, candidate string }{
		{"exact wording after quote", "「recieve」须保持原样，不能纠正拼写。", "「receive」须保持原样，不能纠正拼写。"},
		{"separate positive exact requirement", "无需逐字复述背景但须原样保留「recieve」。", "无需逐字复述背景但须原样保留「receive」。"},
		{"actual JSON", `JSON: {"spelling":"recieve"}`, `JSON: {"spelling":"receive"}`},
		{"multiline JSON", "JSON: [\n\"READY\",\n\"recieve\"\n]", "JSON: [\n\"READY\",\n\"receive\"\n]"},
		{"assignment", `spelling = "recieve"`, `spelling = "receive"`},
		{"fixed output", "返回「recieve」。", "返回「receive」。"},
		{"ordinary quoted quantity", "普通说明「不少于 7 天」。", "普通说明: ≥8天。"},
	} {
		for _, mode := range []string{"compare", "blind"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				checkQuoteReviewBoundary(t, mode, tc.original, tc.candidate, true)
			})
		}
	}
}
