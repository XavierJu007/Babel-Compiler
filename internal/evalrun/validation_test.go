package evalrun

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func compareReceiptFixture(status string) Record {
	issues := []any{}
	if status == "failed" {
		issues = append(issues, "fixture rejection")
	}
	return Record{"validation": Record{
		"status": status, "method": "llm_compare", "policy_version": ComparePolicy, "issues": issues,
	}}
}

// These fixtures exercise receipt recognition, not a real model's semantics.
func blindReceiptFixture() Record {
	return Record{"validation": Record{
		"status": "passed", "method": "blind_read", "policy_version": BlindPolicy, "issues": []any{},
		"stages": []any{
			Record{"name": "local_checks", "status": "completed", "policy_version": ComparePolicy, "issues": []any{}},
			Record{
				"name": "reader", "status": "completed", "policy_version": "blind-reader-v1",
				"model": "fixture-model", "issues": []any{},
				"reading": Record{
					"schema_version": 1, "entities": []any{"cache"}, "ambiguities": []any{},
					"relations": []any{Record{
						"subject": "cache", "relation": "is", "object": "empty", "conditions": []any{},
						"scope": "", "modality": "fact", "negated": false, "quantities": []any{},
						"evidence": "cache is empty",
					}},
				},
			},
			Record{"name": "comparator", "status": "passed", "policy_version": "blind-comparator-v1",
				"model": "fixture-model", "issues": []any{}},
		},
	}}
}

func receiptStage(body Record, name string) Record {
	for _, value := range array(object(body["validation"])["stages"]) {
		stage := object(value)
		if textValue(stage["name"]) == name {
			return stage
		}
	}
	return nil
}

func blindRejectionFixture(stageName string) Record {
	body := blindReceiptFixture()
	validation := object(body["validation"])
	validation["status"], validation["issues"] = "failed", []any{"fixture rejection"}
	stopped := false
	for _, value := range array(validation["stages"]) {
		stage := object(value)
		if textValue(stage["name"]) == stageName {
			stage["status"], stage["issues"] = "failed", []any{"fixture rejection"}
			stopped = true
		} else if stopped {
			stage["status"] = "not_run"
			delete(stage, "reading")
		}
	}
	return body
}

func assertValidationObservation(t *testing.T, httpStatus int, body Record, mode, wantCategory, wantCheck string) Record {
	t.Helper()
	category, detail := ValidationObservation(httpStatus, body, mode)
	if category != wantCategory || (wantCheck != "" && detail["receipt_check"] != wantCheck) {
		t.Fatalf("category=%q receipt=%v; want category=%q receipt=%q", category, detail["receipt_check"], wantCategory, wantCheck)
	}
	if detail["requested_mode"] != mode {
		t.Fatalf("requested mode changed: %v", detail["requested_mode"])
	}
	return detail
}

func TestValidationCompareRequiresCurrentMethodPolicyAndIssues(t *testing.T) {
	for _, status := range []string{"passed", "failed"} {
		t.Run(status, func(t *testing.T) {
			assertValidationObservation(t, 200, compareReceiptFixture(status), "compare", status, "recognized_compare_receipt")
			for _, tc := range []struct {
				field string
				value any
				check string
			}{
				{"method", "legacy_audit", "compare_method_mismatch"},
				{"policy_version", "semantic-gate-v1", "compare_policy_mismatch"},
				{"issues", nil, "inconsistent_issues"},
				{"issues", []any{" "}, "inconsistent_issues"},
				{"issues", []any{true}, "inconsistent_issues"},
			} {
				body := compareReceiptFixture(status)
				object(body["validation"])[tc.field] = tc.value
				assertValidationObservation(t, 200, body, "compare", "unknown", tc.check)
			}
		})
	}
	body := compareReceiptFixture("failed")
	object(body["validation"])["method"] = "local_checks"
	assertValidationObservation(t, 200, body, "compare", "failed", "recognized_compare_receipt")
	object(body["validation"])["status"], object(body["validation"])["issues"] = "passed", []any{}
	assertValidationObservation(t, 200, body, "compare", "unknown", "compare_method_mismatch")
	assertValidationObservation(t, 200, blindReceiptFixture(), "compare", "unknown", "compare_policy_mismatch")
	assertValidationObservation(t, 200, compareReceiptFixture("passed"), "other", "unknown", "unsupported_requested_mode")
}

