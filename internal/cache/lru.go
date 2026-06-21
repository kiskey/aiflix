package cache

import (
	"time"

	"github.com/patrickmn/go-cache"
)

type LRU struct {
	cache *cache.Cache
}

func NewLRU(sizeHint int, ttl time.Duration) *LRU {
	cleanupInterval := ttl * 2
	return &LRU{
		cache: cache.New(ttl, cleanupInterval),
	}
}

// Get fetches an item from the cache. Redundant mutex removed to leverage go-cache's internal thread safety.
func (l *LRU) Get(key string) (interface{}, bool) {
	return l.cache.Get(key)
}

func (l *LRU) Set(key string, value interface{}) {
	l.cache.SetDefault(key, value)
}

func (l *LRU) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	l.cache.Set(key, value, ttl)
}

func (l *LRU) Delete(key string) {
	l.cache.Delete(key)
}

func (l *LRU) Flush() {
	l.cache.Flush()
}

func (l *LRU) ItemCount() int {
	return l.cache.ItemCount()
}
