package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyForwardClientUsesResponseHeaderTimeoutNotTotalTimeout(t *testing.T) {
	client := newProxyForwardHTTPClient()
	if client.Timeout != 0 {
		t.Fatalf("proxy forward client Timeout = %v; want 0 so long SSE streams are not truncated", client.Timeout)
	}
	ot, ok := client.Transport.(*outboundTransport)
	if !ok || ot.base == nil {
		t.Fatalf("unexpected proxy forward transport type: %T", client.Transport)
	}
	if ot.base.ResponseHeaderTimeout <= 0 {
		t.Fatalf("proxy forward transport ResponseHeaderTimeout = %v; want > 0", ot.base.ResponseHeaderTimeout)
	}
}

func TestProxyStreamUpstreamDropAbortsClientConnection(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream ResponseWriter does not support Hijacker")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		defer conn.Close()
		// Send valid HTTP/1.1 chunked headers + one SSE chunk, then close without the 0-length terminator.
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
		_, _ = rw.WriteString("37\r\n" + chunk + "\r\n")
		_ = rw.Flush()
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(newServerMux())
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(`{"model":"consumer","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")

	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("expected error on truncated upstream SSE stream, got clean EOF with body=%q", string(data))
	}
}

func TestNegativeCacheHitsWithDomainGuidance(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyNegativeCacheTTL = 10 * time.Minute

	var genCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		writeModelContent(w, body.Messages[1].Content+"（补充说明：故意保持更长以写入负缓存并验证命中。）")
	}))
	defer compiler.Close()

	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Request-level dialect has empty text guidance, while block-level dialect matches api-contract entries.
	domains := []string{"api-contract"}
	reqDialect, err := composeDialect(defaultDialectName, domains, "")
	if err != nil {
		t.Fatal(err)
	}
	source := "POST /orders 必须携带 Idempotency-Key，且重试请求必须在 24 小时内返回相同的订单创建结果。"
	blockDialect, err := composeDialect(defaultDialectName, domains, source)
	if err != nil {
		t.Fatal(err)
	}
	if blockDialect.Guidance == reqDialect.Guidance {
		t.Fatalf("test fixture did not produce distinct block guidance: %q", blockDialect.Guidance)
	}

	profile := getActiveProfile(loadConfig(), "compiler")
	translateMessageBlock("openai", "user", source, profile, reqDialect, nil, domains)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("first call generation=%d; want 1", got)
	}
	translateMessageBlock("openai", "user", source, profile, reqDialect, nil, domains)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("negative cache missed when domain guidance matched block text: genCalls=%d", got)
	}
}

func TestProxyCachedAdoptionRespectsConfiguredTokenEncoding(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyValidationCacheTTL = 10 * time.Minute

	source := "Please review the supplied document and summarize every operational requirement, condition and exception clearly."
	// ASCII word that is fewer tokens in heuristic estimate? Let's test both:
	// 1) Shorter under o200k_base -> adopted with exact measurement kind/encoding.
	shorter := "Review doc; summarize all requirements, conditions, exceptions."
	// 2) A candidate with symbols where estimateTokensBPE might differ or where exact count is >= original.
	// Use a deterministic candidate that is equal or longer in o200k_base.
	var genCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		writeModelContent(w, shorter)
	}))
	defer compiler.Close()

	if err := writeConfigFile(Config{ActiveProfile: "exact", Profiles: map[string]Profile{
		"exact": {Name: "exact", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen", TokenEncoding: "o200k_base"},
	}}); err != nil {
		t.Fatal(err)
	}

	profile := getActiveProfile(loadConfig(), "exact")
	dialect := getDialect(defaultDialectName)
	out1, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if out1 != shorter {
		t.Fatalf("first call did not adopt shorter candidate: got %q", out1)
	}
	out2, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, nil, nil)
	if out2 != shorter || genCalls.Load() != 1 {
		t.Fatalf("cached adoption failed: out=%q genCalls=%d", out2, genCalls.Load())
	}
	logsMutex.RLock()
	latest := proxyLogs[0]
	logsMutex.RUnlock()
	if latest.MeasurementKind != "exact" || latest.MeasurementEncoding != "o200k_base" {
		t.Fatalf("cached adoption ignored configured token encoding: kind=%q encoding=%q", latest.MeasurementKind, latest.MeasurementEncoding)
	}

	// Verify exact-vs-heuristic disagreement: craft a candidate whose o200k_base tokens >= original,
	// even if heuristic estimateTokensBPE(candidate) < estimateTokensBPE(original).
	// Box-drawing chars score 2.5 in estimateTokensBPE, while ASCII runs score len/3.8.
	origTricky := strings.Repeat("├ ", 30) + "rule"
	candTricky := strings.Repeat("a1! ", 15) + "rule"
	estOrig, estCand := estimateTokensBPE(origTricky), estimateTokensBPE(candTricky)
	exactOrig, err1 := measureTokens(origTricky, "o200k_base")
	exactCand, err2 := measureTokens(candTricky, "o200k_base")
	if err1 != nil || err2 != nil {
		t.Fatalf("measureTokens failed: %v %v", err1, err2)
	}
	if estCand < estOrig && exactCand.Tokens >= exactOrig.Tokens {
		blockDialect, _ := composeDialect(defaultDialectName, nil, origTricky)
		ck := makeCacheKey(blockDialect, profile, origTricky)
		cacheMutex.Lock()
		diskCache[ck] = CacheEntry{Compiled: candTricky, Model: profile.Model, Mode: defaultDialectName, LastAccess: time.Now().Unix()}
		cacheMutex.Unlock()
		vk := makeValidationCacheKey(CompileReq{Text: origTricky}, profile.ValidationMode, profile, profile, candTricky)
		validationCache.Put(vk, newValidation("passed", "llm_compare"), time.Minute)
		if _, _, _, hit := proxyCachedAdoption("openai", "user", origTricky, profile, blockDialect, nil, nil, ck); hit {
			t.Fatalf("proxyCachedAdoption adopted candidate that was longer in o200k_base (%d -> %d)", exactOrig.Tokens, exactCand.Tokens)
		}
	}
}

func TestProxyAsyncWarmupUsesActiveKeywords(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyAsyncWarmup = true
	proxyValidationCacheTTL = 10 * time.Minute

	const source = "订单处于待审核状态时，必须由主管批准后才能继续处理，并记录审批人与操作时间。"
	const compiled = "状态=待审⇒须主管批准∧记录{审批人,时间}"

	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		writeModelContent(w, compiled)
	}))
	defer compiler.Close()

	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}

	profile := getActiveProfile(loadConfig(), "compiler")
	dialect := getDialect(defaultDialectName)

	t.Cleanup(func() {
		drainShadowWarmup(2 * time.Second)
	})
	// Pass reqKeywords=["json"] (as OpenAI proxy does for response_format=json_object)
	// even though source does NOT contain "json". Warmup must filter to activeKeywords (empty).
	out, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, []string{"json"}, nil)
	if out != source {
		t.Fatalf("first cold call should pass through original; got %q", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _, _, _ := translateMessageBlock("openai", "user", source, profile, dialect, []string{"json"}, nil)
		if got == compiled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async warmup with filtered activeKeywords never populated validation cache for adoption")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestProxyAsyncLogRedactionOrdersUsageUpdate(t *testing.T) {
	isolateProxyState(t)
	oldAsync := logRedactAsync.Load()
	startAsyncLogRedaction()
	t.Cleanup(func() {
		drainAsyncLogRedaction(2 * time.Second)
		logRedactAsync.Store(oldAsync)
	})

	const total = 20
	for i := 0; i < total; i++ {
		id := "ordered-usage-" + string(rune('a'+i))
		appendProxyLog(ProxyLog{ID: id, Original: "hello sk-abcdefghijklmnopqrstuv", Compiled: "hi"})
		applyUpstreamUsage([]string{id}, 100, 50, 77+i)
	}
	drainAsyncLogRedaction(2 * time.Second)

	logsMutex.RLock()
	defer logsMutex.RUnlock()
	if len(proxyLogs) < total {
		t.Fatalf("expected at least %d logs, got %d", total, len(proxyLogs))
	}
	for i := 0; i < total; i++ {
		id := "ordered-usage-" + string(rune('a'+i))
		found := false
		for _, entry := range proxyLogs {
			if entry.ID == id {
				found = true
				if entry.RealPromptTokens != 77+i {
					t.Fatalf("log %s lost usage update due to async race: got %d want %d", id, entry.RealPromptTokens, 77+i)
				}
				if !entry.Redacted {
					t.Fatalf("log %s was not redacted", id)
				}
				break
			}
		}
		if !found {
			t.Fatalf("log %s missing after drain", id)
		}
	}
}

func TestProxyMarshalDisablesHTMLEscaping(t *testing.T) {
	encoded, err := marshalProxyPayload(map[string]any{
		"content": "if a < b && c > d { return }",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`\u003c`)) || bytes.Contains(encoded, []byte(`\u003e`)) || bytes.Contains(encoded, []byte(`\u0026`)) {
		t.Fatalf("marshalProxyPayload escaped HTML characters: %s", string(encoded))
	}
}

func TestIsNonProsePayloadSingleFenceOnly(t *testing.T) {
	singleFence := "```json\n{\"a\": 1, \"b\": 2}\n```"
	if !isNonProsePayload(singleFence) {
		t.Fatal("single fenced block should be detected as non-prose")
	}
	sandwichedProse := "```json\n{\"input\": 1}\n```\n\n请严格遵守以下十条自然语言业务规则，对上面的输入进行合规性审查并给出详细结论。\n\n```json\n{\"output\": \"ok\"}\n```"
	if isNonProsePayload(sandwichedProse) {
		t.Fatal("prose sandwiched between two fenced code blocks must NOT be skipped as non-prose")
	}
}

func TestProxyTransparentHeadersAndHopByHopStripping(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	var gotReqHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Trace", "trace-123")
		w.Header().Set("Connection", "X-Resp-Hop")
		w.Header().Set("X-Resp-Hop", "should-be-stripped")
		w.Header().Set("Keep-Alive", "timeout=5")
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer user-key")
	req.Header.Set("OpenAI-Organization", "org-xyz")
	req.Header.Set("OpenAI-Beta", "assistants=v2")
	req.Header.Set("HTTP-Referer", "https://app.example")
	req.Header.Set("X-Title", "MyApp")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "must-not-forward")
	req.Header.Set("X-Babel-Profile", "fixture")
	req.Header.Set("X-Upstream-BaseURL", upstream.URL)

	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]string{
		"OpenAI-Organization": "org-xyz",
		"OpenAI-Beta":         "assistants=v2",
		"HTTP-Referer":        "https://app.example",
		"X-Title":             "MyApp",
		"traceparent":         "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		if got := gotReqHeaders.Get(k); got != want {
			t.Errorf("upstream missing forwarded header %s: got %q want %q", k, got, want)
		}
	}
	for _, forbidden := range []string{"X-Custom-Hop", "X-Babel-Profile", "X-Upstream-BaseURL"} {
		if got := gotReqHeaders.Get(forbidden); got != "" {
			t.Errorf("upstream leaked stripped header %s=%q", forbidden, got)
		}
	}
	if rec.Header().Get("X-Upstream-Trace") != "trace-123" {
		t.Fatalf("response lost X-Upstream-Trace: %#v", rec.Header())
	}
	for _, stripped := range []string{"Connection", "X-Resp-Hop", "Keep-Alive"} {
		if got := rec.Header().Get(stripped); got != "" {
			t.Errorf("response leaked hop-by-hop header %s=%q", stripped, got)
		}
	}

	// Anthropic: both x-api-key and Authorization must survive when caller sends both.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "anth-key")
	req2.Header.Set("Authorization", "Bearer extra-auth")
	rec2 := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("anthropic status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if gotReqHeaders.Get("x-api-key") != "anth-key" || gotReqHeaders.Get("Authorization") != "Bearer extra-auth" {
		t.Fatalf("anthropic dropped one of dual auth headers: x-api-key=%q auth=%q", gotReqHeaders.Get("x-api-key"), gotReqHeaders.Get("Authorization"))
	}
}

