package main

import (
	"container/list"
	"sync"
	"time"
)

// negativeCache 记录「确定性低收益」的文本指纹，避免同一段长文反复消耗编译配额。
// 只允许写入可复现的结果：未变短或结构未达标。
// 429、5xx、超时与熔断属于瞬时状态，绝不能进入这里。
type negativeCacheStore struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]*list.Element
	order    *list.List
}

type negativeEntry struct {
	key     string
	expires time.Time
}

var negativeCache = &negativeCacheStore{
	capacity: 2000,
	entries:  make(map[string]*list.Element, 2000),
	order:    list.New(),
}

func (c *negativeCacheStore) Get(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return false
	}
	entry := element.Value.(*negativeEntry)
	if time.Now().After(entry.expires) {
		c.order.Remove(element)
		delete(c.entries, key)
		return false
	}
	c.order.MoveToFront(element)
	return true
}

func (c *negativeCacheStore) Put(key string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := time.Now().Add(ttl)
	if element, ok := c.entries[key]; ok {
		element.Value.(*negativeEntry).expires = expires
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&negativeEntry{key: key, expires: expires})
	c.entries[key] = element
	for len(c.entries) > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*negativeEntry).key)
	}
}

func (c *negativeCacheStore) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element, c.capacity)
	c.order.Init()
}

// negativeCacheKey 绑定表达策略与目标编码：换模型、换词典或换端点后必须重试。
func negativeCacheKey(dialect Dialect, p Profile, text string) string {
	return makeCacheKey(dialect, p, text)
}
