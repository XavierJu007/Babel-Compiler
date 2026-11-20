package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Public symbols are expression-library data, not output-preset instructions.
// Expanding this catalog is a reviewed code/data change. Domain packs supply
// applicability in use_when/preserve; they cannot redefine a public meaning.
//
//go:embed public_symbols.json
var publicSymbolsJSON []byte

type publicSymbolRegistry struct {
	ordered  []DomainSymbol
	meanings map[string]string
}

// Initialize independently of builtinDomainPacks to avoid a validation cycle.
var publicSymbolCatalog = mustLoadPublicSymbols()

func mustLoadPublicSymbols() publicSymbolRegistry {
	decoder := json.NewDecoder(bytes.NewReader(publicSymbolsJSON))
	decoder.DisallowUnknownFields()
	var symbols []DomainSymbol
	if err := decoder.Decode(&symbols); err != nil {
		panic("invalid public symbol catalog: " + err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(symbols) == 0 {
		panic("public symbol catalog must contain one nonempty JSON array")
	}
	registry := publicSymbolRegistry{meanings: make(map[string]string, len(symbols))}
	for _, symbol := range symbols {
		symbol.Symbol = strings.TrimSpace(symbol.Symbol)
		symbol.Meaning = strings.TrimSpace(symbol.Meaning)
		if symbol.Symbol == "" || symbol.Meaning == "" {
			panic("empty public symbol or meaning")
		}
		if _, exists := registry.meanings[symbol.Symbol]; exists {
			panic("duplicate public symbol: " + symbol.Symbol)
		}
		registry.meanings[symbol.Symbol] = symbol.Meaning
		registry.ordered = append(registry.ordered, symbol)
	}
	return registry
}

func listPublicSymbols() []DomainSymbol {
	return append([]DomainSymbol{}, publicSymbolCatalog.ordered...)
}

func canonicalPublicSymbol(symbol DomainSymbol) (DomainSymbol, error) {
	name := strings.TrimSpace(symbol.Symbol)
	meaning, exists := publicSymbolCatalog.meanings[name]
	if !exists {
		return DomainSymbol{}, fmt.Errorf("符号 %q 不在公共符号目录中，不能作为自定义代号使用", name)
	}
	// Older official arrow descriptions required redundant relation labels.
	// Accept only those exact published descriptions, then canonicalize them on
	// load/save; this is not permission to redefine other symbols or meanings.
	legacyMeaning := ""
	switch name {
	case "→":
		legacyMeaning = "在明确标注的关系中表示方向；不能省略其关系类型"
	case "->":
		legacyMeaning = "在明确标注的关系中表示方向的ASCII写法；不能省略其关系类型"
	}
	provided := strings.TrimSpace(symbol.Meaning)
	if provided != meaning && (legacyMeaning == "" || provided != legacyMeaning) {
		return DomainSymbol{}, fmt.Errorf("符号 %q 的含义必须为公共目录中的 %q；业务语境请填写 use_when 或 preserve", name, meaning)
	}
	return DomainSymbol{Symbol: name, Meaning: meaning}, nil
}

// Call only after validateDomainPack. The copy prevents normalization from
// mutating a caller's pack or the immutable builtin registry.
func normalizeDomainSymbols(p DomainPack) DomainPack {
	p = cloneDomainPack(p)
	for i := range p.Entries {
		for j, symbol := range p.Entries[i].Symbols {
			canonical, err := canonicalPublicSymbol(symbol)
			if err == nil {
				p.Entries[i].Symbols[j] = canonical
			}
		}
	}
	return p
}
