package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/vendorname"
)

// MemQL is company-agnostic, and this is the sweep that keeps it that way.
//
// The names are in core/vendorname, not here. That package's doc comment says
// what qualifies as one, why the GitHub organisation and the product's own
// container registry deliberately do not, and carries the caveat this sweep
// inherits: it is a banned-NAMES list, so it catches these names and not the
// next one. Do not read a passing run as proof that no operator's hostname or
// resource is baked in anywhere.
//
// SCOPE: THE WHOLE REPOSITORY. It did not start that way, and the reasoning
// that narrowed it is worth recording because it was locally sound and
// globally wrong.
//
// The first version (memql#3593, memql#4217) covered deploy/, scripts/,
// editors/ and docs/public/operate/ -- "the artifacts that build and address a
// cluster" -- and exempted two roots on purpose:
//
//   - component/, where every one of ~40 mentions was an EXAMPLE rather than
//     configuration: fixtures picking a plausible hostname, doc comments
//     naming the `*.local.<domain>` dev-wildcard shape. None decided what any
//     cluster served, so renaming them would not make one more cluster
//     correct.
//   - docs/ outside docs/public/operate, which records what the tree USED to
//     do, where rewriting history to satisfy a linter would make the record
//     lie.
//
// Both exemptions are now closed, and neither by ignoring its objection.
//
// The component/ argument was true about each mention and false about the
// sum. An operator reading a fixture cannot tell an example from a default,
// and one occurrence was not an example at all: avatardirect's isLocalOnlyURL
// matched a company's dev wildcard as a substring, so a vendor domain was a
// branch condition in shipped product code. The sweep that would have found
// it was the one told not to look there.
//
// The docs/ argument was answered rather than overruled. A claim about what a
// cluster actually served is reworded ("the vendor domain then in use"), never
// given a substitute literal -- a different domain there would assert a past
// that did not happen -- while an incidental example hostname inside sample
// code takes an RFC 2606 reserved name. Each affected document says at the top
// that a redaction happened and which rule applied where, so none of them
// silently lies about its own provenance.
//
// So there are no root exemptions left. The skip list below is build output
// and vendored trees only, and the one file exemption is the package that
// holds the list, which cannot avoid containing it.
func TestNoVendorDomainLiterals(t *testing.T) {
	// Directory names whose CONTENT is not our source. Matched as a path
	// segment, so a package legitimately named `dist` elsewhere is safe.
	skipDirs := map[string]bool{
		"node_modules": true,
		"dist":         true,
		"dist-test":    true,
	}

	// The one file allowed to contain a banned name: the package that defines
	// them. Its own test deliberately spells none, so it needs no exemption --
	// see the comment at the top of core/vendorname/vendorname_test.go.
	const listDefinition = "core/vendorname/vendorname.go"

	// Police GIT-TRACKED files only, exactly as the neutrality sweep does:
	// untracked local files are not repo content.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	// Files are read whole and tested with bytes.Contains before any line
	// splitting. Two reasons, both of which bit the narrower version the
	// moment its scope grew: the repository tracks a multi-megabyte
	// single-line JSON artifact that a bufio.Scanner refuses outright
	// ("token too long"), and scanning ~4,400 files line by line to find
	// nothing is work the fast path does not need to do.
	var hits []string
	var scanned, unread int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || rel == listDefinition {
			continue
		}
		var skipped bool
		for dir := range skipDirs {
			if strings.HasPrefix(rel, dir+"/") || strings.Contains(rel, "/"+dir+"/") {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}

		body, err := os.ReadFile(rel)
		if err != nil {
			// Deleted-in-worktree, a broken symlink, an LFS pointer. Counted
			// rather than swallowed: a sweep that hides what it could not
			// examine makes its own pass a claim about the tool.
			unread++
			continue
		}
		scanned++
		if _, ok := vendorname.FirstIn(string(body)); !ok {
			continue
		}
		for i, line := range bytes.Split(body, []byte("\n")) {
			name, ok := vendorname.FirstIn(string(line))
			if !ok {
				continue
			}
			hits = append(hits, rel+":"+itoa(i+1)+": names "+name.Text+
				" ("+name.What+"): "+strings.TrimSpace(string(line)))
			if len(hits) > 40 {
				break
			}
		}
		if len(hits) > 40 {
			break
		}
	}

	// A sweep that examined nothing passes. Assert it examined the tree.
	if scanned < 1000 {
		t.Fatalf("only %d files scanned -- the sweep is not looking at the repository", scanned)
	}
	if unread > 0 {
		t.Logf("%d tracked file(s) could not be read and were NOT examined", unread)
	}

	if len(hits) > 0 {
		t.Errorf("the engine must name no operator's domain or resource -- those are values, "+
			"supplied at install time and held in that operator's own instance repository. "+
			"%d hit(s):\n  %s", len(hits), strings.Join(hits, "\n  "))
	}
}
