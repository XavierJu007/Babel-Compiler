package evalrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real executable's save-only path. These commands never request
// compilation or start the service, and use only isolated fixture configuration.
func TestRealCLICredentialOriginIsolation(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("BABEL_TEST_BINARY"))
	if binary == "" {
		t.Skip("set BABEL_TEST_BINARY to a built Babel binary for CLI integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	const oldURL = "https://provider.example.invalid/v1/chat/completions"
	const otherURL = "https://alternate.example.invalid/v1/chat/completions"
	const sameOriginURL = "https://provider.example.invalid/gateway/custom?region=fixture"
	const oldKey, newKey = "stored-compiler-fixture-key", "explicit-new-fixture-key"
	type profile struct {
		Name        string `json:"name"`
		APIKey      string `json:"api_key"`
		BaseURL     string `json:"base_url"`
		Model       string `json:"model"`
		UpstreamURL string `json:"upstream_url"`
	}
	type config struct {
		ActiveProfile string             `json:"active_profile"`
		Profiles      map[string]profile `json:"profiles"`
	}
	initial := config{ActiveProfile: "fixture", Profiles: map[string]profile{
		"fixture": {Name: "fixture", APIKey: oldKey, BaseURL: oldURL, Model: "fixture-model", UpstreamURL: "https://consumer.example.invalid/v1"},
		"other":   {Name: "other", APIKey: "other-profile-fixture-key", BaseURL: "https://other.example.invalid/v1", Model: "other-model"},
	}}
	before, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	before = append(before, '\n')
	for _, test := range []struct {
		name, endpoint, suppliedKey, wantKey string
		reject                               bool
	}{
		{"cross origin without new key", otherURL, "", oldKey, true},
		{"cross origin with explicit new key", otherURL, newKey, newKey, false},
		{"same origin new path", sameOriginURL, "", oldKey, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "babel.config.json")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-profile", "fixture", "-base-url", test.endpoint, "-save-config"}
			if test.suppliedKey != "" {
				args = append(args, "-api-key", test.suppliedKey)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, args...)
			command.Dir = root
			for _, setting := range os.Environ() {
				key, _, _ := strings.Cut(setting, "=")
				if key != "HOME" && key != "USERPROFILE" && !strings.HasPrefix(key, "BABEL_") {
					command.Env = append(command.Env, setting)
				}
			}
			command.Env = append(command.Env, "HOME="+root, "USERPROFILE="+root)
			output, runErr := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("save-only CLI did not finish: %v", ctx.Err())
			}
			for _, secret := range []string{oldKey, newKey, initial.Profiles["other"].APIKey} {
				if bytes.Contains(output, []byte(secret)) {
					t.Fatal("CLI output exposed a fixture credential")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.reject {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() <= 0 {
					t.Fatalf("cross-origin credential reuse was not rejected by the executable: err=%v output=%s", runErr, output)
				}
				if !bytes.Equal(after, before) {
					t.Fatal("rejected CLI save changed the original configuration bytes")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("authorized credential save failed: %v output=%s", runErr, output)
			}
			var saved config
			if err := json.Unmarshal(after, &saved); err != nil {
				t.Fatal(err)
			}
			wantProfile := initial.Profiles["fixture"]
			wantProfile.BaseURL, wantProfile.APIKey = test.endpoint, test.wantKey
			if saved.ActiveProfile != "fixture" || len(saved.Profiles) != 2 || saved.Profiles["fixture"] != wantProfile || saved.Profiles["other"] != initial.Profiles["other"] {
				t.Fatal("CLI save changed endpoint/key incorrectly or modified unrelated profile fields")
			}
		})
	}
}
