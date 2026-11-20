package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCachedCandidateUsesCurrentBlindReaderProfile(t *testing.T) {
	isolateDomainIntegration(t)
	const candidate = "系统允许删除日志。"
	reading := blindReadingJSON(t, blindFixtureReading())
	var generations, comparisons, blindComparisons, externalReads, sameModelReads atomic.Int32
	compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
			compilerRequestBody
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "compiler-model" || r.Header.Get("Authorization") != "Bearer compiler-key" {
			t.Error("compiler profile identity changed")
		}
		switch body.Messages[0].Content {
		case verifyGatePrompt:
			comparisons.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		case blindComparatorPrompt:
			blindComparisons.Add(1)
			writeModelContent(w, `{"pass":true,"missing":[]}`)
		case blindReaderPrompt:
			sameModelReads.Add(1)
			writeModelContent(w, reading)
		default:
			generations.Add(1)
			writeModelContent(w, candidate)
		}
	}))
	defer compiler.Close()
	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
			compilerRequestBody
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "reader-model" || r.Header.Get("Authorization") != "Bearer reader-key" || body.Messages[0].Content != blindReaderPrompt {
			t.Error("named reader did not receive its own isolated request")
		}
		externalReads.Add(1)
		writeModelContent(w, reading)
	}))
	defer reader.Close()
	if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
		"test":   {APIKey: "compiler-key", BaseURL: compiler.URL, Model: "compiler-model", ReaderProfile: "reader"},
		"reader": {APIKey: "reader-key", BaseURL: reader.URL, Model: "reader-model"},
	}}); err != nil {
		t.Fatal(err)
	}
	req := CompileReq{Text: "这条规则明确说明：系统允许删除日志。", StrictGate: true, ValidationMode: "compare"}
	out, err := doCompile(req)
	if err != nil || out.Validation.Method != "llm_compare" || out.CacheHit {
		t.Fatalf("initial comparison: %+v %v", out, err)
	}
	req.ValidationMode = "blind"
	for i := 0; i < 2; i++ {
		out, err = doCompile(req)
		if err != nil || !out.CacheHit || out.Validation.Method != "blind_read" || out.Validation.Status != "passed" || out.Validation.Stages[1].Model != "reader-model" {
			t.Fatalf("cache hit did not run selected reader: %+v %v", out, err)
		}
	}
	sameCompiler := ""
	req.ReaderProfile = &sameCompiler
	out, err = doCompile(req)
	if err != nil || out.Validation.Stages[1].Model != "compiler-model" {
		t.Fatalf("explicit reader clear was ignored: %+v %v", out, err)
	}
	if generations.Load() != 1 || comparisons.Load() != 1 || blindComparisons.Load() != 3 || externalReads.Load() != 2 || sameModelReads.Load() != 1 {
		t.Fatalf("validation shared or routed incorrectly: generation=%d compare=%d blind_compare=%d external=%d same=%d", generations.Load(), comparisons.Load(), blindComparisons.Load(), externalReads.Load(), sameModelReads.Load())
	}
}

func TestCandidateValidationAPISeparatesRejectionAndServiceFailure(t *testing.T) {
	for _, tc := range []struct {
		name, mode, response, want        string
		upstreamStatus, httpStatus, calls int
	}{
		{"compare pass", "compare", `{"pass":true,"missing":[]}`, "passed", 200, 200, 1},
		{"compare reject", "compare", `{"pass":false,"missing":["约束缺失"]}`, "failed", 200, 200, 1},
		{"compare invalid", "compare", "NOT PASS", "error", 200, 502, 1},
		{"blind pass", "blind", "reading", "passed", 200, 200, 2},
		{"blind ambiguous", "blind", "ambiguous", "failed", 200, 200, 1},
		{"blind service failure", "blind", "unavailable", "error", 503, 502, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			reading := blindFixtureReading()
			if tc.response == "ambiguous" {
				reading.Ambiguities = []string{"无法确定许可范围"}
			}
			readerJSON := blindReadingJSON(t, reading)
			var calls atomic.Int32
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				system := body.Messages[0].Content
				if system != verifyGatePrompt && system != blindReaderPrompt && system != blindComparatorPrompt {
					t.Error("validation-only API attempted candidate generation")
				}
				if tc.upstreamStatus != http.StatusOK {
					w.WriteHeader(tc.upstreamStatus)
					return
				}
				if system == blindReaderPrompt {
					writeModelContent(w, readerJSON)
				} else if system == blindComparatorPrompt {
					writeModelContent(w, `{"pass":true,"missing":[]}`)
				} else {
					writeModelContent(w, tc.response)
				}
			}))
			defer service.Close()
			body := sendDomainAPI(t, http.MethodPost, "/api/validate", map[string]any{
				"text": "系统允许删除日志。", "candidate": "系统允许删除日志。",
				"api_key": "test-key", "base_url": service.URL, "model": "test", "validation_mode": tc.mode,
			}, tc.httpStatus)
			var result ValidationResult
			if err := json.Unmarshal(body["validation"], &result); err != nil || result.Status != tc.want || int(calls.Load()) != tc.calls {
				t.Fatalf("validation outcome: %+v calls=%d error=%v", result, calls.Load(), err)
			}
		})
	}
}

