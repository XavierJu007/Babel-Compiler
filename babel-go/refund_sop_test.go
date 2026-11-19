package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// This is a handwritten expression target, not a model-generated answer or a
// proof of semantic equivalence. Keep its complete source alongside it so the
// overlapping amount tiers, unusual retry order and stray input remain visible.
func refundSOPFixture(t *testing.T) (string, string) {
	t.Helper()
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "docs", "examples", name))
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(data) {
			t.Fatalf("fixture %s is not UTF-8", name)
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	return read("refund-sop.source.txt"), read("refund-sop.babel.txt")
}

func TestRefundSOPRelationFixture(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	// These explicit fixture checks document facts that a lexical gate alone
	// cannot prove, including the exact quoted states and input oddity.
	for _, literal := range []string{
		"「已支付未发货」", "「已签收未超过 7 天」", "「满 3 件 8 折」",
		"「处理中」", "「余额不足」", "「账户异常」", "「合规」",
		"「流程瑕疵」", "「资金风险」", "「待确认」", "09:00", "SOP", "SKU", "å",
	} {
		if !strings.Contains(source, literal) || !strings.Contains(candidate, literal) {
			t.Errorf("source/candidate fixture lost literal %q", literal)
		}
	}
	if issues := checkDeterministicInvariants(source, candidate, nil); len(issues) != 0 {
		t.Fatalf("refund relation fixture rejected before model comparison: %v", issues)
	}
	if got := compactDSL(candidate); got != candidate {
		t.Fatal("postprocessing changed the refund relation fixture")
	}
}

func TestRefundSOPTokenMeasurements(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	for _, encoding := range []string{"cl100k_base", "o200k_base"} {
		t.Run(encoding, func(t *testing.T) {
			before, err := measureTokens(source, encoding)
			if err != nil {
				t.Fatal(err)
			}
			after, err := measureTokens(candidate, encoding)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("handwritten refund fixture, %s: %d -> %d text tokens; source/candidate runes: %d/%d",
				encoding, before.Tokens, after.Tokens, utf8.RuneCountInString(source), utf8.RuneCountInString(candidate))
			if after.Tokens >= before.Tokens {
				t.Fatal("refund relation fixture is no shorter in this encoding")
			}
		})
	}
}

func TestRefundSOPKnownLiteralAndNumericDamageIsRejected(t *testing.T) {
	source, candidate := refundSOPFixture(t)
	if issues := checkDeterministicInvariants(source, candidate, nil); len(issues) != 0 {
		t.Fatalf("cannot attribute rejection to a mutation while its baseline is rejected: %v", issues)
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"quoted processing state", "「处理中」", "「已完成」"},
		{"idempotency duration", "300秒", "301秒"},
		{"retention minimum", "≥180天", "<180天"},
		{"reconciliation threshold", ">0.01元", ">0.02元"},
		{"offline disbursement prohibition", "禁止线上直接放款", "允许线上直接放款"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if !strings.Contains(candidate, mutation.before) {
				t.Fatal("mutation no longer targets the candidate")
			}
			broken := strings.ReplaceAll(candidate, mutation.before, mutation.after)
			if issues := checkDeterministicInvariants(source, broken, nil); len(issues) == 0 {
				t.Fatal("known literal, numeric or prohibition damage was not rejected")
			}
		})
	}
}
