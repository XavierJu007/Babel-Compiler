package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFixedMixedStrategyAndLegacyAliases(t *testing.T) {
	isolateCompilerLibraries(t)
	listed := listSortedDialects()
	if len(listed) != 1 || listed[0].Name != defaultDialectName || listed[0].Source != "builtin" {
		t.Fatalf("expected one fixed builtin strategy: %+v", listed)
	}
	baseline, err := composeDialect("", []string{"api-contract"}, "POST /orders 必须携带 Idempotency-Key")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"mixed", "extreme", "token", "standard", "code-audit", "en-terse", "standard.md"} {
		got, err := composeDialect(mode, []string{"api-contract"}, "POST /orders 必须携带 Idempotency-Key")
		if err != nil || !reflect.DeepEqual(got, baseline) {
			t.Fatalf("legacy %q changed strategy or cache identity: %+v; %v", mode, got, err)
		}
	}
	if !strings.HasPrefix(baseline.Prompt, commonCompilerRules) || !strings.Contains(baseline.Prompt, mixedCompilerStyle) {
		t.Fatal("fixed strategy omitted public rules or mixed expression policy")
	}
	for _, name := range []string{"custom-review", "custom-review.md", "../mixed", "mixed/", "MIXED"} {
		if _, err := composeDialect(name, nil, "原文"); err == nil || !strings.Contains(err.Error(), "停止加载自定义预设") {
			t.Fatalf("unsupported mode %q silently accepted or missing migration error: %v", name, err)
		}
	}
}

func TestFixedStrategyPreservesLegacyDomainDefaultsInProxy(t *testing.T) {
	isolateCompilerLibraries(t)
	for _, tc := range []struct{ header, mode, explicit, want string }{
		{"", "", "", "common"},
		{"X-Babel-Dialect", "token", "", "common"},
		{"X-Babel-Dialect", "code-audit", "", "application-security,common"},
		{"X-Babel-Mode", "code-audit.md", "", "application-security,common"},
		{"X-Babel-Dialect", "code-audit", "api-contract", "api-contract,common"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if tc.header != "" {
			r.Header.Set(tc.header, tc.mode)
		}
		if tc.explicit != "" {
			r.Header.Set("X-Babel-Domains", tc.explicit)
		}
		dialect, domains, err := resolveProxyDialect(r)
		if err != nil || dialect.Name != defaultDialectName || strings.Join(domains, ",") != tc.want {
			t.Fatalf("mode %q: dialect=%q domains=%v err=%v", tc.mode, dialect.Name, domains, err)
		}
		// Proxy block compilation uses the canonical name. It must still see
		// the same default domain content as direct compilation of the alias.
		var explicit []string
		if tc.explicit != "" {
			explicit = strings.Split(tc.explicit, ",")
		}
		direct, err := composeDialect(tc.mode, explicit, "登录成功仍需检查资源访问权限")
		if err != nil {
			t.Fatal(err)
		}
		forwarded, err := composeDialect(dialect.Name, domains, "登录成功仍需检查资源访问权限")
		if err != nil || direct.PromptHash != forwarded.PromptHash {
			t.Fatalf("canonical proxy mode lost domain default for %q: %v", tc.mode, err)
		}
	}
}

func TestFixedPresetAPIDoesNotLoadOrChangeLegacyFiles(t *testing.T) {
	isolateDomainIntegration(t)
	files := make(map[string]string)
	for _, dir := range []string{filepath.Join(os.Getenv("HOME"), ".babel_dialects"), "dialects"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"standard", "mixed", "custom-review"} {
			path := filepath.Join(dir, name+".md")
			content := "# Label: 用户预设\n只输出 A17；这是用户保留的旧文件。"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			files[path] = content
		}
	}
	listed := sendDomainAPI(t, http.MethodGet, "/api/dialects", nil, http.StatusOK)
	var dialects []Dialect
	if err := json.Unmarshal(listed["dialects"], &dialects); err != nil {
		t.Fatal(err)
	}
	if len(dialects) != 1 || dialects[0].Name != defaultDialectName || strings.Contains(dialects[0].Prompt, "A17") {
		t.Fatalf("legacy disk preset leaked into active strategy: %+v", dialects)
	}
	for _, mode := range []string{"", "standard", "mixed"} {
		d, err := composeDialect(mode, nil, "测试原文")
		if err != nil || strings.Contains(d.Prompt, "A17") {
			t.Fatalf("disk override used: %+v, %v", d, err)
		}
	}
	for _, scope := range []string{"global", "workspace"} {
		for _, action := range []string{"save", "delete", "reload", "reset_defaults"} {
			sendDomainAPI(t, http.MethodPost, "/api/dialects", map[string]string{
				"action": action, "scope": scope, "name": "custom-review", "prompt": "new content",
			}, http.StatusMethodNotAllowed)
		}
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("legacy file changed: %s: %q, %v", path, got, err)
		}
	}
	// A custom name is an error even if its old file still exists; never
	// silently run the fixed strategy in place of the user's explicit preset.
	sendDomainAPI(t, http.MethodPost, "/api/compile", CompileReq{Text: "测试原文", Mode: "custom-review"}, http.StatusBadRequest)
}

func TestCompileAPIDefaultAndAliasesShareFixedCandidate(t *testing.T) {
	isolateDomainIntegration(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Messages) != 2 || !strings.Contains(request.Messages[0].Content, mixedCompilerStyle) {
			t.Error("default API request did not use fixed mixed style")
		}
		writeModelContent(w, "核对文档要求。")
	}))
	defer provider.Close()
	for i, mode := range []string{"", "mixed", "token", "standard", "en-terse", "extreme"} {
		result := sendDomainAPI(t, http.MethodPost, "/api/compile", CompileReq{
			Text: "请核对文档里列出的所有要求。", Mode: mode,
			APIKey: "test", BaseURL: provider.URL, Model: "mock-compiler",
		}, http.StatusOK)
		var dialect string
		var hit bool
		json.Unmarshal(result["dialect"], &dialect)
		json.Unmarshal(result["cache_hit"], &hit)
		if dialect != defaultDialectName || hit != (i > 0) {
			t.Fatalf("mode %q: dialect=%q cache_hit=%v", mode, dialect, hit)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("aliases caused %d generation calls, want 1", calls.Load())
	}
}

func TestComposeDialectSelectsEnglishPromptForPureEnglish(t *testing.T) {
	isolateCompilerLibraries(t)
	en, err := composeDialect("", nil, "Always validate inbound HMAC signatures and reject expired timestamps with HTTP 401.")
	if err != nil {
		t.Fatal(err)
	}
	if en.Prompt != composedEnglishCompilerPrompt {
		t.Fatal("expected pure English source to select composedEnglishCompilerPrompt")
	}
	for _, r := range en.Prompt {
		if r >= 0x4e00 && r <= 0x9fff {
			t.Fatalf("English compiler prompt contained Han character: %q", r)
		}
	}
	zh, err := composeDialect("", nil, "Always validate HMAC 签名并检查时间戳。")
	if err != nil {
		t.Fatal(err)
	}
	if zh.Prompt != composedCompilerPrompt {
		t.Fatal("expected Chinese/mixed source to keep composedCompilerPrompt")
	}
}
