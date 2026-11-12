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

// Concepts that say nothing about which account their rows belong to (design
// record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md).
//
// A GAUGE, not a counter, because the question it answers is a state: how many
// concepts in the product DSL this node mounted declare no `@belongsTo` RIGHT
// NOW. A counter adding the same nine concepts on every load would rise with
// restarts rather than with undeclared concepts, and could never come back down
// when a bundle declares them -- which is the one event an operator watching it
// is waiting for.
//
// No label. The concept names are in the WARN line the same load writes; a
// label per concept would mint a series for every name a product bundle ever
// shipped, and the number is what an alert needs.
var dslBelongsToUndeclared = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: namespace,
	Subsystem: "dsl",
	Name:      "belongs_to_undeclared",
	Help:      "EVERY NODE TYPE writes this series: the number of concepts in the DSL this node MOUNTED -- a product bundle at MEMQL_DSL_PATH, or a pack registered into the binary; never the engine's own embedded tree, which a build gate holds to zero -- that declare no @belongsTo, set once per load of the tree (engine Init). A concept that declares nothing still loads, and its rows count as cluster-wide: nothing records which account they belong to, so an account's inventory and export do not include them. The same load logs one WARN line naming every such concept and the annotation to add: @belongsTo(account), @belongsTo(account, via=\"<field>\"), @belongsTo(account, shared), @belongsTo(account, stated) or @belongsTo(cluster). ZERO IS THE HEALTHY VALUE, and it is what a node with no product bundle reports. A non-zero value names work to do in the bundle rather than a fault in the node; it is the same on every node that mounted the same bundle, so read one. Alert on it staying above zero across a bundle release that was meant to declare them.",
})

// BelongsToUndeclared records how many mounted concepts declare no @belongsTo,
// as one load of the DSL tree found them. Zero is a real answer and is set like
// any other, so a bundle that has since declared takes the gauge back down. A
// negative n is ignored: a count cannot be one.
func BelongsToUndeclared(n int) {
	if n < 0 {
		return
	}
	dslBelongsToUndeclared.Set(float64(n))
}

// BelongsToUndeclaredValue returns the gauge's current value, for tests.
func BelongsToUndeclaredValue() float64 {
	return gaugeValue(dslBelongsToUndeclared)
}
