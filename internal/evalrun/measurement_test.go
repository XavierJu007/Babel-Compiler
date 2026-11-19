package evalrun

import (
	"encoding/json"
	"testing"
)

func TestMeasurementRejectsIncompleteAndConflictingEvidence(t *testing.T) {
	for _, change := range []struct {
		field  string
		value  any
		reason string
	}{
		{"original_tokens", true, "invalid_token_counts"},
		{"compiled_tokens", -1, "invalid_token_counts"},
		{"compiled_tokens", json.Number("2.5"), "invalid_token_counts"},
		{"measurement_version", "", "missing_measurement_metadata"},
		{"measurement_kind", "guess", "unsupported_measurement_kind"},
		{"measurement_scope", "request", "unsupported_measurement_scope"},
		{"measurement_encoding", "estimate", "inconsistent_measurement_metadata"},
		{"measurement_source", "Heuristic-v2", "inconsistent_measurement_metadata"},
	} {
		stats := runnerStats()
		stats[change.field] = change.value
		measurement, selection := PipelineMeasurement(stats, "passed", true)
		if measurement["reason"] != change.reason || selection["output"] != "source" {
			t.Fatalf("%s: %v %v", change.field, measurement, selection)
		}
	}
	measurement, selection := PipelineMeasurement(nil, "not_validated", false)
	if measurement["reason"] != "candidate_unavailable" || selection["reason"] != "compile_unavailable" {
		t.Fatal("missing candidate treated as a measured saving")
	}
}

func TestMeasurementGroupsSeparateEncodingsAndFallbackCounts(t *testing.T) {
	groups, excluded := map[string]Record{}, map[string]int{}
	for _, entry := range []struct {
		encoding, result string
		after            int
	}{
		{"o200k_base", "passed", 20}, {"o200k_base", "failed", 5},
		{"cl100k_base", "passed", 40}, {"cl100k_base", "passed", 30},
	} {
		stats := runnerStats()
		stats["measurement_encoding"], stats["compiled_tokens"] = entry.encoding, entry.after
		measurement, selection := PipelineMeasurement(stats, entry.result, true)
		addPipelineMeasurement(groups, excluded, measurement, selection)
	}
	if len(groups) != 2 || len(excluded) != 0 {
		t.Fatalf("mixed measurement sources: %v %v", groups, excluded)
	}
	for _, raw := range sortedMeasurementGroups(groups) {
		group := object(raw)
		savings, _ := integer(group["effective_savings_tokens"])
		if group["measurement_encoding"] == "o200k_base" && savings != 10 {
			t.Fatalf("failed candidate counted as saving: %v", group)
		}
		if group["measurement_encoding"] == "cl100k_base" && savings != 0 {
			t.Fatalf("longer/equal candidate counted as saving: %v", group)
		}
	}
}

func TestMeasurementAggregateDoesNotWrapLargeCounts(t *testing.T) {
	groups, excluded := map[string]Record{}, map[string]int{}
	stats := runnerStats()
	stats["original_tokens"], stats["compiled_tokens"] = int64(1<<63-1), int64(1<<63-2)
	measurement, selection := PipelineMeasurement(stats, "passed", true)
	addPipelineMeasurement(groups, excluded, measurement, selection)
	addPipelineMeasurement(groups, excluded, measurement, selection)
	if excluded["measurement_sum_overflow"] != 1 {
		t.Fatalf("overflow not recorded: %v", excluded)
	}
	group := object(sortedMeasurementGroups(groups)[0])
	total, _ := integer(group["original_tokens"])
	cases, _ := integer(group["cases"])
	if total != int64(1<<63-1) || cases != 1 {
		t.Fatalf("prior measurement corrupted by overflow: %v", group)
	}
}
