package buildinfo

import (
	"regexp"
	"strings"
	"testing"
)

// releaseShaped is the shape a version comparator accepts as a release tag:
// optional leading v, then major.minor.patch. It mirrors what
// editors/vscode/src/version/compare.ts parses (memql#3991) closely enough to
// answer the only question these tests ask -- "would a client believe this
// string names a release?" -- which is the property the whole package exists
// to get right.
var releaseShaped = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// stampRelease sets the link-time variable for the duration of one test. Tests
// are the only place Go may write it; see the package comment.
func stampRelease(t *testing.T, value string) {
	t.Helper()
	prior := release
	release = value
	t.Cleanup(func() { release = prior })
}

// TestUnstampedBuildDoesNotNameARelease is THE regression this package exists
// to prevent (memql#3998).
//
// The failure it guards is not "the version is missing" -- it is "the version
// is present, release-shaped, and wrong", which is what the checked-in VERSION
// file produced for every build between v0.16.1 and this change. A client
// comparing `0.15.0` against the newest release gets a confident answer to a
// question it should have refused, and an operator on v0.18.0 is told they are
// three releases behind (or, worse, that they are current) on the strength of a
// number nobody set.
//
// A test build is by definition unstamped, so this asserts the default.
func TestUnstampedBuildDoesNotNameARelease(t *testing.T) {
	if got := Release(); got != "" {
		t.Fatalf("test binaries are not release builds; Release() = %q, want \"\"", got)
	}
	if IsRelease() {
		t.Fatal("IsRelease() = true on an unstamped build")
	}

	got := Version()
	if got != DevVersion {
		t.Fatalf("Version() = %q on an unstamped build, want %q", got, DevVersion)
	}
	if releaseShaped.MatchString(got) {
		t.Fatalf("Version() = %q on an unstamped build, which parses as a release tag -- "+
			"a build that was not cut from a release must not name one", got)
	}
}

// TestDevVersionIsNotReleaseShaped pins the constant itself. Replacing "dev"
// with something tidier-looking such as "0.0.0" would reintroduce the exact
// failure above while looking like a cosmetic change, because 0.0.0 parses and
// compares.
func TestDevVersionIsNotReleaseShaped(t *testing.T) {
	if DevVersion == "" {
		t.Fatal("DevVersion is empty; Version() must never return an empty string")
	}
	if releaseShaped.MatchString(DevVersion) {
		t.Fatalf("DevVersion = %q parses as a release tag; pick a value no release parser accepts", DevVersion)
	}
}

func TestStampedBuildStatesItsRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp string
		want  string
	}{
		{name: "v-prefixed tag", stamp: "v0.18.1", want: "v0.18.1"},
		{name: "unprefixed tag", stamp: "0.18.1", want: "0.18.1"},
		// The build arg arrives through a shell and a Dockerfile; a trailing
		// newline or a stray space must not become part of the version.
		{name: "surrounding whitespace", stamp: "  v0.19.0\n", want: "v0.19.0"},
		// An explicitly empty -X (the build arg was declared but not set) is
		// the same as no stamp at all, not a version of "".
		{name: "whitespace only", stamp: "   ", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stampRelease(t, tc.stamp)

			if got := Release(); got != tc.want {
				t.Fatalf("Release() = %q, want %q", got, tc.want)
			}
			if got, want := IsRelease(), tc.want != ""; got != want {
				t.Fatalf("IsRelease() = %v, want %v", got, want)
			}

			wantVersion := tc.want
			if wantVersion == "" {
				wantVersion = DevVersion
			}
			if got := Version(); got != wantVersion {
				t.Fatalf("Version() = %q, want %q", got, wantVersion)
			}
		})
	}
}

// TestVersionIsNeverEmpty guards the one contract every caller relies on:
// Version is stamped into logs, health output, ServerHello.engine_version and
// the memqlVersion() builtin, none of which have a sensible rendering for "".
func TestVersionIsNeverEmpty(t *testing.T) {
	for _, stamp := range []string{"", " ", "\n", "v1.2.3"} {
		stampRelease(t, stamp)
		if Version() == "" {
			t.Fatalf("Version() = \"\" with release stamped as %q", stamp)
		}
	}
}

// stampCommit sets the link-time commit variable for the duration of one test.
func stampCommit(t *testing.T, value string) {
	t.Helper()
	prior := commit
	commit = value
	t.Cleanup(func() { commit = prior })
}

