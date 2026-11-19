package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
)

type tokenCountReferenceCase struct {
	Name   string         `json:"name"`
	Text   string         `json:"text"`
	Counts map[string]int `json:"counts"`
}

func tokenCountReferenceCases(t *testing.T) []tokenCountReferenceCase {
	t.Helper()
	data, err := os.ReadFile("testdata/token_count_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference struct {
			Implementation string `json:"implementation"`
			Version        string `json:"version"`
			Method         string `json:"method"`
		} `json:"reference"`
		Cases []tokenCountReferenceCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Reference.Implementation != "openai/tiktoken" || fixture.Reference.Version != "0.12.0" || fixture.Reference.Method != "encode_ordinary" || len(fixture.Cases) == 0 {
		t.Fatal("missing independent ordinary-text reference metadata or cases")
	}
	return fixture.Cases
}

// These counts come from OpenAI's Python implementation, not from the Go
// dependency under test. Normal go test runs need no Python or network access.
func TestTokenCounterOfficialReferenceVectors(t *testing.T) {
	for _, tc := range tokenCountReferenceCases(t) {
		for encoding, want := range tc.Counts {
			t.Run(tc.Name+"/"+encoding, func(t *testing.T) {
				got, err := measureTokens(tc.Text, encoding)
				if err != nil {
					t.Fatal(err)
				}
				if got.Tokens != want {
					t.Errorf("tokens = %d, official tiktoken encode_ordinary = %d for %q", got.Tokens, want, tc.Text)
				}
				if got.Kind != "exact" || got.Source != "tiktoken-go/tokenizer" || got.Scope != "text" || got.Encoding != encoding || got.Version != tokenCounterVersion {
					t.Errorf("misleading measurement metadata: %+v", got)
				}
			})
		}
	}
}

func TestTokenCounterExplicitEstimate(t *testing.T) {
	const input = "保留 100 ms 上限，禁止泄露秘密。"
	for _, encoding := range []string{"", "estimate", "  estimate  ", "ESTIMATE"} {
		got, err := measureTokens(input, encoding)
		if err != nil {
			t.Fatal(err)
		}
		if got.Tokens != estimateTokensBPE(input) || got.Kind != "estimated" || got.Source != "heuristic-v2" || got.Scope != "text" || got.Encoding != "estimate" || got.Version != "2" {
			t.Errorf("estimate %q has incorrect count or provenance: %+v", encoding, got)
		}
	}
}

func TestTokenCounterUnknownEncodingFailsClosed(t *testing.T) {
	// A model name is not an encoding. In particular, choosing a compiler model
	// from another family never opts the user into an OpenAI tokenizer.
	for _, encoding := range []string{"gpt-4o", "qwen3", "claude-sonnet-4", "auto", "exact", "o200k_harmony", "cl100k_base,estimate"} {
		if err := validateTokenEncoding(encoding); err == nil {
			t.Errorf("validateTokenEncoding(%q) unexpectedly succeeded", encoding)
		}
		got, err := measureTokens("Some text", encoding)
		if err == nil || got != (TokenCount{}) {
			t.Errorf("unsupported encoding %q returned a measurement: %+v, %v", encoding, got, err)
		}
	}
}

func TestTokenCounterInvalidUTF8FailsClosed(t *testing.T) {
	for _, encoding := range supportedTokenEncodings() {
		got, err := measureTokens(string([]byte{'a', 0xff, 'b'}), encoding)
		if err == nil || got != (TokenCount{}) {
			t.Errorf("invalid UTF-8 produced a measurement for %s: %+v, %v", encoding, got, err)
		}
	}
}

func TestTokenCounterEncodingSelection(t *testing.T) {
	encodings := supportedTokenEncodings()
	encodings[0] = "gpt-4o"
	if supportedTokenEncodings()[0] != "estimate" {
		t.Fatal("caller mutated the supported encoding list")
	}
	for _, encoding := range supportedTokenEncodings() {
		if err := validateTokenEncoding(encoding); err != nil {
			t.Fatal(err)
		}
	}
	got, err := measureTokens("お誕生日おめでとう", "  O200K_BASE  ")
	if err != nil || got.Encoding != "o200k_base" || got.Tokens != 8 {
		t.Fatalf("explicit normalized encoding was not respected: %+v, %v", got, err)
	}
}

func TestTokenCounterConcurrent(t *testing.T) {
	cases := tokenCountReferenceCases(t)
	var wg sync.WaitGroup
	errors := make(chan error, 24)
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for step := 0; step < 24; step++ {
				tc := cases[(offset+step)%len(cases)]
				for encoding, want := range tc.Counts {
					got, err := measureTokens(tc.Text, encoding)
					if err != nil || got.Tokens != want {
						errors <- fmt.Errorf("%s/%s: got %+v, want %d, error %v", tc.Name, encoding, got, want, err)
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}
