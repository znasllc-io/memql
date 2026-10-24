//go:build computeruse

package worker

// capabilitiesForBuildTagImpl on the computeruse build advertises HEADLESS
// + computer-use. The cockpit's actual ability to deliver computer-use tools depends
// on TCC permissions / X11 display reachability at register time,
// which the wizard probes; this list is just what the binary is
// CAPABLE of when permissions allow.
func capabilitiesForBuildTagImpl() []string {
	return []string{"HEADLESS", "COMPUTERUSE"}
}

// BuildHasComputerUse reports whether this binary was built with the computeruse
// tag. The wizard uses this to decide whether to run the TCC
// pre-flight.
func BuildHasComputerUse() bool { return true }
