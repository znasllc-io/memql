package pipelinesteps

import (
	"fmt"
	"strings"
)

// The operator labels and taints build nodes with the same key/value. A
// repository never chooses node labels or broad tolerations. This one exact
// NoSchedule toleration cannot admit steps to unrelated reserved pools.
const pipelinePoolLabel = "memql.io/pipeline-pool"

// ValidatePlacement refuses malformed operator placement before any effects.
func (c Config) ValidatePlacement() error {
	if c.NodePool != "" && (len(c.NodePool) > 63 || !dnsLabelShape.MatchString(c.NodePool)) {
		return fmt.Errorf("%s must be a lowercase DNS label of at most 63 characters", envNodePool)
	}
	return nil
}

func (c Config) nodeSelector(platform string) map[string]string {
	labels := stepNodeSelector(platform)
	if c.NodePool != "" {
		labels[pipelinePoolLabel] = c.NodePool
	}
	return labels
}

func (c Config) tolerations() []Toleration {
	if c.NodePool == "" {
		return nil
	}
	return []Toleration{{Key: pipelinePoolLabel, Operator: "Equal", Value: c.NodePool, Effect: "NoSchedule"}}
}

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
