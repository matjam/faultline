package tools

import (
	"sync"
	"time"
)

// webCacheEntry holds a cached web page fetch result.
type webCacheEntry struct {
	content   string
	fetchedAt time.Time
}

// WebCache is a TTL cache for web_fetch / wiki_fetch results with
// proactive eviction. Lifecycle is owned by the composition root, not
// the Executor: with multiple Executor instances (primary + subagents)
// sharing one cache, no single Executor's Close() can be allowed to
// stop the cache. main.go constructs one WebCache and defers its
// Close() at process exit; per-Executor Close() leaves it alone.
type WebCache struct {
	mu        sync.Mutex
	entries   map[string]webCacheEntry
	ttl       time.Duration
	stop      chan struct{}
	closeOnce sync.Once
}

// NewWebCache constructs a cache with the given entry TTL and starts
// a background eviction goroutine that wakes every ttl/2.
func NewWebCache(ttl time.Duration) *WebCache {
	c := &WebCache{
		entries: make(map[string]webCacheEntry),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}
	go c.evictLoop()
	return c
}

// Close stops the background eviction goroutine. Idempotent: safe to
// call more than once. The composition root is responsible for
// invoking this exactly once on process exit.
func (c *WebCache) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
}

// evictLoop periodically removes expired entries.
func (c *WebCache) evictLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.evict()
		}
	}
}

func (c *WebCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for url, entry := range c.entries {
		if now.Sub(entry.fetchedAt) > c.ttl {
			delete(c.entries, url)
		}
	}
}

// Get returns cached content and true if the entry exists and hasn't
// expired.
func (c *WebCache) Get(url string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[url]
	if !ok || time.Since(entry.fetchedAt) > c.ttl {
		if ok {
			delete(c.entries, url)
		}
		return "", false
	}
	return entry.content, true
}

// Set stores content for a URL.
func (c *WebCache) Set(url, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[url] = webCacheEntry{content: content, fetchedAt: time.Now()}
}
