// Static guards for the database operand image lane (epic memql#3842, task
// memql#3844).
//
// # What these are for
//
// The TimescaleDB version pair -- current and N-1 -- is written down in THREE
// places: the Dockerfile's ARG defaults, the release workflow's dispatch-input
// defaults, and the local build script's cap_param defaults. That is one fact
// with three copies, which is the shape of drift that no compiler and no
// reviewer reliably catches (a bump lands in two of the three and the third
// keeps quietly building something else).
//
// The consequence is not cosmetic. The whole reason the image carries TWO
// versioned `.so` files is so a rolling restart can land BEFORE
// `ALTER EXTENSION` runs; if the local build and the release build disagree
// about which pair that is, the local cluster stops being a rehearsal for the
// upgrade the release will actually perform.
//
// # Why the push-ordering guard
//
// The workflow builds, smoke-tests, and only then pushes. That ordering is the
// difference between a broken database image staying on the runner and one
// sitting in ACR where an overlay can pin it. It is also invisible: reordering
// two steps in a YAML file looks like tidying, and the resulting workflow is
// still green on every run where the image happens to be fine.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// dbImageRepoRoot resolves the repository root from this file's location.
//
// Deliberately file-prefixed and unexported, matching the convention
// workflow_timeout_test.go set in this package: scripts/ci hosts several
// independent guards, and a shared helper would turn their independence into a
// compile-level conflict.
func dbImageRepoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func dbImageRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dbImageRepoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// first returns the first capture of re in body, or "" when it does not match.
func dbImageFirst(re *regexp.Regexp, body string) string {
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// TestDatabaseImageVersionsAgreeEverywhere is the drift gate on the one fact
// that is written down three times.
func TestDatabaseImageVersionsAgreeEverywhere(t *testing.T) {
	dockerfile := dbImageRead(t, "deploy/db-image/Dockerfile")
	workflow := dbImageRead(t, ".github/workflows/build-db-image.yml")
	buildScript := dbImageRead(t, "scripts/db-image/build.sh")
	smokeTest := dbImageRead(t, "deploy/db-image/smoke-test.sh")

	// Each source states the pair in its own syntax, so each needs its own
	// expression. Anchored tightly enough that a rename fails loudly here
	// rather than silently matching nothing and passing.
	sources := []struct {
		what     string
		current  string
		previous string
	}{
		{
			what:     "deploy/db-image/Dockerfile (ARG defaults)",
			current:  dbImageFirst(regexp.MustCompile(`(?m)^ARG TIMESCALEDB_VERSION=([0-9.]+)`), dockerfile),
			previous: dbImageFirst(regexp.MustCompile(`(?m)^ARG TIMESCALEDB_PREVIOUS_VERSION=([0-9.]+)`), dockerfile),
		},
		{
			what:     ".github/workflows/build-db-image.yml (dispatch-input defaults)",
			current:  dbImageFirst(regexp.MustCompile(`(?s)timescaledb_version:.*?default:\s*"([0-9.]+)"`), workflow),
			previous: dbImageFirst(regexp.MustCompile(`(?s)timescaledb_previous_version:.*?default:\s*"([0-9.]+)"`), workflow),
		},
		{
			what:     "scripts/db-image/build.sh (cap_param defaults)",
			current:  dbImageFirst(regexp.MustCompile(`cap_param timescaledb "([0-9.]+)"`), buildScript),
			previous: dbImageFirst(regexp.MustCompile(`cap_param timescaledbPrevious "([0-9.]+)"`), buildScript),
		},
		{
			what:     "deploy/db-image/smoke-test.sh (positional defaults)",
			current:  dbImageFirst(regexp.MustCompile(`TS_CURRENT="\$\{3:-([0-9.]+)\}"`), smokeTest),
			previous: dbImageFirst(regexp.MustCompile(`TS_PREVIOUS="\$\{4:-([0-9.]+)\}"`), smokeTest),
		},
	}

	for _, s := range sources {
		if s.current == "" || s.previous == "" {
			t.Fatalf("could not read the TimescaleDB version pair out of %s "+
				"(current=%q previous=%q) -- the declaration was renamed or restructured, "+
				"so this guard is no longer reading what it claims to", s.what, s.current, s.previous)
		}
	}

	want := sources[0]
	for _, s := range sources[1:] {
		if s.current != want.current {
			t.Errorf("TimescaleDB CURRENT disagrees: %s says %s, %s says %s.\n"+
				"One bump landed in some of the copies and not the others; the local build "+
				"is now rehearsing a different upgrade than the release will perform.",
				want.what, want.current, s.what, s.current)
		}
		if s.previous != want.previous {
			t.Errorf("TimescaleDB PREVIOUS (N-1) disagrees: %s says %s, %s says %s.\n"+
				"The N-1 library is what lets a rolling restart land before ALTER EXTENSION runs; "+
				"disagreeing copies mean one of these builds ships the wrong one.",
				want.what, want.previous, s.what, s.previous)
		}
	}

	// The two must not be equal: "previous" being a copy of "current" would
	// build an image with ONE library while every comment claims two, and the
	// Dockerfile's `test -f` assertions would still pass (both paths resolving
	// to the same file).
	if want.current == want.previous {
		t.Errorf("TIMESCALEDB_VERSION and TIMESCALEDB_PREVIOUS_VERSION are both %s. "+
			"The image would carry one library while the upgrade choreography assumes two, "+
			"and the build-time assertions would not notice.", want.current)
	}
}

// TestDatabaseImageDockerfileAssertsBothLibraries checks that the build fails
// at BUILD time when a library is missing, rather than at upgrade time.
//
// The stash-and-restore of the N-1 `.so` is the fiddliest part of the
// Dockerfile and the easiest to break while editing something adjacent. Its
// failure is silent -- the image builds, runs, and serves traffic perfectly
// until the day of an upgrade.
func TestDatabaseImageDockerfileAssertsBothLibraries(t *testing.T) {
	dockerfile := dbImageRead(t, "deploy/db-image/Dockerfile")

	for _, want := range []struct{ needle, why string }{
		{`test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb.so"`, "the loader"},
		{`test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_VERSION}.so"`, "the current versioned library"},
		{`test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TIMESCALEDB_PREVIOUS_VERSION}.so"`, "the N-1 versioned library, which is the entire point of the two-version image"},
		{`test -f "/usr/share/postgresql/${PG_MAJOR}/extension/vector.control"`, "pgvector, which comes from the `standard` base flavor and would vanish silently on a switch to `minimal`"},
	} {
		if !strings.Contains(dockerfile, want.needle) {
			t.Errorf("the Dockerfile no longer asserts %s at build time.\n"+
				"Expected to find: %s\n"+
				"Without it the failure surfaces during an upgrade or at the first "+
				"CREATE EXTENSION in a live cluster, not here.", want.why, want.needle)
		}
	}
}

// TestDatabaseImageSmokeTestGatesThePush is the ordering guard.
//
// A database image that reached ACR broken is pinnable by an overlay, and the
// symptom of an Apache-build regression is not a failed pull -- it is a
// migration failing much later with an error naming a missing function.
func TestDatabaseImageSmokeTestGatesThePush(t *testing.T) {
	workflow := dbImageRead(t, ".github/workflows/build-db-image.yml")

	smokeAt := strings.Index(workflow, "deploy/db-image/smoke-test.sh")
	if smokeAt < 0 {
		t.Fatal("the build-db-image workflow does not run deploy/db-image/smoke-test.sh at all")
	}
	pushAt := strings.Index(workflow, "- name: Push memql-db")
	if pushAt < 0 {
		t.Fatal("the build-db-image workflow has no `Push memql-db` step; this guard can no longer find what it is ordering against")
	}
	if smokeAt > pushAt {
		t.Error("the smoke test runs AFTER the push step. The push must be gated on the smoke test: " +
			"an image that reaches ACR broken can be pinned by an overlay, and an Apache-build " +
			"regression does not show up as a failed pull -- it shows up as a migration failing later.")
	}

	// The pre-push build must not push. `push: true` on the first build would
	// defeat the ordering above while leaving both steps present and the guard
	// above satisfied.
	firstBuild := workflow[:smokeAt]
	if !strings.Contains(firstBuild, "load: true") {
		t.Error("the pre-smoke-test build does not `load: true`, so there is no local image for the smoke test to exercise")
	}
	if !strings.Contains(firstBuild, "push: false") {
		t.Error("the pre-smoke-test build does not explicitly set `push: false`; the smoke test is then gating nothing")
	}
}