func TestValidationBlindDoesNotAcceptCompareOrLegacyReceipt(t *testing.T) {
	for _, status := range []string{"passed", "failed"} {
		assertValidationObservation(t, 200, compareReceiptFixture(status), "blind", "unknown", "blind_method_or_policy_mismatch")
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"method", "llm_compare"}, {"method", nil},
		{"policy_version", ComparePolicy}, {"policy_version", nil},
	} {
		body := blindReceiptFixture()
		object(body["validation"])[tc.field] = tc.value
		assertValidationObservation(t, 200, body, "blind", "unknown", "blind_method_or_policy_mismatch")
	}
}

func TestValidationBlindSuccessNeedsCompleteStagesAndReading(t *testing.T) {
	body := blindReceiptFixture()
	detail := assertValidationObservation(t, 200, body, "blind", "passed", "recognized_blind_receipt")
	if !reflect.DeepEqual(object(array(detail["stages"])[1])["reading"], receiptStage(body, "reader")["reading"]) {
		t.Fatal("recognized reading evidence was changed or omitted")
	}
	for _, tc := range []struct {
		name   string
		mutate func(Record)
		check  string
	}{
		{"missing stages", func(body Record) { delete(object(body["validation"]), "stages") }, "missing_blind_stages"},
		{"omitted stage", func(body Record) {
			object(body["validation"])["stages"] = array(object(body["validation"])["stages"])[:2]
		}, "missing_blind_stages"},
		{"malformed stage", func(body Record) { array(object(body["validation"])["stages"])[1] = false }, "malformed_blind_stage"},
		{"duplicate stage", func(body Record) { receiptStage(body, "reader")["name"] = "local_checks" }, "unknown_or_duplicate_blind_stage"},
		{"nonstring stage name", func(body Record) { receiptStage(body, "reader")["name"] = []any{} }, "unknown_or_duplicate_blind_stage"},
		{"old stage policy", func(body Record) { receiptStage(body, "reader")["policy_version"] = "blind-reader-v0" }, "blind_stage_policy_mismatch"},
		{"missing stage policy", func(body Record) { delete(receiptStage(body, "comparator"), "policy_version") }, "blind_stage_policy_mismatch"},
		{"local checks skipped", func(body Record) { receiptStage(body, "local_checks")["status"] = "not_run" }, "blind_success_stages_incomplete"},
		{"reader approval", func(body Record) { receiptStage(body, "reader")["status"] = "passed" }, "blind_success_stages_incomplete"},
		{"comparator undecided", func(body Record) { receiptStage(body, "comparator")["status"] = "completed" }, "blind_success_stages_incomplete"},
		{"missing reading", func(body Record) { delete(receiptStage(body, "reader"), "reading") }, "missing_or_ambiguous_reader_receipt"},
		{"boolean reading version", func(body Record) { object(receiptStage(body, "reader")["reading"])["schema_version"] = true }, "missing_or_ambiguous_reader_receipt"},
		{"float reading version", func(body Record) {
			object(receiptStage(body, "reader")["reading"])["schema_version"] = json.Number("1.0")
		}, "missing_or_ambiguous_reader_receipt"},
		{"missing relations", func(body Record) { delete(object(receiptStage(body, "reader")["reading"]), "relations") }, "missing_or_ambiguous_reader_receipt"},
		{"empty relations", func(body Record) { object(receiptStage(body, "reader")["reading"])["relations"] = []any{} }, "missing_or_ambiguous_reader_receipt"},
		{"reader ambiguity", func(body Record) { object(receiptStage(body, "reader")["reading"])["ambiguities"] = []any{"unclear"} }, "missing_or_ambiguous_reader_receipt"},
		{"top issues", func(body Record) { object(body["validation"])["issues"] = []any{"conflicting issue"} }, "inconsistent_issues"},
		{"stage issues", func(body Record) { receiptStage(body, "comparator")["issues"] = []any{"conflicting issue"} }, "inconsistent_blind_stage_issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := blindReceiptFixture()
			tc.mutate(body)
			assertValidationObservation(t, 200, body, "blind", "unknown", tc.check)
		})
	}
	body = blindReceiptFixture()
	object(receiptStage(body, "reader")["reading"])["entities"] = []any{}
	assertValidationObservation(t, 200, body, "blind", "passed", "recognized_blind_receipt")
	stages := array(object(body["validation"])["stages"])
	stages[0], stages[2] = stages[2], stages[0]
	assertValidationObservation(t, 200, body, "blind", "passed", "recognized_blind_receipt")
}

