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

func isolateDomainIntegration(t *testing.T) {
	t.Helper()
	isolateCompilerState(t)
	t.Setenv("HOME", t.TempDir())
	isolateCompilerLibraries(t)
}

func sendDomainAPI(t *testing.T, method, path string, payload any, wantStatus int) map[string]json.RawMessage {
	t.Helper()
	var encoded []byte
	if payload != nil {
		encoded, _ = json.Marshal(payload)
	}
	w := httptest.NewRecorder()
	newServerMux().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(string(encoded))))
	if w.Code != wantStatus {
		t.Fatalf("%s %s returned %d; want %d: %s", method, path, w.Code, wantStatus, w.Body.String())
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDomainAPIListsSymbolsAndScopesOverrides(t *testing.T) {
	isolateDomainIntegration(t)
	listed := sendDomainAPI(t, http.MethodGet, "/api/domains", nil, http.StatusOK)
	var symbols []DomainSymbol
	if err := json.Unmarshal(listed["public_symbols"], &symbols); err != nil || len(symbols) == 0 {
		t.Fatalf("missing public symbol choices: %v", err)
	}
	pack := DomainPack{SchemaVersion: 1, Name: "review-policy", Label: "全局审查规范", Version: 1,
		Entries: []DomainEntry{{ID: "scope", Canonical: "审查范围", Triggers: []string{"审查"}, UseWhen: []string{"原文限定审查范围时"}}}}
	sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "save", "scope": "global", "domain": pack}, http.StatusOK)
	pack.Label = "工作区审查规范"
	sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "save", "scope": "workspace", "domain": pack}, http.StatusOK)
	sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "delete", "scope": "global", "name": pack.Name}, http.StatusOK)
	if _, err := os.Stat(filepath.Join(workspaceDomainsDir(), pack.Name+".json")); err != nil {
		t.Fatalf("deleting global removed workspace override: %v", err)
	}
	packs, err := resolveDomainPacks("standard", []string{pack.Name})
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range packs {
		if selected.Name == pack.Name && selected.Label != pack.Label {
			t.Fatalf("wrong surviving override: %+v", selected)
		}
	}
	sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "delete", "scope": "workspace", "name": pack.Name}, http.StatusOK)
	if _, err := resolveDomainPacks("standard", []string{pack.Name}); err == nil {
		t.Fatal("deleted custom pack still loaded")
	}
	pack.Entries[0].Symbols = []DomainSymbol{{Symbol: "A17", Meaning: "第三段第二句"}}
	sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "save", "scope": "global", "domain": pack}, http.StatusBadRequest)
	if _, err := os.Stat(filepath.Join(globalDomainsDir(), pack.Name+".json")); !os.IsNotExist(err) {
		t.Fatal("private symbol pack was written")
	}
}

func TestCompileAPIComposesDomainsAndRefreshesCandidateIdentity(t *testing.T) {
	isolateDomainIntegration(t)
	pack := DomainPack{SchemaVersion: 1, Name: "access-policy", Label: "资源访问", Version: 1,
		Entries: []DomainEntry{{ID: "access", Canonical: "资源访问权限", Triggers: []string{"权限"}, UseWhen: []string{"原文说明资源访问权限时"}}}}
	save := func() {
		sendDomainAPI(t, http.MethodPost, "/api/domains", map[string]any{"action": "save", "scope": "workspace", "domain": pack}, http.StatusOK)
	}
	save()
	var calls atomic.Int32
	prompts := make(chan compilerRequestBody, 4)
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		json.NewDecoder(r.Body).Decode(&body)
		calls.Add(1)
		prompts <- body
		writeModelContent(w, "检查用户访问资源的权限。")
	}))
	defer compiler.Close()
	req := CompileReq{Text: "登录成功后仍需检查用户访问此资源的权限。", Mode: "standard", Domains: []string{pack.Name}, APIKey: "test", BaseURL: compiler.URL, Model: "compiler-model"}
	first := sendDomainAPI(t, http.MethodPost, "/api/compile", req, http.StatusOK)
	var cached bool
	json.Unmarshal(first["cache_hit"], &cached)
	if cached {
		t.Fatal("first candidate unexpectedly cached")
	}
	prompt := <-prompts
	input := decodeCompilerInput(t, prompt.Messages[1].Content)
	if !strings.Contains(input.DomainGuidance, `"公开表达":"资源访问权限"`) || !strings.Contains(prompt.Messages[0].Content, commonCompilerRules) || strings.Contains(input.DomainGuidance, `"id":"access"`) || strings.Contains(prompt.Messages[0].Content, `"公开表达":"资源访问权限"`) {
		t.Fatal("domain data/public rules boundary missing, or internal ID leaked")
	}
	sendDomainAPI(t, http.MethodPost, "/api/compile", req, http.StatusOK)
	if calls.Load() != 1 {
		t.Fatal("unchanged configuration did not reuse candidate")
	}
	// Even without bumping a user-maintained version, content edits invalidate it.
	pack.Entries[0].Preserve = []string{"原文资源名称"}
	save()
	sendDomainAPI(t, http.MethodPost, "/api/compile", req, http.StatusOK)
	if calls.Load() != 2 {
		t.Fatal("edited domain reused an obsolete candidate")
	}
	updated := <-prompts
	if !strings.Contains(decodeCompilerInput(t, updated.Messages[1].Content).DomainGuidance, "原文资源名称") {
		t.Fatal("edited domain did not reach compiler")
	}
	req.Domains = []string{"does-not-exist"}
	sendDomainAPI(t, http.MethodPost, "/api/compile", req, http.StatusBadRequest)
	if calls.Load() != 2 {
		t.Fatal("invalid domain selection called model")
	}
}

