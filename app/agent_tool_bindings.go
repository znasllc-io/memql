package app

import "strings"

// The harness owns these short, provider-facing aliases. Resolve both their
// schemas and their calls against the same explicit engine namespace. A
// product bundle can declare a namesake without removing an execution surface
// (or its approval gate) from Ask. The general registry remains ambiguity-safe.
func agentToolBinding(name string) string {
	name = strings.TrimSpace(name)
	switch name {
	case "workerStatus", "requestComputerUseScope", "workerHost", "workerComputer":
		return "worker." + name
	case "workbenchHost":
		return "workbench." + name
	default:
		return name
	}
}