func TestProxyPreservesQueryStringAndOpenAIHostCompletion(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	for _, tc := range []struct {
		in, want string
	}{
		{"https://api.openai.com", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1?tenant=a", "https://api.openai.com/v1/chat/completions?tenant=a"},
		{"https://azure.example/openai/deployments/d/chat/completions?api-version=2024-10-21", "https://azure.example/openai/deployments/d/chat/completions?api-version=2024-10-21"},
	} {
		if got := buildOpenAICompletionsURL(tc.in); got != tc.want {
			t.Errorf("buildOpenAICompletionsURL(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := buildAnthropicMessagesURL("https://api.anthropic.com?beta=1"); got != "https://api.anthropic.com/v1/messages?beta=1" {
		t.Errorf("buildAnthropicMessagesURL lost query: %q", got)
	}

	var gotURI string
	var gotAzureKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		gotAzureKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-version=2024-10-21", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", "azure-secret")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotURI != "/v1/chat/completions?api-version=2024-10-21" || gotAzureKey != "azure-secret" {
		t.Fatalf("query or api-key lost: uri=%q api-key=%q", gotURI, gotAzureKey)
	}
}

func TestProxyV1CatchAllPassthrough(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	var lastMethod, lastURI string
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastMethod = r.Method
		lastURI = r.RequestURI
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "stored-secret", BaseURL: upstream.URL + "/v1", UpstreamURL: upstream.URL + "/v1", Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated request must be rejected with 401 JSON and never touch upstream.
	unauth := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, unauth)
	if rec.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthenticated /v1/models not rejected: status=%d calls=%d", rec.Code, calls.Load())
	}

	// GET /v1/models passes through without duplicating /v1.
	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models?limit=10", nil)
	modelsReq.Header.Set("Authorization", "Bearer caller-key")
	rec = httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, modelsReq)
	if rec.Code != http.StatusOK || lastMethod != http.MethodGet || lastURI != "/v1/models?limit=10" {
		t.Fatalf("GET /v1/models failed: status=%d method=%s uri=%s body=%s", rec.Code, lastMethod, lastURI, rec.Body.String())
	}

	// POST /v1/responses streams SSE cleanly.
	respReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	respReq.Header.Set("Authorization", "Bearer caller-key")
	respReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, respReq)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "response.completed") || lastURI != "/v1/responses" {
		t.Fatalf("POST /v1/responses failed: status=%d uri=%s body=%s", rec.Code, lastURI, rec.Body.String())
	}

	// POST /v1/messages/count_tokens passes through with Anthropic headers.
	ctReq := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude","messages":[]}`))
	ctReq.Header.Set("x-api-key", "anth-caller")
	ctReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, ctReq)
	if rec.Code != http.StatusOK || lastURI != "/v1/messages/count_tokens" {
		t.Fatalf("POST /v1/messages/count_tokens failed: status=%d uri=%s", rec.Code, lastURI)
	}
}

