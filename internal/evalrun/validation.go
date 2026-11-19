package evalrun

import "encoding/json"

const (
	ComparePolicy = "semantic-gate-v2"
	BlindPolicy   = "semantic-blind-v1"
)

func receiptIssuesValid(value any, status string) bool {
	issues := array(value)
	if issues == nil {
		return false
	}
	for _, issue := range issues {
		if !nonempty(issue) {
			return false
		}
	}
	if status == "failed" {
		return len(issues) > 0
	}
	return len(issues) == 0
}

func readerReceiptPresent(stage Record, rejected bool) bool {
	reading := object(stage["reading"])
	if reading == nil {
		return false
	}
	if version, ok := integer(reading["schema_version"]); !ok || version != 1 {
		return false
	}
	for _, key := range []string{"entities", "relations", "ambiguities"} {
		if array(reading[key]) == nil {
			return false
		}
	}
	// This recognizes receipt structure, not the truth of its semantic reading.
	// A completed reader needs relations and no unresolved ambiguity. Entities
	// may be empty; a rejected reader may have empty relations or ambiguities.
	return rejected || (len(array(reading["relations"])) > 0 && len(array(reading["ambiguities"])) == 0)
}

func blindStagePolicy(name string) string {
	switch name {
	case "local_checks":
		return ComparePolicy
	case "reader":
		return "blind-reader-v1"
	case "comparator":
		return "blind-comparator-v1"
	default:
		return ""
	}
}

func recognizedValidation(validation Record, expectedMode string) (string, string) {
	status := textValue(validation["status"])
	if status == "error" {
		return "error", "service_reported_error"
	}
	if status != "passed" && status != "failed" {
		return "unknown", "undecided_or_invalid_status"
	}
	if expectedMode == "compare" {
		if textValue(validation["policy_version"]) != ComparePolicy {
			return "unknown", "compare_policy_mismatch"
		}
		method := textValue(validation["method"])
		if (method != "llm_compare" && method != "local_checks") || (status == "passed" && method != "llm_compare") {
			return "unknown", "compare_method_mismatch"
		}
		if !receiptIssuesValid(validation["issues"], status) {
			return "unknown", "inconsistent_issues"
		}
		return status, "recognized_compare_receipt"
	}
	if expectedMode != "blind" {
		return "unknown", "unsupported_requested_mode"
	}
	if textValue(validation["method"]) != "blind_read" || textValue(validation["policy_version"]) != BlindPolicy {
		return "unknown", "blind_method_or_policy_mismatch"
	}
	rawStages := array(validation["stages"])
	if len(rawStages) != 3 {
		return "unknown", "missing_blind_stages"
	}
	stages := make(map[string]Record, 3)
	orderedStages := make([]Record, 0, 3)
	for _, value := range rawStages {
		stage := object(value)
		if stage == nil {
			return "unknown", "malformed_blind_stage"
		}
		name := textValue(stage["name"])
		policy := blindStagePolicy(name)
		if policy == "" || stages[name] != nil {
			return "unknown", "unknown_or_duplicate_blind_stage"
		}
		if textValue(stage["policy_version"]) != policy {
			return "unknown", "blind_stage_policy_mismatch"
		}
		stages[name] = stage
		orderedStages = append(orderedStages, stage)
	}
	for _, stage := range orderedStages {
		if textValue(stage["status"]) == "error" {
			return "error", "blind_stage_reported_error"
		}
	}
	if !receiptIssuesValid(validation["issues"], status) {
		return "unknown", "inconsistent_issues"
	}
	for _, stage := range orderedStages {
		stageStatus := textValue(stage["status"])
		if stageStatus != "completed" && stageStatus != "passed" && stageStatus != "failed" && stageStatus != "not_run" {
			return "unknown", "invalid_blind_stage_status"
		}
		if !receiptIssuesValid(stage["issues"], stageStatus) {
			return "unknown", "inconsistent_blind_stage_issues"
		}
	}
	ordered := [3]string{textValue(stages["local_checks"]["status"]), textValue(stages["reader"]["status"]), textValue(stages["comparator"]["status"])}
	if status == "passed" {
		if ordered != [3]string{"completed", "completed", "passed"} {
			return "unknown", "blind_success_stages_incomplete"
		}
		if !readerReceiptPresent(stages["reader"], false) {
			return "unknown", "missing_or_ambiguous_reader_receipt"
		}
		return "passed", "recognized_blind_receipt"
	}
	switch ordered {
	case [3]string{"failed", "not_run", "not_run"}:
		return "failed", "recognized_blind_local_rejection"
	case [3]string{"completed", "failed", "not_run"}:
		if readerReceiptPresent(stages["reader"], true) {
			return "failed", "recognized_blind_reader_rejection"
		}
		return "unknown", "missing_rejected_reader_receipt"
	case [3]string{"completed", "completed", "failed"}:
		if readerReceiptPresent(stages["reader"], false) {
			return "failed", "recognized_blind_comparator_rejection"
		}
		return "unknown", "missing_or_ambiguous_reader_receipt"
	default:
		return "unknown", "inconsistent_blind_rejection_stages"
	}
}

