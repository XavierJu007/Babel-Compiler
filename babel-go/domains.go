package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	domainSchemaVersion      = 1
	maxDomainFileBytes       = 256 * 1024
	maxSelectedDomainEntries = 8
	maxDomainGuidanceRunes   = 6000
)

type DomainSymbol struct {
	Symbol  string `json:"symbol"`
	Meaning string `json:"meaning"`
}

type DomainExample struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type DomainEntry struct {
	ID             string          `json:"id"`
	Canonical      string          `json:"canonical"`
	Aliases        []string        `json:"aliases,omitempty"`
	Triggers       []string        `json:"triggers,omitempty"`
	UseWhen        []string        `json:"use_when"`
	Preserve       []string        `json:"preserve,omitempty"`
	DoNotMergeWith []string        `json:"do_not_merge_with,omitempty"`
	Symbols        []DomainSymbol  `json:"symbols,omitempty"`
	Examples       []DomainExample `json:"examples,omitempty"`
}

type DomainPack struct {
	SchemaVersion int           `json:"schema_version"`
	Name          string        `json:"name"`
	Label         string        `json:"label"`
	Version       int           `json:"version"`
	Entries       []DomainEntry `json:"entries"`
	Source        string        `json:"source,omitempty"`
	FilePath      string        `json:"file_path,omitempty"`
}

//go:embed domains_builtin.json
var builtinDomainsJSON []byte

var (
	domainsLifecycleMutex sync.Mutex // serialize file mutation, scanning and publication
	domainsMutex          sync.RWMutex
	loadedDomains         map[string]DomainPack
	domainFileErrors      map[string]error
	builtinDomainPacks    = mustLoadBuiltinDomains()
)

func globalDomainsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "domains"
	}
	return filepath.Join(home, ".babel_domains")
}

func workspaceDomainsDir() string { return "domains" }

func validDomainName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validateDomainPack(p DomainPack) error {
	if p.SchemaVersion != domainSchemaVersion {
		return fmt.Errorf("schema_version 必须为 %d", domainSchemaVersion)
	}
	if !validDomainName(p.Name) {
		return fmt.Errorf("领域名称须以小写字母开头，仅含小写字母、数字、下划线或连字符，最长64字节")
	}
	if strings.TrimSpace(p.Label) == "" || utf8.RuneCountInString(p.Label) > 120 {
		return fmt.Errorf("领域 label 不能为空且最长120字符")
	}
	if p.Version < 1 {
		return fmt.Errorf("领域 version 必须为正整数")
	}
	if len(p.Entries) == 0 || len(p.Entries) > 128 {
		return fmt.Errorf("领域须包含1至128个词条")
	}
	seen := make(map[string]bool)
	for _, e := range p.Entries {
		if strings.TrimSpace(e.ID) == "" || len(e.ID) > 128 || seen[e.ID] {
			return fmt.Errorf("词条 id 不能为空、重复或超过128字节: %q", e.ID)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Canonical) == "" || utf8.RuneCountInString(e.Canonical) > 240 {
			return fmt.Errorf("词条 %s 的 canonical 不能为空且最长240字符", e.ID)
		}
		if len(e.UseWhen) == 0 {
			return fmt.Errorf("词条 %s 必须提供 use_when 适用条件", e.ID)
		}
		for _, group := range [][]string{e.Aliases, e.Triggers, e.UseWhen, e.Preserve, e.DoNotMergeWith} {
			if len(group) > 32 {
				return fmt.Errorf("词条 %s 的单个列表最多32项", e.ID)
			}
			for _, item := range group {
				if strings.TrimSpace(item) == "" || utf8.RuneCountInString(item) > 600 {
					return fmt.Errorf("词条 %s 的列表项不能为空且最长600字符", e.ID)
				}
			}
		}
		if len(e.Symbols) > 16 || len(e.Examples) > 4 {
			return fmt.Errorf("词条 %s 最多16个符号和4个示例", e.ID)
		}
		for _, symbol := range e.Symbols {
			if strings.TrimSpace(symbol.Symbol) == "" || utf8.RuneCountInString(symbol.Symbol) > 24 ||
				strings.TrimSpace(symbol.Meaning) == "" || utf8.RuneCountInString(symbol.Meaning) > 600 {
				return fmt.Errorf("词条 %s 的符号及含义无效", e.ID)
			}
			if _, err := canonicalPublicSymbol(symbol); err != nil {
				return fmt.Errorf("词条 %s: %w", e.ID, err)
			}
		}
		for _, example := range e.Examples {
			if strings.TrimSpace(example.Input) == "" || strings.TrimSpace(example.Output) == "" ||
				utf8.RuneCountInString(example.Input) > 1600 || utf8.RuneCountInString(example.Output) > 1600 {
				return fmt.Errorf("词条 %s 的示例输入/输出不能为空且最长1600字符", e.ID)
			}
		}
	}
	b, _ := json.Marshal(p)
	if len(b) > maxDomainFileBytes {
		return fmt.Errorf("领域文件超过 %d 字节", maxDomainFileBytes)
	}
	return nil
}