func TestProxyReturnsProtocolNativeJSONErrors(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	// OpenAI 401 error must be OpenAI JSON schema.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("openai 401 not JSON: status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var openAIErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &openAIErr); err != nil || openAIErr.Error.Message == "" || openAIErr.Error.Type == "" {
		t.Fatalf("openai error did not match protocol schema: %s", rec.Body.String())
	}

	// Anthropic 401 error must be Anthropic JSON schema.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"c","messages":[]}`))
	rec2 := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized || !strings.Contains(rec2.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("anthropic 401 not JSON: status=%d ct=%q", rec2.Code, rec2.Header().Get("Content-Type"))
	}
	var anthErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &anthErr); err != nil || anthErr.Type != "error" || anthErr.Error.Message == "" {
		t.Fatalf("anthropic error did not match protocol schema: %s", rec2.Body.String())
	}
}

func TestProxyOversizeBodyStreamsDirectlyWithoutCompiling(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	t.Setenv("BABEL_PROXY_MAX_BODY_BYTES", "8192")
	t.Setenv("BABEL_PROXY_MAX_COMPILE_BYTES", "256")

	var compilerCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compilerCalls.Add(1)
		writeModelContent(w, "short")
	}))
	defer compiler.Close()

	var forwardedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"model":"consumer","messages":[{"role":"user","content":"` + strings.Repeat("A", 1024) + `"}]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	rec := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("oversize streaming passthrough failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if compilerCalls.Load() != 0 {
		t.Fatalf("oversize body triggered compiler %d times; want 0", compilerCalls.Load())
	}
	if !bytes.Equal(forwardedBody, payload) {
		t.Fatalf("oversize body was altered during streaming passthrough: len=%d/%d", len(forwardedBody), len(payload))
	}
}

func TestProxyCompileBudgetFailsOpenToUpstream(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	compilerDone := make(chan struct{})
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(compilerDone)
		select {
		case <-time.After(2 * time.Second):
			writeModelContent(w, "too late")
		case <-r.Context().Done():
		}
	}))
	defer compiler.Close()
	t.Cleanup(func() {
		select {
		case <-compilerDone:
		case <-time.After(time.Second):
		}
		// Wait for singleflight goroutine to exit before isolateCompilerState resets compileFlight.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			compileFlight.mu.Lock()
			empty := len(compileFlight.calls) == 0
			compileFlight.mu.Unlock()
			if empty {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	const source = "Please review this entire technical specification and retain every single operational condition, boundary and exception clearly."
	var forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 {
			forwarded = body.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{
		"model":    "consumer",
		"messages": []any{map[string]any{"role": "user", "content": source}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	req.Header.Set("X-Babel-Compile-Budget", "80ms")

	start := time.Now()
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("compile budget timeout should fail-open to upstream: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("compile budget did not cap latency: elapsed=%v", elapsed)
	}
	if forwarded != source {
		t.Fatalf("expected original source forwarded on budget timeout; got %q", forwarded)
	}
	if got := rec.Header().Get("X-Babel-Status"); got != "budget_timeout" {
		t.Fatalf("X-Babel-Status = %q; want budget_timeout", got)
	}
	if rec.Header().Get("X-Babel-Compile-Ms") == "" {
		t.Fatal("missing X-Babel-Compile-Ms diagnostic header")
	}
}

func TestProxySkipCacheControlProtectsCachedPrefix(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	const cachedSys0 = "Prefix system instruction block zero that precedes a cache_control breakpoint and must remain completely untouched."
	const cachedSys1 = "Prefix system instruction block one carrying an explicit Anthropic ephemeral cache_control marker for prompt caching."
	const cachedTool0 = "Tool zero description preceding a cache_control breakpoint on a later tool definition; must stay byte-identical."
	const cachedTool1 = "Tool one description carrying an explicit cache_control marker so tools zero and one form a cached prefix."
	const uncachedTool2 = "Tool two description located after the final cache_control marker so it remains eligible for semantic compression."
	const uncachedTool2Short = "Tool 2 after cache_control; compressible."
	const cachedUser0 = "Earlier user message in multi-turn history marked with cache_control so the conversation prefix stays cached."
	const latestUser = "Latest uncached user request asking to summarize every operational requirement, condition and exception clearly."
	const latestUserShort = "Summarize all operational requirements, conditions, exceptions."

	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		orig := decodeCompilerInput(t, req.Messages[1].Content).Original
		switch orig {
		case uncachedTool2:
			writeModelContent(w, uncachedTool2Short)
		case latestUser:
			writeModelContent(w, latestUserShort)
		default:
			t.Errorf("compiler received a cache_control-protected prefix block: %q", orig)
			writeModelContent(w, orig)
		}
	}))
	defer compiler.Close()

	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded, _ = decodeProxyPayload(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":50}}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"model":      "claude-sonnet",
		"max_tokens": 128,
		"system": []any{
			map[string]any{"type": "text", "text": cachedSys0},
			map[string]any{"type": "text", "text": cachedSys1, "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"tools": []any{
			map[string]any{"name": "t0", "description": cachedTool0, "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "t1", "description": cachedTool1, "cache_control": map[string]any{"type": "ephemeral"}, "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "t2", "description": uncachedTool2, "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": cachedUser0, "cache_control": map[string]any{"type": "ephemeral"}}}},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": latestUser},
		},
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "anth-key")
	req.Header.Set("X-Babel-Skip-Cache-Control", "true")

	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Babel-Status") != "adopted" {
		t.Fatalf("X-Babel-Status=%q; want adopted", rec.Header().Get("X-Babel-Status"))
	}

	sys := forwarded["system"].([]any)
	if sys[0].(map[string]any)["text"] != cachedSys0 || sys[1].(map[string]any)["text"] != cachedSys1 {
		t.Fatal("cached system prefix was mutated")
	}
	tools := forwarded["tools"].([]any)
	if tools[0].(map[string]any)["description"] != cachedTool0 || tools[1].(map[string]any)["description"] != cachedTool1 {
		t.Fatal("cached tools prefix was mutated")
	}
	if tools[2].(map[string]any)["description"] != uncachedTool2Short {
		t.Fatalf("uncached tool after breakpoint was not compressed: got %q", tools[2].(map[string]any)["description"])
	}
	msgs := forwarded["messages"].([]any)
	if msgs[2].(map[string]any)["content"] != latestUserShort {
		t.Fatalf("uncached latest user message was not compressed: got %q", msgs[2].(map[string]any)["content"])
	}
}

