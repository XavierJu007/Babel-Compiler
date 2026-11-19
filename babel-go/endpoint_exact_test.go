package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 记录线上真实 RequestURI，而不是只测 URL 拼接函数。
// 完整端点必须原样发送一次；只有 `/v1` 这类基址才允许补全 /chat/completions。
func TestCompilerAndProbeUseConfiguredEndpoint(t *testing.T) {
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
		name    string
		uri     string
		wantURI string
	}{
		{"v1 base", "/v1", "/v1/chat/completions"},
		{"v1 base trailing slash", "/v1/", "/v1/chat/completions"},
		{"v1 base with query", "/v1?tenant=fixture", "/v1/chat/completions?tenant=fixture"},
		{"host only", "", "/v1/chat/completions"},
		{"custom route", "/gateway/custom-completion", "/gateway/custom-completion"},
		{"custom route trailing slash", "/gateway/custom-completion/", "/gateway/custom-completion/"},
		{"custom route query", "/gateway/custom-completion?api-version=2025-01&route=a%2Fb&tag=one&tag=two", "/gateway/custom-completion?api-version=2025-01&route=a%2Fb&tag=one&tag=two"},
		{"complete route", "/v1/chat/completions", "/v1/chat/completions"},
		{"complete route query", "/v1/chat/completions?deployment=fixture&literal=%2Fv1%2Fchat%2Fcompletions", "/v1/chat/completions?deployment=fixture&literal=%2Fv1%2Fchat%2Fcompletions"},
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
							configured := server.URL + endpoint.uri
							effective := server.URL + endpoint.wantURI
							profile := Profile{Name: "endpoint-fixture", BaseURL: configured, APIKey: "fixture-key", Model: "endpoint-fixture"}
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
								if result.TargetURL != effective {
									t.Errorf("probe target=%q; want %q", result.TargetURL, effective)
								}
							}

							mu.Lock()
							got := append([]request(nil), requests...)
							mu.Unlock()
							if len(got) != 1 {
								t.Fatalf("received %d requests: %+v; want exactly one, without route fallback or retry", len(got), got)
							}
							if got[0].method != http.MethodPost || got[0].uri != endpoint.wantURI {
								t.Errorf("wire request=%+v; want POST %q", got[0], endpoint.wantURI)
							}
						})
					}
				})
			}
		})
	}
}

// 基址补全与完整端点保留的纯规则表，覆盖大小写、查询参数与非法输入。
func TestCompilerRequestURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to default", "", defaultCompilerRequestURL},
		{"blank falls back to default", "   ", defaultCompilerRequestURL},
		{"host only", "https://api.deepseek.com", "https://api.deepseek.com/v1/chat/completions"},
		{"host with trailing slash", "https://api.deepseek.com/", "https://api.deepseek.com/v1/chat/completions"},
		{"v1 base", "https://models.gds-services.com/v1", "https://models.gds-services.com/v1/chat/completions"},
		{"v1 base trailing slash", "https://models.gds-services.com/v1/", "https://models.gds-services.com/v1/chat/completions"},
		{"v1 base uppercase", "https://models.gds-services.com/V1", "https://models.gds-services.com/V1/chat/completions"},
		{"prefixed v1 base", "https://gateway.example.com/openai/v1", "https://gateway.example.com/openai/v1/chat/completions"},
		{"v1 base with query", "https://gateway.example.com/v1?tenant=a", "https://gateway.example.com/v1/chat/completions?tenant=a"},
		{"complete endpoint kept", "https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"complete endpoint with query kept", "https://x.example.com/v1/chat/completions?api-version=2025-01", "https://x.example.com/v1/chat/completions?api-version=2025-01"},
		{"complete endpoint trailing slash kept", "https://x.example.com/v1/chat/completions/", "https://x.example.com/v1/chat/completions/"},
		{"custom route kept", "https://x.example.com/gateway/complete", "https://x.example.com/gateway/complete"},
		{"custom route resembling version kept", "https://x.example.com/v2/responses", "https://x.example.com/v2/responses"},
		{"whitespace trimmed", "  https://api.openai.com/v1/chat/completions  ", "https://api.openai.com/v1/chat/completions"},
		{"unsupported scheme untouched", "ftp://example.com/v1", "ftp://example.com/v1"},
		{"garbage untouched", "not a url", "not a url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compilerRequestURL(tc.in); got != tc.want {
				t.Errorf("compilerRequestURL(%q)=%q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
