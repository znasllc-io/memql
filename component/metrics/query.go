package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var queryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: namespace,
	Subsystem: "query",
	Name:      "duration_seconds",
	Help:      "Engine Execute latency including parse, authorization, database work and shaping, by registered construct name and outcome. Includes cache hits. Nested constructs are separate calls; do not sum their durations as CPU time. Raw queries and parse failures share <adhoc>; argument values are never labels.",
	Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
}, []string{"query", "outcome"})

// QueryDuration accepts only a registered construct's name (the resolved
// plan's SourceFunction), never query text, arguments, IDs or error strings.
func QueryDuration(name string, elapsed time.Duration, failed bool) {
	if name == "" {
		name = AdhocQueryLabel
	}
	outcome := "success"
	if failed {
		outcome = "error"
	}
	queryDuration.WithLabelValues(name, outcome).Observe(elapsed.Seconds())
}
