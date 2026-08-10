// The tree-side evidence the coverage guard's exemptions are checked against
// (znasllc-io/memql#3451).
//
// # Why this exists
//
// `coverageAllowList` asks every exemption for a reason, and until memql#3451
// nothing checked whether the reason was TRUE. The entry that caused the issue
// read "no lane consults it, so no bucket can verify it" about
// `.claude/settings.json` -- a file `scripts/dev/repo_hygiene_test.go` names
// explicitly, in a test that runs in a lane. A single `grep` would have
// falsified it. Where a mechanism demands a justification, the justification is
// the part a future reader is entitled to trust, and it was the one part
// nothing verified.
//
// So the most common reason -- "nothing consults this" -- is now stated in a
// form a test can check: an exemption declares the exact set of gate-corpus
// files that MENTION the paths it exempts, and that declaration is compared
// against the tree in both directions.
//
// # What the corpus is, and what a "mention" proves
//
// The corpus is every tracked `*_test.go`, every file under
// `.github/workflows/`, and the `Makefile` -- the files that decide what CI
// runs and what a gate reads.
//
// `scripts/**` at large is deliberately NOT in it. A shell script naming a
// path is usually a RUNTIME relationship rather than a gate: `scripts/k3d/
// seed-secrets.sh` invokes `deploy/k8s/base/tls/gen-internal-ca.sh` during
// `make up`, which no CI lane runs. The scripts that ARE gates are Go tests,
// and those are in the corpus as `*_test.go`.
//
// Be exact about the strength of the claim, because it is a proxy and naming
// it for the conclusion someone wants to draw would be the same overreach the
// issue is about:
//
//   - A mention does NOT prove a gate reads the file. A comment mentions
//     paths, and this scan reads comments too.
//   - An absence of mentions does NOT prove no gate reads the file. A test
//     that sweeps `git ls-files` and inspects whatever it returns -- this
//     repository has seven of those -- names no path at all.
//
// The asymmetry is deliberate. A false "something mentions it" pushes the
// author toward routing, which costs a lane. A false "nothing mentions it"
// would restore the defect. So the check is tuned to over-report, and the
// residual is stated rather than papered over.
package ci

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowListDeclarationFile declares coverageAllowList. It is excluded from the
// corpus: every entry names its own pattern, so including it would make every
// exemption "mentioned" by the act of being written down.
const allowListDeclarationFile = "scripts/ci/path_bucket_coverage_test.go"

// gateCorpus is the corpus, read once and kept in memory.
type gateCorpus struct {
	order []string          // corpus paths, sorted
	body  map[string]string // corpus path -> contents
}

// isGateCorpusMember reports whether a tracked path belongs to the corpus.
func isGateCorpusMember(path string) bool {
	switch {
	case path == "Makefile":
		return true
	case strings.HasPrefix(path, ".github/workflows/"):
		return true
	case strings.HasSuffix(path, "_test.go"):
		return true
	}
	return false
}

// loadGateCorpus reads the corpus from disk.
func loadGateCorpus(t *testing.T) *gateCorpus {
	t.Helper()

	c := &gateCorpus{body: map[string]string{}}
	sawDeclaration := false
	for _, f := range trackedFiles(t) {
		if f == allowListDeclarationFile {
			sawDeclaration = true
			continue
		}
		if !isGateCorpusMember(f) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(RepoRoot(), filepath.FromSlash(f)))
		if err != nil {
			t.Fatalf("read corpus file %s: %v", f, err)
		}
		c.order = append(c.order, f)
		c.body[f] = string(raw)
	}
	sort.Strings(c.order)

	// Two fail-closed checks. A corpus that quietly narrowed to nothing would
	// report every exemption's "nothing mentions it" as true, which is the
	// exact failure this file exists to prevent.
	if len(c.order) == 0 {
		t.Fatal("the gate corpus is empty -- no *_test.go, no workflow, no Makefile. The " +
			"mention check would report every allow-list claim true having read nothing " +
			"(memql#3451)")
	}
	if !sawDeclaration {
		t.Fatalf("allowListDeclarationFile is %q, which git does not track. The constant has "+
			"gone stale after a rename: until it is updated the allow list is scanned against "+
			"itself, and every entry appears to be mentioned by its own declaration.",
			allowListDeclarationFile)
	}
	return c
}

