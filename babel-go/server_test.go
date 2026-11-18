package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompileAPIReportsValidationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, audit, status        string
		upstreamStatus, wantStatus int
		strict                     bool
	}{
		{"rejected", `{"pass":false,"missing":["lost condition"]}`, "failed", 200, 422, true},
		{"unavailable", "temporarily unavailable", "error", 503, 502, true},
		{"malformed", "NOT PASS", "error", 200, 502, true},
		{"not audited", "", "not_run", 200, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateProxyState(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				content := "Summarize the specification for the engineering team."
				if len(req.Messages) > 0 && req.Messages[0].Content == verifyGatePrompt {
					content = tc.audit
					if tc.upstreamStatus != 200 {
						w.WriteHeader(tc.upstreamStatus)
						w.Write([]byte(content))
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer upstream.Close()
			body, _ := json.Marshal(CompileReq{
				Text:   "Please summarize the supplied specification clearly for the engineering team.",
				APIKey: "test", BaseURL: upstream.URL, Model: "test", NoCache: true, StrictGate: tc.strict,
			})
			w := httptest.NewRecorder()
			newServerMux().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/compile", strings.NewReader(string(body))))
			if w.Code != tc.wantStatus {
				t.Fatalf("status %d; want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			var response struct {
				Validation ValidationResult `json:"validation"`
				Compiled   string           `json:"compiled"`
			}
			if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Validation.Status != tc.status {
				t.Fatalf("validation %q; want %q", response.Validation.Status, tc.status)
			}
			if tc.wantStatus != http.StatusOK && response.Compiled != "" {
				t.Fatal("strict failure exposed a usable candidate")
			}
		})
	}
}

func TestCompileAPIRequiresPOST(t *testing.T) {
	w := httptest.NewRecorder()
	newServerMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/compile", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
}
