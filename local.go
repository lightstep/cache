package cache

import (
	"container/list"
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Default maximum number of cache entries.
	maximumCapacity = 1 << 30
	// Buffer size of entry channels
	chanBufSize = 1
	// Maximum number of entries to be drained in a single clean up.
	drainMax = 16
	// Number of cache access operations that will trigger clean up.
	drainThreshold = 64
)

// currentTime is an alias for time.Now, used for testing.
var currentTime = time.Now

// localCache is an asynchronous LRU cache.
type localCache struct {
	// user configurations
	policyName        string
	expireAfterAccess time.Duration
	expireAfterWrite  time.Duration
	jitterFraction    float64

	onInsertion Func
	onRemoval   Func

	loader LoaderFuncContext
	stats  StatsCounter

	// internal data structure
	cap   int
	cache cache

	entries     policy
	addEntry    chan *entry
	hitEntry    chan *list.Element
	deleteEntry chan *list.Element

	// readCount is a counter of the number of reads since the last write.
	readCount int32

	// for closing routines created by this cache.
	closeMu sync.Mutex
	closeCh chan struct{}
}

// newLocalCache returns a default localCache.
// init must be called before this cache can be used.
func newLocalCache() *localCache {
	return &localCache{
		cap: maximumCapacity,
		cache: cache{
			data: make(map[Key]*list.Element),
		},
		stats: &statsCounter{},
	}
}

// init initializes cache replacement policy after all user configuration properties are set.
func (c *localCache) init() {
	c.entries = newPolicy(c.policyName)
	c.entries.init(&c.cache, c.cap)

	c.addEntry = make(chan *entry, chanBufSize)
	c.hitEntry = make(chan *list.Element, chanBufSize)
	c.deleteEntry = make(chan *list.Element, chanBufSize)

	c.closeCh = make(chan struct{})
	go c.processEntries()
}

// Close implements io.Closer and always returns a nil error.
func (c *localCache) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closeCh != nil {
		c.closeCh <- struct{}{}
		// Wait for the goroutine to close this channel
		// (should use sync.WaitGroup or a new channel instead?)
		<-c.closeCh
		c.closeCh = nil
	}
	return nil
}

// GetIfPresent gets cached value from entries list and updates
// last access time for the entry if it is found.
func (c *localCache) GetIfPresent(k Key) (Value, bool) {
	c.cache.mu.RLock()
	el, hit := c.cache.data[k]
	c.cache.mu.RUnlock()
	if !hit {
		c.stats.RecordMisses(1)
		return nil, false
	}
	en := getEntry(el)
	if c.isExpired(en, currentTime()) {
		c.deleteEntry <- el
		c.stats.RecordMisses(1)
		return nil, false
	}
	c.hitEntry <- el
	c.stats.RecordHits(1)
	return en.value, true
}

// Put adds new entry to entries list.
func (c *localCache) Put(k Key, v Value) {
	c.cache.mu.RLock()
	el, hit := c.cache.data[k]
	c.cache.mu.RUnlock()
	if hit {
		// Update list element value
		getEntry(el).value = v
		c.hitEntry <- el
	} else {
		en := &entry{
			key:   k,
			value: v,
			hash:  sum(k),
		}
		c.addEntry <- en
	}
}

// Invalidate removes the entry associated with key k.
func (c *localCache) Invalidate(k Key) {
	c.cache.mu.RLock()
	el, hit := c.cache.data[k]
	c.cache.mu.RUnlock()
	if hit {
		c.deleteEntry <- el
	}
}

// InvalidateAll resets entries list.
func (c *localCache) InvalidateAll() {
	c.deleteEntry <- nil
}

// Get returns value associated with k or call underlying loader to retrieve value
// if it is not in the cache. The returned value is only cached when loader returns
// nil error.
func (c *localCache) Get(k Key) (Value, error) {
	return c.GetContext(context.Background(), k)
}

// GetContext returns value associated with k or call underlying loader to retrieve value
// if it is not in the cache. The returned value is only cached when loader returns
// nil error.
func (c *localCache) GetContext(ctx context.Context, k Key) (Value, error) {
	c.cache.mu.RLock()
	el, hit := c.cache.data[k]
	c.cache.mu.RUnlock()
	if !hit {
		c.stats.RecordMisses(1)
		return c.load(ctx, k)
	}
	en := getEntry(el)
	// Check if this entry needs to be refreshed
	if c.isExpired(en, currentTime()) {
		c.stats.RecordMisses(1)
		return c.refresh(ctx, en), nil
	}
	c.stats.RecordHits(1)
	v := en.value
	c.hitEntry <- el
	return v, nil
}

