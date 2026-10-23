//go:build !computeruse

package worker

import "fmt"

// runSetupWizard on the default (non-computeruse) build prints the
// guidance message that this binary cannot enable computer-use capability.
// The computeruse-tagged sibling (setup_computeruse.go) drives the real wizard.
func runSetupWizard() error {
	fmt.Println("memql worker setup (headless build)")
	fmt.Println()
	fmt.Println("This binary was built without the computeruse tag, so it can")
	fmt.Println("only register HEADLESS capability. There is nothing to")
	fmt.Println("set up beyond enrolling a home in ~/.memql/workers.yaml with a token.")
	fmt.Println()
	fmt.Println("To enable computer-use tools (mouse / keyboard / screenshot),")
	fmt.Println("install the computeruse build of memql:")
	fmt.Println()
	fmt.Println("  make cockpit-computeruse          (host build)")
	fmt.Println("  make cockpit-computeruse-darwin-arm64  (cross-platform)")
	fmt.Println()
	fmt.Println("Then run `memql worker setup` for the")
	fmt.Println("interactive macOS TCC / Linux X11 permissions wizard.")
	return nil
}
