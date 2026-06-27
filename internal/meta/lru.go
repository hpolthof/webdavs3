package meta

import (
	"container/list"
	"sync"
)

// LRUCache caches open BucketDB instances by bucket ID, evicting the
// least-recently-used entry when the capacity is exceeded.
type LRUCache struct {
	mu      sync.Mutex
	cap     int
	items   map[string]*list.Element
	order   *list.List
	factory func(id string) (BucketDB, error)
}

type lruEntry struct {
	id string
	db BucketDB
	// rw serializes use vs. eviction: callers hold a read-lock for the
	// duration they use db; eviction acquires the write-lock before Close.
	rw sync.RWMutex
}

// NewLRUCache creates a cache with the given capacity and factory function.
// factory is called when a bucket is not in the cache.
func NewLRUCache(capacity int, factory func(id string) (BucketDB, error)) *LRUCache {
	return &LRUCache{
		cap:     capacity,
		items:   make(map[string]*list.Element),
		order:   list.New(),
		factory: factory,
	}
}

// Get returns a BucketDB for the given bucket ID, opening it via the factory if not cached.
// The caller MUST call the returned release function when done with the BucketDB to
// allow safe eviction.
func (c *LRUCache) Get(id string) (BucketDB, func(), error) {
	c.mu.Lock()

	if el, ok := c.items[id]; ok {
		c.order.MoveToFront(el)
		entry := el.Value.(*lruEntry)
		// Acquire read-lock while holding the cache lock so eviction cannot
		// sneak in between Get returning and the caller acquiring the lock.
		entry.rw.RLock()
		c.mu.Unlock()
		return entry.db, entry.rw.RUnlock, nil
	}

	db, err := c.factory(id)
	if err != nil {
		c.mu.Unlock()
		return nil, nil, err
	}

	entry := &lruEntry{id: id, db: db}

	if c.order.Len() >= c.cap {
		// Evict LRU (back of list).
		back := c.order.Back()
		if back != nil {
			victim := back.Value.(*lruEntry)
			delete(c.items, victim.id)
			c.order.Remove(back)
			// Acquire write-lock outside the cache lock to avoid deadlock;
			// release the cache lock first, then wait for all readers.
			c.mu.Unlock()
			victim.rw.Lock()
			victim.db.Close()
			victim.rw.Unlock()
			c.mu.Lock()
		}
	}

	el := c.order.PushFront(entry)
	c.items[id] = el

	// Acquire read-lock before releasing the cache lock (same reason as above).
	entry.rw.RLock()
	c.mu.Unlock()

	return db, entry.rw.RUnlock, nil
}

// Remove evicts a single entry from the cache and closes its BucketDB.
func (c *LRUCache) Remove(id string) {
	c.mu.Lock()
	el, ok := c.items[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	entry := el.Value.(*lruEntry)
	delete(c.items, id)
	c.order.Remove(el)
	c.mu.Unlock()

	// Wait for in-flight readers before closing.
	entry.rw.Lock()
	entry.db.Close()
	entry.rw.Unlock()
}

// CloseAll closes all cached BucketDB instances and clears the cache.
func (c *LRUCache) CloseAll() {
	c.mu.Lock()
	entries := make([]*lruEntry, 0, len(c.items))
	for _, el := range c.items {
		entries = append(entries, el.Value.(*lruEntry))
	}
	c.items = make(map[string]*list.Element)
	c.order.Init()
	c.mu.Unlock()

	for _, entry := range entries {
		entry.rw.Lock()
		entry.db.Close()
		entry.rw.Unlock()
	}
}
