package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyHTTPKeepsOriginalAfterValidationFailure(t *testing.T) {
	for _, serviceError := range []bool{false, true} {
		t.Run(map[bool]string{false: "semantic rejection", true: "audit service error"}[serviceError], func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			var generations, audits atomic.Int32
			compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
					t.Error("invalid compiler fixture request")
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if request.Messages[0].Content == verifyGatePrompt {
					audits.Add(1)
					if serviceError {
						http.Error(w, "fixture audit unavailable", http.StatusBadGateway)
						return
					}
					writeModelContent(w, `{"pass":false,"missing":["fixture: condition omitted"]}`)
					return
				}
				generations.Add(1)
				if decodeCompilerInput(t, request.Messages[1].Content).Original != failedCandidateSource {
					t.Error("compiler did not receive the original prompt")
				}
				writeModelContent(w, failedCandidateText)
			}))
			defer compiler.Close()

			forwarded := make(chan map[string]any, 1)
			const upstreamResponse = `{"choices":[{"message":{"role":"assistant","content":"原文处理完成"}}]}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "unreadable request", http.StatusBadRequest)
					return
				}
				body, err := decodeProxyPayload(raw)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if r.Header.Get("Authorization") != "Bearer consumer-key" {
					t.Error("fallback lost the consumer authorization")
				}
				select {
				case forwarded <- body:
				default:
					t.Error("fallback sent the final request more than once")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Fixture-Provider", "original-prompt")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(upstreamResponse))
			}))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
				"fixture": {APIKey: "compiler-key", BaseURL: compiler.URL, Model: "compiler-fixture", UpstreamURL: upstream.URL},
			}}); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{
				"model": "consumer-model", "messages": []any{map[string]any{"role": "user", "content": failedCandidateSource}},
				"response_format": map[string]any{"type": "json_object"},
				"metadata":        map[string]any{"request_id": json.Number("9007199254740993")},
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(newServerMux())
			defer proxy.Close()
			request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer consumer-key")
			client := proxy.Client()
			client.Timeout = 10 * time.Second
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusAccepted || string(responseBody) != upstreamResponse || response.Header.Get("X-Fixture-Provider") != "original-prompt" {
				t.Fatalf("validation failure interrupted provider passthrough: %d %s", response.StatusCode, responseBody)
			}
			select {
			case got := <-forwarded:
				if !reflect.DeepEqual(got, payload) {
					t.Fatalf("failed candidate changed the original provider request: %#v", got)
				}
			default:
				t.Fatal("final provider never received the original request")
			}
			if generations.Load() != 1 || audits.Load() != 1 {
				t.Fatalf("validation fallback was not exercised: generations=%d audits=%d", generations.Load(), audits.Load())
			}
			logsMutex.RLock()
			entries := append([]ProxyLog(nil), proxyLogs...)
			logsMutex.RUnlock()
			if len(entries) != 1 || entries[0].Decision != "kept_original" || entries[0].Original != failedCandidateSource || entries[0].Compiled != failedCandidateSource || entries[0].ValidationMethod != "llm_compare" {
				t.Fatalf("fallback log does not describe the forwarded prompt: %+v", entries)
			}
			wantFailures := 0
			if serviceError {
				wantFailures = 1
			}
			if status := compilerCB.Status(); status["state"] != "CLOSED" || status["failures"] != wantFailures {
				t.Fatalf("semantic rejection and service failure were conflated: %+v", status)
			}
		})
	}
}
