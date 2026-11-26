package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newMockCompilerAndUpstream(t *testing.T, candidate string, seenUpstreamBodies *[]string, mu *sync.Mutex) (*httptest.Server, *httptest.Server) {
	t.Helper()
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content := candidate
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			content = `{"pass":true,"missing":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]string{"content": content},
			}},
		})
	}))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if seenUpstreamBodies != nil {
			if mu != nil {
				mu.Lock()
			}
			*seenUpstreamBodies = append(*seenUpstreamBodies, string(body))
			if mu != nil {
				mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":42}}`))
	}))

	t.Cleanup(func() {
		compiler.Close()
		upstream.Close()
	})
	return compiler, upstream
}

func TestProxyDryRunHeaderAndServerPolicy(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	originalPrompt := "Please inspect every inbound customer refund request carefully, verify the cryptographic signature and order identifier, confirm the current order status is pending before changing state, and return a structured JSON audit summary."
	shortCandidate := "∀refund req: check sig+order_id; if state=pending→update; else reject; return JSON."

	var upstreamBodies []string
	var bodyMu sync.Mutex
	compiler, upstream := newMockCompilerAndUpstream(t, shortCandidate, &upstreamBodies, &bodyMu)

	if err := writeConfigFile(Config{
		ActiveProfile: "fixture",
		Profiles: map[string]Profile{
			"fixture": {
				Name:        "fixture",
				APIKey:      "compiler-key",
				BaseURL:     compiler.URL,
				UpstreamURL: upstream.URL,
				Model:       "compiler-model",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(newServerMux())
	defer proxy.Close()

	reqBody := `{"model":"consumer-model","messages":[{"role":"user","content":"` + originalPrompt + `"}]}`
	before := proxyMetricsSnapshot()

	// 1) Request header X-Babel-Dry-Run: true forces dry-run even when server default is false.
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	req.Header.Set("X-Babel-Dry-Run", "true")

	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.Header.Get("X-Babel-Status") != "dry-run-hit" {
		t.Fatalf("expected X-Babel-Status=dry-run-hit, got %q", resp.Header.Get("X-Babel-Status"))
	}
	savedHdr, err := strconv.Atoi(resp.Header.Get("X-Babel-Dry-Run-Saved-Tokens"))
	if err != nil || savedHdr <= 0 {
		t.Fatalf("expected positive X-Babel-Dry-Run-Saved-Tokens header, got %q (%v)", resp.Header.Get("X-Babel-Dry-Run-Saved-Tokens"), err)
	}

	bodyMu.Lock()
	if len(upstreamBodies) != 1 || upstreamBodies[0] != reqBody {
		bodyMu.Unlock()
		t.Fatalf("dry-run altered upstream body: got %v want %q", upstreamBodies, reqBody)
	}
	bodyMu.Unlock()

	logsMutex.RLock()
	if len(proxyLogs) == 0 || proxyLogs[0].Decision != "dry_run_would_adopt" || proxyLogs[0].Compiled != shortCandidate {
		logsMutex.RUnlock()
		t.Fatalf("unexpected dry-run log entry: %+v", proxyLogs)
	}
	logsMutex.RUnlock()

	afterDry := proxyMetricsSnapshot()
	if afterDry["dry_run_total"] != before["dry_run_total"]+1 ||
		afterDry["dry_run_would_adopt_total"] != before["dry_run_would_adopt_total"]+1 ||
		afterDry["dry_run_potential_saved_tokens_total"] != before["dry_run_potential_saved_tokens_total"]+uint64(savedHdr) {
		t.Fatalf("unexpected dry-run metrics delta: before=%v after=%v savedHdr=%d", before, afterDry, savedHdr)
	}
	if afterDry["adopted_total"] != before["adopted_total"] {
		t.Fatalf("dry-run incremented adopted_total unexpectedly: %d -> %d", before["adopted_total"], afterDry["adopted_total"])
	}

	// 2) Server-level proxyDryRun = true + validation cache fast path also respects dry-run,
	//    while X-Babel-Dry-Run: false explicitly overrides it and adopts.
	proxyPolicyMu.Lock()
	proxyDryRun = true
	proxyValidationCacheTTL = 10 * time.Minute
	proxyPolicyMu.Unlock()

	req2, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer consumer-key")
	resp2, err := proxy.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if resp2.Header.Get("X-Babel-Status") != "dry-run-hit" {
		t.Fatalf("server dry-run fast path returned status=%q", resp2.Header.Get("X-Babel-Status"))
	}
	bodyMu.Lock()
	if len(upstreamBodies) != 2 || upstreamBodies[1] != reqBody {
		bodyMu.Unlock()
		t.Fatalf("server dry-run modified upstream body: got %q", upstreamBodies[1])
	}
	bodyMu.Unlock()

	// 3) Override server dry-run with X-Babel-Dry-Run: false -> must adopt and record saved tokens.
	req3, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer consumer-key")
	req3.Header.Set("X-Babel-Dry-Run", "false")
	resp3, err := proxy.Client().Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp3.Body)
	resp3.Body.Close()

	if resp3.Header.Get("X-Babel-Status") != "adopted" {
		t.Fatalf("override dry-run=false returned status=%q", resp3.Header.Get("X-Babel-Status"))
	}
	bodyMu.Lock()
	if len(upstreamBodies) != 3 || !strings.Contains(upstreamBodies[2], shortCandidate) || strings.Contains(upstreamBodies[2], originalPrompt) {
		bodyMu.Unlock()
		t.Fatalf("expected compressed candidate forwarded on dry-run=false, got %q", upstreamBodies[2])
	}
	bodyMu.Unlock()

	afterAdopt := proxyMetricsSnapshot()
	if afterAdopt["adopted_total"] != before["adopted_total"]+1 ||
		afterAdopt["adopted_saved_tokens_total"] <= before["adopted_saved_tokens_total"] ||
		afterAdopt["adopted_orig_tokens_total"] <= before["adopted_orig_tokens_total"] {
		t.Fatalf("expected adoption ROI counters to increase: before=%v after=%v", before, afterAdopt)
	}
}

func TestProxyUpdatePolicyAPIValidationAndConcurrency(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	compiler, upstream := newMockCompilerAndUpstream(t, "Short valid prompt.", nil, nil)
	if err := writeConfigFile(Config{
		ActiveProfile: "fixture",
		Profiles: map[string]Profile{
			"fixture": {
				Name:        "fixture",
				APIKey:      "compiler-key",
				BaseURL:     compiler.URL,
				UpstreamURL: upstream.URL,
				Model:       "compiler-model",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	mux := newServerMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postPolicy := func(payload string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/logs", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Reject invalid compile_budget and negative warmup_user_min_hits with HTTP 400.
	for _, bad := range []string{
		`{"action":"update_policy","compile_budget":"not-a-duration"}`,
		`{"action":"update_policy","compile_budget":"-5s"}`,
		`{"action":"update_policy","warmup_user_min_hits":-1}`,
	} {
		resp := postPolicy(bad)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d (%s)", bad, resp.StatusCode, string(body))
		}
	}

	// Apply valid runtime policy update and confirm both /api/logs and /healthz reflect it.
	resp := postPolicy(`{"action":"update_policy","dry_run":true,"skip_cache_control":true,"compress_developer":true,"async_warmup":false,"compile_budget":"10s","warmup_user_min_hits":3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid update_policy, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, endpoint := range []string{"/api/logs", "/healthz"} {
		r, err := srv.Client().Get(srv.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			ProxySpeed map[string]any `json:"proxy_speed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			r.Body.Close()
			t.Fatal(err)
		}
		r.Body.Close()
		if doc.ProxySpeed["dry_run"] != true ||
			doc.ProxySpeed["skip_cache_control"] != true ||
			doc.ProxySpeed["compress_developer"] != true ||
			doc.ProxySpeed["async_warmup"] != false ||
			doc.ProxySpeed["compile_budget"] != "10s" ||
			doc.ProxySpeed["warmup_user_min_hits"] != float64(3) {
			t.Fatalf("%s proxy_speed mismatch: %#v", endpoint, doc.ProxySpeed)
		}
	}

	// Concurrent policy toggles alongside /v1/chat/completions proxy traffic must pass -race cleanly.
	reqBody := []byte(`{"model":"consumer-model","messages":[{"role":"developer","content":"Always verify input payloads before forwarding or storing any sensitive fields in memory."}]}`)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				dry := (id+j)%2 == 0
				raw, _ := json.Marshal(map[string]any{
					"action":               "update_policy",
					"dry_run":              dry,
					"compress_developer":   !dry,
					"warmup_user_min_hits": j % 3,
				})
				pReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/logs", bytes.NewReader(raw))
				pReq.Header.Set("Content-Type", "application/json")
				pResp, err := srv.Client().Do(pReq)
				if err == nil {
					_, _ = io.Copy(io.Discard, pResp.Body)
					pResp.Body.Close()
				}
			}
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				cReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
				cReq.Header.Set("Content-Type", "application/json")
				cReq.Header.Set("Authorization", "Bearer consumer-key")
				cResp, err := srv.Client().Do(cReq)
				if err == nil {
					_, _ = io.Copy(io.Discard, cResp.Body)
					cResp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()
}
