// Static guard: no filter bucket may be wider than it is written to be
// (znasllc-io/memql#3222).
//
// # The failure mode
//
// `.github/workflows/ci.yml` declared:
//
//	server-grpc:
//	  - 'component/grpc/**'
//	  - '!component/grpc/gen/**'
//
// which reads as "component/grpc excluding its generated tree" and is nothing
// of the sort. `dorny/paths-filter` compiles each pattern into its OWN matcher
// and combines them with `.some()`, so the second line is an independent term
// meaning "any path that is not under component/grpc/gen" -- true for almost
// every file in the repository. Measured against the pinned action's own
// matcher, `server-grpc` was true for 3085 of 3085 tracked files, including
// `component/grpc/gen/memql.pb.go`, the one file the negation existed to
// remove.
//
// Exclusion is not expressible in a paths-filter pattern list. That is a
// property of the action, so the guard is written against the property rather
// than against the one bucket that tripped over it.
//
// # Why this shape of assertion
//
// A negated pattern can only ever ADD matches under OR. So the invariant is
// exact and needs no judgement call: scoring a bucket WITH its negations must
// equal scoring it WITHOUT them. Any bucket where those differ is matching
// files its author did not write it to match.
//
// The consequence is deliberate: it makes `!` unusable in a bucket unless it
// is a no-op, which is correct, because a `!` that does anything at all is
// doing the opposite of what it looks like.
package ci

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// trackedFiles returns every file git tracks, which is the population CI
// filters actually score.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = RepoRoot()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned nothing -- the guard fails closed rather than " +
			"reporting a clean tree it never examined")
	}
	return files
}

// compiledBuckets parses ci.yml and compiles every declared bucket.
func compiledBuckets(t *testing.T) map[string]*BucketMatcher {
	t.Helper()
	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	buckets, err := ParseChangesFilters(raw)
	if err != nil {
		t.Fatal(err)
	}
	compiled := make(map[string]*BucketMatcher, len(buckets))
	for name, patterns := range buckets {
		b, err := CompileBucket(name, patterns)
		if err != nil {
			t.Fatalf("%v", err)
		}
		compiled[name] = b
	}
	return compiled
}

// TestNoNegatedPatternWidensItsBucket is the guard for the memql#3222 cause.
func TestNoNegatedPatternWidensItsBucket(t *testing.T) {
	buckets := compiledBuckets(t)
	files := trackedFiles(t)

	for _, name := range SortedKeys(buckets) {
		b := buckets[name]

		var negated []string
		for _, p := range b.Patterns {
			if strings.HasPrefix(p, "!") {
				negated = append(negated, p)
			}
		}
		if len(negated) == 0 {
			continue
		}

		var widened []string
		for _, f := range files {
			if b.Match(f) && !b.MatchPositiveOnly(f) {
				widened = append(widened, f)
			}
		}
		if len(widened) == 0 {
			continue
		}

		sample := widened
		if len(sample) > 5 {
			sample = sample[:5]
		}
		t.Errorf("bucket %q carries the negated pattern(s) %v, and they WIDEN it: %d of %d "+
			"tracked files match the bucket only because of a negation.\n"+
			"  e.g. %v\n"+
			"dorny/paths-filter compiles each pattern separately and ORs the results, so `!x` is "+
			"an independent term meaning \"anything that is not x\" -- it adds matches instead of "+
			"removing them. Exclusion cannot be expressed in a pattern list. Enumerate the "+
			"subpaths you DO want positively.",
			name, negated, len(widened), len(files), sample)
	}
}

// TestNoBucketMatchesEveryTrackedFile is the symptom-side guard: a bucket true
// for every possible PR routes nothing and provides no scoping at all.
//
// The `ci` bucket is not exempted, because it does not need to be -- it is
// true only for `.github/workflows/**` and `Makefile`. A bucket that genuinely
// must always fire should be an unconditional lane, not a filter.
func TestNoBucketMatchesEveryTrackedFile(t *testing.T) {
	buckets := compiledBuckets(t)
	files := trackedFiles(t)

	for _, name := range SortedKeys(buckets) {
		b := buckets[name]
		matched := 0
		for _, f := range files {
			if b.Match(f) {
				matched++
			}
		}
		if matched == len(files) {
			t.Errorf("bucket %q matches ALL %d tracked files, so it is true for every PR and "+
				"scopes nothing. Patterns: %v", name, len(files), b.Patterns)
		}
		if matched == 0 {
			t.Errorf("bucket %q matches NO tracked file, so any lane gated on it never runs "+
				"while the gate stays green. Patterns: %v", name, b.Patterns)
		}
	}
}

// TestServerGrpcBucketEnumeratesEveryNonGenSubtree keeps the positive
// enumeration honest.
//
// Replacing the negation with `component/grpc/*` + each non-`gen` subtree is
// faithful, but it is an enumeration, and an enumeration goes stale silently:
// a new `component/grpc/<thing>/` would simply not be matched, and an
// under-matching bucket is this repository's recurring failure -- a lane that
// stops running while the gate stays green.
//
// Derived from disk rather than from a pinned list, so adding a subdirectory
// fails here and forces the routing decision to be made rather than skipped.
func TestServerGrpcBucketEnumeratesEveryNonGenSubtree(t *testing.T) {
	const grpcDir = "component/grpc"

	buckets := compiledBuckets(t)
	b, ok := buckets["server-grpc"]
	if !ok {
		t.Fatal("no `server-grpc` bucket declared in ci.yml")
	}

	entries, err := os.ReadDir(filepath.Join(RepoRoot(), grpcDir))
	if err != nil {
		t.Fatalf("read %s: %v", grpcDir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A representative path inside the subtree; the bucket's verdict is
		// the same for every file in it.
		probe := path.Join(grpcDir, e.Name(), "probe.go")

		if e.Name() == "gen" {
			if b.Match(probe) {
				t.Errorf("bucket `server-grpc` matches %q, but component/grpc/gen is `wire` and "+
					"is targeted to version independently -- it belongs to the `proto` bucket, "+
					"not this one", probe)
			}
			continue
		}
		if !b.Match(probe) {
			t.Errorf("bucket `server-grpc` does NOT match %q. %s/%s/ exists but is not "+
				"enumerated, so a lane gated on this bucket would silently skip changes to it "+
				"while the gate stayed green. Add `%s/%s/**` to the bucket. Patterns: %v",
				probe, grpcDir, e.Name(), grpcDir, e.Name(), b.Patterns)
		}
	}

	// The top-level sources are the bulk of the module and the reason the
	// bucket exists at all.
	if !b.Match(path.Join(grpcDir, "server.go")) {
		t.Errorf("bucket `server-grpc` does not match %s/server.go. Patterns: %v", grpcDir, b.Patterns)
	}
}
