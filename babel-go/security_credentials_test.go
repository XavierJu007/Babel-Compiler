package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCredentialOverridesRequireExplicitKeyForNewOrigin(t *testing.T) {
	base := Profile{Name: "saved", BaseURL: "https://service.example/v1", APIKey: "saved-fixture-key"}
	for _, tc := range []struct {
		name, endpoint, key string
		allowed             bool
	}{
		{"no endpoint change", "", "", true},
		{"same origin exact path", "https://service.example/custom/compile/?route=other", "", true},
		{"default port and host case", "https://SERVICE.example:443/v1", "", true},
		{"different host", "https://other.example/v1", "", false},
		{"different port", "https://service.example:8443/v1", "", false},
		{"scheme downgrade", "http://service.example/v1", "", false},
		{"mask is not explicit authority", "https://other.example/v1", "saved-***", false},
		{"malformed destination", "://invalid", "", false},
		{"userinfo destination", "https://user@service.example/v1", "", false},
		{"explicit new credential", "https://other.example/v1", "new-fixture-key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyCompilerCredentials(base, tc.endpoint, tc.key)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), base.APIKey) {
					t.Fatal("credential was exposed in an error")
				}
				return
			}
			wantKey := base.APIKey
			if tc.key != "" {
				wantKey = tc.key
			}
			if got.APIKey != wantKey || tc.endpoint != "" && got.BaseURL != tc.endpoint {
				t.Fatal("explicit request endpoint or credential was changed")
			}
		})
	}
}

func TestAPIOverridesCannotReuseStoredKeyAtAnotherOrigin(t *testing.T) {
	isolateDomainIntegration(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, "ok")
	}))
	defer provider.Close()
	stored := Profile{Name: "saved", BaseURL: "https://configured.example/v1", Model: "fixture", APIKey: "saved-fixture-key"}
	if err := writeConfigFile(Config{ActiveProfile: "saved", Profiles: map[string]Profile{"saved": stored}}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/compile", "/api/validate"} {
		sendDomainAPI(t, http.MethodPost, route, map[string]any{
			"text": "保留当前规则。", "candidate": "保留当前规则。", "base_url": provider.URL,
		}, http.StatusBadRequest)
	}
	probe := sendDomainAPI(t, http.MethodPost, "/api/config/test", map[string]any{"base_url": provider.URL}, http.StatusOK)
	var ok bool
	json.Unmarshal(probe["ok"], &ok)
	if ok || !strings.Contains(string(probe["detail"]), "必须显式填写") {
		t.Fatal("probe override silently borrowed the stored credential")
	}
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{
		"action": "save", "profile_name": "saved", "base_url": provider.URL,
	}, http.StatusBadRequest)
	if got := loadConfig().Profiles["saved"]; got != stored {
		t.Fatal("a rejected origin change altered the saved profile")
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected API overrides made %d outbound calls", calls.Load())
	}
	// Intentional user-supplied credentials can still save and test a new origin.
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{
		"action": "save", "profile_name": "saved", "base_url": provider.URL, "api_key": "new-fixture-key",
	}, http.StatusOK)
	got := loadConfig().Profiles["saved"]
	if got.APIKey != "new-fixture-key" || got.BaseURL != provider.URL {
		t.Fatal("explicit credential replacement could not select the new service")
	}
}

func TestLegacyConfigDoesNotMoveEnvironmentCredentials(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, key, wantKey string }{
		{"other origin", "https://other.example/v1", "", ""},
		{"same origin", "https://configured.example/custom/", "", "environment-fixture-key"},
		{"explicit config credential", "https://other.example/v1", "config-fixture-key", "config-fixture-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			t.Setenv("BABEL_API_KEY", "environment-fixture-key")
			t.Setenv("BABEL_BASE_URL", "https://configured.example/v1")
			data, _ := json.Marshal(map[string]string{"base_url": tc.endpoint, "api_key": tc.key, "model": "fixture"})
			if err := os.WriteFile("babel.config.json", data, 0600); err != nil {
				t.Fatal(err)
			}
			p := getActiveProfile(loadConfig(), "")
			if p.BaseURL != tc.endpoint || p.APIKey != tc.wantKey {
				t.Fatal("legacy config crossed the environment credential boundary")
			}
		})
	}
}

func TestOversizedModelResponseKeepsProxyOriginal(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("x", 32*1024)
		for written := 0; written <= maxModelResponseBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer provider.Close()
	profile := Profile{Name: "test", APIKey: "fixture", BaseURL: provider.URL, Model: "fixture"}
	const original = "请阅读下列说明并用中文简洁概括其中的主要意思，保留其中实际提出的要求，并且不要添加原文未提供的事实。"
	text, _, before, after := translateMessageBlockContext(context.Background(), "openai", "user", original, profile, getDialect(defaultDialectName), nil, nil)
	if text != original || before != after || calls.Load() != 1 {
		t.Fatalf("oversized provider response was adopted or retried: calls=%d", calls.Load())
	}
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	if len(diskCache) != 0 {
		t.Fatal("an oversized response entered the candidate cache")
	}
}
