package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func isolateProxyState(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cacheMutex.Lock()
	oldCache := diskCache
	diskCache = make(map[string]CacheEntry)
	cacheMutex.Unlock()
	logsMutex.Lock()
	oldLogs := proxyLogs
	proxyLogs = nil
	logsMutex.Unlock()
	oldBreaker := compilerCB
	compilerCB = newCircuitBreaker(3, time.Minute)
	t.Cleanup(func() {
		cacheMutex.Lock()
		diskCache = oldCache
		cacheMutex.Unlock()
		logsMutex.Lock()
		proxyLogs = oldLogs
		logsMutex.Unlock()
		compilerCB = oldBreaker
	})
}

func TestProxyAdoptsOnlySmallerVerifiedCandidates(t *testing.T) {
	original := "Please review the supplied specification and summarize its relevant operational requirements clearly for the engineering team."
	for _, tc := range []struct {
		name, candidate, audit, want string
	}{
		{"shorter", "Review the specification; summarize operational requirements for engineers.", `{"pass":true,"missing":[]}`, "adopted"},
		{"equal", original, `{"pass":true,"missing":[]}`, "kept_original"},
		{"longer", original + " Please provide a detailed explanation of the summarized operational requirements.", `{"pass":true,"missing":[]}`, "kept_original"},
		{"invalid audit", "Review the specification.", "NOT PASS", "kept_original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProxyState(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode: %v", err)
					return
				}
				content := tc.candidate
				if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
					content = tc.audit
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer server.Close()
			profile := Profile{APIKey: "test", BaseURL: server.URL, Model: "test-model"}
			output, id, before, after := translateMessageBlock("openai", "user", original, profile, getDialect("token"), nil, nil)
			if id == "" {
				t.Fatal("expected a request-specific log ID")
			}
			if tc.want == "adopted" {
				if output != tc.candidate || after >= before {
					t.Fatalf("candidate was not adopted: %q, %d -> %d", output, before, after)
				}
			} else if output != original || before != after {
				t.Fatalf("fallback changed source: %q, %d -> %d", output, before, after)
			}
			logsMutex.RLock()
			entry := proxyLogs[0]
			logsMutex.RUnlock()
			if entry.Decision != tc.want {
				t.Fatalf("decision %q; want %q", entry.Decision, tc.want)
			}
			_, repeatedID, _, _ := translateMessageBlock("openai", "user", original, profile, getDialect("token"), nil, nil)
			if repeatedID == id {
				t.Fatal("repeated source reused a request log ID")
			}
		})
	}
}

func TestUpstreamUsageKeepsMeasurementsSeparate(t *testing.T) {
	isolateProxyState(t)
	appendProxyLog(ProxyLog{ID: "request-1"})
	appendProxyLog(ProxyLog{ID: "request-2"})
	applyUpstreamUsage([]string{"request-1"}, 120, 80, 200)
	logsMutex.RLock()
	defer logsMutex.RUnlock()
	for _, entry := range proxyLogs {
		if entry.ID == "request-2" {
			if entry.RealPromptTokens != 0 {
				t.Fatal("usage attached to an unrelated request")
			}
			continue
		}
		if entry.RealPromptTokens != 200 || entry.EstOrigTokens != 120 {
			t.Fatalf("measurement changed: %+v", entry)
		}
		if !strings.Contains(entry.RealBillingRatio, "文本估算 120→80") || strings.Contains(entry.RealBillingRatio, "省") || strings.Contains(entry.RealBillingRatio, "%") {
			t.Fatalf("misleading usage label: %s", entry.RealBillingRatio)
		}
	}
}

func TestOpenAIProxyDoesNotInventJSONInstruction(t *testing.T) {
	isolateProxyState(t)
	input := map[string]any{
		"model":           "consumer-model",
		"messages":        []any{map[string]any{"role": "user", "content": "Return the result."}},
		"response_format": map[string]any{"type": "json_object"},
	}
	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&forwarded)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	t.Setenv("BABEL_BASE_URL", upstream.URL)
	t.Setenv("BABEL_UPSTREAM_URL", upstream.URL)
	body, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	openAIProxyHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(forwarded, input) {
		t.Fatalf("proxy changed an uncompressed request: %#v", forwarded)
	}
}
