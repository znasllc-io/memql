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

// CheckRunName is the name a pipeline's check run carries on GitHub, and so
// the name a ruleset requires.
func CheckRunName(pipelineName string) string {
	return "MemQL / " + pipelineName
}
