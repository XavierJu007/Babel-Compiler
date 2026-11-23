package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// ttlLRU 是带容量上限与过期时间的进程内 LRU，只存活于内存，不落盘。
type ttlLRU[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	entries  map[K]*list.Element
	order    *list.List // 头部最新
}

type ttlEntry[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
}

func newTTLLRU[K comparable, V any](capacity int) *ttlLRU[K, V] {
	if capacity <= 0 {
		capacity = 1024
	}
	return &ttlLRU[K, V]{capacity: capacity, entries: make(map[K]*list.Element, capacity), order: list.New()}
}

func (c *ttlLRU[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	entry := element.Value.(*ttlEntry[K, V])
	if !entry.expires.IsZero() && time.Now().After(entry.expires) {
		c.order.Remove(element)
		delete(c.entries, key)
		return zero, false
	}
	c.order.MoveToFront(element)
	return entry.value, true
}

func (c *ttlLRU[K, V]) Put(key K, value V, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := time.Now().Add(ttl)
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*ttlEntry[K, V])
		entry.value = value
		entry.expires = expires
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&ttlEntry[K, V]{key: key, value: value, expires: expires})
	c.entries[key] = element
	for len(c.entries) > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*ttlEntry[K, V]).key)
	}
}

func (c *ttlLRU[K, V]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[K]*list.Element, c.capacity)
	c.order.Init()
}

// validationCacheKey 覆盖全部会改变「通过/不通过」判断的因子：
// 候选内容、原策略版本、校验端点/模型/凭据、盲读阅读器，以及必含关键词。
type validationCacheKey struct {
	SourceSHA256     string
	CandidateSHA256  string
	ValidationMode   string
	PolicyVersion    string
	AuditEndpoint    string
	AuditModel       string
	AuditCredSHA256  string
	ReaderEndpoint   string
	ReaderModel      string
	ReaderCredSHA256 string
	RequiredKeywords string
}

var validationCache = newTTLLRU[validationCacheKey, ValidationResult](4096)

// cloneValidationResult 复制 Issues/Stages，避免缓存与调用方共享可变切片。
func cloneValidationResult(result ValidationResult) ValidationResult {
	cloned := result
	cloned.Issues = append([]string(nil), result.Issues...)
	if result.Stages != nil {
		cloned.Stages = make([]ValidationStage, len(result.Stages))
		for i, stage := range result.Stages {
			stage.Issues = append([]string(nil), stage.Issues...)
			cloned.Stages[i] = stage
		}
	}
	return cloned
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func makeValidationCacheKey(req CompileReq, mode string, auditor, reader Profile, candidate string) validationCacheKey {
	policy := validationPolicyVersion
	if normalizeValidationMode(mode) == "blind" {
		policy = blindValidationPolicyVersion
	}
	keywords := append([]string(nil), req.RequiredKeywords...)
	sort.Strings(keywords)
	return validationCacheKey{
		SourceSHA256:     sha256Hex(req.Text),
		CandidateSHA256:  sha256Hex(candidate),
		ValidationMode:   normalizeValidationMode(mode),
		PolicyVersion:    policy,
		AuditEndpoint:    compilerRequestURL(auditor.BaseURL),
		AuditModel:       auditor.Model,
		AuditCredSHA256:  sha256Hex(auditor.APIKey),
		ReaderEndpoint:   compilerRequestURL(reader.BaseURL),
		ReaderModel:      reader.Model,
		ReaderCredSHA256: sha256Hex(reader.APIKey),
		RequiredKeywords: strings.Join(keywords, "\x00"),
	}
}
