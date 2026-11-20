package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func guardedLocalHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := newServerHandler("127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestLocalHTTPRejectsCrossOriginCredentialProbe(t *testing.T) {
	isolateDomainIntegration(t)
	var calls atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, "ok")
	}))
	defer collector.Close()
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
		"test": {Name: "test", APIKey: "private-fixture-secret", BaseURL: "http://unused.invalid/v1", Model: "fixture"},
	}}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"base_url": collector.URL})
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/config/test", strings.NewReader(string(data)))
	r.Header.Set("Origin", "http://malicious.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	w := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("cross-origin probe reached credential-bearing provider: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestLocalHTTPRejectsCrossOriginDomainReplacement(t *testing.T) {
	isolateDomainIntegration(t)
	pack := DomainPack{SchemaVersion: 1, Name: "common", Label: "cross-site replacement", Version: 1,
		Entries: []DomainEntry{{ID: "refund", Canonical: "refund", Triggers: []string{"refund"},
			UseWhen: []string{"Refund processing"}, Examples: []DomainExample{{Input: "refund", Output: "attacker-chosen replacement"}}}}}
	data, _ := json.Marshal(map[string]any{"action": "save", "scope": "global", "domain": pack})
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/domains", strings.NewReader(string(data)))
	r.Header.Set("Origin", "http://malicious.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	w := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(w, r)
	packs, err := resolveDomainPacks(defaultDialectName, nil)
	if w.Code != http.StatusForbidden || err != nil || len(packs) != 1 || packs[0].Label == pack.Label {
		t.Fatalf("cross-origin request altered the default domain: status=%d packs=%+v err=%v", w.Code, packs, err)
	}
}

func TestLocalHTTPRejectsRebindingHostLogRead(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	appendProxyLog(ProxyLog{ID: "private-fixture", Original: "private prompt fixture", Compiled: "private prompt fixture"})
	r := httptest.NewRequest(http.MethodGet, "http://rebind.attacker.example:8080/api/logs", nil)
	r.Header.Set("Origin", "http://rebind.attacker.example:8080")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "private prompt fixture") {
		t.Fatalf("untrusted Host could read prompt logs: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLocalHTTPSameOriginManagementRemainsUsable(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	handler := guardedLocalHandler(t)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/config", strings.NewReader(`{"action":"save","profile_name":"default","model":"local-fixture"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || loadConfig().Profiles["default"].Model != "local-fixture" {
		t.Fatalf("same-origin JSON configuration was blocked: %d %s", w.Code, w.Body.String())
	}
	appendProxyLog(ProxyLog{ID: "fixture"})
	r = httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8080/api/logs", nil)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	logsMutex.RLock()
	remaining := len(proxyLogs)
	logsMutex.RUnlock()
	if w.Code != http.StatusOK || remaining != 0 {
		t.Fatalf("typed same-origin deletion was blocked: %d remaining=%d", w.Code, remaining)
	}
}

func TestLocalHTTPOriginAndHostBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin, fetchSite string
		want                          int
	}{
		{"native local client", "127.0.0.1:8080", "", "", 204},
		{"same-origin browser", "127.0.0.1:8080", "http://127.0.0.1:8080", "same-origin", 204},
		{"localhost alias", "localhost:8080", "http://localhost:8080", "same-origin", 204},
		{"local navigation", "127.0.0.1:8080", "", "none", 204},
		{"opaque origin", "127.0.0.1:8080", "null", "", 403},
		{"foreign origin", "127.0.0.1:8080", "http://outside.example", "", 403},
		{"other local service", "127.0.0.1:8080", "http://127.0.0.1:9090", "same-site", 403},
		{"cross-site without origin", "127.0.0.1:8080", "", "cross-site", 403},
		{"same-site without origin", "127.0.0.1:8080", "", "same-site", 403},
		{"wrong scheme", "127.0.0.1:8080", "https://127.0.0.1:8080", "same-origin", 403},
		{"origin with path", "127.0.0.1:8080", "http://127.0.0.1:8080/path", "same-origin", 403},
		{"different alias origin", "localhost:8080", "http://127.0.0.1:8080", "same-origin", 403},
		{"arbitrary host", "rebind.example:8080", "http://rebind.example:8080", "same-origin", 403},
		{"wrong port", "127.0.0.1:9090", "", "", 403},
		{"different loopback IP", "127.0.0.2:8080", "", "", 403},
		{"host with userinfo", "local@127.0.0.1:8080", "", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			guard := &localHTTPGuard{host: "127.0.0.1", port: "8080", next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			})}
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/config", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, r)
			if w.Code != tc.want || calls != map[bool]int{true: 1, false: 0}[tc.want == 204] {
				t.Fatalf("boundary returned status=%d calls=%d; want %d", w.Code, calls, tc.want)
			}
		})
	}
}

func TestLocalHTTPJSONOnlyAndBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name, path, contentType string
		length                  int64
		want                    int
	}{
		{"JSON management", "/api/config", "application/json", 2, 204},
		{"JSON charset", "/api/config", "application/json; charset=utf-8", 2, 204},
		{"missing type", "/api/config", "", 2, 415},
		{"simple text request", "/api/config", "text/plain", 2, 415},
		{"simple form request", "/api/config", "application/x-www-form-urlencoded", 2, 415},
		{"too large", "/api/compile", "application/json", maxLocalJSONBodyBytes + 1, 413},
		{"OpenAI proxy type", "/v1/chat/completions", "text/plain", 2, 415},
		{"Anthropic proxy size", "/v1/messages", "application/json", maxLocalJSONBodyBytes + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			guard := &localHTTPGuard{host: "127.0.0.1", port: "8080", next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			})}
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+tc.path, strings.NewReader("{}"))
			r.Header.Set("Content-Type", tc.contentType)
			r.ContentLength = tc.length
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, r)
			if w.Code != tc.want || calls != map[bool]int{true: 1, false: 0}[tc.want == 204] {
				t.Fatalf("JSON boundary returned status=%d calls=%d; want %d", w.Code, calls, tc.want)
			}
		})
	}
	guard := &localHTTPGuard{host: "127.0.0.1", port: "8080", next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, err := io.Copy(io.Discard, r.Body)
		var tooLarge *http.MaxBytesError
		if read != maxLocalJSONBodyBytes || !errors.As(err, &tooLarge) {
			t.Errorf("unknown-size body escaped its limit: read=%d err=%v", read, err)
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/compile", strings.NewReader(strings.Repeat("x", maxLocalJSONBodyBytes+1)))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1
	guard.ServeHTTP(httptest.NewRecorder(), r)
}

func TestLocalHTTPSecurityHeadersSurviveResponsesAndStreaming(t *testing.T) {
	for _, path := range []string{"/api/logs", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			guard := &localHTTPGuard{host: "127.0.0.1", port: "8080", next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Content-Security-Policy", "script-src * 'unsafe-inline'")
				w.Header().Set("X-Content-Type-Options", "unsafe")
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte("data: fixture\n\n"))
			})}
			w := httptest.NewRecorder()
			guard.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil))
			if !w.Flushed || w.Body.String() != "data: fixture\n\n" || w.Result().Header.Get("Content-Security-Policy") != localDataCSP || w.Result().Header.Get("X-Content-Type-Options") != "nosniff" || w.Result().Header.Get("X-Frame-Options") != "DENY" {
				t.Fatalf("response policy or streaming was lost: %#v body=%q", w.Result().Header, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != localPageCSP || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("local page lost its usable scripts or frame protection: %d %#v", w.Code, w.Header())
	}
}

func TestLocalHTTPOnlyLoopbackListeners(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "outside.example:8080", "127.0.0.1:not-a-port", "127.0.0.1:65536"} {
		if _, _, err := loopbackListenAddress(addr); err == nil {
			t.Errorf("accepted nonlocal or invalid listener %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.2:8080", "[::1]:8080", "localhost:8080", "127.0.0.1:0"} {
		if _, _, err := loopbackListenAddress(addr); err != nil {
			t.Errorf("rejected explicit loopback listener %q: %v", addr, err)
		}
	}
	listener, addr, err := bindAvailableListener("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("localhost binding resolved outside the fixed loopback address: %s", addr)
	}
}
