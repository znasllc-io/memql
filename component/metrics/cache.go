package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Cache observability (memql#4532).
//
// Before this file, "is caching working?" was unanswerable without
// grepping pod logs: all three caches emitted a 5-minute aggregate Info
// line and nothing else, and the per-query `cached` bool the engine
// stamps on every query-executed event was consumed by nobody. An
// operator could not name a query and get its hit ratio, could not see
// whether invalidations were firing, and could not tell an empty cache
// from a working one.
//
// EXPORTED, NOT RE-COUNTED. Every cache here already maintains its own
// counters -- Ristretto's built-in Metrics for the result cache
// (enabled since the phase-0 instrumentation), atomic counters for the
// two AI caches -- and each exposes a cheap Stats() snapshot. Rather
// than incrementing a Prometheus counter beside every existing
// increment (two sources of truth that drift the first time someone
// adds an early return), a cache REGISTERS its snapshot function here
// and the collector reads it at scrape time. The numbers Prometheus
// reports and the numbers the log emitter reports are then the same
// numbers by construction.
//
// The one genuine exception is invalidation: the cache.invalidate.*
// eviction path is not a Ristretto counter (Ristretto sees a Del, not a
// reason), so ResultCacheInvalidationEviction is a real counter,
// incremented where the eviction happens. It is not double-counting --
// those keys also appear in Ristretto's KeysEvicted, but only this
// series says WHY.

// Cache name label values for the AI cache family. Closed set: a label
// value is an alert dimension, so it is these two and never a free
// string.
const (
	AICacheResponse = "response" // exact-match AI response cache
	AICacheSemantic = "semantic" // embedding-similarity AI cache
)

// AdhocQueryLabel is the bucket every non-named read folds into on the
// per-query series.
//
// CARDINALITY IS THE WHOLE REASON THIS CONSTANT EXISTS. The per-query
// label is bounded ONLY because it carries the name of a registered
// query construct -- a corpus of a few hundred names that changes when
// somebody edits the DSL tree, which is exactly the shape Prometheus
// labels are for. Raw query TEXT is unbounded (every distinct argument
// value would mint a new series, and a query text can carry a user id
// or an email), so an ad-hoc read must never reach the label. Callers
// pass "" for a read that resolved to no named construct and the
// recorder folds it here.
const AdhocQueryLabel = "<adhoc>"

// CacheStats is a point-in-time snapshot of one cache's counters, in
// the shape every MemQL cache already reports. Counters are monotonic
// since process start, which is what lets them be exported as
// Prometheus counters directly.
type CacheStats struct {
	Hits        uint64
	Misses      uint64
	KeysAdded   uint64
	KeysEvicted uint64
	CostAdded   uint64
	CostEvicted uint64
}

var (
	// Result cache: one process-wide instance, so no label.
	resultCacheDescs = newCacheDescs("result_cache", nil)

	// AI caches: two instances, distinguished by the `cache` label.
	aiCacheDescs = newCacheDescs("ai_cache", []string{"cache"})

	cacheSourcesMu sync.RWMutex
	// resultCacheSource is the engine result cache's snapshot function.
	resultCacheSource func() CacheStats
	// aiCacheSources maps an AICache* label value to its snapshot function.
	aiCacheSources = map[string]func() CacheStats{}

	resultCacheInvalidationEvictions = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "result_cache",
		Name:      "invalidation_evictions_total",
		Help:      "Cached query results dropped because a write to a concept they depend on invalidated them (the cache.invalidate.* path). Distinct from keys_evicted_total, which also counts Ristretto's own TTL/cost eviction.",
	})

	resultCacheInvalidationEvents = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "result_cache",
		Name:      "invalidation_events_total",
		Help:      "cache.invalidate.<concept> events this node acted on, local writes and mesh-forwarded remote writes alike. A flat line here while writes are happening means invalidation is not reaching this replica.",
	})

	resultCacheQueryReads = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "result_cache",
			Name:      "query_reads_total",
			Help:      "Cacheable query reads, labelled by the registered query construct's name and whether the read was served from cache. rate(...{cached=\"true\"}) / rate(...) is one query's hit ratio.",
		},
		[]string{"query", "cached"},
	)
)

// cacheDescs is the description set for one cache metric family. The
// families differ only in subsystem name and label set, so they share
// this shape and the collector below.
type cacheDescs struct {
	labels      []string
	hits        *prometheus.Desc
	misses      *prometheus.Desc
	keysAdded   *prometheus.Desc
	keysEvicted *prometheus.Desc
	costAdded   *prometheus.Desc
	costEvicted *prometheus.Desc
}

