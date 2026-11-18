package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
)

var cachePersistMutex sync.Mutex

const candidateCacheVersion = "candidate-v2"

func cacheFilePath() string {
	localCache := "babel.cache.json"
	if _, err := os.Stat("babel.config.json"); err == nil {
		return localCache
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return localCache
	}
	return filepath.Join(home, ".babel_cache.json")
}

func initDiskCache() {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return
	}
	var restored map[string]CacheEntry
	if json.Unmarshal(data, &restored) != nil || restored == nil {
		return
	}
	for key, entry := range restored {
		entry.Audit = ""
		restored[key] = entry
	}
	cacheMutex.Lock()
	diskCache = restored
	evictCacheIfNeededLocked()
	cacheMutex.Unlock()
}

func evictCacheIfNeededLocked() {
	for len(diskCache) > maxCacheEntries {
		var oldestKey string
		var oldestTime int64 = math.MaxInt64
		for k, v := range diskCache {
			if v.LastAccess < oldestTime {
				oldestTime = v.LastAccess
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(diskCache, oldestKey)
		} else {
			break
		}
	}
}

func saveDiskCache() {
	// Serialize snapshots and atomically replace the file: concurrent compiles must
	// not truncate each other's JSON or let an older snapshot win a later write.
	cachePersistMutex.Lock()
	defer cachePersistMutex.Unlock()
	cacheMutex.Lock()
	evictCacheIfNeededLocked()
	data, err := json.MarshalIndent(diskCache, "", "  ")
	cacheMutex.Unlock()
	if err != nil {
		return
	}
	path := cacheFilePath()
	f, err := os.CreateTemp(filepath.Dir(path), ".babel-cache-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func makeCacheKey(dialect Dialect, p Profile, text string) string {
	p = effectiveCompilerProfile(p)
	// Hash the full composed prompt as well as its version identifier. Do not
	// trim source text: whitespace can be part of a protected literal.
	promptHash := sha256.Sum256([]byte(dialect.Prompt))
	identity := struct {
		Version       string
		Endpoint      string
		Model         string
		Dialect       string
		PromptVersion string
		PromptSHA256  string
		Text          string
	}{candidateCacheVersion, buildOpenAICompletionsURL(p.BaseURL), p.Model,
		dialect.Name, dialect.PromptHash, hex.EncodeToString(promptHash[:]), text}
	raw, _ := json.Marshal(identity)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
