package main

import (
	"os"
	"testing"
)

func TestCandidateCacheIdentityIncludesProviderPromptAndExactSource(t *testing.T) {
	dialect := Dialect{Name: "token", Prompt: "public policy", PromptHash: "policy-version"}
	profile := Profile{BaseURL: "https://compiler.example/v1", Model: "model"}
	text := "  a literal  \n"
	baseline := makeCacheKey(dialect, profile, text)
	changedProfile := profile
	changedProfile.BaseURL = "https://other.example/v1"
	changedModel := profile
	changedModel.Model = "other-model"
	changedPrompt := dialect
	changedPrompt.Prompt = "another public policy"
	changedVersion := dialect
	changedVersion.PromptHash = "next-version"
	for name, key := range map[string]string{
		"provider":          makeCacheKey(dialect, changedProfile, text),
		"model":             makeCacheKey(dialect, changedModel, text),
		"full prompt":       makeCacheKey(changedPrompt, profile, text),
		"prompt version":    makeCacheKey(changedVersion, profile, text),
		"source whitespace": makeCacheKey(dialect, profile, "a literal"),
	} {
		if key == baseline {
			t.Errorf("%s omitted from cache identity", name)
		}
	}
	for _, address := range []string{
		"https://compiler.example/v1/chat/completions", "https://compiler.example/v1/",
		"https://compiler.example/v1?route=a", "https://compiler.example/custom",
	} {
		differentEndpoint := profile
		differentEndpoint.BaseURL = address
		if makeCacheKey(dialect, differentEndpoint, text) == baseline {
			t.Errorf("distinct request endpoint shares a candidate: %s", address)
		}
	}
	sameEndpoint := profile
	sameEndpoint.BaseURL = "  " + profile.BaseURL + " \n"
	if makeCacheKey(dialect, sameEndpoint, text) != baseline {
		t.Fatal("outer URL whitespace changed the actual endpoint identity")
	}
	rotatedKey := profile
	rotatedKey.APIKey = "do-not-store-secret"
	if makeCacheKey(dialect, rotatedKey, text) != baseline {
		t.Fatal("API key should not be part of the persisted cache identity")
	}
}

func TestInvalidDiskCacheDoesNotReplaceLiveMap(t *testing.T) {
	isolateCompilerState(t)
	diskCache["preserved"] = CacheEntry{Compiled: "existing"}
	for _, raw := range []string{"null", "{invalid"} {
		if err := os.WriteFile(cacheFilePath(), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		initDiskCache()
		if diskCache["preserved"].Compiled != "existing" {
			t.Fatal("invalid cache replaced live state")
		}
	}
	if err := os.WriteFile(cacheFilePath(), []byte(`{"candidate":{"compiled":"text","audit":"[PASS] legacy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	initDiskCache()
	if diskCache["candidate"].Compiled != "text" || diskCache["candidate"].Audit != "" {
		t.Fatal("legacy audit must not be restored as evidence")
	}
}
