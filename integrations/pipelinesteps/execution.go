package pipelinesteps

import "strings"

// An unspecified container architecture may use either Linux architecture;
// an explicit target must survive into Kubernetes scheduling, just as it does
// into the worker's live runtime check. No host can reinterpret that target.
func stepNodeSelector(platform string) map[string]string {
	labels := map[string]string{"kubernetes.io/os": "linux"}
	if _, arch, ok := strings.Cut(platform, "/"); ok {
		labels["kubernetes.io/arch"] = arch
	}
	return labels
}
