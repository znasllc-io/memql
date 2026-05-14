package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"

	databaseMemoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/core/env"
	"github.com/visionarys-io/memql/core/logger"
)

type (
	Cache struct {
		cache       *ristretto.Cache
		ttl         time.Duration
		listTTL     time.Duration
		logger      *slog.Logger
		maxCost     int64
		numCounters int64
		bufferItems int64

		mu        sync.RWMutex
		listKeys  map[string]map[string]struct{}
		fallback  map[string]cacheEntry
		lifecycle common.Lifecycle
		readyCh   chan struct{}
	}

	CacheOption func(*cacheConfig)

	cacheConfig struct {
		numCounters int64
		maxCost     int64
		bufferItems int64
		metrics     bool
		listTTL     time.Duration
	}
)

const (
	ComponentName = common.ComponentName("cache")
)

func WithNumCounters(numCounters int64) CacheOption {
	return func(cfg *cacheConfig) {
		if numCounters > 0 {
			cfg.numCounters = numCounters
		}
	}
}

func WithMaxCost(maxCost int64) CacheOption {
	return func(cfg *cacheConfig) {
		if maxCost > 0 {
			cfg.maxCost = maxCost
		}
	}
}

func WithBufferItems(bufferItems int64) CacheOption {
	return func(cfg *cacheConfig) {
		if bufferItems > 0 {
			cfg.bufferItems = bufferItems
		}
	}
}

func WithMetrics(enabled bool) CacheOption {
	return func(cfg *cacheConfig) {
		cfg.metrics = enabled
	}
}

func WithListTTL(ttl time.Duration) CacheOption {
	return func(cfg *cacheConfig) {
		if ttl > 0 {
			cfg.listTTL = ttl
		}
	}
}

func New(maxItems int64, ttl time.Duration, opts ...CacheOption) (*Cache, error) {
	if maxItems <= 0 {
		return nil, fmt.Errorf("maxItems must be greater than zero")
	}

	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be greater than zero")
	}

	cfg := cacheConfig{
		numCounters: max(maxItems*10, 1),
		maxCost:     max(maxItems, 1),
		bufferItems: 64,
		metrics:     false,
		listTTL:     0,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.listTTL <= 0 {
		cfg.listTTL = ttl
	}

	ristrettoCache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: cfg.numCounters,
		MaxCost:     cfg.maxCost,
		BufferItems: cfg.bufferItems,
		Metrics:     cfg.metrics,
	})

	if err != nil {
		return nil, fmt.Errorf("create ristretto cache: %w", err)
	}

	loggerLevel := resolveCacheLoggerLevel()

	memoryCache := &Cache{
		cache:       ristrettoCache,
		ttl:         ttl,
		listTTL:     cfg.listTTL,
		logger:      logger.New(ComponentName, os.Stdout, loggerLevel),
		maxCost:     cfg.maxCost,
		numCounters: cfg.numCounters,
		bufferItems: cfg.bufferItems,
		listKeys:    make(map[string]map[string]struct{}),
		fallback:    make(map[string]cacheEntry),
		readyCh:     make(chan struct{}),
	}

	memoryCache.logger.Info("instance created")

	return memoryCache, nil
}

func (c *Cache) Start(ctx context.Context) {
	if c == nil {
		return
	}

	if err := c.lifecycle.Start(ctx, c.logger, common.LifecycleHooks{
		Run: func(runCtx context.Context, markStarted func()) error {
			return c.run(runCtx, markStarted)
		},
		OnStarted: func() {
			select {
			case <-c.readyCh:
			default:
				close(c.readyCh)
			}
		},
		OnStop: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.listKeys = make(map[string]map[string]struct{})
			c.fallback = make(map[string]cacheEntry)
		},
	}); err != nil && !errors.Is(err, common.ErrLifecycleAlreadyRunning) {
		c.logger.Error("failed to start", "error", err)
	}
}

func (c *Cache) Close() {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.Close()
}

func (c *Cache) Wait() {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.Wait()
}

