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
// It detects two drift directions against the envregistry manifest registry
// (scripts/secrets/manifest.yaml, the locked source of truth):
//
//   - forward drift  -- a var read by code that is NOT registered.
//   - reverse drift   -- a registered var that appears NOWHERE in the
//     repo (code / k8s / .env / dsl). Stale registry entry.
//
// Reads are resolved by parsing, so a key named through a constant is
// seen (memql#3818). What cannot be resolved statically -- a key that is
// a parameter or a loop variable -- is REPORTED rather than dropped:
// every mode states the residual count, and -unresolvable lists the
// sites. `241 reads, no drift` would only have moved the blind spot;
// `241 reads, 30 unresolvable sites, no drift` is a claim a reader can
// check.
//
// Usage:
//
//	envscan -list              # print every detected read (key\tfile)
//	envscan -check             # exit non-zero on forward or reverse drift
//	envscan -unregistered      # print only reads missing from the registry
//	envscan -unresolvable      # print the sites whose key could not be resolved
//	envscan -root <dir>        # module root (default: cwd)
package main

import (
	"flag"
	"fmt"
	"io"
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
		doUnresolvable bool
	)
	flag.StringVar(&root, "root", ".", "module root to scan")
	flag.BoolVar(&doList, "list", false, "print every detected env read")
	flag.BoolVar(&doCheck, "check", false, "exit non-zero on registry drift (forward + reverse)")
	flag.BoolVar(&doUnregistered, "unregistered", false, "print reads missing from the registry")
	flag.BoolVar(&doUnresolvable, "unresolvable", false,
		"print read-shaped sites whose env key could not be resolved statically")
	flag.Parse()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fail("resolve root: %v", err)
	}

	switch {
	case doList:
		out, err := scan.ScanReads(absRoot)
		if err != nil {
			fail("scan: %v", err)
		}
		var b strings.Builder
		scan.PrintReads(&b, out.Reads)
		fmt.Print(b.String())
		// The residual goes to STDERR so `-list | wc -l` stays a count of
		// reads, while a human running the command still cannot miss what
		// the scan could not see.
		reportUnresolvable(os.Stderr, out.Unresolvable)
	case doUnregistered:
		out, err := scan.ScanReads(absRoot)
		if err != nil {
			fail("scan: %v", err)
		}
		manifest, err := scan.LoadRegistry(absRoot)
		if err != nil {
			fail("load registry: %v", err)
		}
		for _, k := range scan.UnregisteredKeys(out.Reads, scan.RegisteredSet(manifest)) {
			fmt.Println(k)
		}
		reportUnresolvable(os.Stderr, out.Unresolvable)
	case doUnresolvable:
		out, err := scan.ScanReads(absRoot)
		if err != nil {
			fail("scan: %v", err)
		}
		var b strings.Builder
		scan.PrintUnresolvable(&b, out.Unresolvable)
		fmt.Print(b.String())
	case doCheck:
		os.Exit(runCheck(absRoot))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// reportUnresolvable writes the residual block. Always writes the count,
// including when it is zero -- "0 unresolvable sites" is a claim worth
// making explicitly, and its absence is what made the old summary line
// read as coverage.
func reportUnresolvable(w io.Writer, sites []scan.Unresolvable) {
	if len(sites) == 0 {
		fmt.Fprintln(w, "INFO: 0 read-shaped sites unresolvable -- every detected read folded to a constant")
		return
	}
	// The SPLIT, not just the total: an unresolved key is fixed at the CALL
	// SITE (name it with a constant), a reader-prefix site is fixed in the
	// SCANNER (trace the reader's constructor prefix). One number tells a
	// reader how much the check cannot see but not what to do about any of it.
	keys := scan.CountKind(sites, scan.KindUnresolvedKey)
	readers := scan.CountKind(sites, scan.KindReaderPrefix)
	fmt.Fprintf(w, "INFO: %d read-shaped site(s) could NOT be resolved statically -- %d whose key is a "+
		"parameter / loop variable / computed value, and %d env.NewEnvReader reads whose prefix this "+
		"scanner does not trace. These are not drift; they are the surface this check cannot speak "+
		"for:\n", len(sites), keys, readers)
	var b strings.Builder
	scan.PrintUnresolvable(&b, sites)
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		fmt.Fprintf(w, "  - %s\n", line)
	}
}

// reportExempt writes the pre-convention exemption block: memQL-owned keys
// this scan READ that the registry cannot yet name, because
// TestOwnedVarsArePrefixed refuses a non-MEMQL_-prefixed entry and none of them
// is a legacy alias (see scan.ownedPreConvention).
//
// It prints unconditionally when non-empty, and the wording says "not
// registered" rather than "allowed", because an exemption nobody reads is how a
// gap becomes permanent.
func reportExempt(w io.Writer, keys []string) {
	if len(keys) == 0 {
		return
	}
	fmt.Fprintf(w, "WARNING: %d memQL-owned key(s) are READ but NOT REGISTERED, exempted from forward "+
		"drift only because they predate the MEMQL_ prefix convention and a non-prefixed registry "+
		"entry fails TestOwnedVarsArePrefixed (memql#2106). They need renaming, not tolerating:\n",
		len(keys))
	for _, k := range keys {
		fmt.Fprintf(w, "  - %s\n", k)
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

	// The residual and the pre-convention exemption are part of the report
	// in BOTH outcomes. A summary that states only what it resolved reads as
	// coverage, which is the defect memql#3818 is about one level up.
	reportUnresolvable(os.Stdout, res.Unresolvable)
	reportExempt(os.Stdout, res.ExemptUnprefixed)

	if res.OK() {
		fmt.Printf("envscan: OK -- %d reads, %d unresolvable sites, %d unregistered-by-exemption, "+
			"%d registry entries, no drift\n",
			res.ReadCount, len(res.Unresolvable), len(res.ExemptUnprefixed), res.RegistrySize)
		return 0
	}
	fmt.Fprintf(os.Stderr, "envscan: FAIL -- %d drift violation(s) (%d reads, %d unresolvable sites, "+
		"%d unregistered-by-exemption)\n",
		res.Violations(), res.ReadCount, len(res.Unresolvable), len(res.ExemptUnprefixed))
	return 1
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "envscan: "+format+"\n", args...)
	os.Exit(2)
}
