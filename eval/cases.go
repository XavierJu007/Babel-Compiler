// Package fixtures embeds the reviewable regression examples for babel-eval.
package fixtures

import _ "embed"

//go:embed cases.jsonl
var Cases []byte