func TestValidationBlindRejectionsPreserveEarlyStopRules(t *testing.T) {
	checks := map[string]string{
		"local_checks": "recognized_blind_local_rejection",
		"reader":       "recognized_blind_reader_rejection",
		"comparator":   "recognized_blind_comparator_rejection",
	}
	for name, check := range checks {
		body := blindRejectionFixture(name)
		assertValidationObservation(t, 200, body, "blind", "failed", check)
		object(body["validation"])["status"], object(body["validation"])["issues"] = "passed", []any{}
		assertValidationObservation(t, 200, body, "blind", "unknown", "blind_success_stages_incomplete")
	}
	body := blindRejectionFixture("reader")
	reading := object(receiptStage(body, "reader")["reading"])
	reading["relations"], reading["ambiguities"] = []any{}, []any{"unresolved"}
	assertValidationObservation(t, 200, body, "blind", "failed", "recognized_blind_reader_rejection")
	delete(receiptStage(body, "reader"), "reading")
	assertValidationObservation(t, 200, body, "blind", "unknown", "missing_rejected_reader_receipt")
	body = blindRejectionFixture("comparator")
	object(receiptStage(body, "reader")["reading"])["relations"] = []any{}
	assertValidationObservation(t, 200, body, "blind", "unknown", "missing_or_ambiguous_reader_receipt")
	body = blindReceiptFixture()
	object(body["validation"])["status"], object(body["validation"])["issues"] = "failed", []any{"no failing stage"}
	assertValidationObservation(t, 200, body, "blind", "unknown", "inconsistent_blind_rejection_stages")
}

func TestValidationHTTPAndStageErrorsNeverBecomeRejections(t *testing.T) {
	for _, status := range []string{"passed", "failed", "error"} {
		assertValidationObservation(t, 502, compareReceiptFixture(status), "blind", "error", "transport_or_http_error")
	}
	assertValidationObservation(t, 0, nil, "compare", "error", "transport_or_http_error")
	for _, status := range []string{"passed", "failed"} {
		body := blindReceiptFixture()
		object(body["validation"])["status"] = status
		receiptStage(body, "reader")["status"] = "error"
		assertValidationObservation(t, 200, body, "blind", "error", "blind_stage_reported_error")
	}
	assertValidationObservation(t, 200, compareReceiptFixture("error"), "compare", "error", "service_reported_error")
}

func TestValidationErrorAndUnknownEvidenceOmitsProviderText(t *testing.T) {
	for _, status := range []int{200, 502} {
		body := blindReceiptFixture()
		validation := object(body["validation"])
		validation["status"], validation["issues"] = "error", []any{"provider secret fixture"}
		stage := receiptStage(body, "reader")
		stage["issues"] = []any{"provider secret fixture"}
		stage["reading"] = Record{"provider_body": "provider secret fixture"}
		stage["debug"] = "provider secret fixture"
		stage["duration_ms"] = json.Number("7")
		category, detail := ValidationObservation(status, body, "blind")
		if category != "error" || detail["issues_omitted"] != true {
			t.Fatalf("service error was not retained separately: %s %+v", category, detail)
		}
		safe := object(array(detail["stages"])[1])
		if safe["issues_omitted"] != true || safe["duration_ms"] != int64(7) || safe["model"] != "fixture-model" {
			t.Fatalf("safe stage metadata was lost: %+v", safe)
		}
		encoded, err := json.Marshal(detail)
		if err != nil || strings.Contains(string(encoded), "provider secret fixture") {
			t.Fatalf("error detail leaked provider text: %s %v", encoded, err)
		}
	}
	body := blindReceiptFixture()
	object(body["validation"])["policy_version"] = "old-policy"
	receiptStage(body, "reader")["issues"] = []any{"provider secret fixture"}
	receiptStage(body, "reader")["reading"] = Record{"provider_body": "provider secret fixture"}
	_, detail := ValidationObservation(200, body, "blind")
	encoded, err := json.Marshal(detail)
	if err != nil || strings.Contains(string(encoded), "provider secret fixture") {
		t.Fatalf("unknown detail leaked provider text: %s %v", encoded, err)
	}
}

func TestValidationDurationsRequireNonnegativeIntegers(t *testing.T) {
	for _, value := range []any{true, -1, float64(1), json.Number("1.0"), json.Number("1e0"), "1"} {
		body := blindReceiptFixture()
		receiptStage(body, "reader")["duration_ms"] = value
		_, detail := ValidationObservation(200, body, "blind")
		if _, ok := object(array(detail["stages"])[1])["duration_ms"]; ok {
			t.Fatalf("invalid duration included: %#v", value)
		}
	}
}

func TestValidationUnrecognizedObjectsRemainUnknown(t *testing.T) {
	for _, body := range []Record{
		{}, {"validation": "PASS"}, {"validation": Record{"status": "FAIL"}},
		{"validation": Record{"status": false}}, {"validation": Record{"status": []any{}}},
	} {
		assertValidationObservation(t, 200, body, "compare", "unknown", "")
	}
}