// pathLeadByte reports whether b can precede a path character inside a longer
// path. Used to reject `Dockerfile` matching the tail of
// `cmd/deploy-gate-check/Dockerfile`.
func pathLeadByte(b byte) bool {
	return b == '_' || b == '.' || b == '-' || b == '/' || isAlnum(b)
}

// pathTailByte reports whether b continues a path after a match.
//
// `.` is excluded on purpose: prose ends sentences with one ("...reads the
// Dockerfile."), and treating that as a non-mention would silently drop the
// most common way a path is named in a comment. The cost is that
// `Dockerfile.md` counts as a mention of `Dockerfile` -- an over-report, which
// is the safe direction.
func pathTailByte(b byte) bool {
	return b == '_' || b == '-' || b == '/' || isAlnum(b)
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// mentionsPath reports whether body names path as a path, rather than as the
// tail or head of a longer one.
func mentionsPath(body, path string) bool {
	for i := 0; i+len(path) <= len(body); {
		j := strings.Index(body[i:], path)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(path)
		leadOK := j == 0 || !pathLeadByte(body[j-1])
		tailOK := end == len(body) || !pathTailByte(body[end])
		if leadOK && tailOK {
			return true
		}
		i = j + 1
	}
	return false
}

// mentionedBy returns the corpus files that name path, sorted. The file itself
// is skipped -- a workflow naming its own path proves nothing.
func (c *gateCorpus) mentionedBy(path string) []string {
	var out []string
	for _, f := range c.order {
		if f == path {
			continue
		}
		if mentionsPath(c.body[f], path) {
			out = append(out, f)
		}
	}
	return out
}

// The mechanism must not be able to pass vacuously. Both directions are
// asserted against known facts about this tree: a path a gate demonstrably
// names, and a path nothing can name because it does not exist.
func TestGateCorpusSeesMentionsAndAbsences(t *testing.T) {
	c := loadGateCorpus(t)

	// `.gitignore` is read by scripts/dev/repo_hygiene_test.go
	// (TestLockfileNegationsHaveATrackedFileBehindThem). This is the same
	// shape as the memql#3451 entry: a file whose exemption once claimed
	// nothing consulted it.
	const named = ".gitignore"
	if got := c.mentionedBy(named); len(got) == 0 {
		t.Errorf("the corpus finds no mention of %q, but scripts/dev/repo_hygiene_test.go "+
			"reads it. The mention check is scanning the wrong thing and would report every "+
			"allow-list claim true (memql#3451).", named)
	}

	// Assembled rather than written as a literal. THIS FILE is in the corpus,
	// and mentionedBy only skips a corpus file when it IS the path being
	// scored -- so a literal would be found in the probe that asserts it
	// cannot be found, and the negative direction would be untestable from
	// inside the corpus.
	absent := strings.Join([]string{"no", "such", "path", "invented-for-this-guard"}, "/") + ".txt"
	if got := c.mentionedBy(absent); len(got) != 0 {
		t.Errorf("the corpus reports %q mentioned by %v. A check that matches everything "+
			"is as useless as one that matches nothing.", absent, got)
	}

	// The boundary rule, asserted directly rather than through the corpus:
	// without it a bare `Dockerfile` matches the tail of every nested one.
	if mentionsPath("cmd/deploy-gate-check/Dockerfile", "Dockerfile") {
		t.Error("mentionsPath matched a bare `Dockerfile` inside a longer path; every nested " +
			"path would then count as a mention of its own basename")
	}
	if !mentionsPath("reads the Dockerfile.", "Dockerfile") {
		t.Error("mentionsPath missed a path at the end of a sentence; prose is where most " +
			"mentions live, so this would under-report in the unsafe direction")
	}
}
