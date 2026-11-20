package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestProxyPreservesJSONNumberLiterals(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			const schema = "{\"type\":\"object\",\"properties\":{\"id\":{\"type\":\"integer\",\"enum\":[9007199254740993,9223372036854775807]},\"ratio\":{\"type\":\"number\",\"default\":0.1234567890123456789}}}"
			tool := "{\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"parameters\":" + schema + "}}"
			if route == "/v1/messages" {
				tool = "{\"name\":\"lookup\",\"input_schema\":" + schema + "}"
			}
			payload := "{\"model\":\"consumer\",\"messages\":[{\"role\":\"user\",\"content\":\"Keep this exact.\"}],\"max_tokens\":256,\"seed\":9007199254740993,\"tools\":[" + tool + "]}"
			decode := func(raw []byte) map[string]any {
				t.Helper()
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.UseNumber()
				var value map[string]any
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			forwarded := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				forwarded <- body
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("{\"choices\":[]}"))
			}))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
				"fixture": {Name: "fixture", BaseURL: upstream.URL, UpstreamURL: upstream.URL, Model: "fixture", APIKey: "fixture"},
			}}); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer consumer-fixture")
			newServerMux().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			got := <-forwarded
			if !reflect.DeepEqual(decode(got), decode([]byte(payload))) {
				t.Fatalf("proxy changed numeric literals:\n%s", got)
			}
		})
	}
}

func TestProxyRejectsNonObjectOrTrailingJSONBeforeForwarding(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, body := range []string{"null", "[]", "{} {}", "{} true", "{\"seed\":01}", ""} {
			response := httptest.NewRecorder()
			newServerMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, route, strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s accepted malformed/objectless request %q: %d", route, body, response.Code)
			}
		}
	}
}
