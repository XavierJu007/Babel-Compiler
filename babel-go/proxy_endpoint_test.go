package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenAIProxyReusesExactCompilerEndpoint(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	const route = "/gateway/v1/?route=chat%2Fcustom&tag=a&tag=b"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.RequestURI != route || r.Header.Get("Authorization") != "Bearer consumer-fixture" {
			t.Errorf("proxy changed its configured fallback endpoint: %s %s", r.Method, r.RequestURI)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer provider.Close()
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
		"test": {BaseURL: provider.URL + route, Model: "fixture"},
	}}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"consumer","messages":[]}`))
	request.Header.Set("Authorization", "Bearer consumer-fixture")
	response := httptest.NewRecorder()
	newServerMux().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"ok":true}` || calls.Load() != 1 {
		t.Fatalf("exact proxy fallback failed: status=%d calls=%d", response.Code, calls.Load())
	}
}

func TestAnthropicProxyRequiresExplicitUpstreamBeforeModelWork(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected inferred route: %s", r.RequestURI)
		}
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer provider.Close()
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
		"test": {APIKey: "compiler-fixture", BaseURL: provider.URL + "/custom/compiler", Model: "fixture"},
	}}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"consumer","max_tokens":10,"messages":[{"role":"user","content":"Please review this entire technical specification and retain every condition and all exceptions."}]}`))
	response := httptest.NewRecorder()
	newServerMux().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "upstream_url") || calls.Load() != 0 {
		t.Fatalf("missing upstream started model work or guessed a route: status=%d calls=%d", response.Code, calls.Load())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"consumer","messages":[]}`))
	request.Header.Set("X-Upstream-BaseURL", provider.URL+"/v1")
	response = httptest.NewRecorder()
	newServerMux().ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("explicit Anthropic upstream no longer works: status=%d calls=%d", response.Code, calls.Load())
	}
}