func TestProxyExecutionHeadersAreValidatedBeforeForwarding(t *testing.T) {
	isolateDomainIntegration(t)
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, headers := range []map[string]string{
			{"X-Babel-Profile": "missing"},
			{"X-Babel-Token-Encoding": "guess-from-model"},
			{"X-Babel-Validation-Mode": "trust-cache"},
			{"X-Babel-Validation-Mode": "blind", "X-Babel-Reader-Profile": "missing"},
		} {
			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"consumer","messages":[{"role":"user","content":"short"}]}`))
			for name, value := range headers {
				r.Header.Set(name, value)
			}
			r.Header.Set("Authorization", "Bearer consumer-fixture")
			w := httptest.NewRecorder()
			newServerMux().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s invalid options returned %d: %s", route, w.Code, w.Body.String())
			}
		}
	}
}

func TestProxyHeadersSelectBlindReaderAndTokenEncoding(t *testing.T) {
	const source = "这里重复阐述同一个许可，没有附加条件：系统允许删除日志。这里重复说明该许可，系统允许删除日志。"
	const candidate = "系统允许删除日志。"
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			reading := blindReadingJSON(t, blindFixtureReading())
			var generations, reads, comparisons atomic.Int32
			compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
					compilerRequestBody
				}
				json.NewDecoder(r.Body).Decode(&body)
				switch body.Messages[0].Content {
				case blindReaderPrompt:
					if body.Model != "reader-model" || r.Header.Get("Authorization") != "Bearer reader-key" {
						t.Error("proxy ignored reader profile header")
					}
					reads.Add(1)
					writeModelContent(w, reading)
				case blindComparatorPrompt:
					comparisons.Add(1)
					writeModelContent(w, `{"pass":true,"missing":[]}`)
				case verifyGatePrompt:
					t.Error("proxy ran compare despite blind header")
					writeModelContent(w, `{"pass":false,"missing":[]}`)
				default:
					generations.Add(1)
					writeModelContent(w, candidate)
				}
			}))
			defer compiler.Close()
			forwarded := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body compilerRequestBody
				json.NewDecoder(r.Body).Decode(&body)
				forwarded <- body.Messages[0].Content
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "test", Profiles: map[string]Profile{
				"test":   {APIKey: "compiler-key", BaseURL: compiler.URL, Model: "compiler-model", UpstreamURL: upstream.URL},
				"reader": {APIKey: "reader-key", BaseURL: compiler.URL, Model: "reader-model"},
			}}); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(map[string]any{"model": "consumer", "max_tokens": 64, "messages": []map[string]string{{"role": "user", "content": source}}})
			r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(encoded)))
			r.Header.Set("X-Babel-Validation-Mode", "blind")
			r.Header.Set("Authorization", "Bearer consumer-fixture")
			r.Header.Set("X-Babel-Reader-Profile", "reader")
			r.Header.Set("X-Babel-Token-Encoding", "o200k_base")
			w := httptest.NewRecorder()
			newServerMux().ServeHTTP(w, r)
			if w.Code != http.StatusOK || generations.Load() != 1 || reads.Load() != 1 || comparisons.Load() != 1 {
				t.Fatalf("wrong proxy route/calls: status=%d generation=%d reader=%d comparator=%d body=%s", w.Code, generations.Load(), reads.Load(), comparisons.Load(), w.Body.String())
			}
			if got := <-forwarded; got != candidate {
				t.Fatalf("verified smaller candidate not forwarded: %q", got)
			}
			logsMutex.RLock()
			entry := proxyLogs[0]
			logsMutex.RUnlock()
			if entry.Decision != "adopted" || entry.MeasurementKind != "exact" || entry.MeasurementEncoding != "o200k_base" || entry.ValidationMethod != "blind_read" {
				t.Fatalf("missing proxy decision evidence: %+v", entry)
			}
		})
	}
}

func TestProxySelectedProfileNeverBorrowsActiveAPIKey(t *testing.T) {
	isolateDomainIntegration(t)
	isolateProxyState(t)
	var calls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeModelContent(w, `{"pass":true,"missing":[]}`)
	}))
	defer service.Close()
	// Legacy saved profiles may omit name. The map key must remain the identity.
	if err := writeConfigFile(Config{ActiveProfile: "active", Profiles: map[string]Profile{
		"active":   {APIKey: "active-key", BaseURL: service.URL, Model: "active-model"},
		"selected": {BaseURL: service.URL, Model: "selected-model"},
	}}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Babel-Profile", "selected")
	p, err := resolveProxyProfile(r, loadConfig())
	if err != nil || p.Name != "selected" || p.APIKey != "" {
		t.Fatalf("profile identity: %+v %v", p, err)
	}
	const source = "Please review the supplied specification and summarize its relevant operational requirements for the engineering team."
	got, _, _, _ := translateMessageBlock("openai", "user", source, p, getDialect("token"), nil, nil)
	if got != source || calls.Load() != 0 {
		t.Fatalf("empty selected key fell back to active credentials: calls=%d output=%q", calls.Load(), got)
	}
}
