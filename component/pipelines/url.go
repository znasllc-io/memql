package pipelines

import (
	"net/url"
	"strings"
)

// RunPageURL is the check run's details link: the run's page in MemQL OS,
// which the OS opens from the pipelineRun query parameter at boot. osOrigin is
// the cluster's OS origin ("https://os.<domain>"); the domain is a value the
// caller derives, never a literal here.
func RunPageURL(osOrigin, runID string) string {
	return strings.TrimRight(osOrigin, "/") + "/?pipelineRun=" + url.QueryEscape(runID)
}

// CheckRunName is the required PR/merge-candidate context. Other events add
// a suffix in the check-run composer and cannot satisfy this ruleset check.
func CheckRunName(pipelineName string) string {
	return "MemQL / " + pipelineName
}

// IndependentReviewCheckName is reserved for native, exact-proposal review
// evidence. Ordinary package pipelines must never publish this context.
const IndependentReviewCheckName = "MemQL / review"

// ReservedPipelineCheckName rejects case and surrounding-space variations as
// well, so a stored pipeline cannot impersonate independent review evidence.
func ReservedPipelineCheckName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(CheckRunName(strings.TrimSpace(name))), IndependentReviewCheckName)
}
