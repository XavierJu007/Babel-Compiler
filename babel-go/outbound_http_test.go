package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSecurityModelRequestsNeverFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, operation := range []string{"compile", "probe"} {
			t.Run(http.StatusText(status)+"/"+operation, func(t *testing.T) {
				var redirected atomic.Int32
				sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redirected.Add(1)
					writeModelContent(w, "unexpected redirected reply")
				}))
				defer sink.Close()
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, sink.URL+"/capture", status)
				}))
				defer provider.Close()
				profile := Profile{APIKey: "stored-fixture-key", BaseURL: provider.URL, Model: "fixture"}
				if operation == "compile" {
					if _, err := callLLM(profile, "fixture policy", "private fixture input", 0); err == nil || !strings.Contains(err.Error(), "上游 API 错误") {
						t.Fatalf("redirect was not treated as a model error: %v", err)
					}
				} else {
					result := probeProfile(profile)
					if result.OK || result.StatusCode != status {
						t.Fatalf("redirect was not treated as a probe failure: %+v", result)
					}
				}
				if redirected.Load() != 0 {
					t.Fatal("configured endpoint replayed a model request at another address")
				}
			})
		}
	}
}
