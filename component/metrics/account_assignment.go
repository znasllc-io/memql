package metrics

import "github.com/prometheus/client_golang/prometheus"

// Account assignment outcomes, the closed label set of
// memql_account_assignment_rows_total. One constant per value so a caller
// cannot mint a fifth series by a typo.
const (
	AccountAssignmentAssigned = "assigned"
	AccountAssignmentKept     = "kept"
	AccountAssignmentShared   = "shared"
	AccountAssignmentConflict = "conflict"
)

var accountAssignmentRows = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "account_assignment_rows_total",
	Help: "Rows the background assignment of existing rows to accounts has judged, by outcome (component/memql/account_assignment.go). " +
		"assigned: the newest version of a row that had no accountId was given one, in place. kept: the row already had an account and was left alone. " +
		"shared: a row of a concept declared @belongsTo(account, shared) that nothing placed, left cluster-provided on purpose. " +
		"conflict: a row the job will not guess at (a store bound by sites of two accounts, an update the Library retirement fence refused), " +
		"recorded in account_assignment_conflict and left without an account. A row waiting for its parent to be assigned counts under none of them. " +
		"Written only by nodes on which account stamping is on and not held; a first pass over a large database moves assigned at about 500 rows per five seconds per node, and then settles to the rate new unstamped rows appear, which is zero once every node stamps.",
}, []string{"outcome"})

// AccountAssignmentRows adds n rows to one outcome. n <= 0 records nothing.
func AccountAssignmentRows(outcome string, n int) {
	if n <= 0 {
		return
	}
	accountAssignmentRows.WithLabelValues(outcome).Add(float64(n))
}

// AccountAssignmentRowsValue reads one outcome's counter, for tests.
func AccountAssignmentRowsValue(outcome string) float64 {
	return counterValue(accountAssignmentRows.WithLabelValues(outcome))
}
