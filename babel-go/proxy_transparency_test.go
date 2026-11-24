package main

import (
	"bytes"
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
