package evalrun

import (
	"encoding/json"
	"sort"
	"strings"
)

var measurementFields = []string{
	"measurement_kind", "measurement_source", "measurement_scope",
	"measurement_encoding", "measurement_version",
}

// PipelineMeasurement compares only complete, service-reported text counts.
// It neither infers a model's tokenizer nor measures upstream billing.
func PipelineMeasurement(stats any, result string, candidateAvailable bool) (Record, Record) {
	excluded := func(reason string) (Record, Record) {
		selectionReason := "validation_not_passed"
		if result == "passed" {
			selectionReason = "measurement_unavailable"
		}
		return Record{"status": "excluded", "reason": reason}, Record{"output": "source", "reason": selectionReason}
	}
	if !candidateAvailable {
		measurement, selection := excluded("candidate_unavailable")
		selection["reason"] = "compile_unavailable"
		return measurement, selection
	}
	s := object(stats)
	if s == nil {
		return excluded("missing_stats")
	}
	original, originalOK := integer(s["original_tokens"])
	candidate, candidateOK := integer(s["compiled_tokens"])
	if !originalOK || !candidateOK || original < 0 || candidate < 0 {
		return excluded("invalid_token_counts")
	}
	for _, key := range measurementFields {
		if !nonempty(s[key]) {
			return excluded("missing_measurement_metadata")
		}
	}
	kind := textValue(s["measurement_kind"])
	if kind != "exact" && kind != "estimated" {
		return excluded("unsupported_measurement_kind")
	}
	if s["measurement_scope"] != "text" {
		return excluded("unsupported_measurement_scope")
	}
	if kind == "exact" && (strings.EqualFold(strings.TrimSpace(textValue(s["measurement_encoding"])), "estimate") ||
		strings.EqualFold(strings.TrimSpace(textValue(s["measurement_source"])), "heuristic-v2")) {
		return excluded("inconsistent_measurement_metadata")
	}
	change := "unchanged"
	if candidate < original {
		change = "reduced"
	} else if candidate > original {
		change = "expanded"
	}
	adopted := result == "passed" && candidate < original
	effective := original
	output, reason := "source", "validation_not_passed"
	if adopted {
		effective, output, reason = candidate, "candidate", "validation_passed_and_smaller"
	} else if result == "passed" {
		reason = "not_smaller"
	}
	group := Record{}
	for _, key := range measurementFields {
		group[key] = s[key]
	}
	return Record{
		"status": "included", "reason": nil, "group": group,
		"original_tokens": original, "candidate_tokens": candidate,
		"candidate_delta_tokens": candidate - original, "candidate_change": change,
		"effective_tokens": effective, "effective_savings_tokens": original - effective,
	}, Record{"output": output, "reason": reason}
}

func addPipelineMeasurement(groups map[string]Record, excluded map[string]int, measurement Record, selection Record) {
	if measurement["status"] == "excluded" {
		excluded[textValue(measurement["reason"])]++
		return
	}
	metadata := object(measurement["group"])
	values := make([]any, 0, len(measurementFields))
	for _, field := range measurementFields {
		values = append(values, metadata[field])
	}
	encoded, _ := json.Marshal(values)
	key := string(encoded)
	group, exists := groups[key]
	if !exists {
		group = Record{"cases": int64(0), "original_tokens": int64(0), "candidate_tokens": int64(0),
			"candidate_delta_tokens": int64(0), "effective_tokens": int64(0), "effective_savings_tokens": int64(0),
			"candidate_changes":  Record{"reduced": int64(0), "unchanged": int64(0), "expanded": int64(0)},
			"adopted_candidates": int64(0), "fallback_to_source": int64(0)}
		for _, field := range measurementFields {
			group[field] = metadata[field]
		}
		groups[key] = group
	}
	deltas := Record{"cases": int64(1)}
	for _, field := range []string{"original_tokens", "candidate_tokens", "candidate_delta_tokens", "effective_tokens", "effective_savings_tokens"} {
		deltas[field] = measurement[field]
	}
	// A valid individual count must not wrap the aggregate. Keep prior entries
	// intact and explicitly exclude this entry from totals if it cannot fit.
	for field, raw := range deltas {
		current, _ := integer(group[field])
		delta, _ := integer(raw)
		if delta > 0 && current > (1<<63-1)-delta || delta < 0 && current < (-1<<63)-delta {
			excluded["measurement_sum_overflow"]++
			return
		}
	}
	for field, raw := range deltas {
		delta, _ := integer(raw)
		increment(group, field, delta)
	}
	increment(object(group["candidate_changes"]), textValue(measurement["candidate_change"]), 1)
	field := "fallback_to_source"
	if selection["output"] == "candidate" {
		field = "adopted_candidates"
	}
	increment(group, field, 1)
}

func increment(record Record, key string, delta int64) {
	current, _ := integer(record[key])
	record[key] = current + delta
}

func sortedMeasurementGroups(groups map[string]Record) []any {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, groups[key])
	}
	return out
}
