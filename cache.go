package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/go-redis/redis/v7"
	"github.com/vmihailenco/bufpool"
	"go4.org/syncutil/singleflight"
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

	Key   string
	Value interface{}

	// TTL is the cache expiration time.
	// Default TTL is 1 hour.
	TTL time.Duration

	// Func returns value to be cached.
	Func func() (interface{}, error)
}

func (item *Item) Context() context.Context {
	if item.Ctx == nil {
		return context.Background()
	}
	return item.Ctx
}

func (item *Item) value() (interface{}, error) {
	if item.Value != nil {
		return item.Value, nil
	}
	if item.Func != nil {
		return item.Func()
	}
	return nil, nil
}

func (item *Item) exp() time.Duration {
	if item.TTL < 0 {
		return 0
	}
	if item.TTL < time.Second {
		return time.Hour
	}
	return item.TTL
}

//------------------------------------------------------------------------------

type Options struct {
	Redis rediser

	LocalCache    *fastcache.Cache
	LocalCacheTTL time.Duration

	StatsEnabled bool
}

func (opt *Options) init() {
	switch opt.LocalCacheTTL {
	case -1:
		opt.LocalCacheTTL = 0
	case 0:
		opt.LocalCacheTTL = time.Minute
	}
}

type Cache struct {
	opt *Options

	pool  bufpool.Pool
	group singleflight.Group

	hits   uint64
	misses uint64
}

func New(opt *Options) *Cache {
	opt.init()
	return &Cache{
		opt: opt,
	}
}

// Set caches the item.
func (cd *Cache) Set(item *Item) error {
	value, err := item.value()
	if err != nil {
		return err
	}

	buf := cd.pool.Get()
	_, err = cd.set(item.Context(), item.Key, value, item.exp(), buf)
	cd.pool.Put(buf)
	return err
}

func (cd *Cache) set(
	ctx context.Context,
	key string,
	value interface{},
	exp time.Duration,
	buf *bufpool.Buffer,
) ([]byte, error) {
	b, err := marshal(buf, value)
	if err != nil {
		return nil, err
	}

	if cd.opt.LocalCache != nil {
		cd.localSet(key, b)
	}

	if cd.opt.Redis == nil {
		if cd.opt.LocalCache == nil {
			return nil, errRedisLocalCacheNil
		}
		return b, nil
	}

	return b, cd.opt.Redis.Set(key, b, exp).Err()
}

// Exists reports whether value for the given key exists.
func (cd *Cache) Exists(ctx context.Context, key string) bool {
	return cd.Get(ctx, key, nil) == nil
}

// Get gets the value for the given key.
func (cd *Cache) Get(ctx context.Context, key string, value interface{}) error {
	return cd.get(ctx, key, value)
}

func (cd *Cache) get(
	ctx context.Context,
	key string,
	value interface{},
) error {
	b, err := cd.getBytes(key)
	if err != nil {
		return err
	}

	if value == nil || len(b) == 0 {
		return nil
	}

	return unmarshal(b, value)
}

func (cd *Cache) getBytes(key string) ([]byte, error) {
	if cd.opt.LocalCache != nil {
		b, ok := cd.localGet(key)
		if ok {
			return b, nil
		}
	}

	if cd.opt.Redis == nil {
		if cd.opt.LocalCache == nil {
			return nil, errRedisLocalCacheNil
		}
		return nil, ErrCacheMiss
	}

	b, err := cd.opt.Redis.Get(key).Bytes()
	if err != nil {
		if cd.opt.StatsEnabled {
			atomic.AddUint64(&cd.misses, 1)
		}
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		return nil, err
	}

	if cd.opt.StatsEnabled {
		atomic.AddUint64(&cd.hits, 1)
	}

	if cd.opt.LocalCache != nil {
		cd.localSet(key, b)
	}
	return b, nil
}

// Once gets the item.Value for the given item.Key from the cache or
// executes, caches, and returns the results of the given item.Func,
// making sure that only one execution is in-flight for a given item.Key
// at a time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
func (cd *Cache) Once(item *Item) error {
	b, cached, err := cd.getSetItemBytesOnce(item)
	if err != nil {
		return err
	}

	if item.Value == nil || len(b) == 0 {
		return nil
	}

	if err := unmarshal(b, item.Value); err != nil {
		if cached {
			_ = cd.Delete(item.Context(), item.Key)
			return cd.Once(item)
		}
		return err
	}

	return nil
}

func (cd *Cache) getSetItemBytesOnce(item *Item) (b []byte, cached bool, err error) {
	if cd.opt.LocalCache != nil {
		b, ok := cd.localGet(item.Key)
		if ok {
			return b, true, nil
		}
	}

	v, err := cd.group.Do(item.Key, func() (interface{}, error) {
		b, err := cd.getBytes(item.Key)
		if err == nil {
			cached = true
			return b, nil
		}

		value, err := item.Func()
		if err != nil {
			return nil, err
		}

		buf := cd.pool.Get()
		b, err = cd.set(item.Context(), item.Key, value, item.exp(), buf)
		if err != nil {
			return nil, err
		}

		cd.pool.UpdateLen(buf.Len())
		return b, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.([]byte), cached, nil
}

func (cd *Cache) Delete(ctx context.Context, key string) error {
	if cd.opt.LocalCache != nil {
		cd.opt.LocalCache.Del([]byte(key))
	}

	if cd.opt.Redis == nil {
		if cd.opt.LocalCache == nil {
			return errRedisLocalCacheNil
		}
		return nil
	}

	deleted, err := cd.opt.Redis.Del(key).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrCacheMiss
	}
	return nil
}

func (cd *Cache) localSet(key string, b []byte) {
	if cd.opt.LocalCacheTTL > 0 {
		pos := len(b)
		b = append(b, make([]byte, 4)...)
		encodeTime(b[pos:], time.Now())
	}

	cd.opt.LocalCache.Set([]byte(key), b)
}

func (cd *Cache) localGet(key string) ([]byte, bool) {
	b, ok := cd.opt.LocalCache.HasGet(nil, []byte(key))
	if !ok {
		return b, false
	}

	if len(b) == 0 || cd.opt.LocalCacheTTL == 0 {
		return b, true
	}
	if len(b) <= 4 {
		panic("not reached")
	}

	tm := decodeTime(b[len(b)-4:])
	if time.Since(tm) > cd.opt.LocalCacheTTL {
		return nil, false
	}

	return b[:len(b)-4], true
}

//------------------------------------------------------------------------------

type Stats struct {
	Hits   uint64
	Misses uint64
}

// Stats returns cache statistics.
func (cd *Cache) Stats() *Stats {
	if !cd.opt.StatsEnabled {
		return nil
	}
	return &Stats{
		Hits:   atomic.LoadUint64(&cd.hits),
		Misses: atomic.LoadUint64(&cd.misses),
	}
}