// Stats copies cache stats to t.
func (c *localCache) Stats(t *Stats) {
	c.stats.Snapshot(t)
}

func (c *localCache) processEntries() {
	defer close(c.closeCh)
	for {
		select {
		case <-c.closeCh:
			c.removeAll()
			return
		case en := <-c.addEntry:
			c.add(en)
			c.postWriteCleanup()
		case el := <-c.hitEntry:
			c.hit(el)
			c.postReadCleanup()
		case el := <-c.deleteEntry:
			if el == nil {
				c.removeAll()
			} else {
				c.remove(el)
			}
			c.postReadCleanup()
		}
	}
}

func (c *localCache) add(en *entry) {
	now := currentTime()
	en.expireAfterAccessDeadline = now.Add(c.expireAfterAccess) // Not jittered, as this is how c.entries is ordered.
	en.expireAfterWriteDeadline = now.Add(c.jitter(c.expireAfterWrite))

	remEn := c.entries.add(en)
	if c.onInsertion != nil {
		c.onInsertion(en.key, en.value)
	}
	if remEn != nil {
		// An entry has been evicted
		c.stats.RecordEviction()
		if c.onRemoval != nil {
			c.onRemoval(remEn.key, remEn.value)
		}
	}
}

// removeAll remove all entries in the cache.
func (c *localCache) removeAll() {
	c.cache.mu.Lock()
	oldData := c.cache.data
	c.cache.data = make(map[Key]*list.Element)
	c.entries.init(&c.cache, c.cap)
	c.cache.mu.Unlock()

	if c.onRemoval != nil {
		for _, el := range oldData {
			en := getEntry(el)
			c.onRemoval(en.key, en.value)
		}
	}
}

// remove removes the given element from the cache and entries list.
// It also calls onRemoval callback if it is set.
func (c *localCache) remove(el *list.Element) {
	en := c.entries.remove(el)

	if en != nil && c.onRemoval != nil {
		c.onRemoval(en.key, en.value)
	}
}

// hit moves the given element to the top of the entries list.
func (c *localCache) hit(el *list.Element) {
	// Not jittered, as this is how c.entries is ordered.
	getEntry(el).expireAfterAccessDeadline = currentTime().Add(c.expireAfterAccess)
	c.entries.hit(el)
}

// load uses current loader to retrieve value for k and adds new
// entry to the cache only if loader returns a nil error.
func (c *localCache) load(ctx context.Context, k Key) (Value, error) {
	if c.loader == nil {
		panic("loader must be set")
	}
	start := currentTime()
	v, err := c.loader(ctx, k)
	loadTime := currentTime().Sub(start)
	if err != nil {
		c.stats.RecordLoadError(loadTime)
		return nil, err
	}
	en := &entry{
		key:   k,
		value: v,
		hash:  sum(k),
	}
	c.addEntry <- en
	c.stats.RecordLoadSuccess(loadTime)
	return v, nil
}

// refresh reloads value for the given key. If loader returns an error,
// that error will be omitted and current value will be returned.
// Otherwise, the function will returns new value and updates the current
// cache entry.
func (c *localCache) refresh(ctx context.Context, en *entry) Value {
	if c.loader == nil {
		panic("loader must be set")
	}
	start := currentTime()
	newV, err := c.loader(ctx, en.key)
	loadTime := currentTime().Sub(start)
	if err != nil {
		c.stats.RecordLoadError(loadTime)
		return en.value
	}
	en.value = newV
	c.addEntry <- en
	c.stats.RecordLoadSuccess(loadTime)
	return newV
}

// postReadCleanup is run after entry access/delete event.
func (c *localCache) postReadCleanup() {
	if atomic.AddInt32(&c.readCount, 1) > drainThreshold {
		atomic.StoreInt32(&c.readCount, 0)
		c.expireEntries()
	}
}

// postWriteCleanup is run after entry add event.
func (c *localCache) postWriteCleanup() {
	atomic.StoreInt32(&c.readCount, 0)
	c.expireEntries()
}

