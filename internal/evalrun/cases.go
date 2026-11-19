package evalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	fixtures "github.com/XavierJu007/Babel-Compiler/eval"
)

const AnnotationStatus = "assistant_drafted_requires_human_review"

var caseIDPattern = regexp.MustCompile("^[a-z0-9][a-z0-9-]*$")

func exactFields(record Record, fields ...string) []string {
	wanted := make(map[string]bool, len(fields))
	for _, field := range fields {
		wanted[field] = true
	}
	var different []string
	for field := range record {
		if !wanted[field] {
			different = append(different, field)
		}
	}
	for _, field := range fields {
		if _, ok := record[field]; !ok {
			different = append(different, field)
		}
	}
	sort.Strings(different)
	return different
}

func validateTextList(value any, name string, requireNonempty bool) error {
	values := array(value)
	if values == nil {
		return fmt.Errorf("%s must be a list", name)
	}
	if requireNonempty && len(values) == 0 {
		return fmt.Errorf("%s must not be empty", name)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !nonempty(value) {
			return fmt.Errorf("%s must contain nonempty strings", name)
		}
		item := textValue(value)
		if seen[item] {
			return fmt.Errorf("%s contains duplicates", name)
		}
		seen[item] = true
	}
	return nil
}

func validateCase(value any) (Record, error) {
	row := object(value)
	if row == nil {
		return nil, fmt.Errorf("case must be an object")
	}
	if different := exactFields(row, "schema_version", "id", "language", "domains", "tags",
		"annotation_status", "source", "valid_candidate", "expected_relations",
		"protected_literals", "invalid_candidates"); len(different) != 0 {
		return nil, fmt.Errorf("case fields differ: %v", different)
	}
	if version, ok := integer(row["schema_version"]); !ok || version != 1 {
		return nil, fmt.Errorf("schema_version must be 1")
	}
	if !nonempty(row["id"]) || !caseIDPattern.MatchString(textValue(row["id"])) {
		return nil, fmt.Errorf("invalid case id")
	}
	if language := textValue(row["language"]); language != "zh" && language != "en" {
		return nil, fmt.Errorf("language must be zh or en")
	}
	if textValue(row["annotation_status"]) != AnnotationStatus {
		return nil, fmt.Errorf("unsupported annotation status")
	}
	for _, name := range []string{"domains", "tags"} {
		if err := validateTextList(row[name], name, true); err != nil {
			return nil, err
		}
	}
	if !nonempty(row["source"]) {
		return nil, fmt.Errorf("source must be nonempty")
	}
	if !nonempty(row["valid_candidate"]) {
		return nil, fmt.Errorf("valid_candidate must be nonempty")
	}
	if err := validateTextList(row["protected_literals"], "protected_literals", false); err != nil {
		return nil, err
	}
	for _, value := range array(row["protected_literals"]) {
		literal := textValue(value)
		if !strings.Contains(textValue(row["source"]), literal) {
			return nil, fmt.Errorf("protected literal missing from source: %q", literal)
		}
		if !strings.Contains(textValue(row["valid_candidate"]), literal) {
			return nil, fmt.Errorf("protected literal missing from valid_candidate: %q", literal)
		}
	}
	relations := array(row["expected_relations"])
	if len(relations) == 0 {
		return nil, fmt.Errorf("expected_relations must be nonempty")
	}
	relationIDs := make(map[string]bool, len(relations))
	for _, value := range relations {
		relation := object(value)
		if relation == nil || len(exactFields(relation, "id", "statement")) != 0 {
			return nil, fmt.Errorf("each relation needs exactly id and statement")
		}
		id := textValue(relation["id"])
		if !nonempty(relation["id"]) || relationIDs[id] {
			return nil, fmt.Errorf("relation id must be nonempty and unique")
		}
		if !nonempty(relation["statement"]) {
			return nil, fmt.Errorf("relation statement must be nonempty")
		}
		relationIDs[id] = true
	}
	mutants := array(row["invalid_candidates"])
	if len(mutants) == 0 {
		return nil, fmt.Errorf("invalid_candidates must be nonempty")
	}
	mutantIDs := make(map[string]bool, len(mutants))
	for _, value := range mutants {
		mutant := object(value)
		if mutant == nil || len(exactFields(mutant, "id", "text", "violated_relations", "reason")) != 0 {
			return nil, fmt.Errorf("invalid mutant fields")
		}
		id := textValue(mutant["id"])
		if !nonempty(mutant["id"]) || mutantIDs[id] {
			return nil, fmt.Errorf("mutant id must be nonempty and unique")
		}
		mutantIDs[id] = true
		text := textValue(mutant["text"])
		if !nonempty(mutant["text"]) || text == textValue(row["source"]) || text == textValue(row["valid_candidate"]) {
			return nil, fmt.Errorf("mutant text must differ from source/candidate")
		}
		if !nonempty(mutant["reason"]) {
			return nil, fmt.Errorf("mutant reason must be nonempty")
		}
		if err := validateTextList(mutant["violated_relations"], "violated_relations", true); err != nil {
			return nil, err
		}
		for _, relation := range array(mutant["violated_relations"]) {
			if !relationIDs[textValue(relation)] {
				return nil, fmt.Errorf("mutant refers to an unknown relation")
			}
		}
	}
	return row, nil
}

// LoadCases checks draft fixture structure, not semantic correctness. The
// fingerprint covers the original file bytes, including whitespace and newlines.
func LoadCases(path string) ([]Record, string, error) {
	raw := fixtures.Cases
	filename := "cases.jsonl"
	if path != "" {
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		filename = filepath.Base(path)
	}
	if !utf8.Valid(raw) {
		return nil, "", fmt.Errorf("%s: invalid UTF-8", filename)
	}
	// Preserve the previous loader's splitlines behavior without Scanner's
	// default line-size limit. Blank rows still count toward error line numbers.
	lineBreaks := strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
		"\x1c", "\n", "\x1d", "\n", "\x1e", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")
	var rows []Record
	ids := make(map[string]bool)
	for number, line := range strings.Split(lineBreaks.Replace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		row, err := DecodeRecord([]byte(line))
		if err == nil {
			row, err = validateCase(row)
		}
		if err == nil && ids[textValue(row["id"])] {
			err = fmt.Errorf("duplicate case id")
		}
		if err != nil {
			return nil, "", fmt.Errorf("%s:%d: %w", filename, number+1, err)
		}
		rows = append(rows, row)
		ids[textValue(row["id"])] = true
	}
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("case file is empty")
	}
	hash := sha256.Sum256(raw)
	return rows, hex.EncodeToString(hash[:]), nil
}
