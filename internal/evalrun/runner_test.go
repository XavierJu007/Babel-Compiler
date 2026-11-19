package evalrun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runnerStats() Record {
	return Record{"original_tokens": 30, "compiled_tokens": 20, "measurement_kind": "exact",
		"measurement_source": "fixture_encoding", "measurement_scope": "text",
		"measurement_encoding": "o200k_base", "measurement_version": "fixture"}
}

func runnerReceipt(status string) Record {
	issues := []any{}
	if status == "failed" {
		issues = append(issues, "changed condition")
	}
	return Record{"validation": Record{"status": status, "method": "llm_compare", "policy_version": ComparePolicy, "issues": issues}}
}

func invokeRunner(t *testing.T, args ...string) (int, Record, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunCLI(args, &stdout, &stderr)
	var summary Record
	if stdout.Len() > 0 {
		var err error
		summary, err = DecodeRecord(stdout.Bytes())
		if err != nil {
			t.Fatalf("CLI output is not one JSON object: %s (%v)", stdout.String(), err)
		}
	}
	return code, summary, stderr.String()
}

func readRunnerRecords(t *testing.T, path string) []Record {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	records := []Record{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		record, err := DecodeRecord(scanner.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func liveArgs(endpoint, operation, output string, budget int) []string {
	return []string{"--live", operation, "--endpoint", endpoint, "--profile", "explicit-compiler",
		"--limit", "1", "--max-requests", fmt.Sprint(budget), "--output", output}
}

func readRunnerRequest(t *testing.T, r *http.Request) Record {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return nil
	}
	body, err := DecodeRecord(raw)
	if err != nil {
		t.Error(err)
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
		t.Errorf("unexpected method/credentials: %s", r.Method)
	}
	for _, forbidden := range []string{"api_key", "expected_relations", "valid_candidate", "invalid_candidates", "reason", "draft_expected_validation"} {
		if _, exists := body[forbidden]; exists {
			t.Errorf("draft labels or credentials sent upstream: %s", forbidden)
		}
	}
	return body
}

func TestRunnerDefaultsToOfflineEmbeddedCases(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	t.Setenv("BABEL_BASE_URL", server.URL)
	t.Setenv("BABEL_API_KEY", "must-not-be-read")
	code, summary, stderr := invokeRunner(t)
	if code != 0 || stderr != "" || summary["operation"] != "offline_structure_check" || calls.Load() != 0 {
		t.Fatalf("unexpected offline behavior: %d %v %s", code, summary, stderr)
	}
	for key, want := range map[string]int64{"cases": 30, "invalid_candidates": 60, "network_requests": 0} {
		if n, ok := integer(summary[key]); !ok || n != want {
			t.Fatalf("%s=%v", key, summary[key])
		}
	}
	if summary["semantic_quality"] != "not_evaluated" {
		t.Fatal("offline result claimed semantic quality")
	}
}

func TestRunnerRejectsIncompleteLiveOptionsBeforeRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, missing := range []string{"--endpoint", "--profile", "--max-requests", "--output"} {
		args := liveArgs(server.URL, "pipeline", filepath.Join(t.TempDir(), "run.jsonl"), 2)
		for i, arg := range args {
			if arg == missing {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
		if code, _, _ := invokeRunner(t, args...); code != 1 {
			t.Fatalf("accepted missing %s", missing)
		}
	}
	for _, args := range [][]string{
		{"--endpoint", server.URL}, {"--reader-profile="}, {"--max-requests", "1"},
		{"--timeout", "NaN"}, {"--timeout", "+Inf"}, {"--timeout", "1e-10"},
		{"--timeout", "1e20"}, {"--limit", "0"}, {"--case-id", "unknown"},
		{"--mode", "private-codebook"}, {"--token-encoding", "auto"},
	} {
		if code, _, _ := invokeRunner(t, args...); code != 1 {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid options made a request")
	}
}

func TestRunnerAcceptsLegacyBuiltinModeSpellings(t *testing.T) {
	for _, mode := range []string{"", "standard.md", " code-audit.md ", "mixed.md", "en-terse"} {
		if code, _, stderr := invokeRunner(t, "--mode", mode, "--timeout", "1e-9"); code != 0 {
			t.Fatalf("rejected builtin spelling %q: %s", mode, stderr)
		}
	}
}

func TestPipelineForwardsActualCandidateAndComparatorProfile(t *testing.T) {
	candidate := "  generated α → \x60user_id\x60\n\"two  spaces\"\n"
	var requests []Record
	var routes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, readRunnerRequest(t, r))
		routes = append(routes, r.URL.Path)
		if r.URL.Path == "/api/compile" {
			writeRecord(w, Record{"compiled": candidate, "stats": runnerStats(), "usage": Record{"prompt_tokens": 1000}})
			return
		}
		writeRecord(w, runnerReceipt("passed"))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	args := append(liveArgs(server.URL, "pipeline", path, 2),
		"--validation-profile", "explicit-checker", "--token-encoding", "o200k_base")
	code, summary, stderr := invokeRunner(t, args...)
	if code != 0 || stderr != "" || len(requests) != 2 {
		t.Fatalf("pipeline failed: %d %s, requests=%d", code, stderr, len(requests))
	}
	if routes[0] != "/api/compile" || routes[1] != "/api/validate" ||
		requests[1]["candidate"] != candidate || requests[1]["profile"] != "explicit-checker" ||
		requests[0]["mode"] != "mixed" || requests[0]["no_cache"] != true ||
		requests[0]["strict_gate"] != false || requests[0]["verify"] != false {
		t.Fatalf("wrong payloads: %v", requests)
	}
	if _, exists := requests[1]["reader_profile"]; exists {
		t.Fatal("omitted reader override became explicit")
	}
	records := readRunnerRecords(t, path)
	if len(records) != 3 || records[1]["candidate"] != candidate ||
		object(records[1]["selection"])["output"] != "candidate" || summary["semantic_accuracy"] != "not_established" {
		t.Fatalf("wrong pipeline evidence: %v", records)
	}
	if summary["billing_cost"] != "not_measured" {
		t.Fatal("pipeline invented a billing result")
	}
}

func TestPipelineBudgetKeepsGeneratedAndUnstartedCases(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		readRunnerRequest(t, r)
		if r.URL.Path != "/api/compile" {
			t.Error("validation exceeded the request budget")
		}
		writeRecord(w, Record{"compiled": "actual candidate", "stats": runnerStats()})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	args := append(liveArgs(server.URL, "pipeline", path, 1), "--limit", "2")
	code, summary, stderr := invokeRunner(t, args...)
	records := readRunnerRecords(t, path)
	n, _ := integer(summary["not_validated"])
	skipped, _ := integer(summary["skipped_by_budget"])
	if code != 0 || stderr != "" || calls.Load() != 1 || len(records) != 4 || n != 2 || skipped != 3 {
		t.Fatalf("budget behavior: %d %s %v", code, stderr, summary)
	}
	if records[1]["candidate"] != "actual candidate" || records[2]["candidate"] != nil ||
		object(records[1]["selection"])["output"] != "source" {
		t.Fatal("budget exhaustion replaced, discarded or approved a candidate")
	}
	group := object(array(summary["measurement_groups"])[0])
	savings, _ := integer(group["effective_savings_tokens"])
	if savings != 0 {
		t.Fatal("unvalidated candidate counted as effective savings")
	}
}

func TestPipelineCompileErrorsSkipValidationAndOmitProviderBodies(t *testing.T) {
	for _, status := range []int{200, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				readRunnerRequest(t, r)
				w.WriteHeader(status)
				writeRecord(w, Record{"detail": "sensitive-provider-response"})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "run.jsonl")
			code, summary, _ := invokeRunner(t, liveArgs(server.URL, "pipeline", path, 2)...)
			raw, _ := os.ReadFile(path)
			skipped, _ := integer(summary["skipped_after_compile_failure"])
			if code != 2 || calls.Load() != 1 || skipped != 1 || bytes.Contains(raw, []byte("sensitive-provider-response")) {
				t.Fatalf("bad failure evidence: %d %s", code, raw)
			}
		})
	}
}

