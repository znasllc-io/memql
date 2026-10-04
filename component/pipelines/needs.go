package pipelines

import "sort"

// The closed set of environment needs a step may name (D8). It is the
// workbench's environment hint (integrations/workbench, EnvironmentNeeds)
// plus docker, which the substrate adds there too; a parity test in the
// integrations module holds the two lists together. An unknown need is a
// typed refusal, never a silent fallback to somebody's laptop.
const (
	NeedDisplay      = "display"
	NeedDocker       = "docker"
	NeedGPU          = "gpu"
	NeedMacOSTooling = "macos_tooling"
	NeedUserFiles    = "user_files"
)

var knownNeeds = map[string]bool{
	NeedDisplay:      true,
	NeedDocker:       true,
	NeedGPU:          true,
	NeedMacOSTooling: true,
	NeedUserFiles:    true,
}

// Needs is the closed set, sorted.
func Needs() []string {
	out := make([]string, 0, len(knownNeeds))
	for name := range knownNeeds {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// IsNeed reports whether name is in the closed set.
func IsNeed(name string) bool { return knownNeeds[name] }
