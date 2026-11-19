package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCodeLiteralsRespectEscapedOpenersOnly(t *testing.T) {
	for _, count := range []int{1, 3} {
		escaped := strings.Repeat("\\", count) + "`"
		source := "Show " + escaped + " on the left; keep the explanation short; show " + escaped + " on the right."
		candidate := "Left: " + escaped + "; keep explanation short; right: " + escaped + "."
		if code := codeLiterals(source); len(code) != 0 {
			t.Fatalf("%d preceding slashes must escape the opener: %+v", count, code)
		}
		if issues := checkDeterministicInvariants(source, candidate, nil); len(issues) != 0 {
			t.Fatalf("escaped backticks froze ordinary narrative: %v", issues)
		}
	}
	for _, count := range []int{0, 2, 4} {
		prefix := "Run " + strings.Repeat("\\", count) + "`"
		if issues := checkDeterministicInvariants(prefix+"send()`.", prefix+"drop()`.", nil); len(issues) == 0 {
			t.Fatalf("%d preceding slashes incorrectly disabled real code protection", count)
		}
	}
	// Backslashes are literal inside a code span: the next matching run still
	// closes it, even when that run immediately follows a backslash.
	code := codeLiterals("Run `send\\`; then `other`.")
	if len(code) != 2 || code[0].body != "send\\" || code[1].body != "other" {
		t.Fatalf("closing delimiter was treated as an escaped opener: %+v", code)
	}
	code = codeLiterals("Show \\` then `send()`.")
	if len(code) != 1 || code[0].body != "send()" {
		t.Fatalf("escaped prose marker swallowed a later real code span: %+v", code)
	}
}

func TestLiteralGuardsRejectAlteredTechnicalContent(t *testing.T) {
	for _, tc := range []struct{ name, source, candidate string }{
		{"quoted case", `Set the header to "X-Request-ID".`, `Set header to "x-request-id".`},
		{"empty string", `Keep note="" distinct from null.`, `Keep note=null distinct from null.`},
		{"escaped quotes", `Keep payload "say \"hello\" now".`, `Keep payload "say \"goodbye\" now".`},
		{"quote after contraction", `Don't translate 'READY'.`, `Keep 'ready'.`},
		{"quote after possessive", `The user's label is 'READY'.`, `User label: 'ready'.`},
		{"unicode quotes", `返回“完成”作为结果。`, `Return “done”.`},
		{"inline code", "Read `userId`.", "Read `userIds`."},
		{"inline expression", "Run `send()`.", "Run `send() + retry()`."},
		{"long inline delimiter", "```send()```", "```drop()```"},
		{"fenced indent", "Use this code:\n```python\nif ready:\n    send()\n```", "Code:\n```python\nif ready:\nsend()\n```"},
		{"tilde fenced code", "~~~sh\nsend\n~~~", "~~~sh\ndrop\n~~~"},
		{"url suffix", "Send to https://api.example.test/orders?mode=read", "Send to https://api.example.test/orders?mode=reader"},
		{"absolute path", "Read /srv/app/config.json", "Read /srv/app/config.json.bak"},
		{"relative path", "Read ./config.json", "Read ./config.json.bak"},
		{"relative file path", "Read src/main.go", "Read src/main.go.bak"},
		{"windows path", `Read C:\app\config.json`, `Read C:\app\config.json.bak`},
		{"snake identifier", "Validate user_id before saving.", "Validate user_ids before saving."},
		{"camel identifier", "Validate userId before saving.", "Validate userIds before saving."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if issues := checkDeterministicInvariants(tc.source, tc.candidate, nil); len(issues) == 0 {
				t.Fatal("known literal mutation escaped local checks")
			}
		})
	}
}

func TestLiteralGuardsAllowMixedNarrativeAndUnchangedContent(t *testing.T) {
	for _, tc := range []struct{ name, source, candidate string }{
		{"mixed narrative", "请在保存前校验 user_id；目标是 https://api.example.test/orders。", "保存前 validate user_id; target=https://api.example.test/orders。"},
		{"apostrophes", "The user's account doesn't need the reader's permission.", "User account无需 reader permission."},
		{"unmarked prose", "Please review the request and reply in Chinese.", "Review request; 中文回复。"},
		{"inline markup", "Read the `userId` field.", "Read field userId."},
		{"backtick in code", "Run ``print(`ok`)``.", "Execute ``print(`ok`)``."},
		{"framing change", "Run:\n```sh\n  send\n```", "Execute:\n~~~sh\n  send\n~~~"},
		{"code unwrapped", "```text\n  keep  spacing\n```", "  keep  spacing"},
		{"ambiguous slash", "Compute rate = amount /count.", "Compute rate = amount ÷ count."},
		{"natural slash", "Review input/output and report the result.", "审查输入、输出并报告结果。"},
		{"quoted payload", `原样保留 "say \"hello\" now" 作为 payload。`, `payload: "say \"hello\" now"; keep exact.`},
		{"fenced code", "原样执行:\n```sh\n  printf 'hello  world'\n```", "Execute:\n```sh\n  printf 'hello  world'\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if issues := checkDeterministicInvariants(tc.source, tc.candidate, nil); len(issues) != 0 {
				t.Fatalf("unchanged literal or unmarked narrative blocked: %s", strings.Join(issues, "; "))
			}
		})
	}
}

func TestLiteralGuardsRunBeforeBothValidationModes(t *testing.T) {
	const source = `系统可以将 "READY" 写入 user_id。`
	const validCandidate = `系统允许 store "READY" → user_id。`
	for _, mode := range []string{"compare", "blind"} {
		t.Run(mode, func(t *testing.T) {
			var readerCalls, comparisonCalls atomic.Int32
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
					t.Error("invalid validation request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if body.Messages[0].Content == blindReaderPrompt {
					readerCalls.Add(1)
					reading := BlindReading{SchemaVersion: 1, Entities: []string{"系统", "user_id"}, Ambiguities: []string{}, Relations: []BlindRelation{{
						Subject: "系统", Relation: "store", Object: "user_id", Modality: "permission", Conditions: []string{}, Quantities: []BlindQuantity{}, Evidence: validCandidate,
					}}}
					writeModelContent(w, blindReadingJSON(t, reading))
					return
				}
				comparisonCalls.Add(1)
				writeModelContent(w, `{"pass":false,"missing":["fixture model rejection"]}`)
			}))
			defer service.Close()
			p := Profile{APIKey: "test", BaseURL: service.URL, Model: "mock-model"}
			validate := func(candidate string) ValidationResult {
				if mode == "blind" {
					return runBlindValidation(p, p, source, candidate, nil)
				}
				return runValidation(p, source, candidate, nil)
			}
			invalid := validate(strings.Replace(validCandidate, `"READY"`, `"ready"`, 1))
			if invalid.Status != "failed" || readerCalls.Load() != 0 || comparisonCalls.Load() != 0 || !strings.Contains(strings.Join(invalid.Issues, "; "), "技术字面量") {
				t.Fatalf("literal mutation reached a model: %+v; reader=%d comparison=%d", invalid, readerCalls.Load(), comparisonCalls.Load())
			}
			valid := validate(validCandidate)
			wantReader := int32(0)
			if mode == "blind" {
				wantReader = 1
			}
			if valid.Status != "failed" || comparisonCalls.Load() != 1 || readerCalls.Load() != wantReader || !strings.Contains(strings.Join(valid.Issues, "; "), "fixture model rejection") {
				t.Fatalf("unchanged literals must still undergo model comparison: %+v; reader=%d comparison=%d", valid, readerCalls.Load(), comparisonCalls.Load())
			}
		})
	}
}