func TestProxyBypassHeaderStreamsWithoutCompiling(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)

	var compilerCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compilerCalls.Add(1)
		writeModelContent(w, "short")
	}))
	defer compiler.Close()

	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Bypass still enforces caller authentication.
	unauth := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	unauth.Header.Set("X-Babel-Bypass", "true")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, unauth)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bypass without auth should return 401; got %d", rec.Code)
	}

	body := `{"model":"m","messages":[{"role":"user","content":"A very long natural language prompt that would normally trigger compilation if bypass were not enabled."}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer user-key")
	req.Header.Set("X-Babel-Bypass", "1")
	rec = httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || compilerCalls.Load() != 0 || gotBody != body {
		t.Fatalf("bypass failed: status=%d compilerCalls=%d gotBody=%q", rec.Code, compilerCalls.Load(), gotBody)
	}
	if rec.Header().Get("X-Babel-Status") != "bypass" {
		t.Fatalf("X-Babel-Status=%q; want bypass", rec.Header().Get("X-Babel-Status"))
	}
}

func TestSSEUsageSamplerHandlesSplitChunksAndLargePadding(t *testing.T) {
	// OpenAI: split across 7-byte chunks with >8KB of preceding deltas so fixed tail slicing isn't needed.
	oaStream := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\n", 200) +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":432,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n"
	oaSampler := newOpenAISSEUsageSampler()
	for i := 0; i < len(oaStream); i += 7 {
		end := i + 7
		if end > len(oaStream) {
			end = len(oaStream)
		}
		oaSampler.Write([]byte(oaStream[i:end]))
	}
	if got := oaSampler.Tokens(); got != 432 {
		t.Fatalf("OpenAI SSE sampler got %d; want 432", got)
	}

	// Anthropic: >10KB of SSE comments before message_start (would overflow old fixed 8KB headBuf!).
	anthStream := strings.Repeat(": ping keep-alive comment\n", 450) +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":20,\"cache_read_input_tokens\":300}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	anthSampler := newAnthropicSSEUsageSampler()
	for i := 0; i < len(anthStream); i += 13 {
		end := i + 13
		if end > len(anthStream) {
			end = len(anthStream)
		}
		anthSampler.Write([]byte(anthStream[i:end]))
	}
	if got := anthSampler.Tokens(); got != 420 {
		t.Fatalf("Anthropic SSE sampler got %d; want 420", got)
	}
}

func TestValidationCacheSlidingTTLRenewal(t *testing.T) {
	cache := newTTLLRU[string, string](16)
	cache.Put("hot", "v1", 120*time.Millisecond)
	time.Sleep(70 * time.Millisecond)
	if v, ok := cache.Get("hot"); !ok || v != "v1" {
		t.Fatalf("expected hit before initial TTL: ok=%t v=%q", ok, v)
	}
	// Another 70ms (total 140ms > original 120ms): sliding renewal must keep it alive.
	time.Sleep(70 * time.Millisecond)
	if v, ok := cache.Get("hot"); !ok || v != "v1" {
		t.Fatalf("expected sliding TTL renewal to keep active entry alive: ok=%t v=%q", ok, v)
	}
	// Idle past TTL: must expire.
	time.Sleep(150 * time.Millisecond)
	if _, ok := cache.Get("hot"); ok {
		t.Fatal("expected idle entry to expire after TTL")
	}
}

func TestHealthzEndpointAndMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status=%d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid /healthz JSON: %v", err)
	}
	for _, key := range []string{"status", "version", "circuit", "concurrency", "proxy_speed", "metrics"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("/healthz missing key %q: %v", key, payload)
		}
	}

	postReq := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	postRec := httptest.NewRecorder()
	newServerMux().ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status=%d; want 405", postRec.Code)
	}
}

func TestProxyWarmupAdmissionFilterProtectsOneOffUserPrompts(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyAsyncWarmup = true
	proxyWarmupUserMinHits = 2

	var genCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		writeModelContent(w, "压缩结果⇒更短")
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compiler.URL, Model: "gen"},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drainShadowWarmup(2 * time.Second) })

	profile := getActiveProfile(loadConfig(), "compiler")
	dialect := getDialect(defaultDialectName)

	// High-reuse system prompt warms immediately on first hit.
	sysText := "你是风控审核助手，必须严格核对每笔交易的限额、权限与留痕要求，不得跳过任何审批节点。"
	translateMessageBlock("openai", "system", sysText, profile, dialect, nil, nil)
	drainShadowWarmup(2 * time.Second)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("system block should warm immediately on 1st hit: genCalls=%d", got)
	}

	// One-off user prompt is skipped on 1st hit when proxyWarmupUserMinHits=2.
	userText := "请帮我核对今天下午三点提交的采购申请单，确认所有金额、审批人与附件字段是否完整。"
	translateMessageBlock("openai", "user", userText, profile, dialect, nil, nil)
	drainShadowWarmup(2 * time.Second)
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("one-off user prompt should not burn compiler quota on 1st hit: genCalls=%d", got)
	}

	// Seeing the same user prompt a 2nd time triggers warmup.
	translateMessageBlock("openai", "user", userText, profile, dialect, nil, nil)
	drainShadowWarmup(2 * time.Second)
	if got := genCalls.Load(); got != 2 {
		t.Fatalf("repeated user prompt should warm on 2nd hit: genCalls=%d", got)
	}

	// Setting proxyWarmupUserMinHits=0 disables user warmup completely.
	proxyWarmupUserMinHits = 0
	otherUser := "另一段完全不同的用户消息，用来确认当阈值为零时即使多次出现也不会进入后台预热队列。"
	for i := 0; i < 3; i++ {
		translateMessageBlock("openai", "user", otherUser, profile, dialect, nil, nil)
	}
	drainShadowWarmup(2 * time.Second)
	if got := genCalls.Load(); got != 2 {
		t.Fatalf("user warmup should stay disabled when min hits=0: genCalls=%d", got)
	}
}

func TestProxyChunkedOversizeStreamsAndOverHardCapReturns413(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	t.Setenv("BABEL_PROXY_MAX_BODY_BYTES", "4096")
	t.Setenv("BABEL_PROXY_MAX_COMPILE_BYTES", "256")

	var compilerCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compilerCalls.Add(1)
		writeModelContent(w, "short")
	}))
	defer compiler.Close()

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Chunked (ContentLength = -1) body > compileLimit (256) and <= hardCap (4096):
	// must stream directly upstream with X-Babel-Status: oversize and 0 compiler calls.
	payload := []byte(`{"model":"consumer","messages":[{"role":"user","content":"` + strings.Repeat("B", 1024) + `"}]}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", bytes.NewReader(payload))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	rec := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Babel-Status") != "oversize" {
		t.Fatalf("chunked oversize passthrough failed: status=%d header=%q body=%s", rec.Code, rec.Header().Get("X-Babel-Status"), rec.Body.String())
	}
	if compilerCalls.Load() != 0 || !bytes.Equal(gotBody, payload) {
		t.Fatalf("chunked oversize body altered or compiled: calls=%d len=%d/%d", compilerCalls.Load(), len(gotBody), len(payload))
	}

	// Chunked body exceeding hardCap (4096) while still below compileLimit (set compileLimit=8192, hardCap=512):
	// must return 413 Request Entity Too Large (not 400).
	t.Setenv("BABEL_PROXY_MAX_BODY_BYTES", "512")
	t.Setenv("BABEL_PROXY_MAX_COMPILE_BYTES", "8192")
	tooBig := []byte(`{"model":"consumer","messages":[{"role":"user","content":"` + strings.Repeat("C", 1024) + `"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", bytes.NewReader(tooBig))
	req2.ContentLength = -1
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer consumer-key")
	rec2 := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked body over hard cap returned %d; want 413 (body=%s)", rec2.Code, rec2.Body.String())
	}
}

func TestValidateAPIHonorsAuditProfile(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)

	var compilerCalls, auditorCalls atomic.Int32
	compilerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compilerCalls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer compilerSrv.Close()

	auditorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditorCalls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer auditorSrv.Close()

	if err := writeConfigFile(Config{ActiveProfile: "compiler", Profiles: map[string]Profile{
		"compiler": {Name: "compiler", APIKey: "gen-key", BaseURL: compilerSrv.URL, Model: "gen-model", AuditProfile: "auditor"},
		"auditor":  {Name: "auditor", APIKey: "audit-key", BaseURL: auditorSrv.URL, Model: "audit-model"},
	}}); err != nil {
		t.Fatal(err)
	}

	body := `{"profile":"compiler","text":"订单金额大于100元时必须审批。","candidate":"金额>100元⇒须审批"}`
	req := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/validate status=%d body=%s", rec.Code, rec.Body.String())
	}
	if auditorCalls.Load() != 1 || compilerCalls.Load() != 0 {
		t.Fatalf("/api/validate ignored audit_profile: auditorCalls=%d compilerCalls=%d", auditorCalls.Load(), compilerCalls.Load())
	}
}

func TestCompileStreamRewriteNoDuplicateDeltaAndBoundsRawBytes(t *testing.T) {
	isolateCompilerState(t)

	// 1. Multi-section input triggers one structural rewrite; streamed deltas must NOT duplicate candidate.
	source := "你是售后助理，处理退款且每步留痕。\n一、受理资格\n1. 只有订单已签收且商品未拆封，才允许退款。\n二、执行流程\n1. 先锁单，再优先原路退款；锁定期间禁止并发修改金额。\n三、记录要求\n1. 所有写操作保留前后快照，含订单号和金额，保留不少于180天。"
	flatDraft := "一、受理资格\n仅签收且未拆封可退\n二、执行流程\n先锁单再原路退款\n三、记录要求\n所有写操作保留快照180天"
	if len(babelStructureIssues(source, flatDraft)) == 0 {
		t.Fatal("test fixture flatDraft did not trigger babelStructureIssues")
	}
	var callNum atomic.Int32
	rewrittenPart1 := "role:售后助理\n├许可only if已签收∧未拆封\n"
	rewrittenPart2 := "└flow:锁单→原路退款; ∀写操作保留快照≥180天"
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callNum.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + mustJSON(flatDraft) + "},\"finish_reason\":\"stop\"}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + mustJSON(rewrittenPart1) + "}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + mustJSON(rewrittenPart2) + "},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer compiler.Close()

	p := Profile{Name: "test", APIKey: "k", BaseURL: compiler.URL, Model: "m"}
	dialect, err := composeDialect(defaultDialectName, nil, source)
	if err != nil {
		t.Fatal(err)
	}

	var displayed strings.Builder
	got, err := generateBabelCandidateStream(context.Background(), p, dialect, source, streamHooks{
		onReset: func() { displayed.Reset() },
		onDelta: func(chunk string) { displayed.WriteString(chunk) },
	})
	if err != nil {
		t.Fatalf("generateBabelCandidateStream failed: %v", err)
	}
	want := rewrittenPart1 + rewrittenPart2
	if got != want || displayed.String() != want {
		t.Fatalf("rewrite stream duplicated or altered deltas:\n  got=%q\n  displayed=%q\n  want=%q", got, displayed.String(), want)
	}

	// 2. Oversized raw SSE comments (no delta.content) must still be bounded by maxModelResponseBytes.
	commentFlood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		line := strings.Repeat(": keepalive ", 100) + "\n"
		for sent := 0; sent <= maxModelResponseBytes+4096; sent += len(line) {
			if _, err := w.Write([]byte(line)); err != nil {
				return
			}
		}
	}))
	defer commentFlood.Close()
	floodProfile := Profile{Name: "flood", APIKey: "k", BaseURL: commentFlood.URL, Model: "m"}
	_, _, err = callLLMStreamContext(context.Background(), floodProfile, "sys", "user", 0, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("expected raw SSE byte-limit error, got err=%v", err)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParallelChunkCompileIsolatedFromProxySemaphore(t *testing.T) {
	isolateCompilerState(t)

	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeModelContent(w, "├规则⇒执行\n└异常⇒拒绝")
	}))
	defer compiler.Close()

	// Completely saturate proxyCompileSem (1/1 held): chunk compilation must use
	// chunkCompileSem and never self-deadlock waiting on proxyCompileSem.
	oldSem := proxyCompileSem
	proxyCompileSem = newCompileSemaphore(1)
	proxyCompileSem.slots <- struct{}{}
	t.Cleanup(func() {
		<-proxyCompileSem.slots
		proxyCompileSem = oldSem
	})

	paragraph := "审批流程要求在提交之前完成全部检查，并在执行之后记录结果，确保每个环节都能追溯到责任人。\n\n"
	source := strings.Repeat(paragraph, 45)
	p := Profile{Name: "chunk", APIKey: "k", BaseURL: compiler.URL, Model: "m"}
	dialect, _ := composeDialect(defaultDialectName, nil, source)

	done := make(chan error, 1)
	go func() {
		_, err := generateBabelCandidateParallel(context.Background(), p, dialect, source)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parallel chunk compile failed under saturated proxyCompileSem: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parallel chunk compile deadlocked on saturated proxyCompileSem")
	}
}

func TestOpenAIResponsesProxyCompressesInstructionsInputToolsAndDeepSchema(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	const instr = "Follow all operational requirements and retain every authorization condition and exception when reviewing requests."
	const instrShort = "Follow requirements; retain all auth conditions & exceptions."
	const userText = "Please review the supplied payment specification and summarize every rule, threshold and boundary clearly."
	const userShort = "Review payment spec; summarize rules, thresholds, boundaries."
	const toolDesc = "Execute a comprehensive compliance check across the target order and return structured findings for every rule."
	const toolDescShort = "Run order compliance check; return rule findings."
	const defDesc = "Detailed schema definition describing the strict verification policy applied to nested line items in an order."
	const defDescShort = "Verification policy for nested order line items."
	const anyOfDesc = "Alternative filter expression allowing either a numeric threshold or a named tier identifier for matching."
	const anyOfDescShort = "Filter: numeric threshold or named tier."
	const protectedToolOut = "Immutable function_call_output payload that must never be sent to the compiler under any circumstance."

	mapping := map[string]string{
		instr:     instrShort,
		userText:  userShort,
		toolDesc:  toolDescShort,
		defDesc:   defDescShort,
		anyOfDesc: anyOfDescShort,
	}

	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		orig := decodeCompilerInput(t, req.Messages[1].Content).Original
		if out, ok := mapping[orig]; ok {
			writeModelContent(w, out)
			return
		}
		t.Errorf("unexpected text sent to compiler: %q", orig)
		writeModelContent(w, orig)
	}))
	defer compiler.Close()

	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected responses path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		forwarded, _ = decodeProxyPayload(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":188}}}\n\n"))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"model":        "gpt-5",
		"stream":       true,
		"instructions": instr,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": userText},
				},
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  protectedToolOut,
			},
		},
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "audit_order",
				"description": toolDesc,
				"parameters": map[string]any{
					"type": "object",
					"$defs": map[string]any{
						"LinePolicy": map[string]any{"type": "object", "description": defDesc},
					},
					"properties": map[string]any{
						"filter": map[string]any{
							"anyOf": []any{
								map[string]any{"type": "number", "description": anyOfDesc},
								map[string]any{"type": "string"},
							},
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("X-Babel-Status") != "adopted" {
		t.Fatalf("/v1/responses failed: status=%d babel=%q body=%s", rec.Code, rec.Header().Get("X-Babel-Status"), rec.Body.String())
	}
	if forwarded["instructions"] != instrShort {
		t.Fatalf("instructions not compressed: %v", forwarded["instructions"])
	}
	inputs := forwarded["input"].([]any)
	gotUser := inputs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if gotUser != userShort {
		t.Fatalf("input_text not compressed: %v", gotUser)
	}
	if inputs[1].(map[string]any)["output"] != protectedToolOut {
		t.Fatalf("function_call_output was mutated: %v", inputs[1])
	}
	tool := forwarded["tools"].([]any)[0].(map[string]any)
	if tool["description"] != toolDescShort {
		t.Fatalf("flat tool description not compressed: %v", tool["description"])
	}
	params := tool["parameters"].(map[string]any)
	if params["$defs"].(map[string]any)["LinePolicy"].(map[string]any)["description"] != defDescShort {
		t.Fatal("$defs description not compressed")
	}
	if params["properties"].(map[string]any)["filter"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["description"] != anyOfDescShort {
		t.Fatal("anyOf description not compressed")
	}
	logsMutex.RLock()
	hasUsage := len(proxyLogs) > 0 && proxyLogs[0].RealPromptTokens == 188
	logsMutex.RUnlock()
	if !hasUsage {
		t.Fatalf("Responses SSE usage (188) was not recorded in proxyLogs")
	}
}

func TestProxyCompressDeveloperRoleIsOptIn(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	const devText = "You are a strict compliance auditor; preserve every policy rule, numerical threshold and exception in your review."
	const devShort = "role:compliance auditor; preserve all rules, thresholds, exceptions."

	var devCompileCalls atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		devCompileCalls.Add(1)
		writeModelContent(w, devShort)
	}))
	defer compiler.Close()

	var lastDev string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 {
			lastDev = body.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{
		"model":    "o3",
		"messages": []any{map[string]any{"role": "developer", "content": devText}},
	})

	// Default: developer role is NOT compressed.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer user-key")
	rec1 := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || devCompileCalls.Load() != 0 || lastDev != devText {
		t.Fatalf("default developer handling altered text: calls=%d got=%q", devCompileCalls.Load(), lastDev)
	}

	// Opt-in via header: developer role IS compressed.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer user-key")
	req2.Header.Set("X-Babel-Compress-Developer", "true")
	rec2 := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || devCompileCalls.Load() != 1 || lastDev != devShort {
		t.Fatalf("opt-in developer compression failed: status=%d calls=%d got=%q", rec2.Code, devCompileCalls.Load(), lastDev)
	}
}

func TestProxyCORSAllowlistScopedToV1Only(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	t.Setenv("BABEL_PROXY_ALLOWED_ORIGINS", "http://localhost:3000, *, null")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	handler := guardedLocalHandler(t)

	// Allowed origin preflight OPTIONS on /v1/chat/completions succeeds with 204 and exact CORS headers.
	optReq := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:8080/v1/chat/completions", nil)
	optReq.Header.Set("Origin", "http://localhost:3000")
	optReq.Header.Set("Sec-Fetch-Site", "cross-site")
	optReq.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	optRec := httptest.NewRecorder()
	handler.ServeHTTP(optRec, optReq)
	if optRec.Code != http.StatusNoContent || optRec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("preflight failed: status=%d acao=%q", optRec.Code, optRec.Header().Get("Access-Control-Allow-Origin"))
	}

	// Allowed origin POST on /v1/chat/completions succeeds.
	postReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Authorization", "Bearer user-key")
	postReq.Header.Set("Origin", "http://localhost:3000")
	postReq.Header.Set("Sec-Fetch-Site", "cross-site")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK || postRec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("allowed cross-origin proxy POST failed: status=%d body=%s", postRec.Code, postRec.Body.String())
	}

	// Wildcard/null in env var must NEVER allow arbitrary or opaque origins.
	for _, badOrigin := range []string{"http://evil.example", "null"} {
		badReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/chat/completions", strings.NewReader(`{}`))
		badReq.Header.Set("Content-Type", "application/json")
		badReq.Header.Set("Origin", badOrigin)
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusForbidden {
			t.Fatalf("origin %q should be rejected with 403; got %d", badOrigin, badRec.Code)
		}
	}

	// Management API /api/config must NEVER accept cross-origin even when in BABEL_PROXY_ALLOWED_ORIGINS.
	mgmtReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/config", nil)
	mgmtReq.Header.Set("Origin", "http://localhost:3000")
	mgmtRec := httptest.NewRecorder()
	handler.ServeHTTP(mgmtRec, mgmtReq)
	if mgmtRec.Code != http.StatusForbidden {
		t.Fatalf("/api/config leaked cross-origin access: status=%d", mgmtRec.Code)
	}
}

func TestDiskCacheEvictsByByteLimitLRU(t *testing.T) {
	isolateProxyState(t)
	t.Setenv("BABEL_CACHE_MAX_BYTES", "700")

	cacheMutex.Lock()
	diskCache["k1-oldest"] = CacheEntry{Compiled: strings.Repeat("a", 200), LastAccess: 100}
	diskCache["k2-old"] = CacheEntry{Compiled: strings.Repeat("b", 200), LastAccess: 200}
	diskCache["k3-new"] = CacheEntry{Compiled: strings.Repeat("c", 200), LastAccess: 300}
	diskCache["k4-newest"] = CacheEntry{Compiled: strings.Repeat("d", 200), LastAccess: 400}
	evictCacheIfNeededLocked()
	remainingBytes := diskCacheApproxBytesLocked()
	_, hasOldest := diskCache["k1-oldest"]
	_, hasNewest := diskCache["k4-newest"]
	count := len(diskCache)
	cacheMutex.Unlock()

	if remainingBytes > 700 {
		t.Fatalf("diskCache exceeded byte budget: %d > 700", remainingBytes)
	}
	if hasOldest || !hasNewest || count == 0 {
		t.Fatalf("LRU byte eviction kept wrong entries: count=%d hasOldest=%t hasNewest=%t", count, hasOldest, hasNewest)
	}
}

func TestProxyCORSOverwritesUpstreamDuplicateACAOAndStripsOrigin(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	t.Setenv("BABEL_PROXY_ALLOWED_ORIGINS", "http://localhost:3000")

	var upstreamHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "anth-key")
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("HTTP-Referer", "https://myapp.example")
	req.Header.Set("X-Title", "MyLocalApp")
	rec := httptest.NewRecorder()
	guardedLocalHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("CORS proxy request failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "http://localhost:3000" {
		t.Fatalf("Access-Control-Allow-Origin duplicated or wrong: %#v", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("Access-Control-Allow-Credentials should be stripped: %q", rec.Header().Get("Access-Control-Allow-Credentials"))
	}
	varyOriginCount := 0
	for _, v := range rec.Header().Values("Vary") {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "Origin") {
				varyOriginCount++
			}
		}
	}
	if varyOriginCount != 1 {
		t.Fatalf("expected exactly 1 Vary: Origin token, got %d (%#v)", varyOriginCount, rec.Header().Values("Vary"))
	}
	if upstreamHeaders.Get("Origin") != "" || upstreamHeaders.Get("Sec-Fetch-Site") != "" || upstreamHeaders.Get("Sec-Fetch-Mode") != "" {
		t.Fatalf("upstream leaked browser Origin/Sec-Fetch headers: %#v", upstreamHeaders)
	}
	if upstreamHeaders.Get("HTTP-Referer") != "https://myapp.example" || upstreamHeaders.Get("X-Title") != "MyLocalApp" {
		t.Fatalf("upstream lost HTTP-Referer/X-Title: %#v", upstreamHeaders)
	}
}

func TestBuildV1PassthroughURLStripsEndpointSuffix(t *testing.T) {
	for _, tc := range []struct {
		base, reqPath, want string
	}{
		{"https://api.openai.com/v1/chat/completions", "/v1/models", "https://api.openai.com/v1/models"},
		{"https://api.anthropic.com/v1/messages", "/v1/messages/count_tokens", "https://api.anthropic.com/v1/messages/count_tokens"},
		{"https://api.openai.com/v1/responses", "/v1/embeddings", "https://api.openai.com/v1/embeddings"},
		{"https://gateway.example/openai/v1/chat/completions", "/v1/models", "https://gateway.example/openai/v1/models"},
	} {
		got, err := buildV1PassthroughURL(tc.base, tc.reqPath, "")
		if err != nil {
			t.Fatalf("buildV1PassthroughURL(%q, %q) unexpected error: %v", tc.base, tc.reqPath, err)
		}
		if got != tc.want {
			t.Errorf("buildV1PassthroughURL(%q, %q) = %q; want %q", tc.base, tc.reqPath, got, tc.want)
		}
	}
}

func TestProxyCompileBlocksDeduplicatesIdenticalBlocksInProductionMode(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	// 1. With validation cache enabled, 4 identical tool descriptions must generate once and audit once.
	proxyValidationCacheTTL = 10 * time.Minute
	var genCalls, auditCalls atomic.Int32
	const desc = "Inspect the supplied tenant identifier and enforce strict permission verification across all workspace resources."
	const descShort = "Inspect tenant ID; enforce strict workspace permission checks."
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			auditCalls.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		genCalls.Add(1)
		writeModelContent(w, descShort)
	}))
	defer compiler.Close()
	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}
	profile := getActiveProfile(loadConfig(), "fixture")
	dialect := getDialect(defaultDialectName)

	outputs := make([]string, 4)
	var blocks []proxyTextBlock
	var estOrig, estComp int
	for i := 0; i < 4; i++ {
		idx := i
		appendProxyTextBlock(&blocks, "tool:t"+string(rune('0'+i)), desc, nil, func(v string) { outputs[idx] = v }, &estOrig, &estComp)
	}
	var logIDs []string
	mutated := compileProxyBlocks(context.Background(), "openai", blocks, profile, dialect, nil, &logIDs, &estOrig, &estComp)
	if !mutated || genCalls.Load() != 1 || auditCalls.Load() != 1 || len(logIDs) != 1 {
		t.Fatalf("in-request dedup failed: mutated=%t gen=%d audit=%d logIDs=%d", mutated, genCalls.Load(), auditCalls.Load(), len(logIDs))
	}
	for i, out := range outputs {
		if out != descShort {
			t.Fatalf("slot %d was not populated with deduped result: %q", i, out)
		}
	}

	// 2. With async warmup and user min hits = 2, 2 identical user blocks in ONE request must count as 1 hit.
	resetProxySpeedVars(t)
	isolateProxyState(t)
	proxyAsyncWarmup = true
	proxyWarmupUserMinHits = 2
	t.Cleanup(func() { drainShadowWarmup(2 * time.Second) })
	genCalls.Store(0)

	const userText = "Please summarize all operational requirements, boundary conditions and approval rules in this document."
	var userBlocks []proxyTextBlock
	userOuts := make([]string, 2)
	estOrig, estComp = 0, 0
	for i := 0; i < 2; i++ {
		idx := i
		appendProxyTextBlock(&userBlocks, "user", userText, nil, func(v string) { userOuts[idx] = v }, &estOrig, &estComp)
	}
	logIDs = nil
	compileProxyBlocks(context.Background(), "openai", userBlocks, profile, dialect, nil, &logIDs, &estOrig, &estComp)
	drainShadowWarmup(2 * time.Second)
	if genCalls.Load() != 0 {
		t.Fatalf("2 identical user blocks in 1 request bypassed warmup admission threshold: genCalls=%d", genCalls.Load())
	}
}

func TestShadowWarmupAuditErrorAndTimeoutRecordFailure(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	// Auditor returns 502 (ValidationError with Status=="error").
	// Circuit breaker must record failure, NOT reset to CLOSED.
	compilerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			http.Error(w, "audit unavailable", http.StatusBadGateway)
			return
		}
		writeModelContent(w, "短")
	}))
	defer compilerSrv.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compilerSrv.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}
	profile := getActiveProfile(loadConfig(), "fixture")
	dialect := getDialect(defaultDialectName)

	// Start with 1 existing failure (threshold=3). After audit 502 error in warmup,
	// failures must increase to 2 (NOT reset to 0).
	compilerCB = newCircuitBreaker(3, time.Minute)
	compilerCB.RecordFailure()
	shadowWarmupPending.Add(1)
	runShadowWarmup(shadowWarmupJob{
		protocol: "openai",
		role:     "system",
		text:     "请严格执行所有的审批流程，确保在满足条件时留下完整的审计追踪信息。",
		profile:  profile,
		dialect:  dialect,
	})
	status := compilerCB.Status()
	if failures, _ := status["failures"].(int); failures != 2 {
		t.Fatalf("shadow warmup audit 502 did not increment circuit breaker failures: status=%#v", status)
	}
}

func TestOpenAIResponsesSkipCacheControlProtectsInputPrefix(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)

	const cachedInput = "Cached prefix input item marked with cache_control that must remain completely untouched for prompt caching."
	const uncachedInput = "Uncached trailing user input asking to summarize every operational requirement, condition and exception."
	const uncachedShort = "Summarize all operational requirements, conditions, exceptions."

	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req compilerRequestBody
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		orig := decodeCompilerInput(t, req.Messages[1].Content).Original
		if orig == uncachedInput {
			writeModelContent(w, uncachedShort)
			return
		}
		t.Errorf("compiler received cache_control-protected Responses input: %q", orig)
		writeModelContent(w, orig)
	}))
	defer compiler.Close()

	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded, _ = decodeProxyPayload(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":42}}`))
	}))
	defer upstream.Close()

	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {Name: "fixture", APIKey: "k", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "m"},
	}}); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": cachedInput, "cache_control": map[string]any{"type": "ephemeral"}},
				},
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": uncachedInput},
				},
			},
		},
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer consumer-key")
	req.Header.Set("X-Babel-Skip-Cache-Control", "true")
	rec := httptest.NewRecorder()
	newServerMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/responses failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	inputs := forwarded["input"].([]any)
	gotCached := inputs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	gotUncached := inputs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if gotCached != cachedInput {
		t.Fatalf("cached input prefix was mutated: got %q", gotCached)
	}
	if gotUncached != uncachedShort {
		t.Fatalf("uncached trailing input was not compressed: got %q", gotUncached)
	}
}

