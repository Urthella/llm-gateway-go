package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// CacheConfig enables caching of deterministic (temperature 0, non-streaming) responses.
type CacheConfig struct {
	TTL        string `json:"ttl"`
	MaxEntries int    `json:"max_entries"`
}

type cached struct {
	body        []byte
	contentType string
	upstream    string
	expires     time.Time
}

// ponytail: FIFO eviction, not LRU; swap in an LRU list if hit rates show it matters.
type cache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]cached
	order   []string
	now     func() time.Time
}

func newCache(c *CacheConfig) (*cache, error) {
	ttl, err := time.ParseDuration(c.TTL)
	if err != nil || ttl <= 0 {
		return nil, fmt.Errorf("cache ttl %q must be a positive duration like 10m", c.TTL)
	}
	return &cache{ttl: ttl, max: max(c.MaxEntries, 1), entries: map[string]cached{}, now: time.Now}, nil
}

// key is per client, so one client's answers are never served to another, and covers the whole
// request: json.Marshal sorts map keys, so field order in the client's JSON doesn't matter.
func cacheKey(client string, req map[string]any, stream bool) string {
	if stream {
		return ""
	}
	if t, ok := req["temperature"].(float64); !ok || t != 0 {
		return ""
	}
	payload, _ := json.Marshal(req)
	sum := sha256.Sum256(append([]byte(client+"\x00"), payload...))
	return hex.EncodeToString(sum[:])
}

func (c *cache) get(key string) (cached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && c.now().After(e.expires) {
		delete(c.entries, key)
		return cached{}, false
	}
	return e, ok
}

func (c *cache) put(key string, e cached) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	e.expires = c.now().Add(c.ttl)
	c.entries[key] = e
	for len(c.entries) > c.max && len(c.order) > 0 {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	if len(c.order) > 2*c.max { // drop keys that expired out of the map
		live := c.order[:0]
		for _, k := range c.order {
			if _, ok := c.entries[k]; ok {
				live = append(live, k)
			}
		}
		c.order = live
	}
}
