// Package evalrun contains the optional, offline-first Babel regression tool.
package evalrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Record = map[string]any

func object(value any) Record {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []string:
		out := make([]any, len(values))
		for i, value := range values {
			out[i] = value
		}
		return out
	case []Record:
		out := make([]any, len(values))
		for i, value := range values {
			out[i] = value
		}
		return out
	default:
		return nil
	}
}

func textValue(value any) string {
	result, _ := value.(string)
	return result
}

func nonempty(value any) bool { return strings.TrimSpace(textValue(value)) != "" }

// JSON decoding uses Number: booleans, fractions and floats must not silently
// turn into integer schema versions, durations or token counts.
func integer(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func DecodeRecord(raw []byte) (Record, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	result := object(value)
	if result == nil {
		return nil, fmt.Errorf("JSON object required")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("exactly one JSON object required")
	}
	return result, nil
}

func readJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 512 {
		return nil, fmt.Errorf("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		out := Record{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key must be a string")
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		_, err := decoder.Token()
		return out, err
	case '[':
		out := []any{}
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		_, err := decoder.Token()
		return out, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter")
	}
}

func writeRecord(writer io.Writer, record Record) error {
	return json.NewEncoder(writer).Encode(record)
}

func createOutput(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}
