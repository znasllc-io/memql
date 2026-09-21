package metrics

import "github.com/prometheus/client_golang/prometheus"

// The DSL's deprecated forms (memql#5390, D22).
//
// One counter, one series per rule: the uses of a deprecated language form each
// load of the DSL tree found. D22 asks that deprecated use be COUNTED so that
// removing a form is a decision taken on evidence -- "nobody has written this
// in six months" and "this fires on every boot in the fleet" are different
// answers, and a load-time warning in a log nobody reads cannot tell them
// apart.
//
// The rule label is bounded for the same reason the automation label is: it
// names a registered form (component/language/deprecation), a closed set that
// changes only when the language does, never a value from a request. This
// package cannot import the language module, so it does not know the set: the
// engine passes it, adding each registered rule's uses -- zero included, which
// creates the series -- on every load of the tree
// (component/memql/deprecated_uses.go). Every node type loads the tree in its
// engine phase, before it serves /metrics, so no scrape sees a registered rule
// missing: an alert over a series that does not exist evaluates to no data,
// which reads the same as "no use".

var dslDeprecatedUses = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "dsl",
	Name:      "deprecated_uses_total",
	Help:      "EVERY NODE TYPE writes this series: uses of a deprecated MemQL language form found in the DSL this node loaded, by rule, added once per load of the tree (engine Init). A deprecated form still loads and logs a WARN naming what to write instead and the memqlmigrate rewrite that writes it; it is refused from the release its warning names. Every registered rule exists at 0 from boot. A series that keeps rising names a form the loaded DSL -- the engine's own tree or a mounted product bundle -- still uses: run the rewrite before the release that refuses it. A flat zero across the fleet is the evidence that a form can be refused.",
}, []string{"rule"})

// DSLDeprecatedUse records n uses of the deprecated form rule found by one load
// of the DSL tree. n == 0 creates the rule's series at zero without counting,
// which is how every registered rule is scraped from boot; a negative n is
// ignored, since a counter only rises.
func DSLDeprecatedUse(rule string, n int) {
	if n < 0 {
		return
	}
	dslDeprecatedUses.WithLabelValues(rule).Add(float64(n))
}

// DSLDeprecatedUsesValue returns the current count for one rule, for tests.
func DSLDeprecatedUsesValue(rule string) float64 {
	return counterValue(dslDeprecatedUses.WithLabelValues(rule))
}
