package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Tests use explicit temporary directories, without changing HOME or cwd.
func isolateCompilerLibraries(t *testing.T) (string, string) {
	t.Helper()
	domainsMutex.Lock()
	oldDomains, oldErrors := loadedDomains, domainFileErrors
	domainsMutex.Unlock()
	t.Cleanup(func() {
		domainsMutex.Lock()
		loadedDomains, domainFileErrors = oldDomains, oldErrors
		domainsMutex.Unlock()
	})
	globalDir, workspaceDir := t.TempDir(), t.TempDir()
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err != nil {
		t.Fatal(err)
	}
	return globalDir, workspaceDir
}

func writeDomainFixture(t *testing.T, dir string, p DomainPack) {
	t.Helper()
	p.Source, p.FilePath = "", ""
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDomainCompositionKeepsCommonRulesAndRelevantEntries(t *testing.T) {
	isolateCompilerLibraries(t)
	text := "登录成功后，仍需校验用户是否有权删除该订单。禁止绕过权限。"
	composed, err := composeDialect("standard", []string{"application-security"}, text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(composed.Prompt, commonCompilerRules) {
		t.Fatal("common rules missing from composed prompt")
	}
	if !strings.Contains(composed.Guidance, `"公开表达":"授权"`) {
		t.Fatal("matching authorization expression not selected")
	}
	if strings.Contains(composed.Prompt, `"公开表达":"授权"`) {
		t.Fatal("untrusted domain data leaked into the system policy")
	}
	if strings.Contains(composed.Guidance, "Idempotency-Key") || strings.Contains(composed.Guidance, `"id":`) {
		t.Fatal("unselected domains or internal identifiers leaked into guidance")
	}
	if composed.PromptHash == getDialect("standard").PromptHash {
		t.Fatal("composition did not change prompt identity")
	}
	unrelated, err := composeDialect("standard", []string{"application-security"}, "花园里有一朵玫瑰")
	if err != nil {
		t.Fatal(err)
	}
	if unrelated.Guidance != "" {
		t.Fatal("unrelated entries should not be injected")
	}
}

func TestDomainDefaultsAndExplicitSelection(t *testing.T) {
	isolateCompilerLibraries(t)
	packs, err := resolveDomainPacks("code-audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, p := range packs {
		names[p.Name] = true
	}
	if !names["common"] || !names["application-security"] || len(names) != 2 {
		t.Fatalf("unexpected defaults: %#v", names)
	}
	packs, err = resolveDomainPacks("code-audit", []string{"api-contract", "api-contract"})
	if err != nil {
		t.Fatal(err)
	}
	names = make(map[string]bool)
	for _, p := range packs {
		names[p.Name] = true
	}
	if !names["common"] || !names["api-contract"] || names["application-security"] || len(names) != 2 {
		t.Fatalf("explicit domains did not replace mode default: %#v", names)
	}
}

func TestDomainCompositionRejectsUnknownSelection(t *testing.T) {
	isolateCompilerLibraries(t)
	for _, test := range []struct {
		mode    string
		domains []string
	}{
		{"missing", nil}, {"../standard", nil}, {"standard", []string{"missing"}},
		{"standard", []string{"../common"}}, {"standard", []string{""}},
	} {
		if _, err := composeDialect(test.mode, test.domains, "text"); err == nil {
			t.Fatalf("invalid selection accepted: %#v", test)
		}
	}
}

func TestDomainRelevanceBoundariesAndBudget(t *testing.T) {
	for _, test := range []struct {
		text, term string
		want       bool
	}{
		{"order", "or", false}, {"a or b", "or", true}, {"postgres", "post", false},
		{"post /orders", "post", true}, {"expected_version", "version", false},
		{"禁止修改订单", "禁止", true}, {"金额 >= 5", ">=", true},
	} {
		if got := domainTermMatches(test.text, test.term); got != test.want {
			t.Fatalf("match %q in %q = %v", test.term, test.text, got)
		}
	}
	pack := DomainPack{Name: "custom"}
	for i := 0; i < 20; i++ {
		pack.Entries = append(pack.Entries, DomainEntry{ID: string(rune('a' + i)), Canonical: "处理事项", UseWhen: []string{"事项明确时"}})
	}
	guidance := selectDomainGuidance([]DomainPack{pack}, "处理事项")
	if got := strings.Count(guidance, `"公开表达":`); got != maxSelectedDomainEntries {
		t.Fatalf("entry budget not respected: %d", got)
	}
	pack.Entries[0].UseWhen = []string{strings.Repeat("长", maxDomainGuidanceRunes+1)}
	guidance = selectDomainGuidance([]DomainPack{pack}, "处理事项")
	if utf8.RuneCountInString(guidance) > maxDomainGuidanceRunes {
		t.Fatal("guidance character budget exceeded")
	}
	if strings.Contains(guidance, strings.Repeat("长", 20)) || guidance == "" {
		t.Fatal("oversized entry should be skipped without discarding smaller candidates")
	}
}

func TestDomainOverridesAndMalformedSelectedPack(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	global := cloneDomainPack(builtinDomainPacks["application-security"])
	global.Label = "global label"
	writeDomainFixture(t, globalDir, global)
	workspace := cloneDomainPack(global)
	workspace.Label = "workspace label"
	writeDomainFixture(t, workspaceDir, workspace)
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err != nil {
		t.Fatal(err)
	}
	packs, err := resolveDomainPacks("standard", []string{"application-security"})
	if err != nil {
		t.Fatal(err)
	}
	if packs[0].Name != "application-security" || packs[0].Label != "workspace label" {
		t.Fatalf("workspace did not override global: %#v", packs)
	}
	path := filepath.Join(workspaceDir, "application-security.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err == nil {
		t.Fatal("malformed file not reported")
	}
	if _, err := composeDialect("standard", []string{"application-security"}, "权限检查"); err == nil {
		t.Fatal("selected malformed override silently fell back")
	}
	if _, err := composeDialect("standard", []string{"api-contract"}, "POST /orders"); err != nil {
		t.Fatalf("unrelated malformed domain blocked valid selection: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err != nil {
		t.Fatal(err)
	}
	packs, err = resolveDomainPacks("standard", []string{"application-security"})
	if err != nil || packs[0].Label != "global label" {
		t.Fatal("removing workspace override did not restore global")
	}
}

func TestDomainValidationAndCacheIdentity(t *testing.T) {
	isolateCompilerLibraries(t)
	valid := cloneDomainPack(builtinDomainPacks["api-contract"])
	b, _ := json.Marshal(valid)
	if _, err := parseDomainJSON(append(b, []byte("{}")...)); err == nil {
		t.Fatal("multiple JSON objects accepted")
	}
	withUnknown := strings.TrimSuffix(string(b), "}") + `,"extra":true}`
	if _, err := parseDomainJSON([]byte(withUnknown)); err == nil {
		t.Fatal("unknown schema field accepted")
	}
	bad := cloneDomainPack(valid)
	bad.Entries[0].UseWhen = nil
	if err := validateDomainPack(bad); err == nil {
		t.Fatal("missing applicability accepted")
	}
	bad = cloneDomainPack(valid)
	bad.Entries = append(bad.Entries, bad.Entries[0])
	if err := validateDomainPack(bad); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
	if _, err := domainScopeDir("elsewhere"); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if err := deleteDomain("../common", "workspace"); err == nil {
		t.Fatal("invalid deletion path accepted")
	}

	before, err := composeDialect("standard", []string{"api-contract"}, "POST /orders")
	if err != nil {
		t.Fatal(err)
	}
	domainsMutex.Lock()
	changed := cloneDomainPack(loadedDomains["api-contract"])
	changed.Entries[0].Preserve = append(changed.Entries[0].Preserve, "原文中的响应头约束")
	loadedDomains["api-contract"] = changed
	domainsMutex.Unlock()
	after, err := composeDialect("standard", []string{"api-contract"}, "POST /orders")
	if err != nil {
		t.Fatal(err)
	}
	if before.PromptHash == after.PromptHash {
		t.Fatal("changed domain content reused cache identity without a version bump")
	}
	repeated, err := composeDialect("standard", []string{"api-contract"}, "POST /orders")
	if err != nil || repeated.PromptHash != after.PromptHash {
		t.Fatal("composition identity is nondeterministic")
	}
}

func TestDomainListDoesNotExposeMutableRegistry(t *testing.T) {
	isolateCompilerLibraries(t)
	packs := listSortedDomains()
	if len(packs) != 4 || packs[0].Name != "common" {
		t.Fatalf("unexpected builtins: %#v", packs)
	}
	packs[0].Entries[0].Canonical = "mutated outside"
	again := listSortedDomains()
	if again[0].Entries[0].Canonical == "mutated outside" {
		t.Fatal("list mutation affected registry")
	}
}

func TestDomainConcurrentReloadAndComposition(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := composeDialect("standard", []string{"api-contract"}, "POST /orders 必须携带 Idempotency-Key"); err != nil {
				t.Error(err)
				return
			}
			if len(listSortedDomains()) != 4 {
				t.Error("reload published incomplete domain registry")
				return
			}
		}
	}()
	wg.Wait()
}
