package main

import (
	"strings"
	"testing"
)

func TestDomainGuidanceDoesNotMatchReadOnlyInsideThreadOnly(t *testing.T) {
	pack := builtinDomainPacks["application-security"]
	guidance := selectDomainGuidance([]DomainPack{pack}, "Keep the thread-only option unchanged.")
	if strings.Contains(guidance, `"公开表达":"操作范围与副作用"`) {
		t.Fatalf("read-only matched inside the unrelated thread-only identifier: %s", guidance)
	}
	guidance = selectDomainGuidance([]DomainPack{pack}, "Use READ-ONLY access.")
	if !strings.Contains(guidance, `"公开表达":"操作范围与副作用"`) {
		t.Fatal("a complete read-only term should still select the relevant guidance, ignoring case")
	}
}

func TestDomainTermsRequireBoundariesAroundEnglishPhrases(t *testing.T) {
	for _, tc := range []struct {
		text, term string
		want       bool
	}{
		{"keep the empty settings file", "empty set", false},
		{"myidempotency-key is a separate field", "Idempotency-Key", false},
		{"idempotency-keysuffix is a separate field", "Idempotency-Key", false},
		{"thread-only and read-onlyish are different identifiers", "read-only", false},
		{"thread-only; read-only", "read-only", true},
		{"an empty set", "empty set", true},
		{"send Idempotency-Key", "Idempotency-Key", true},
		{"仅允许read-only操作", "READ-ONLY", true},
		{"金额 >= 5", ">=", true},
		{"禁止修改订单", "禁止", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := domainTermMatches(strings.ToLower(tc.text), tc.term); got != tc.want {
				t.Fatalf("match %q in %q = %v; want %v", tc.term, tc.text, got, tc.want)
			}
		})
	}
}
