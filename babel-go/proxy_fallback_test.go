package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyKeepsOriginalWhenTokenCountingFails(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	const original = "请审阅提供的完整规范，汇总其中的运行要求，并用中文 JSON 回答，保留所有适用条件与例外。"
	const candidate = "Review spec; summarize operational requirements; 中文 JSON."
	var generations, audits atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body compilerRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
			t.Error("invalid compiler fixture request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.Messages[0].Content == verifyGatePrompt {
			audits.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
			return
		}
		generations.Add(1)
		writeModelContent(w, candidate)
	}))
	defer compiler.Close()

	forwarded := make(chan map[string]any, 1)
	const upstreamResponse = `{"answer":"原文已收到"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		forwarded <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamResponse))
	}))
	defer upstream.Close()
	if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
		"fixture": {APIKey: "fixture-key", BaseURL: compiler.URL, Model: "compiler-fixture", UpstreamURL: upstream.URL, TokenEncoding: "o200k_base"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Exercise the actual measurement error path after generation. No unknown
	// encoding/header shortcut, estimated fallback or real tokenizer failure is
	// needed; restore the shared codec before any later test runs.
	previous := tokenCodecs["o200k_base"]
	unavailable := &tokenCodecState{err: errors.New("fixture tokenizer unavailable")}
	unavailable.once.Do(func() {})
	tokenCodecs["o200k_base"] = unavailable
	t.Cleanup(func() { tokenCodecs["o200k_base"] = previous })

	payload := map[string]any{
		"model": "consumer-model", "messages": []any{map[string]any{"role": "user", "content": original}},
		"response_format": map[string]any{"type": "json_object"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer consumer-fixture")
	newServerMux().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != upstreamResponse {
		t.Fatalf("counting failure interrupted upstream passthrough: %d %s", response.Code, response.Body.String())
	}
	if got := <-forwarded; !reflect.DeepEqual(got, payload) {
		t.Fatalf("counting failure changed the original request: %#v", got)
	}
	if generations.Load() != 1 || audits.Load() != 0 {
		t.Fatalf("unexpected work after unavailable counting: generation=%d audit=%d", generations.Load(), audits.Load())
	}
	logsMutex.RLock()
	entries := append([]ProxyLog(nil), proxyLogs...)
	logsMutex.RUnlock()
	if len(entries) != 1 || entries[0].Decision != "kept_original" || entries[0].Compiled != original || !strings.Contains(entries[0].Reason, "计数不可用") {
		t.Fatalf("missing accurate fallback log: %+v", entries)
	}
	if status := compilerCB.Status(); status["state"] != "CLOSED" || status["failures"] != 0 {
		t.Fatalf("counting failure was classified as a model service outage: %+v", status)
	}
}
