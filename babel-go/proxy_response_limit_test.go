package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyUsageCaptureHasFixedMemoryBound(t *testing.T) {
	capture := &proxyUsageCapture{}
	chunk := bytes.Repeat([]byte("x"), 8191)
	for written := 0; written <= 3*maxProxyUsageCaptureBytes; written += len(chunk) {
		if n, err := capture.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("usage capture interrupted response forwarding: n=%d err=%v", n, err)
		}
		if len(capture.data) > maxProxyUsageCaptureBytes || cap(capture.data) > maxProxyUsageCaptureBytes {
			t.Fatalf("capture exceeded its bound: len=%d cap=%d", len(capture.data), cap(capture.data))
		}
	}
	if !capture.exceeded || capture.data != nil {
		t.Fatal("oversized response retained a partial usage document")
	}
	var forwarded bytes.Buffer
	small := []byte(`{"usage":{"prompt_tokens":39}}`)
	got, err := forwardProxyResponseBody(&forwarded, bytes.NewReader(small))
	if err != nil || !bytes.Equal(got, small) || !bytes.Equal(forwarded.Bytes(), small) {
		t.Fatalf("small response/usage capture changed: capture=%q forwarded=%q err=%v", got, forwarded.Bytes(), err)
	}
	large := bytes.Repeat([]byte("x"), maxProxyUsageCaptureBytes+1)
	if got, err := forwardProxyResponseBody(io.Discard, bytes.NewReader(large)); err != nil || got != nil {
		t.Fatalf("oversized forwarding retained a document for usage parsing: bytes=%d err=%v", len(got), err)
	}
}

func TestProxyForwardsLargeResponsesAndOnlyParsesCompleteUsage(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, large := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/small usage", true: "/large body"}[large], func(t *testing.T) {
				isolateDomainIntegration(t)
				isolateProxyState(t)
				compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "fixture compiler unavailable", http.StatusServiceUnavailable)
				}))
				defer compiler.Close()
				usage := `"prompt_tokens":39`
				route := "/v1/chat/completions"
				if protocol == "anthropic" {
					usage = `"input_tokens":31,"cache_creation_input_tokens":3,"cache_read_input_tokens":5`
					route = "/v1/messages"
				}
				padding := "small"
				if large {
					padding = strings.Repeat("x", maxProxyUsageCaptureBytes+32768)
				}
				body := []byte(`{"padding":"` + padding + `","usage":{` + usage + `}}`)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write(body)
				}))
				defer upstream.Close()
				if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{"fixture": {APIKey: "compiler-key", BaseURL: compiler.URL, UpstreamURL: upstream.URL, Model: "fixture"}}}); err != nil {
					t.Fatal(err)
				}
				proxy := httptest.NewServer(newServerMux())
				defer proxy.Close()
				raw, _ := json.Marshal(map[string]any{"model": "consumer", "messages": []any{map[string]any{"role": "user", "content": "Please summarize the supplied document while retaining all operational requirements, conditions and exceptions."}}})
				request, err := http.NewRequest(http.MethodPost, proxy.URL+route, bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer consumer-key")
				client := proxy.Client()
				client.Timeout = 5 * time.Second
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				got, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != http.StatusAccepted || !bytes.Equal(got, body) {
					t.Fatalf("provider response was truncated or changed: status=%d bytes=%d/%d err=%v", response.StatusCode, len(got), len(body), err)
				}
				wantTokens := 39
				if large {
					wantTokens = 0
				}
				deadline := time.Now().Add(time.Second)
				for {
					logsMutex.RLock()
					count, tokens := len(proxyLogs), 0
					if count == 1 {
						tokens = proxyLogs[0].RealPromptTokens
					}
					logsMutex.RUnlock()
					if count == 1 && tokens == wantTokens {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("usage did not match capture completeness: logs=%d tokens=%d want=%d", count, tokens, wantTokens)
					}
					time.Sleep(time.Millisecond)
				}
			})
		}
	}
}