// receiptValuePresent follows JSON value truthiness only to retain the prior
// issues_omitted marker. The value itself is never copied on error or unknown.
func receiptValuePresent(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case []string:
		return len(value) > 0
	case []Record:
		return len(value) > 0
	case Record:
		return len(value) > 0
	case json.Number:
		number, err := value.Float64()
		return err != nil || number != 0
	case float64:
		return value != 0
	case float32:
		return value != 0
	default:
		number, ok := integer(value)
		return !ok || number != 0
	}
}

// ValidationObservation recognizes the requested current protocol and returns
// only reportable evidence. HTTP/service errors remain separate from rejection.
func ValidationObservation(httpStatus int, body Record, expectedMode string) (string, Record) {
	validation := object(body["validation"])
	category, receiptCheck := "", ""
	if httpStatus != 200 {
		category, receiptCheck = "error", "transport_or_http_error"
	} else if validation == nil {
		category, receiptCheck = "unknown", "missing_validation_object"
	} else {
		category, receiptCheck = recognizedValidation(validation, expectedMode)
	}
	detail := Record{"requested_mode": expectedMode, "receipt_check": receiptCheck}
	if validation == nil {
		return category, detail
	}
	for _, key := range []string{"status", "method", "policy_version"} {
		if value, ok := validation[key].(string); ok {
			detail[key] = value
		}
	}
	decided := category == "passed" || category == "failed"
	if decided {
		detail["issues"] = validation["issues"]
	} else if receiptValuePresent(validation["issues"]) {
		detail["issues_omitted"] = true
	}
	if stages := array(validation["stages"]); stages != nil {
		safeStages := make([]any, 0, len(stages))
		for _, value := range stages {
			stage := object(value)
			if stage == nil {
				continue
			}
			safe := Record{}
			for _, key := range []string{"name", "status", "policy_version", "model"} {
				if value, ok := stage[key].(string); ok {
					safe[key] = value
				}
			}
			if duration, ok := integer(stage["duration_ms"]); ok && duration >= 0 {
				safe["duration_ms"] = duration
			}
			if decided {
				if issues := array(stage["issues"]); issues != nil {
					valid := true
					for _, issue := range issues {
						valid = valid && nonempty(issue)
					}
					if valid {
						safe["issues"] = stage["issues"]
					}
				}
				if reading := object(stage["reading"]); reading != nil {
					safe["reading"] = reading
				}
			} else if receiptValuePresent(stage["issues"]) {
				safe["issues_omitted"] = true
			}
			safeStages = append(safeStages, safe)
		}
		detail["stages"] = safeStages
	}
	return category, detail
}
