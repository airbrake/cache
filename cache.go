package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/go-redis/cache/v7/internal/singleflight"
	"github.com/vmihailenco/msgpack/v4"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/go-redis/redis/v7"
)

var ErrCacheMiss = errors.New("cache: key is missing")
var errRedisLocalCacheNil = errors.New("cache: both Redis and LocalCache are nil")

type rediser interface {
	Set(key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(key string) *redis.StringCmd
	Del(keys ...string) *redis.IntCmd
}

type Item struct {
	Ctx context.Context

	Key    string
	Object interface{}

	// Func returns object to be cached.
	Func func() (interface{}, error)

	// Expiration is the cache expiration time.
	// Default expiration is 1 hour.
	Expiration time.Duration
}

func (item *Item) Context() context.Context {
	if item.Ctx == nil {
		return context.Background()
	}
	return item.Ctx
}

func (item *Item) object() (interface{}, error) {
	if item.Object != nil {
		return item.Object, nil
	}
	if item.Func != nil {
		return item.Func()
	}
	return nil, nil
}

func (item *Item) exp() time.Duration {
	if item.Expiration < 0 {
		return 0
	}
	if item.Expiration < time.Second {
		return time.Hour
	}
	return item.Expiration
}

type Codec struct {
	Redis rediser

	localCache *fastcache.Cache
	group      singleflight.Group

	hits        uint64
	misses      uint64
	localHits   uint64
	localMisses uint64
}

// UseLocalCache causes Codec to cache items in local LRU cache.
func (cd *Codec) UseLocalCache(maxLen int, expiration time.Duration) {
	cd.localCache = fastcache.New(100000)
	//	cd.localCache = lrucache.New(maxLen, expiration)
}

// Set caches the item.
func (cd *Codec) Set(item *Item) error {
	obj, err := item.object()
	if err != nil {
		return err
	}
	_, err = cd.set(item.Context(), item.Key, obj, item.exp())
	return err
}

func (cd *Codec) set(
	ctx context.Context,
	key string,
	obj interface{},
	exp time.Duration,
) ([]byte, error) {
	b, err := msgpack.Marshal(obj)
	if err != nil {
		return nil, err
	}

	if cd.localCache != nil {
		cd.localCache.Set([]byte(key), b)
	}

	if cd.Redis == nil {
		if cd.localCache == nil {
			return nil, errRedisLocalCacheNil
		}
		return b, nil
	}

	return b, cd.Redis.Set(key, b, exp).Err()
}

// Exists reports whether object for the given key exists.
func (cd *Codec) Exists(ctx context.Context, key string) bool {
	return cd.Get(ctx, key, nil) == nil
}

// Get gets the object for the given key.
func (cd *Codec) Get(ctx context.Context, key string, object interface{}) error {
	return cd.get(ctx, key, object)
}

func (cd *Codec) get(
	ctx context.Context,
	key string,
	object interface{},
) error {
	b, err := cd.getBytes(key)
	if err != nil {
		return err
	}

	if object == nil || len(b) == 0 {
		return nil
	}

	return msgpack.Unmarshal(b, object)
}

func (cd *Codec) getBytes(key string) ([]byte, error) {
	if cd.localCache != nil {
		b, ok := cd.localCache.HasGet(nil, []byte(key))
		if ok {
			atomic.AddUint64(&cd.localHits, 1)
			return b, nil
		}

		atomic.AddUint64(&cd.localMisses, 1)
	}

	if cd.Redis == nil {
		if cd.localCache == nil {
			return nil, errRedisLocalCacheNil
		}
		return nil, ErrCacheMiss
	}

	b, err := cd.Redis.Get(key).Bytes()
	if err != nil {
		atomic.AddUint64(&cd.misses, 1)
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		return nil, err
	}

	atomic.AddUint64(&cd.hits, 1)

	if cd.localCache != nil {
		cd.localCache.Set([]byte(key), b)
	}
	return b, nil
}

// Once gets the item.Object for the given item.Key from the cache or
// executes, caches, and returns the results of the given item.Func,
// making sure that only one execution is in-flight for a given item.Key
// at a time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
func (cd *Codec) Once(item *Item) error {
	b, cached, err := cd.getSetItemBytesOnce(item)
	if err != nil {
		return err
	}

	if item.Object == nil || len(b) == 0 {
		return nil
	}

	if err := msgpack.Unmarshal(b, item.Object); err != nil {
		if cached {
			_ = cd.Delete(item.Context(), item.Key)
			return cd.Once(item)
		}
		return err
	}

	return nil
}

func (cd *Codec) getSetItemBytesOnce(
	item *Item,
) (b []byte, cached bool, err error) {
	if cd.localCache != nil && cd.localCache.Has([]byte(item.Key)) {
		b, ok := cd.localCache.HasGet(nil, []byte(item.Key))
		if ok {
			atomic.AddUint64(&cd.localHits, 1)
			return b, true, nil
		}

		atomic.AddUint64(&cd.localMisses, 1)
	}

	v, err := cd.group.Do(item.Key, func() (interface{}, error) {
		b, err := cd.getBytes(item.Key)
		if err == nil {
			cached = true
			return b, nil
		}

		obj, err := item.Func()
		if err != nil {
			return nil, err
		}

		return cd.set(item.Context(), item.Key, obj, item.exp())
	})
	if err != nil {
		return nil, false, err
	}
	return v.([]byte), cached, nil
}

func (cd *Codec) Delete(ctx context.Context, key string) error {
	if cd.localCache != nil {
		cd.localCache.Del([]byte(key))
	}

	if cd.Redis == nil {
		if cd.localCache == nil {
			return errRedisLocalCacheNil
		}
		return nil
	}

	deleted, err := cd.Redis.Del(key).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrCacheMiss
	}
	return nil
}

//------------------------------------------------------------------------------

type Stats struct {
	Hits        uint64
	Misses      uint64
	LocalHits   uint64
	LocalMisses uint64
}

// Stats returns cache statistics.
func (cd *Codec) Stats() *Stats {
	stats := Stats{
		Hits:   atomic.LoadUint64(&cd.hits),
		Misses: atomic.LoadUint64(&cd.misses),
	}
	if cd.localCache != nil {
		stats.LocalHits = atomic.LoadUint64(&cd.localHits)
		stats.LocalMisses = atomic.LoadUint64(&cd.localMisses)
	}
	return &stats
}