func mustLoadBuiltinDomains() map[string]DomainPack {
	var packs []DomainPack
	if err := json.Unmarshal(builtinDomainsJSON, &packs); err != nil {
		panic("invalid builtin domains: " + err.Error())
	}
	out := make(map[string]DomainPack, len(packs))
	for _, p := range packs {
		if err := validateDomainPack(p); err != nil {
			panic("invalid builtin domain " + p.Name + ": " + err.Error())
		}
		if _, exists := out[p.Name]; exists {
			panic("duplicate builtin domain: " + p.Name)
		}
		p.Source = "builtin"
		p.FilePath = ""
		out[p.Name] = p
	}
	return out
}

func cloneDomainPack(p DomainPack) DomainPack {
	b, _ := json.Marshal(p)
	var cloned DomainPack
	_ = json.Unmarshal(b, &cloned)
	return cloned
}

func parseDomainJSON(b []byte) (DomainPack, error) {
	var p DomainPack
	if len(b) > maxDomainFileBytes {
		return p, fmt.Errorf("领域文件超过 %d 字节", maxDomainFileBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, fmt.Errorf("领域JSON无效: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("领域JSON只能包含一个对象")
	}
	if err := validateDomainPack(p); err != nil {
		return p, err
	}
	p.Source, p.FilePath = "", ""
	return normalizeDomainSymbols(p), nil
}

func scanDomainDir(dir, source string, target map[string]DomainPack, invalid map[string]error) []error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []error{fmt.Errorf("读取领域目录 %s: %w", dir, err)}
	}
	var problems []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(dir, entry.Name())
		var p DomainPack
		var readErr error
		info, infoErr := entry.Info()
		switch {
		case !entry.Type().IsRegular():
			readErr = fmt.Errorf("领域文件必须是普通文件")
		case !validDomainName(name):
			readErr = fmt.Errorf("无效的领域文件名")
		case infoErr != nil:
			readErr = infoErr
		case info.Size() > maxDomainFileBytes:
			readErr = fmt.Errorf("领域文件超过 %d 字节", maxDomainFileBytes)
		default:
			b, err := os.ReadFile(path)
			if err != nil {
				readErr = err
			} else {
				p, readErr = parseDomainJSON(b)
			}
			if readErr == nil && p.Name != name {
				readErr = fmt.Errorf("文件名与领域 name 不一致")
			}
		}
		if readErr != nil {
			problem := fmt.Errorf("%s: %w", path, readErr)
			problems = append(problems, problem)
			if validDomainName(name) {
				invalid[name] = problem
			}
			continue
		}
		p.Source, p.FilePath = source, path
		target[name] = p
		delete(invalid, name)
	}
	return problems
}

// Valid packs remain available when another pack is broken. A selected broken
// override is rejected by composition instead of silently using its builtin.
func reloadDomains() (int, error) {
	return reloadDomainsFromDirs(globalDomainsDir(), workspaceDomainsDir())
}

