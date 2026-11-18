package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

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
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	_ = json.Unmarshal(data, &diskCache)
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
	cacheMutex.Lock()
	evictCacheIfNeededLocked()
	data, err := json.MarshalIndent(diskCache, "", "  ")
	cacheMutex.Unlock()
	if err == nil {
		_ = os.WriteFile(cacheFilePath(), data, 0600)
	}
}

func makeCacheKey(dialect Dialect, model, text string) string {
	raw := fmt.Sprintf("%s#%s|%s|%s", dialect.Name, dialect.PromptHash, model, strings.TrimSpace(text))
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