func TestValidateBudgetAndDraftMatrixExcludeErrorsAndUnknowns(t *testing.T) {
	responses := []Record{runnerReceipt("passed"), runnerReceipt("failed"),
		{"validation": Record{"status": "error", "issues": []any{"provider secret"}}},
		{"validation": Record{"status": "not_run"}}, runnerReceipt("passed")}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRunnerRequest(t, r)
		if len(body) != 4 || body["profile"] != "explicit-compiler" {
			t.Errorf("unexpected validation payload: %v", body)
		}
		if requests == 2 {
			w.WriteHeader(http.StatusBadGateway)
		}
		writeRecord(w, responses[requests])
		requests++
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	code, summary, _ := invokeRunner(t, append(liveArgs(server.URL, "validate", path, 5), "--limit", "2")...)
	if code != 2 || requests != 5 {
		t.Fatalf("budget/result mismatch: %d %d", code, requests)
	}
	for key, want := range map[string]int64{"planned_requests": 6, "skipped_by_budget": 1, "api_success": 3, "draft_label_agreement_denominator": 3, "error": 1, "unknown": 1} {
		if got, _ := integer(summary[key]); got != want {
			t.Fatalf("%s = %v, want %d", key, summary[key], want)
		}
	}
	if summary["semantic_accuracy"] != "not_established" {
		t.Fatal("draft-label agreement was relabeled as semantic accuracy")
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("provider secret")) {
		t.Fatal("provider failure details copied into the run")
	}
}

