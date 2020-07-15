package cache

import (
	"container/list"
	"sync"
	"time"
)

// entry stores cached entry key and value.
type entry struct {
	key   Key
	value Value

	// expireAfterAccessDeadline is the
	// time that this entry should be removed, due
	// to the ExpireAfterAccess config.
	// This will be updated each time the entry is
	// accessed.
	// This is not jittered as the entries list is
	// ordered by this.
	expireAfterAccessDeadline time.Time

	// expireAfterWriteDeadline is the
	// time that this entry should be removed, due
	// to the ExpireAfterWrite config.
	// This will be updated each time the entry is
	// accessed.
	// It may be jittered, so the time may not align
	// exactly with ExpireAfterWrite.
	expireAfterWriteDeadline time.Time

	// listID is ID of the list which this entry is currently in.
	listID listID
	// hash is the hash value of this entry key
	hash uint64
}

// getEntry returns the entry attached to the given list element.
func getEntry(el *list.Element) *entry {
	return el.Value.(*entry)
}

// setEntry updates value of the given list element.
func setEntry(el *list.Element, en *entry) {
	el.Value = en
}

// cache is a data structure for cache entries.
type cache struct {
	mu   sync.RWMutex
	data map[Key]*list.Element
}

// policy is a cache policy.
type policy interface {
	init(cache *cache, maximumSize int)
	add(newEntry *entry) *entry
	hit(element *list.Element)
	remove(element *list.Element) *entry
	walk(func(list *list.List))
}

func newPolicy(name string) policy {
	switch name {
	case "", "slru":
		return &slruCache{}
	case "lru":
		return &lruCache{}
	case "tinylfu":
		return &tinyLFU{}
	default:
		panic("cache: unsupported policy " + name)
	}
}