func reloadDomainsFromDirs(globalDir, workspaceDir string) (int, error) {
	domainsLifecycleMutex.Lock()
	defer domainsLifecycleMutex.Unlock()
	return reloadDomainsFromDirsLocked(globalDir, workspaceDir)
}

// The lifecycle lock covers the entire snapshot, not just the final assignment:
// an earlier slow scan must not overwrite a newer completed reload/save.
func reloadDomainsFromDirsLocked(globalDir, workspaceDir string) (int, error) {
	next := make(map[string]DomainPack, len(builtinDomainPacks))
	for name, p := range builtinDomainPacks {
		next[name] = cloneDomainPack(p)
	}
	invalid := make(map[string]error)
	problems := scanDomainDir(globalDir, "global", next, invalid)
	problems = append(problems, scanDomainDir(workspaceDir, "workspace", next, invalid)...)
	domainsMutex.Lock()
	loadedDomains, domainFileErrors = next, invalid
	domainsMutex.Unlock()
	return len(next), errors.Join(problems...)
}

func ensureDomainsLoaded() {
	domainsMutex.RLock()
	empty := loadedDomains == nil
	domainsMutex.RUnlock()
	if empty {
		_, _ = reloadDomains()
	}
}

func listSortedDomains() []DomainPack {
	ensureDomainsLoaded()
	domainsMutex.RLock()
	packs := make([]DomainPack, 0, len(loadedDomains))
	for _, p := range loadedDomains {
		packs = append(packs, cloneDomainPack(p))
	}
	domainsMutex.RUnlock()
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].Name == "common" {
			return true
		}
		if packs[j].Name == "common" {
			return false
		}
		return packs[i].Name < packs[j].Name
	})
	return packs
}

func domainScopeDir(scope string) (string, error) {
	switch scope {
	case "global":
		return globalDomainsDir(), nil
	case "workspace":
		return workspaceDomainsDir(), nil
	default:
		return "", fmt.Errorf("领域保存位置仅支持 global 或 workspace")
	}
}

func saveDomain(p DomainPack, scope string) error {
	p.Source, p.FilePath = "", ""
	if err := validateDomainPack(p); err != nil {
		return err
	}
	p = normalizeDomainSymbols(p)
	dir, err := domainScopeDir(scope)
	if err != nil {
		return err
	}
	domainsLifecycleMutex.Lock()
	defer domainsLifecycleMutex.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if len(b)+1 > maxDomainFileBytes {
		return fmt.Errorf("领域文件超过 %d 字节", maxDomainFileBytes)
	}
	f, err := os.CreateTemp(dir, ".domain-*.tmp")
	if err != nil {
		return err
	}
	tempName := f.Name()
	defer os.Remove(tempName)
	if err := f.Chmod(0644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filepath.Join(dir, p.Name+".json")); err != nil {
		return err
	}
	// Unrelated invalid files remain visible through the API reload warning.
	// They do not turn a successful write into a misleading save failure.
	_, _ = reloadDomainsFromDirsLocked(globalDomainsDir(), workspaceDomainsDir())
	return nil
}

