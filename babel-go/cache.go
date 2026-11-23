package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var cachePersistMutex sync.Mutex

const candidateCacheVersion = "candidate-v3"

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
	snapshot := make(map[string]CacheEntry, len(diskCache))
	for k, v := range diskCache {
		snapshot[k] = v
	}
	cacheMutex.Unlock()

	// Marshal outside cacheMutex so large snapshots never stall concurrent readers.
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	path := cacheFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".babel-cache-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

var (
	cacheFlushMu      sync.Mutex
	cacheFlushActive  bool
	cacheFlushTrigger chan struct{}
	cacheFlushStop    chan struct{}
	cacheFlushDone    chan struct{}
)

// startAsyncCacheFlusher 在服务端模式把突发的缓存写入合并成一次落盘；
// 返回的 stop 函数会先停掉定时器再把未落盘的内容同步刷下去。
// 未启动时 scheduleDiskCacheSave 自动退回同步写盘，CLI 与单测行为不变。
func startAsyncCacheFlusher(debounce time.Duration) func() {
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	cacheFlushMu.Lock()
	if cacheFlushActive {
		cacheFlushMu.Unlock()
		return func() {}
	}
	trigger := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	cacheFlushTrigger = trigger
	cacheFlushStop = stop
	cacheFlushDone = done
	cacheFlushActive = true
	cacheFlushMu.Unlock()

	go func() {
		defer close(done)
		var (
			timer   *time.Timer
			timerCh <-chan time.Time
			dirty   bool
		)
		stopTimer := func() {
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer = nil
				timerCh = nil
			}
		}
		for {
			select {
			case <-trigger:
				dirty = true
				if timer == nil {
					timer = time.NewTimer(debounce)
					timerCh = timer.C
				}
			case <-timerCh:
				timer = nil
				timerCh = nil
				if dirty {
					dirty = false
					saveDiskCache()
				}
			case <-stop:
				stopTimer()
				select {
				case <-trigger:
					dirty = true
				default:
				}
				if dirty {
					saveDiskCache()
				}
				return
			}
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cacheFlushMu.Lock()
			if !cacheFlushActive || cacheFlushStop != stop {
				cacheFlushMu.Unlock()
				return
			}
			cacheFlushActive = false
			cacheFlushTrigger = nil
			cacheFlushStop = nil
			cacheFlushDone = nil
			close(stop)
			cacheFlushMu.Unlock()
			<-done
		})
	}
}

func scheduleDiskCacheSave() {
	cacheFlushMu.Lock()
	active := cacheFlushActive
	trigger := cacheFlushTrigger
	cacheFlushMu.Unlock()
	if !active || trigger == nil {
		saveDiskCache()
		return
	}
	select {
	case trigger <- struct{}{}:
	default:
	}
}

func makeCacheKey(dialect Dialect, p Profile, text string) string {
	p = effectiveCompilerProfile(p)
	// Domain guidance is untrusted user-message data, but still affects the
	// generated candidate. Credentials can route the same endpoint/model to
	// different tenants, so isolate both persisted candidates and singleflight.
	// Only irreversible digests enter the identity; never persist a raw key.
	// Do not trim source text: whitespace can be part of a protected literal.
	promptHash := sha256.Sum256([]byte(dialect.Prompt))
	guidanceHash := sha256.Sum256([]byte(dialect.Guidance))
	credentialHash := sha256.Sum256([]byte(p.APIKey))
	identity := struct {
		Version          string
		Endpoint         string
		Model            string
		Dialect          string
		PromptVersion    string
		PromptSHA256     string
		GuidanceSHA256   string
		CredentialSHA256 string
		Text             string
	}{Version: candidateCacheVersion, Endpoint: compilerRequestURL(p.BaseURL), Model: p.Model,
		Dialect: dialect.Name, PromptVersion: dialect.PromptHash, PromptSHA256: hex.EncodeToString(promptHash[:]),
		GuidanceSHA256: hex.EncodeToString(guidanceHash[:]), CredentialSHA256: hex.EncodeToString(credentialHash[:]), Text: text}
	raw, _ := json.Marshal(identity)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
