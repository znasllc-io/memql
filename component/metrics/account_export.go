package metrics

import "github.com/prometheus/client_golang/prometheus"

// Account export outcomes, the closed label set of memql_account_exports_total.
const (
	AccountExportComplete  = "complete"
	AccountExportFailed    = "failed"
	AccountExportCancelled = "cancelled"
)

var accountExports = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Name:      "account_exports_total",
	Help: "Account exports that reached a final state, by outcome (integrations/accounts/export_runner.go). " +
		"complete: the file was written, sealed and read back. failed: the run stopped with a failure code, or a run was taken over after its third attempt. " +
		"cancelled: a person cancelled it before it finished. Counted by the node that wrote the final state; a draft that was never started counts under none.",
}, []string{"outcome"})

// AccountExportFinished counts one export reaching outcome.
func AccountExportFinished(outcome string) {
	accountExports.WithLabelValues(outcome).Inc()
}

// AccountExportsValue reads one outcome's counter, for tests.
func AccountExportsValue(outcome string) float64 {
	return counterValue(accountExports.WithLabelValues(outcome))
}