// expireEntries removes expired entries.
func (c *localCache) expireEntries() {
	if c.expireAfterAccess <= 0 {
		return
	}
	now := currentTime()
	remain := drainMax
	c.entries.walk(func(ls *list.List) {
		for ; remain > 0; remain-- {
			el := ls.Back()
			if el == nil {
				// List is empty
				break
			}
			en := getEntry(el)
			if !en.expireAfterAccessDeadline.Before(now) {
				// Can break since the entries list is sorted by access time
				break
			}
			// The deadline is in the past; remove the entry.
			c.remove(el)
			c.stats.RecordEviction()
		}
	})
}

func (c *localCache) isExpired(en *entry, now time.Time) bool {
	if c.expireAfterAccess > 0 && en.expireAfterAccessDeadline.Before(now) {
		return true
	}
	if c.expireAfterWrite > 0 && en.expireAfterWriteDeadline.Before(now) {
		return true
	}
	return false
}

// jitter adds random jitter to the duration.
//
// This adds or subtracts time from the duration within a given jitter fraction.
// For example for 10s and jitter 0.1, it will return a time within [9s, 11s])
// Based on https://github.com/grpc-ecosystem/go-grpc-middleware/blob/b28fb2bb3547ce89f6a5cfe23c5445d1aa29d52b/util/backoffutils/backoff.go#L20.
func (c *localCache) jitter(dur time.Duration) time.Duration {
	multiplier := c.jitterFraction * (rand.Float64()*2 - 1)
	return time.Duration(float64(dur) * (1 + multiplier))
}

// New returns a local in-memory Cache.
func New(options ...Option) Cache {
	c := newLocalCache()
	for _, opt := range options {
		opt(c)
	}
	c.init()
	return c
}

// NewLoadingCache returns a new LoadingCache with given loader function
// and cache options.
func NewLoadingCacheContext(loader LoaderFuncContext, options ...Option) LoadingCache {
	c := newLocalCache()
	c.loader = loader
	for _, opt := range options {
		opt(c)
	}
	c.init()
	return c
}

// NewLoadingCache returns a new LoadingCache with given loader function
// and cache options.
func NewLoadingCache(loader LoaderFunc, options ...Option) LoadingCache {
	return NewLoadingCacheContext(
		func(_ context.Context, key Key) (Value, error) {
			return loader(key)
		}, options...,
	)
}

// Option add options for default Cache.
type Option func(c *localCache)

// WithMaximumSize returns an Option which sets maximum size for the cache.
// Any non-positive numbers is considered as unlimited.
func WithMaximumSize(size int) Option {
	if size < 0 {
		size = 0
	}
	if size > maximumCapacity {
		size = maximumCapacity
	}
	return func(c *localCache) {
		c.cap = size
	}
}

// WithRemovalListener returns an Option to set cache to call onRemoval for each
// entry evicted from the cache.
func WithRemovalListener(onRemoval Func) Option {
	return func(c *localCache) {
		c.onRemoval = onRemoval
	}
}

// WithExpireAfterAccess returns an option to expire a cache entry after the
// given duration without being accessed.
func WithExpireAfterAccess(d time.Duration) Option {
	return func(c *localCache) {
		c.expireAfterAccess = d
	}
}

// WithExpireAfterWrite returns an option to expire a cache entry after the
// given duration from creation.
func WithExpireAfterWrite(d time.Duration) Option {
	return func(c *localCache) {
		c.expireAfterWrite = d
	}
}

// WithStatsCounter returns an option which overrides default cache stats counter.
func WithStatsCounter(st StatsCounter) Option {
	return func(c *localCache) {
		c.stats = st
	}
}

// WithPolicy returns an option which set cache policy associated to the given name.
// Supported policies are: lru, slru, tinylfu.
func WithPolicy(name string) Option {
	return func(c *localCache) {
		c.policyName = name
	}
}

// WithJitterFraction returns an option that can be used to apply
// some amount of jitter to the option WithExpireAfterWrite.
//
// Example, given a jitter fraction of 0.1 (10%)
// An a ExpireAfterWrite of 10 seconds,
// the effective ExpireAfterWrite, on a per-item basis,
// will be in [9, 11].
func WithJitterFraction(jf float64) Option {
	return func(c *localCache) {
		c.jitterFraction = jf
	}
}

// withInsertionListener is used for testing.
func withInsertionListener(onInsertion Func) Option {
	return func(c *localCache) {
		c.onInsertion = onInsertion
	}
}