func TestReaderOverrideOmittedEmptyAndNamedAreDistinct(t *testing.T) {
	for _, variant := range []struct {
		present bool
		value   string
	}{{false, ""}, {true, ""}, {true, "independent-reader"}} {
		t.Run(fmt.Sprint(variant), func(t *testing.T) {
			var captured Record
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = readRunnerRequest(t, r)
				writeRecord(w, Record{"validation": Record{"status": "not_run"}})
			}))
			defer server.Close()
			args := append(liveArgs(server.URL, "validate", filepath.Join(t.TempDir(), "run.jsonl"), 1), "--validation-mode", "blind")
			if variant.present {
				args = append(args, "--reader-profile="+variant.value)
			}
			invokeRunner(t, args...)
			value, exists := captured["reader_profile"]
			if exists != variant.present || (exists && value != variant.value) {
				t.Fatalf("reader override changed: %v", captured)
			}
		})
	}
}

func TestRunnerNeverOverwritesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(path, []byte("previous evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	code, _, _ := invokeRunner(t, liveArgs(server.URL, "compile", path, 1)...)
	raw, _ := os.ReadFile(path)
	if code != 1 || calls.Load() != 0 || string(raw) != "previous evidence" {
		t.Fatal("existing evidence overwritten or network request made before output validation")
	}
}

func TestRequestJSONRejectsRedirectsOversizedAndMalformedBodies(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1))
		case "/duplicate":
			fmt.Fprint(w, "{\"validation\":{\"status\":\"failed\",\"status\":\"passed\"}}")
		default:
			fmt.Fprint(w, "[1,2]")
		}
	}))
	defer server.Close()
	for _, path := range []string{"/redirect", "/large", "/duplicate", "/array"} {
		status, body := requestJSON(server.URL+path, Record{"test": true}, time.Second)
		if body["response_error"] == nil || (path == "/redirect" && status != http.StatusTemporaryRedirect) {
			t.Fatalf("accepted untrusted response at %s: %d %v", path, status, body)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
	for _, endpoint := range []string{"https://user:secret@example.test", "ftp://example.test", "https://example.test?q=1", "https://example.test/#part", "example.test"} {
		if _, err := validateEndpoint(endpoint); err == nil {
			t.Fatalf("accepted ambiguous endpoint %q", endpoint)
		}
	}
}

func TestCompileRunnerPreservesExactCandidateAndReportedMetadata(t *testing.T) {
	const candidate = "  candidate\nwith whitespace\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readRunnerRequest(t, r)
		writeRecord(w, Record{"compiled": candidate, "stats": runnerStats()})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	code, _, stderr := invokeRunner(t, liveArgs(server.URL, "compile", path, 1)...)
	records := readRunnerRecords(t, path)
	if code != 0 || stderr != "" || records[1]["candidate"] != candidate ||
		records[1]["semantic_quality"] != "not_evaluated" || object(records[1]["stats"])["measurement_source"] != "fixture_encoding" {
		t.Fatalf("compile evidence changed: %d %s %v", code, stderr, records)
	}
}

func TestDecodeRecordRejectsDuplicateKeysAndNonIntegralSchemaNumbers(t *testing.T) {
	for _, raw := range []string{
		"{\"a\":1,\"a\":2}", "{\"reading\":{\"status\":\"failed\",\"status\":\"passed\"}}",
		"{\"rows\":[{\"x\":1,\"\\u0078\":2}]}", "null", "{} {}", "{\"x\":NaN}", string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}),
	} {
		if _, err := DecodeRecord([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid/ambiguous JSON: %q", raw)
		}
	}
	for _, v := range []any{true, 1.0, json.Number("1.0"), json.Number("1e0")} {
		if _, ok := integer(v); ok {
			t.Fatalf("accepted non-integer value: %#v", v)
		}
	}
}
