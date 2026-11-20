package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyStreamingFallbackPreservesClientOptionsAndMessageBlocks(t *testing.T) {
	const original = "Please review the supplied specification and summarize its relevant operational requirements clearly for the engineering team."
	const candidate = "Review specification; summarize operational requirements for engineers."
	for _, test := range []struct {
		name, protocol, failure string
		options                 map[string]any
		requests                int
	}{
		{"OpenAI generation error", "openai", "generation", nil, 1},
		{"OpenAI partial generation error", "openai", "partial", nil, 1},
		{"OpenAI cached rejection", "openai", "rejection", nil, 2},
		{"OpenAI explicit usage enabled", "openai", "audit", map[string]any{"include_usage": true, "include_obfuscation": false}, 1},
		{"OpenAI explicit usage disabled", "openai", "measurement", map[string]any{"include_usage": false}, 1},
		{"Anthropic generation error", "anthropic", "generation", nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			var generations, audits, forwards atomic.Int32
			compiler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request compilerRequestBody
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
					t.Error("invalid compiler request")
					http.Error(w, "invalid compiler request", http.StatusBadRequest)
					return
				}
				if request.Messages[0].Content == verifyGatePrompt {
					audits.Add(1)
					if test.failure == "audit" {
						http.Error(w, "fixture audit unavailable", http.StatusBadGateway)
						return
					}
					writeModelContent(w, `{"pass":false,"missing":["fixture semantic rejection"]}`)
					return
				}
				generations.Add(1)
				if decodeCompilerInput(t, request.Messages[1].Content).Original != original {
					t.Error("proxy compiled a nontext block or changed the source")
				}
				if test.failure == "generation" {
					http.Error(w, "fixture generator unavailable", http.StatusBadGateway)
					return
				}
				if test.failure == "partial" {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
						"finish_reason": "length", "message": map[string]string{"content": candidate},
					}}})
					return
				}
				writeModelContent(w, candidate)
			}))
			defer compiler.Close()

			payload := map[string]any{
				"model": "consumer-model", "stream": true,
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": original},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA==", "detail": "low"}},
				}}},
			}
			route := "/v1/chat/completions"
			first := "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"
			last := "data: {\"choices\":[{\"delta\":{\"content\":\" second\"}}]}\n\ndata: [DONE]\n\n"
			if test.options != nil {
				payload["stream_options"] = test.options
			}
			if test.protocol == "anthropic" {
				route = "/v1/messages"
				payload["max_tokens"] = 64
				payload["system"] = []any{map[string]any{"type": "text", "text": "Follow the request.", "cache_control": map[string]any{"type": "ephemeral"}}}
				payload["messages"] = []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": original, "cache_control": map[string]any{"type": "ephemeral"}},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}},
				}}}
				first = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n"
				last = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			want, err := decodeProxyPayload(raw)
			if err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwards.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "unreadable request", http.StatusBadRequest)
					return
				}
				got, err := decodeProxyPayload(body)
				if err != nil || !reflect.DeepEqual(got, want) {
					// A compatible gateway need not accept options the client never
					// supplied. Reject mutations rather than silently tolerating them.
					http.Error(w, "gateway rejects changed request or unrequested stream options", http.StatusUnprocessableEntity)
					return
				}
				if test.protocol == "openai" && r.Header.Get("Authorization") != "Bearer consumer-key" {
					t.Error("OpenAI fallback lost consumer authorization")
				}
				if test.protocol == "anthropic" && (r.Header.Get("x-api-key") != "consumer-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "fixture-beta") {
					t.Error("Anthropic fallback lost protocol headers")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Fixture-Stream", "preserved")
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, last)
			}))
			defer upstream.Close()
			profile := Profile{APIKey: "compiler-key", BaseURL: compiler.URL, Model: "compiler-fixture", UpstreamURL: upstream.URL}
			if test.failure == "measurement" {
				profile.TokenEncoding = "o200k_base"
				previous := tokenCodecs["o200k_base"]
				unavailable := &tokenCodecState{err: errors.New("fixture tokenizer unavailable")}
				unavailable.once.Do(func() {})
				tokenCodecs["o200k_base"] = unavailable
				t.Cleanup(func() { tokenCodecs["o200k_base"] = previous })
			}
			if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{"fixture": profile}}); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(newServerMux())
			defer proxy.Close()
			defer unblock()
			for attempt := 1; attempt <= test.requests; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+route, bytes.NewReader(raw))
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer consumer-key")
				request.Header.Set("x-api-key", "consumer-key")
				request.Header.Set("anthropic-version", "2023-06-01")
				request.Header.Set("anthropic-beta", "fixture-beta")
				response, err := proxy.Client().Do(request)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK || response.Header.Get("X-Fixture-Stream") != "preserved" {
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					cancel()
					t.Fatalf("internal failure broke compatible gateway passthrough: status=%d body=%s", response.StatusCode, body)
				}
				// The upstream waits for this read before sending the rest. A proxy
				// that buffers the whole response will hit the request deadline.
				prefix := make([]byte, len(first))
				_, err = io.ReadFull(response.Body, prefix)
				if err != nil {
					response.Body.Close()
					cancel()
					t.Fatalf("first SSE event was buffered or truncated: %v", err)
				}
				unblock()
				tail, err := io.ReadAll(response.Body)
				response.Body.Close()
				cancel()
				if err != nil || string(prefix) != first || string(tail) != last {
					t.Fatalf("SSE bytes changed or response failed: first=%q tail=%q err=%v", prefix, tail, err)
				}
				if forwards.Load() != int32(attempt) {
					t.Fatalf("final request repeated: forwards=%d attempts=%d", forwards.Load(), attempt)
				}
			}
			wantAudits := int32(0)
			if test.failure == "rejection" || test.failure == "audit" {
				wantAudits = int32(test.requests)
			}
			if generations.Load() != 1 || audits.Load() != wantAudits {
				t.Fatalf("failure path or cache revalidation not exercised: generations=%d audits=%d", generations.Load(), audits.Load())
			}
			logsMutex.RLock()
			entries := append([]ProxyLog(nil), proxyLogs...)
			logsMutex.RUnlock()
			if len(entries) != test.requests {
				t.Fatalf("unexpected fallback log count: %+v", entries)
			}
			for _, entry := range entries {
				if entry.Decision != "kept_original" || entry.Original != original || entry.Compiled != original || entry.RealPromptTokens != 0 {
					t.Fatalf("failed candidate adopted or upstream usage invented: %+v", entry)
				}
			}
			if test.requests > 1 && !entries[0].CacheHit {
				t.Fatal("repeated rejection did not exercise the cached candidate")
			}
		})
	}
}
