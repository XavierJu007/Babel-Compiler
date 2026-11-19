package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Record the wire RequestURI rather than testing a URL-construction helper.
// Both production entry points must send the configured endpoint exactly once,
// including when the upstream rejects the request.
func TestCompilerAndProbeUseExactConfiguredEndpoint(t *testing.T) {
	type request struct {
		method string
		uri    string
	}
	var mu sync.Mutex
	var requests []request
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, request{method: r.Method, uri: r.RequestURI})
		code := status
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code != http.StatusOK {
			_, _ = io.WriteString(w, `{"error":{"message":"fixture upstream failure"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"endpoint-fixture","choices":[{"finish_reason":"stop","message":{"content":"fixture response"}}]}`)
	}))
	t.Cleanup(server.Close)

	endpoints := []struct {
		name string
		uri  string
	}{
		{"v1", "/v1"},
		{"v1 trailing slash", "/v1/"},
		{"custom route", "/gateway/custom-completion"},
		{"custom route trailing slash", "/gateway/custom-completion/"},
		{"custom route query", "/gateway/custom-completion?api-version=2025-01&route=a%2Fb&tag=one&tag=two"},
		{"complete route", "/v1/chat/completions"},
		{"complete route query", "/v1/chat/completions?deployment=fixture&literal=%2Fv1%2Fchat%2Fcompletions"},
	}
	for _, entryPoint := range []string{"callLLM", "probeProfile"} {
		t.Run(entryPoint, func(t *testing.T) {
			for _, endpoint := range endpoints {
				t.Run(endpoint.name, func(t *testing.T) {
					for _, outcome := range []struct {
						name string
						code int
					}{{"success", http.StatusOK}, {"upstream 502", http.StatusBadGateway}} {
						t.Run(outcome.name, func(t *testing.T) {
							mu.Lock()
							status = outcome.code
							requests = nil
							mu.Unlock()
							url := server.URL + endpoint.uri
							profile := Profile{Name: "endpoint-fixture", BaseURL: url, APIKey: "fixture-key", Model: "endpoint-fixture"}
							if entryPoint == "callLLM" {
								content, err := callLLM(profile, "fixture system", "fixture input", 0)
								if outcome.code == http.StatusOK {
									if err != nil || content != "fixture response" {
										t.Errorf("content=%q err=%v; want fixture response", content, err)
									}
								} else if err == nil || !strings.Contains(err.Error(), "502") {
									t.Errorf("err=%v; want an upstream HTTP 502 error", err)
								}
							} else {
								result := probeProfile(profile)
								if result.OK != (outcome.code == http.StatusOK) || result.StatusCode != outcome.code {
									t.Errorf("probe=%+v; want OK=%v status=%d", result, outcome.code == http.StatusOK, outcome.code)
								}
								if result.TargetURL != url {
									t.Errorf("probe target=%q; want exact configured URL %q", result.TargetURL, url)
								}
							}

							mu.Lock()
							got := append([]request(nil), requests...)
							mu.Unlock()
							if len(got) != 1 {
								t.Fatalf("received %d requests: %+v; want exactly one, without route fallback or retry", len(got), got)
							}
							if got[0].method != http.MethodPost || got[0].uri != endpoint.uri {
								t.Errorf("wire request=%+v; want POST %q", got[0], endpoint.uri)
							}
						})
					}
				})
			}
		})
	}
}
