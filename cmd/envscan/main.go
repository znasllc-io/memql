// Command envscan is the shared env-var classifier for Epic 7
// (memql#2103). It is the single source of truth for "which env vars
// does the code read", used both by the registry audit (7.1 / #2104)
// and by the CI drift gate (7.2 / #2105) so the two can never diverge.
//
// The reusable core lives in cmd/envscan/scan; this command is a thin
// CLI wrapper over it so the drift logic is importable by the
// TestNoEnvRegistryDrift Go test (which is what makes the check a CI
// gate via the go-checks `go test ./...` lane).
//
// It detects two drift directions against the genesis manifest registry
// (scripts/secrets/manifest.yaml, the locked source of truth):
//
//   - forward drift  -- a var read by code that is NOT registered.
//   - reverse drift   -- a registered var that appears NOWHERE in the
//     repo (code / k8s / .env / dsl). Stale registry entry.
//
// Usage:
//
//	envscan -list              # print every detected read (key\tfile)
//	envscan -check             # exit non-zero on forward or reverse drift
//	envscan -unregistered      # print only reads missing from the registry
//	envscan -root <dir>        # module root (default: cwd)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/cmd/envscan/scan"
)

func main() {
	var (
		root           string
		doList         bool
		doCheck        bool
		doUnregistered bool
	)
	flag.StringVar(&root, "root", ".", "module root to scan")
	flag.BoolVar(&doList, "list", false, "print every detected env read")
	flag.BoolVar(&doCheck, "check", false, "exit non-zero on registry drift (forward + reverse)")
	flag.BoolVar(&doUnregistered, "unregistered", false, "print reads missing from the registry")
	flag.Parse()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fail("resolve root: %v", err)
	}

	switch {
	case doList:
		reads, err := scan.ScanReads(absRoot)
		if err != nil {
			fail("scan: %v", err)
		}
		var b strings.Builder
		scan.PrintReads(&b, reads)
		fmt.Print(b.String())
	case doUnregistered:
		reads, err := scan.ScanReads(absRoot)
		if err != nil {
			fail("scan: %v", err)
		}
		manifest, err := scan.LoadRegistry(absRoot)
		if err != nil {
			fail("load registry: %v", err)
		}
		for _, k := range scan.UnregisteredKeys(reads, scan.RegisteredSet(manifest)) {
			fmt.Println(k)
		}
	case doCheck:
		os.Exit(runCheck(absRoot))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// runCheck performs the drift check and prints the human-readable
// report, returning a process exit code (0 = clean, 1 = drift).
func runCheck(root string) int {
	res, err := scan.CheckDrift(root)
	if err != nil {
		fail("%v", err)
	}

	if len(res.Unregistered) > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: env vars read in code but NOT registered in scripts/secrets/manifest.yaml:")
		for _, k := range res.Unregistered {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}
	if len(res.Stale) > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: registry entries that appear nowhere in the repo (stale):")
		for _, k := range res.Stale {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}

	if res.OK() {
		fmt.Printf("envscan: OK -- %d reads, %d registry entries, no drift\n",
			res.ReadCount, res.RegistrySize)
		return 0
	}
	fmt.Fprintf(os.Stderr, "envscan: FAIL -- %d drift violation(s)\n", res.Violations())
	return 1
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "envscan: "+format+"\n", args...)
	os.Exit(2)
}
