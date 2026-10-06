package pipelines

import "context"

// RunnerReadiness describes one workbench's observed execution prerequisites.
// It contains no credentials and does not start a Job or an isolation probe.
type RunnerReadiness struct {
	NodeID     string `json:"nodeId"`
	Namespace  string `json:"namespace,omitempty"`
	Available  bool   `json:"available"`
	Isolation  string `json:"isolation"`
	CheckedAt  string `json:"checkedAt,omitempty"`
	ValidUntil string `json:"validUntil,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// ReadinessReporter is an executor's read-only view of the remote runners.
// An executor without this capability has unknown readiness, not a pass.
type ReadinessReporter interface {
	Readiness(context.Context) []RunnerReadiness
}
