package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func symbolTestPack(name string, symbols []DomainSymbol) DomainPack {
	return DomainPack{SchemaVersion: 1, Name: name, Label: name, Version: 1, Entries: []DomainEntry{{ID: "orders", Canonical: "订单", UseWhen: []string{"原文涉及订单时"}, Symbols: symbols}}}
}

func TestPublicSymbolCatalogPreservesBuiltinsAndReturnsCopy(t *testing.T) {
	symbols := listPublicSymbols()
	found := make(map[string]string)
	for _, symbol := range symbols {
		if _, duplicate := found[symbol.Symbol]; duplicate {
			t.Fatalf("duplicate symbol %q", symbol.Symbol)
		}
		found[symbol.Symbol] = symbol.Meaning
	}
	for _, name := range []string{"∵", "∧", "∨", "⇒", "⇔", "¬", "∈", "∉", "⊆", "∅", "→", ">=", "<", "<=", ">", "==", "!=", "&&", "||", "!", "∀", "∃", "∩", "∪", "≤", "≥"} {
		if found[name] == "" {
			t.Errorf("required public symbol missing: %s", name)
		}
	}
	for _, pack := range builtinDomainPacks {
		for _, entry := range pack.Entries {
			for _, symbol := range entry.Symbols {
				if found[symbol.Symbol] != symbol.Meaning {
					t.Errorf("builtin meaning changed: %s", symbol.Symbol)
				}
			}
		}
	}
	symbols[0].Meaning = "private replacement"
	if listPublicSymbols()[0].Meaning == "private replacement" {
		t.Fatal("caller modified immutable public catalog")
	}
}

func TestPrivateAndRedefinedSymbolsAreRejected(t *testing.T) {
	for _, symbol := range []DomainSymbol{
		{Symbol: "A17", Meaning: "原文第三段第二句"},
		{Symbol: "π@", Meaning: "当前执行人"},
		{Symbol: "∧", Meaning: "删除订单"},
		{Symbol: "∈", Meaning: "左侧操作必须先于右侧操作"},
		{Symbol: "∵", Meaning: "只要左侧发生就必须执行右侧动作"},
		{Symbol: "→", Meaning: "许可自动提升为义务"},
	} {
		t.Run(symbol.Symbol, func(t *testing.T) {
			pack := symbolTestPack("review-symbols", []DomainSymbol{symbol})
			if err := validateDomainPack(pack); err == nil {
				t.Fatal("private or redefined symbol passed validation")
			}
			raw, _ := json.Marshal(pack)
			if _, err := parseDomainJSON(raw); err == nil {
				t.Fatal("private or redefined symbol loaded from JSON")
			}
			// Validation precedes filesystem access, including for an invalid scope.
			if err := saveDomain(pack, "invalid-scope"); err == nil || strings.Contains(err.Error(), "保存位置") {
				t.Fatalf("save did not reject symbol before filesystem dispatch: %v", err)
			}
		})
	}
}

func TestQuickInsertSymbolsAreAvailableToDomainEditor(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`insertOperator\('([^']+)'\)`).FindAllStringSubmatch(string(html), -1)
	if len(matches) == 0 {
		t.Fatal("no quick-insert symbol controls found")
	}
	for _, match := range matches {
		if publicSymbolCatalog.meanings[match[1]] == "" {
			t.Errorf("quick-insert symbol %q is unavailable in the domain editor", match[1])
		}
	}
}

func TestLegacyOfficialArrowMeaningsNormalizeOnLoadAndSave(t *testing.T) {
	isolateDomainIntegration(t)
	for i, symbol := range []DomainSymbol{
		{Symbol: "→", Meaning: "在明确标注的关系中表示方向；不能省略其关系类型"},
		{Symbol: "->", Meaning: "在明确标注的关系中表示方向的ASCII写法；不能省略其关系类型"},
	} {
		name := []string{"legacy-arrow", "legacy-ascii-arrow"}[i]
		pack := symbolTestPack(name, []DomainSymbol{symbol})
		raw, err := json.Marshal(pack)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := parseDomainJSON(raw)
		if err != nil {
			t.Fatalf("previously valid official symbol failed to load: %v", err)
		}
		want := publicSymbolCatalog.meanings[symbol.Symbol]
		if loaded.Entries[0].Symbols[0].Meaning != want {
			t.Fatalf("legacy meaning was not normalized on load: %+v", loaded.Entries[0].Symbols[0])
		}
		if err := saveDomain(pack, "global"); err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(filepath.Join(globalDomainsDir(), name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved DomainPack
		if err := json.Unmarshal(written, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Entries[0].Symbols[0].Meaning != want {
			t.Fatalf("legacy meaning was not normalized on save: %+v", saved.Entries[0].Symbols[0])
		}
	}
}

func TestPublicSymbolWhitespaceNormalizesWithoutChangingContext(t *testing.T) {
	symbol := listPublicSymbols()[0]
	padded := DomainSymbol{Symbol: "  " + symbol.Symbol + "\n", Meaning: " \n" + symbol.Meaning + "  "}
	pack := symbolTestPack("review-symbols", []DomainSymbol{padded})
	pack.Entries[0].Preserve = []string{"订单业务范围"}
	raw, _ := json.Marshal(pack)
	parsed, err := parseDomainJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Entries[0].Symbols[0] != symbol {
		t.Fatalf("not canonicalized: %+v", parsed.Entries[0].Symbols[0])
	}
	if parsed.Entries[0].Preserve[0] != "订单业务范围" {
		t.Fatal("business applicability changed")
	}
	if pack.Entries[0].Symbols[0] != padded {
		t.Fatal("normalization mutated caller input")
	}
}

func TestConflictingSymbolOverrideCannotEnterComposition(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	symbol := DomainSymbol{Symbol: "∈", Meaning: publicSymbolCatalog.meanings["∈"]}
	first := symbolTestPack("review-first", []DomainSymbol{symbol})
	second := symbolTestPack("review-second", []DomainSymbol{{Symbol: "∈", Meaning: "左侧操作必须先于右侧操作"}})
	writeDomainFixture(t, globalDir, first)
	// Write malformed data directly to exercise the loader, not the save validator.
	raw, _ := json.Marshal(second)
	if err := os.WriteFile(filepath.Join(workspaceDir, second.Name+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err == nil {
		t.Fatal("conflicting public meaning was not reported")
	}
	if _, err := composeDialect("standard", []string{first.Name, second.Name}, "订单"); err == nil {
		t.Fatal("conflicting selected pack composed successfully")
	}
	if _, err := composeDialect("standard", []string{first.Name}, "订单"); err != nil {
		t.Fatalf("unrelated invalid pack blocked valid selection: %v", err)
	}
}

func TestDomainCanCombineCatalogSymbols(t *testing.T) {
	pack := symbolTestPack("review-combination", nil)
	for _, name := range []string{"∵", "∀", "∈", "∧", ">=", "≤", "∩", "∪"} {
		pack.Entries[0].Symbols = append(pack.Entries[0].Symbols, DomainSymbol{Symbol: name, Meaning: publicSymbolCatalog.meanings[name]})
	}
	if err := validateDomainPack(pack); err != nil {
		t.Fatalf("valid public combination rejected: %v", err)
	}
	guidance := selectDomainGuidance([]DomainPack{pack}, "订单")
	for _, symbol := range pack.Entries[0].Symbols {
		if !strings.Contains(guidance, symbol.Meaning) {
			t.Errorf("selected symbol meaning missing: %s", symbol.Symbol)
		}
	}
}