func deleteDomain(name, scope string) error {
	if !validDomainName(name) {
		return fmt.Errorf("无效的领域名称")
	}
	dir, err := domainScopeDir(scope)
	if err != nil {
		return err
	}
	domainsLifecycleMutex.Lock()
	defer domainsLifecycleMutex.Unlock()
	err = os.Remove(filepath.Join(dir, name+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = reloadDomainsFromDirsLocked(globalDomainsDir(), workspaceDomainsDir())
	return nil
}

func resolveDomainPacks(mode string, names []string) ([]DomainPack, error) {
	if len(names) > 16 {
		return nil, fmt.Errorf("每次最多选择16个领域")
	}
	ensureDomainsLoaded()
	selected := map[string]bool{"common": true}
	if len(names) == 0 && mode == "code-audit" {
		selected["application-security"] = true
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if !validDomainName(name) {
			return nil, fmt.Errorf("无效的领域名称: %q", name)
		}
		selected[name] = true
	}
	ordered := make([]string, 0, len(selected))
	for name := range selected {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	domainsMutex.RLock()
	defer domainsMutex.RUnlock()
	packs := make([]DomainPack, 0, len(ordered))
	for _, name := range ordered {
		if err := domainFileErrors[name]; err != nil {
			return nil, fmt.Errorf("领域 %s 加载失败: %w", name, err)
		}
		p, ok := loadedDomains[name]
		if !ok {
			return nil, fmt.Errorf("领域不存在: %s", name)
		}
		p = cloneDomainPack(p)
		// Storage location and UI metadata do not affect compilation identity.
		p.Source, p.FilePath = "", ""
		packs = append(packs, p)
	}
	return packs, nil
}

// ASCII word edges need boundaries, including phrases and terms with punctuation
// ("read-only" must not match "thread-only"). Chinese and symbol edges still
// match directly, so ordinary mixed-language text does not require added spaces.
func domainTermMatches(text, term string) bool {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return false
	}
	wordByte := func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' }
	needsStartBoundary := wordByte(term[0])
	needsEndBoundary := wordByte(term[len(term)-1])
	if !needsStartBoundary && !needsEndBoundary {
		return strings.Contains(text, term)
	}
	for start := 0; start < len(text); {
		index := strings.Index(text[start:], term)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(term)
		if (!needsStartBoundary || index == 0 || !wordByte(text[index-1])) &&
			(!needsEndBoundary || end == len(text) || !wordByte(text[end])) {
			return true
		}
		start = index + 1
	}
	return false
}

func domainEntryScore(e DomainEntry, text string) int {
	score := 0
	if domainTermMatches(text, e.Canonical) {
		score += 6
	}
	seen := make(map[string]bool)
	for _, term := range append(append([]string{}, e.Aliases...), e.Triggers...) {
		normalized := strings.ToLower(strings.TrimSpace(term))
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		if domainTermMatches(text, normalized) {
			score += 2
		}
	}
	for _, symbol := range e.Symbols {
		if strings.Contains(text, strings.ToLower(symbol.Symbol)) {
			score++
		}
	}
	return score
}

func selectDomainGuidance(packs []DomainPack, text string) string {
	type candidate struct {
		domain string
		entry  DomainEntry
		score  int
	}
	text = strings.ToLower(text)
	var candidates []candidate
	for _, p := range packs {
		for _, entry := range p.Entries {
			score := domainEntryScore(entry, text)
			if score == 0 {
				continue
			}
			if p.Name != "common" {
				score += 20
			}
			candidates = append(candidates, candidate{p.Name, entry, score})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].domain != candidates[j].domain {
			return candidates[i].domain < candidates[j].domain
		}
		return candidates[i].entry.ID < candidates[j].entry.ID
	})
	var selected []string
	total := 0
	for _, candidate := range candidates {
		if len(selected) >= maxSelectedDomainEntries {
			break
		}
		// Match-only metadata and internal IDs are not necessary in the prompt.
		e := candidate.entry
		snippet, _ := json.Marshal(struct {
			Canonical      string          `json:"公开表达"`
			UseWhen        []string        `json:"适用条件"`
			Preserve       []string        `json:"原文有则保留"`
			DoNotMergeWith []string        `json:"不可混淆"`
			Symbols        []DomainSymbol  `json:"公开符号,omitempty"`
			Examples       []DomainExample `json:"示例,omitempty"`
		}{e.Canonical, e.UseWhen, e.Preserve, e.DoNotMergeWith, e.Symbols, e.Examples})
		size := utf8.RuneCount(snippet) + 1
		if total+size > maxDomainGuidanceRunes {
			continue
		}
		total += size
		selected = append(selected, string(snippet))
	}
	return strings.Join(selected, "\n")
}
