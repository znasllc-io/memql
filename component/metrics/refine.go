package metrics

import "github.com/prometheus/client_golang/prometheus"

// refine.go -- the rows a query's `refine` clause keeps and drops (epic
// memql#5363, task memql#5366).
//
// `refine row => <expr>` is the one construct that evaluates an expression in
// process over rows: the page `paginate` already read from SQL is filtered by
// EvalExpr, so a query may return fewer rows than its page size. That is a
// deliberate trade, and its cost is exactly the rows it throws away -- a page
// read from the database to be discarded. So the count is the signal: a refine
// that drops most of every page is a condition that belongs in the filter,
// where the database would not have read those rows at all.

var queryRefineRows = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "query",
		Name:      "refine_rows_total",
		Help: "Rows a query's refine clause (an in-process predicate over the SQL page, after paginate) kept and dropped, " +
			"by query construct and outcome (kept|dropped). Written by whichever node runs the query. The dropped share " +
			"is the page the database read only to have it discarded: a query whose dropped rate stays near its kept " +
			"rate is filtering in process what its filter could push down, and a page of all-dropped rows reads as a " +
			"short page to the caller.",
	},
	[]string{"query", "outcome"},
)

func init() {
	registry.MustRegister(queryRefineRows)
}

// QueryRefineRows records one refine pass over a page. An empty query name
// folds into AdhocQueryLabel, for the cardinality reason that constant gives.
func QueryRefineRows(query string, kept, dropped int) {
	if query == "" {
		query = AdhocQueryLabel
	}
	if kept > 0 {
		queryRefineRows.WithLabelValues(query, "kept").Add(float64(kept))
	}
	if dropped > 0 {
		queryRefineRows.WithLabelValues(query, "dropped").Add(float64(dropped))
	}
}

// QueryRefineRowsValue returns the count for one (query, outcome) pair, for tests.
func QueryRefineRowsValue(query, outcome string) float64 {
	if query == "" {
		query = AdhocQueryLabel
	}
	c, err := queryRefineRows.GetMetricWithLabelValues(query, outcome)
	if err != nil {
		return 0
	}
	return counterValue(c)
}