func newCacheDescs(subsystem string, labels []string) cacheDescs {
	fq := func(name string) string { return namespace + "_" + subsystem + "_" + name }
	return cacheDescs{
		labels:      labels,
		hits:        prometheus.NewDesc(fq("hits_total"), "Cache lookups served from cache.", labels, nil),
		misses:      prometheus.NewDesc(fq("misses_total"), "Cache lookups that found nothing usable and fell through to the underlying read.", labels, nil),
		keysAdded:   prometheus.NewDesc(fq("keys_added_total"), "Entries stored in the cache.", labels, nil),
		keysEvicted: prometheus.NewDesc(fq("keys_evicted_total"), "Entries removed from the cache for any reason (TTL, cost pressure, invalidation).", labels, nil),
		costAdded:   prometheus.NewDesc(fq("cost_added_total"), "Total admission cost of stored entries.", labels, nil),
		costEvicted: prometheus.NewDesc(fq("cost_evicted_total"), "Total admission cost of evicted entries.", labels, nil),
	}
}

func (d cacheDescs) describe(ch chan<- *prometheus.Desc) {
	ch <- d.hits
	ch <- d.misses
	ch <- d.keysAdded
	ch <- d.keysEvicted
	ch <- d.costAdded
	ch <- d.costEvicted
}

func (d cacheDescs) collect(ch chan<- prometheus.Metric, s CacheStats, labelValues ...string) {
	emit := func(desc *prometheus.Desc, v uint64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), labelValues...)
	}
	emit(d.hits, s.Hits)
	emit(d.misses, s.Misses)
	emit(d.keysAdded, s.KeysAdded)
	emit(d.keysEvicted, s.KeysEvicted)
	emit(d.costAdded, s.CostAdded)
	emit(d.costEvicted, s.CostEvicted)
}

// cacheCollector reads the registered snapshot functions at scrape
// time. A cache that has not registered (a node type that builds no
// result cache, an AI cache that is disabled) contributes NO series,
// which is the honest answer -- a hard zero would look like a cache
// that exists and is never hit.
type cacheCollector struct{}

func (cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	resultCacheDescs.describe(ch)
	aiCacheDescs.describe(ch)
}

func (cacheCollector) Collect(ch chan<- prometheus.Metric) {
	cacheSourcesMu.RLock()
	result := resultCacheSource
	ai := make(map[string]func() CacheStats, len(aiCacheSources))
	for name, fn := range aiCacheSources {
		ai[name] = fn
	}
	cacheSourcesMu.RUnlock()

	if result != nil {
		resultCacheDescs.collect(ch, result())
	}
	for name, fn := range ai {
		if fn != nil {
			aiCacheDescs.collect(ch, fn(), name)
		}
	}
}

// RegisterResultCacheStatsSource wires the engine result cache's
// snapshot function into the /metrics scrape. Called once at engine
// construction; a nil fn unregisters (used by tests).
func RegisterResultCacheStatsSource(fn func() CacheStats) {
	cacheSourcesMu.Lock()
	resultCacheSource = fn
	cacheSourcesMu.Unlock()
}

// RegisterAICacheStatsSource wires one AI cache's snapshot function
// into the /metrics scrape under the given cache label (AICacheResponse
// / AICacheSemantic).
func RegisterAICacheStatsSource(cache string, fn func() CacheStats) {
	if cache == "" {
		return
	}
	cacheSourcesMu.Lock()
	if fn == nil {
		delete(aiCacheSources, cache)
	} else {
		aiCacheSources[cache] = fn
	}
	cacheSourcesMu.Unlock()
}

// ResultCacheInvalidationEviction records one cache.invalidate event
// and the number of cached results it dropped. keys may be 0 -- an
// event that evicted nothing still proves invalidation is REACHING this
// replica, which is the question the events series answers and the
// evictions series cannot.
func ResultCacheInvalidationEviction(keys int) {
	resultCacheInvalidationEvents.Inc()
	if keys > 0 {
		resultCacheInvalidationEvictions.Add(float64(keys))
	}
}

// ResultCacheQueryRead records one cacheable read of a named query
// construct and whether it was served from cache. An empty query name
// folds into AdhocQueryLabel; see that constant for why the label can
// never carry raw query text.
func ResultCacheQueryRead(query string, cached bool) {
	if query == "" {
		query = AdhocQueryLabel
	}
	hit := "false"
	if cached {
		hit = "true"
	}
	resultCacheQueryReads.WithLabelValues(query, hit).Inc()
}

// ResultCacheInvalidationValues returns the (events, evictions) counts, for tests.
func ResultCacheInvalidationValues() (events, evictions float64) {
	return counterValue(resultCacheInvalidationEvents), counterValue(resultCacheInvalidationEvictions)
}

// ResultCacheQueryReadValue returns the count for one (query, cached) pair, for tests.
func ResultCacheQueryReadValue(query string, cached bool) float64 {
	if query == "" {
		query = AdhocQueryLabel
	}
	hit := "false"
	if cached {
		hit = "true"
	}
	c, err := resultCacheQueryReads.GetMetricWithLabelValues(query, hit)
	if err != nil {
		return 0
	}
	return counterValue(c)
}

func counterValue(c prometheus.Metric) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