func (c *Cache) Get(id string) (*databaseMemoryNodes.MemoryNode, bool) {
	if c == nil || c.cache == nil || id == "" {
		return nil, false
	}

	key := c.nodeKey(id)
	if node, ok := c.getFallbackNode(key); ok {
		return node, true
	}

	if value, ok := c.cache.Get(key); ok {
		if node, valid := value.(*databaseMemoryNodes.MemoryNode); valid && node != nil {
			cloned := cloneMemoryNode(node)
			return &cloned, true
		}
	}

	return nil, false
}

func (c *Cache) Set(node *databaseMemoryNodes.MemoryNode) bool {
	if c == nil || c.cache == nil || node == nil || node.ID == "" {
		return false
	}

	key := c.nodeKey(node.ID)
	cloned := cloneMemoryNode(node)
	stored := c.cache.SetWithTTL(key, &cloned, 1, c.ttl)
	c.cache.Wait()
	c.storeFallback(key, cloned, c.ttl)
	return stored
}

func (c *Cache) Delete(id string) {
	if c == nil || c.cache == nil || id == "" {
		return
	}
	key := c.nodeKey(id)
	c.cache.Del(key)
	c.removeFallback(key)
}

func (c *Cache) GetList(createdBy string, limit int) ([]databaseMemoryNodes.MemoryNode, bool) {
	if c == nil || c.cache == nil {
		return nil, false
	}

	key := c.listKey(createdBy, limit)
	if key == "" {
		return nil, false
	}

	if nodes, ok := c.getFallbackList(key); ok {
		return nodes, true
	}

	if value, ok := c.cache.Get(key); ok {
		if nodes, valid := value.([]databaseMemoryNodes.MemoryNode); valid {
			return cloneMemoryNodes(nodes), true
		}
	}

	return nil, false
}

func (c *Cache) SetList(createdBy string, limit int, nodes []databaseMemoryNodes.MemoryNode) bool {
	if c == nil || c.cache == nil {
		return false
	}

	if limit <= 0 {
		return false
	}

	key := c.listKey(createdBy, limit)
	if key == "" {
		return false
	}

	cloned := cloneMemoryNodes(nodes)
	cost := int64(len(cloned))
	if cost <= 0 {
		cost = 1
	}

	stored := c.cache.SetWithTTL(key, cloned, cost, c.listTTL)
	c.cache.Wait()
	c.storeFallback(key, cloned, c.listTTL)
	if stored {
		c.registerListKey(createdBy, key)
	}
	return stored
}

func (c *Cache) DeleteList(createdBy string, limit int) {
	if c == nil || c.cache == nil {
		return
	}

	key := c.listKey(createdBy, limit)
	if key == "" {
		return
	}

	c.cache.Del(key)
	c.unregisterListKey(createdBy, key)
	c.removeFallback(key)
}

func (c *Cache) InvalidateListsFor(createdBy string) {
	if c == nil || c.cache == nil {
		return
	}

	c.mu.Lock()
	keys := c.listKeys[createdBy]
	delete(c.listKeys, createdBy)
	c.mu.Unlock()

	for key := range keys {
		c.cache.Del(key)
		c.removeFallback(key)
	}
}

func (c *Cache) Metrics() *ristretto.Metrics {
	if c == nil || c.cache == nil {
		return nil
	}
	return c.cache.Metrics
}

func (c *Cache) Stop(ctx context.Context) {
	if c == nil {
		return
	}

	if err := c.lifecycle.Stop(ctx, c.logger); err != nil && !errors.Is(err, common.ErrLifecycleNotRunning) {
		c.logger.Error("failed to stop", "error", err)
	}
}

func (c *Cache) IsRunning() bool {
	if c == nil {
		return false
	}

	return c.lifecycle.IsRunning()
}

func (c *Cache) run(ctx context.Context, markStarted func()) error {
	ctx = common.EnsureContext(ctx)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	markStarted()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Cache) Order() int {
	return 10
}

func (c *Cache) ComponentName() common.ComponentName {
	return ComponentName
}

