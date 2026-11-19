package evalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixtures "github.com/XavierJu007/Babel-Compiler/eval"
)

func draftCaseFixture() Record {
	return Record{
		"schema_version": 1, "id": "en-approval", "language": "en",
		"domains": []any{"common"}, "tags": []any{"necessary-condition"},
		"annotation_status":  AnnotationStatus,
		"source":             "Keep user_id unchanged. Allow access only after approval.",
		"valid_candidate":    "Access requires approval; preserve user_id.",
		"protected_literals": []any{"user_id"},
		"expected_relations": []any{Record{"id": "r1", "statement": "Approval is necessary for access."}},
		"invalid_candidates": []any{Record{
			"id": "mutant-1", "text": "Access needs no approval.",
			"violated_relations": []any{"r1"}, "reason": "Removes the necessary condition.",
		}},
	}
}

func TestLoadCasesEmbeddedDraftsAndRawFingerprint(t *testing.T) {
	rows, fingerprint, err := LoadCases("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 30 {
		t.Fatalf("embedded cases = %d; want 30", len(rows))
	}
	mutants := 0
	for _, row := range rows {
		if row["annotation_status"] != AnnotationStatus {
			t.Fatal("a draft fixture was relabelled as reviewed")
		}
		mutants += len(array(row["invalid_candidates"]))
	}
	if mutants != 60 {
		t.Fatalf("embedded invalid candidates = %d; want 60", mutants)
	}
	hash := sha256.Sum256(fixtures.Cases)
	if fingerprint != hex.EncodeToString(hash[:]) {
		t.Fatal("embedded fingerprint does not cover original JSONL bytes")
	}
}

func TestLoadCasesExternalBytesAndLineNumbers(t *testing.T) {
	encoded, err := json.Marshal(draftCaseFixture())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixtures.jsonl")
	raw := append([]byte("\r\n  \n"), encoded...)
	raw = append(raw, []byte("\r\n\n")...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rows, fingerprint, err := LoadCases(path)
	if err != nil || len(rows) != 1 {
		t.Fatalf("load external fixture: rows=%d err=%v", len(rows), err)
	}
	hash := sha256.Sum256(raw)
	if fingerprint != hex.EncodeToString(hash[:]) {
		t.Fatal("fingerprint normalized whitespace or line endings")
	}
	duplicate := "\n" + string(encoded) + "\n" + string(encoded) + "\n"
	if err := os.WriteFile(path, []byte(duplicate), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCases(path); err == nil || !strings.Contains(err.Error(), "fixtures.jsonl:3: duplicate case id") {
		t.Fatalf("duplicate case did not retain the physical line number: %v", err)
	}
}

func TestCaseValidationPreservesDraftSchemaBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(Record)
		want   string
	}{
		{"unknown field", func(row Record) { row["reviewed"] = true }, "case fields differ"},
		{"missing field", func(row Record) { delete(row, "tags") }, "case fields differ"},
		{"boolean schema", func(row Record) { row["schema_version"] = true }, "schema_version"},
		{"fractional schema", func(row Record) { row["schema_version"] = json.Number("1.0") }, "schema_version"},
		{"float schema", func(row Record) { row["schema_version"] = float64(1) }, "schema_version"},
		{"invalid id", func(row Record) { row["id"] = "Bad-ID" }, "invalid case id"},
		{"invalid language", func(row Record) { row["language"] = "mixed" }, "language"},
		{"review claim", func(row Record) { row["annotation_status"] = "human_reviewed" }, "annotation status"},
		{"null domains", func(row Record) { row["domains"] = nil }, "domains must be a list"},
		{"empty tags", func(row Record) { row["tags"] = []any{} }, "tags must not be empty"},
		{"duplicate tags", func(row Record) { row["tags"] = []any{"a", "a"} }, "tags contains duplicates"},
		{"nontext tags", func(row Record) { row["tags"] = []any{true} }, "nonempty strings"},
		{"empty source", func(row Record) { row["source"] = " \t" }, "source must be nonempty"},
		{"missing source literal", func(row Record) { row["source"] = "Access requires approval." }, "missing from source"},
		{"changed candidate literal", func(row Record) { row["valid_candidate"] = "Keep userId; require approval." }, "missing from valid_candidate"},
		{"null literals", func(row Record) { row["protected_literals"] = nil }, "protected_literals must be a list"},
		{"empty relations", func(row Record) { row["expected_relations"] = []any{} }, "expected_relations"},
		{"extra relation field", func(row Record) { object(array(row["expected_relations"])[0])["extra"] = true }, "exactly id and statement"},
		{"duplicate relation id", func(row Record) {
			row["expected_relations"] = append(array(row["expected_relations"]), Record{"id": "r1", "statement": "Other relation."})
		}, "relation id"},
		{"empty mutants", func(row Record) { row["invalid_candidates"] = []any{} }, "invalid_candidates"},
		{"unknown relation", func(row Record) {
			object(array(row["invalid_candidates"])[0])["violated_relations"] = []any{"missing"}
		}, "unknown relation"},
		{"duplicate violation", func(row Record) {
			object(array(row["invalid_candidates"])[0])["violated_relations"] = []any{"r1", "r1"}
		}, "violated_relations contains duplicates"},
		{"unchanged mutant", func(row Record) {
			object(array(row["invalid_candidates"])[0])["text"] = row["valid_candidate"]
		}, "mutant text must differ"},
		{"extra mutant field", func(row Record) {
			object(array(row["invalid_candidates"])[0])["score"] = 1
		}, "invalid mutant fields"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := draftCaseFixture()
			tc.mutate(row)
			if _, err := validateCase(row); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want %q", err, tc.want)
			}
		})
	}
	row := draftCaseFixture()
	row["protected_literals"] = []any{}
	if _, err := validateCase(row); err != nil {
		t.Fatalf("an empty protected-literal list is valid: %v", err)
	}
	if _, err := validateCase([]any{}); err == nil {
		t.Fatal("non-object case accepted")
	}
}

func TestLoadCasesRejectsEmptyInvalidAndNonObjectInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty", []byte(" \n\r\n"), "case file is empty"},
		{"invalid UTF-8", []byte{0xff, '\n'}, "invalid UTF-8"},
		{"non-object", []byte("\n[]\n"), "fixtures.jsonl:2:"},
		{"trailing JSON", []byte("{} {}\n"), "fixtures.jsonl:1:"},
		{"malformed JSON", []byte("{\n"), "fixtures.jsonl:1:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixtures.jsonl")
			if err := os.WriteFile(path, tc.raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadCases(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want %q", err, tc.want)
			}
		})
	}
}
