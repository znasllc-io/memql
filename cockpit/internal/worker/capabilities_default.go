//go:build !computeruse

package worker

// capabilitiesForBuildTagImpl returns the cockpit-cockpit-worker's
// advertised capability set on the headless build. HEADLESS is
// mandatory; computer-use is added only by the computeruse-tagged sibling.
func capabilitiesForBuildTagImpl() []string {
	return []string{"HEADLESS"}
}

// BuildHasComputerUse reports whether this binary was built with the computeruse
// tag. The wizard uses this to decide whether to run the TCC
// pre-flight (no-op on headless builds since there's nothing to
// probe).
func BuildHasComputerUse() bool { return false }