// TestStampedCommitWins pins the source ORDER (memql#4486). The link-time stamp
// must beat the toolchain's VCS table, because those two disagree in exactly
// the case that matters: an image built from a Docker context has no .git, so
// only the stamp is populated -- and a developer binary built from a checkout
// has both, where the stamp is the one the build deliberately set.
func TestStampedCommitWins(t *testing.T) {
	stampCommit(t, "0123456789abcdef0123456789abcdef01234567")

	if got, want := Commit(), "0123456789abcdef0123456789abcdef01234567"; got != want {
		t.Fatalf("Commit() = %q, want the link-time stamp %q", got, want)
	}
	if got, want := ShortCommit(), "0123456789ab"; got != want {
		t.Fatalf("ShortCommit() = %q, want %q (git's 12-character abbreviation)", got, want)
	}
}

// TestCommitStampIsTrimmed mirrors the release half: the value arrives through
// a shell and a Dockerfile, and a trailing newline must not become part of the
// revision. A whitespace-only stamp is an unset build arg, not a revision of "".
func TestCommitStampIsTrimmed(t *testing.T) {
	stampCommit(t, "  abc123def456\n")
	if got, want := Commit(), "abc123def456"; got != want {
		t.Fatalf("Commit() = %q, want %q", got, want)
	}

	stampCommit(t, "   ")
	// Falls through to the VCS table, which in a test binary may or may not be
	// populated depending on how the test was invoked. The contract being
	// pinned is only that a blank stamp is not itself the answer.
	if got := Commit(); strings.TrimSpace(got) != got {
		t.Fatalf("Commit() = %q; a whitespace-only stamp must not be returned as a revision", got)
	}
}

// TestDirtyRevisionKeepsItsSuffix guards the abbreviation from silently
// laundering a dirty build into a clean-looking one. ShortCommit truncates the
// sha, and a naive truncation drops the suffix -- which turns "these bytes are
// not what that commit contains" into a confident, wrong revision, the same
// failure class the release half of this package exists to prevent.
func TestDirtyRevisionKeepsItsSuffix(t *testing.T) {
	stampCommit(t, "0123456789abcdef0123456789abcdef01234567-dirty")

	if got, want := ShortCommit(), "0123456789ab-dirty"; got != want {
		t.Fatalf("ShortCommit() = %q, want %q -- a dirty build must not abbreviate to a clean revision", got, want)
	}
}

// TestLogAttrsOmitsAnUnknownCommit pins the omission rather than an empty
// field. A structured log field carrying "" matches a query filtering on
// commit and reads, to a person, as an answered question.
func TestLogAttrsOmitsAnUnknownCommit(t *testing.T) {
	stampCommit(t, "")

	attrs := LogAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("LogAttrs() = %v, which is not alternating key/value pairs", attrs)
	}
	for i := 0; i < len(attrs); i += 2 {
		if attrs[i] == "commit" {
			if v, _ := attrs[i+1].(string); v == "" {
				t.Fatal("LogAttrs() emitted commit=\"\"; an unknown revision must be omitted, not logged empty")
			}
		}
	}

	// And when it IS known, it must be present.
	stampCommit(t, "0123456789abcdef")
	attrs = LogAttrs()
	var found bool
	for i := 0; i < len(attrs); i += 2 {
		if attrs[i] == "commit" {
			found = true
			if got, want := attrs[i+1], "0123456789ab"; got != want {
				t.Fatalf("LogAttrs() commit = %v, want %v", got, want)
			}
		}
	}
	if !found {
		t.Fatal("LogAttrs() omitted commit on a stamped build")
	}
}

// TestLogAttrsAlwaysNamesAVersion is the boot line's contract: whatever else is
// unknown, the line states a version, because a boot line that omits it leaves
// the operator exactly where memql#4486 found them.
func TestLogAttrsAlwaysNamesAVersion(t *testing.T) {
	for _, stamp := range []string{"", "v1.2.3"} {
		stampRelease(t, stamp)
		attrs := LogAttrs()
		var version any
		for i := 0; i < len(attrs); i += 2 {
			if attrs[i] == "version" {
				version = attrs[i+1]
			}
		}
		if v, _ := version.(string); v == "" {
			t.Fatalf("LogAttrs() named no version with release stamped %q", stamp)
		}
	}
}
