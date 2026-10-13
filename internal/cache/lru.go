package cache

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

const lruShards = 32

// LRU is a sharded, TTL-aware, size-bounded in-process cache.
type LRU struct {
	shards [lruShards]lruShard
	ttl    time.Duration
}

type lruShard struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

type lruItem struct {
	key     string
	val     *Entry
	expires time.Time
}

// NewLRU creates a cache holding up to maxEntries items for ttl each.
func NewLRU(maxEntries int, ttl time.Duration) *LRU {
	per := max(maxEntries/lruShards, 1)
	l := &LRU{ttl: ttl}
	for i := range l.shards {
		l.shards[i] = lruShard{max: per, ll: list.New(), items: map[string]*list.Element{}}
	}
	return l
}

func (l *LRU) shard(key string) *lruShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &l.shards[h.Sum32()%lruShards]
}

// Get returns the cached entry if present and unexpired.
func (l *LRU) Get(key string) (*Entry, bool) {
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*lruItem)
	if time.Now().After(it.expires) {
		s.ll.Remove(el)
		delete(s.items, key)
		return nil, false
	}
	s.ll.MoveToFront(el)
	return it.val, true
}

// Set inserts or replaces key.
func (l *LRU) Set(key string, e *Entry) {
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	exp := time.Now().Add(l.ttl)
	if el, ok := s.items[key]; ok {
		it := el.Value.(*lruItem)
		it.val, it.expires = e, exp
		s.ll.MoveToFront(el)
		return
	}
	s.items[key] = s.ll.PushFront(&lruItem{key: key, val: e, expires: exp})
	for s.ll.Len() > s.max {
		last := s.ll.Back()
		s.ll.Remove(last)
		delete(s.items, last.Value.(*lruItem).key)
	}
}

// Len returns the number of resident entries.
func (l *LRU) Len() int {
	n := 0
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		n += s.ll.Len()
		s.mu.Unlock()
	}
	return n
}