// Ready returns a channel that is closed when the cache is ready.
func (c *Cache) Ready() <-chan struct{} {
	return c.readyCh
}

func (c *Cache) registerListKey(createdBy, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.listKeys == nil {
		c.listKeys = make(map[string]map[string]struct{})
	}

	keys := c.listKeys[createdBy]
	if keys == nil {
		keys = make(map[string]struct{})
		c.listKeys[createdBy] = keys
	}

	keys[key] = struct{}{}
}

func (c *Cache) unregisterListKey(createdBy, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.listKeys == nil {
		return
	}

	keys := c.listKeys[createdBy]
	if keys == nil {
		return
	}

	delete(keys, key)
	if len(keys) == 0 {
		delete(c.listKeys, createdBy)
	}
}

func (c *Cache) nodeKey(id string) string {
	if id == "" {
		return ""
	}
	return "MEMORY_NODE:" + id
}

func (c *Cache) listKey(createdBy string, limit int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf("LIST:%s:%d", createdBy, limit)
}

func resolveCacheLoggerLevel() slog.Level {
	reader := env.NewEnvReader("CACHE")
	if value, ok := reader.String("CAPABILITIES_LOGGING_LOG_LEVEL"); ok {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return slog.LevelInfo
		}
		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(trimmed))); err == nil {
			return level
		}
	}
	return slog.LevelInfo
}

func cloneMemoryNode(node *databaseMemoryNodes.MemoryNode) databaseMemoryNodes.MemoryNode {
	if node == nil {
		return databaseMemoryNodes.MemoryNode{}
	}

	cloned := *node
	if node.Schema != nil {
		cloned.Schema = append([]byte(nil), node.Schema...)
	}
	if node.Payload != nil {
		cloned.Payload = append([]byte(nil), node.Payload...)
	}
	return cloned
}

func cloneMemoryNodes(nodes []databaseMemoryNodes.MemoryNode) []databaseMemoryNodes.MemoryNode {
	cloned := make([]databaseMemoryNodes.MemoryNode, len(nodes))
	for i := range nodes {
		cloned[i] = cloneMemoryNode(&nodes[i])
	}
	return cloned
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

func (e cacheEntry) expired(now time.Time) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return now.After(e.expiresAt)
}

func (c *Cache) storeFallback(key string, value any, ttl time.Duration) {
	if c == nil || key == "" {
		return
	}

	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	c.mu.Lock()
	if c.fallback == nil {
		c.fallback = make(map[string]cacheEntry)
	}
	c.fallback[key] = cacheEntry{value: value, expiresAt: expiresAt}
	c.mu.Unlock()
}

func (c *Cache) removeFallback(key string) {
	if c == nil || key == "" {
		return
	}

	c.mu.Lock()
	delete(c.fallback, key)
	c.mu.Unlock()
}

func (c *Cache) fallbackEntry(key string) (cacheEntry, bool) {
	if c == nil || key == "" {
		return cacheEntry{}, false
	}

	c.mu.RLock()
	entry, ok := c.fallback[key]
	c.mu.RUnlock()
	if !ok {
		return cacheEntry{}, false
	}

	if entry.expired(time.Now()) {
		c.removeFallback(key)
		return cacheEntry{}, false
	}

	return entry, true
}

func (c *Cache) getFallbackNode(key string) (*databaseMemoryNodes.MemoryNode, bool) {
	entry, ok := c.fallbackEntry(key)
	if !ok {
		return nil, false
	}

	nodeValue, valid := entry.value.(databaseMemoryNodes.MemoryNode)
	if !valid {
		return nil, false
	}

	cloned := cloneMemoryNode(&nodeValue)
	return &cloned, true
}

func (c *Cache) getFallbackList(key string) ([]databaseMemoryNodes.MemoryNode, bool) {
	entry, ok := c.fallbackEntry(key)
	if !ok {
		return nil, false
	}

	nodesValue, valid := entry.value.([]databaseMemoryNodes.MemoryNode)
	if !valid {
		return nil, false
	}

	return cloneMemoryNodes(nodesValue), true
}
