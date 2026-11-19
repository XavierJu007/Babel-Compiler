package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProfilePartialSavePreservesProxyUpstream(t *testing.T) {
	isolateDomainIntegration(t)
	original := Profile{Name: "test", APIKey: "saved-key", BaseURL: "http://compiler.invalid/v1", Model: "fixture",
		UpstreamURL: "http://consumer.invalid/v1"}
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{"test": original}}); err != nil {
		t.Fatal(err)
	}
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{
		"action": "save", "profile_name": "test", "token_encoding": "o200k_base",
	}, http.StatusOK)
	got := loadConfig().Profiles["test"]
	if got.UpstreamURL != original.UpstreamURL || got.BaseURL != original.BaseURL || got.APIKey != original.APIKey {
		t.Fatal("saving a measurement option changed the configured provider routes or key")
	}
	sendDomainAPI(t, http.MethodPost, "/api/config", map[string]any{
		"action": "save", "profile_name": "test", "upstream_url": "",
	}, http.StatusOK)
	if loadConfig().Profiles["test"].UpstreamURL != "" {
		t.Fatal("explicitly empty upstream_url did not clear the saved route")
	}
}

func TestProfileProbeDoesNotBorrowCredentialsForNewDraft(t *testing.T) {
	for _, tc := range []struct {
		name, profile, key, wantAuth string
	}{
		{"new draft without key", "new-draft", "", ""},
		{"new draft with mask", "new-draft", "saved-***", ""},
		{"new draft with explicit key", "new-draft", "draft-key", "Bearer draft-key"},
		{"saved profile inherits its key", "test", "", "Bearer saved-key"},
		{"active profile inherits its key", "", "", "Bearer saved-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			t.Setenv("BABEL_API_KEY", "environment-key-not-for-the-draft")
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != tc.wantAuth {
					t.Error("probe used credentials from another profile")
				}
				json.NewEncoder(w).Encode(map[string]any{"model": "fixture", "choices": []any{
					map[string]any{"message": map[string]string{"content": "ok"}},
				}})
			}))
			defer provider.Close()
			if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
				"test": {Name: "test", APIKey: "saved-key", BaseURL: provider.URL, Model: "fixture"},
			}}); err != nil {
				t.Fatal(err)
			}
			result := sendDomainAPI(t, http.MethodPost, "/api/config/test", map[string]any{
				"profile_name": tc.profile, "api_key": tc.key, "base_url": provider.URL, "model": "fixture",
			}, http.StatusOK)
			var ok bool
			if err := json.Unmarshal(result["ok"], &ok); err != nil {
				t.Fatal(err)
			}
			if tc.wantAuth == "" {
				if ok || calls.Load() != 0 || !strings.Contains(string(result["detail"]), "未设置 API Key") {
					t.Fatalf("draft without its own key made a probe: ok=%v calls=%d", ok, calls.Load())
				}
			} else if !ok || calls.Load() != 1 {
				t.Fatalf("explicit/saved credentials could not probe: ok=%v calls=%d", ok, calls.Load())
			}
			if len(loadConfig().Profiles) != 1 {
				t.Fatal("testing a draft unexpectedly saved it")
			}
		})
	}
}