func TestProxyDomainHeaderIsValidatedForBothProtocols(t *testing.T) {
	isolateDomainIntegration(t)
	for _, mode := range []string{"token", "code-audit"} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Babel-Dialect", mode)
		r.Header.Set("X-Babel-Domains", "application-security, api-contract")
		selected, domains, err := resolveProxyDialect(r)
		if err != nil || selected.Name != defaultDialectName || strings.Join(domains, ",") != "api-contract,application-security,common" {
			t.Fatalf("selection: %q %v %v", selected.Name, domains, err)
		}
	}
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, bad := range []struct{ header, value string }{{"X-Babel-Domains", "missing"}, {"X-Babel-Dialect", "missing"}, {"X-Babel-Domains", "api-contract,"}} {
			// Short messages skip compilation, but invalid configuration must still
			// be reported before any upstream call rather than silently ignored.
			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"consumer","messages":[{"role":"user","content":"short"}]}`))
			r.Header.Set(bad.header, bad.value)
			r.Header.Set("Authorization", "Bearer consumer-fixture")
			w := httptest.NewRecorder()
			newServerMux().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s invalid %s status=%d", route, bad.header, w.Code)
			}
		}
	}
}

// These mock providers test transport and structural preservation, not the
// semantic quality of a real compiler or audit model.
func TestDomainProxyForwardsOriginalProtocolBoundaries(t *testing.T) {
	const original = "请核对用户访问资源的权限，并在处理完成后简要记录结果，提交给相应的审查人员参考。"
	const candidate = "核对用户访问资源的权限；完成后简记结果，交审查人员参考。"
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			var generations atomic.Int32
			compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model    string                           `json:"model"`
					Messages []struct{ Role, Content string } `json:"messages"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Model != "compiler-model" || r.Header.Get("Authorization") != "Bearer compiler-key" {
					t.Error("compiler identity mixed with consumer identity")
				}
				if body.Messages[0].Content == verifyGatePrompt {
					writeModelContent(w, `{"pass":true,"missing":[]}`)
					return
				}
				input := decodeCompilerInput(t, body.Messages[1].Content)
				if !strings.Contains(input.DomainGuidance, `"公开表达":"授权"`) || strings.Contains(body.Messages[0].Content, `"公开表达":"授权"`) {
					t.Error("selected domain did not reach proxy compilation")
				}
				generations.Add(1)
				writeModelContent(w, candidate)
			}))
			defer compiler.Close()
			forwarded := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if protocol == "openai" && r.Header.Get("Authorization") != "Bearer consumer-key" {
					t.Error("consumer authorization not preserved")
				}
				if protocol == "anthropic" && r.Header.Get("x-api-key") != "consumer-key" {
					t.Error("consumer API key not preserved")
				}
				forwarded <- body
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[]}`))
			}))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{"test": {
				Name: "test", APIKey: "compiler-key", BaseURL: compiler.URL, Model: "compiler-model", UpstreamURL: upstream.URL,
			}}}); err != nil {
				t.Fatal(err)
			}
			schema := map[string]any{"type": "object", "required": []string{"resource_id"}, "additionalProperties": false,
				"properties": map[string]any{"resource_id": map[string]any{"type": "string", "enum": []string{"primary", "backup"}, "description": original}}}
			payload := map[string]any{"model": "consumer-model", "messages": []any{
				map[string]any{"role": "user", "content": original}, map[string]any{"role": "assistant", "content": original},
			}}
			route := "/v1/chat/completions"
			if protocol == "openai" {
				payload["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup_resource", "strict": true, "description": original, "parameters": schema}}}
			} else {
				route = "/v1/messages"
				payload["max_tokens"] = 256
				payload["system"] = original
				payload["tools"] = []any{map[string]any{"name": "lookup_resource", "description": original, "input_schema": schema}}
			}
			body, _ := json.Marshal(payload)
			var expected map[string]any
			json.Unmarshal(body, &expected)
			expected["messages"].([]any)[0].(map[string]any)["content"] = candidate
			// Only user text may change. Policies and tool contracts are outside
			// the compiler's authority even if a fixture auditor would approve.
			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(body)))
			r.Header.Set("X-Babel-Domains", "application-security")
			r.Header.Set("Authorization", "Bearer consumer-key")
			r.Header.Set("x-api-key", "consumer-key")
			w := httptest.NewRecorder()
			newServerMux().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("proxy status %d: %s", w.Code, w.Body.String())
			}
			if got := <-forwarded; !reflect.DeepEqual(got, expected) {
				t.Fatalf("protocol fields changed or compression missing:\ngot: %#v\nwant: %#v", got, expected)
			}
			if generations.Load() == 0 {
				t.Fatal("proxy skipped the compiler")
			}
		})
	}
}