func TestProxyCachedAdoptionRefreshesDiskCacheLRU(t *testing.T) {
	isolateDomainIntegration(t)
	isolateCompilerState(t)
	isolateProxyState(t)
	resetProxySpeedVars(t)
	proxyValidationCacheTTL = 10 * time.Minute

	const source = "Please review the supplied document and summarize every operational requirement, condition and exception clearly."
	const shorter = "Review doc; summarize requirements, conditions, exceptions."
	p := Profile{Name: "fixture", APIKey: "k", BaseURL: "https://compiler.example/v1/chat/completions", Model: "m"}
	dialect, err := composeDialect(defaultDialectName, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	ck := makeCacheKey(dialect, p, source)
	oldAccess := time.Now().Unix() - 120
	cacheMutex.Lock()
	diskCache[ck] = CacheEntry{Compiled: shorter, Model: p.Model, Mode: defaultDialectName, LastAccess: oldAccess}
	cacheMutex.Unlock()

	vk := makeValidationCacheKey(CompileReq{Text: source}, p.ValidationMode, p, p, shorter)
	validationCache.Put(vk, newValidation("passed", "llm_compare"), time.Minute)

	out, logID, _, hit := proxyCachedAdoption("openai", "user", source, p, dialect, nil, nil, ck)
	if !hit || out != shorter || logID == "" {
		t.Fatalf("expected fast-path cached adoption hit; got hit=%t out=%q logID=%q", hit, out, logID)
	}

	cacheMutex.RLock()
	updatedAccess := diskCache[ck].LastAccess
	cacheMutex.RUnlock()
	if updatedAccess <= oldAccess {
		t.Fatalf("proxyCachedAdoption did not refresh diskCache LastAccess: old=%d updated=%d", oldAccess, updatedAccess)
	}
}

func TestClearProxyLogsOrderedAfterAsyncAppend(t *testing.T) {
	isolateProxyState(t)
	oldAsync := logRedactAsync.Load()
	startAsyncLogRedaction()
	t.Cleanup(func() {
		drainAsyncLogRedaction(2 * time.Second)
		logRedactAsync.Store(oldAsync)
	})

	for i := 0; i < 20; i++ {
		appendProxyLog(ProxyLog{ID: "pre-clear-" + string(rune('a'+i)), Original: "sk-abcdefghijklmnopqrstuv", Compiled: "ok"})
	}
	clearProxyLogs()
	drainAsyncLogRedaction(2 * time.Second)

	logsMutex.RLock()
	count := len(proxyLogs)
	logsMutex.RUnlock()
	if count != 0 {
		t.Fatalf("stale async log entries survived clearProxyLogs: count=%d", count)
	}
}

func TestProbeProfileContextRespectsCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	begin := time.Now()
	res := probeProfileContext(ctx, Profile{Name: "slow", APIKey: "k", BaseURL: srv.URL, Model: "m"})
	elapsed := time.Since(begin)
	if res.OK {
		t.Fatal("expected canceled probeProfileContext to fail")
	}
	if elapsed > time.Second {
		t.Fatalf("probeProfileContext ignored context deadline: elapsed=%v", elapsed)
	}
}

func TestSelectDomainGuidanceEmptyTextAndPrecomputedSnippet(t *testing.T) {
	packs, err := resolveDomainPacks(defaultDialectName, []string{"api-contract"})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectDomainGuidance(packs, "   "); got != "" {
		t.Fatalf("expected empty guidance for blank text, got %q", got)
	}
	got := selectDomainGuidance(packs, "POST /orders 必须携带 Idempotency-Key")
	if got == "" {
		t.Fatal("expected non-empty guidance for matching api-contract text")
	}
	for _, p := range packs {
		for _, e := range p.Entries {
			if e.guidanceSnippet == "" || e.guidanceRunes <= 0 {
				t.Fatalf("domain entry %s missing precomputed guidance snippet", e.ID)
			}
		}
	}
}
