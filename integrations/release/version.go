package release

import (
	"fmt"
	"regexp"
	"strconv"
)

// version.go -- release-tag arithmetic.
//
// WHY THIS IS NOT integrations/deployversion. That package bumps a version
// somebody hands it, in BARE X.Y.Z form, mirroring what release.sh accepts.
// This one answers a different question -- "what is the newest release tag on
// this repository, and what comes after it" -- over GIT TAGS, which carry a
// leading `v` and arrive mixed with everything else a repository has ever
// tagged. The parse rules therefore differ in the one way that matters: here
// the `v` is REQUIRED, and a tag that does not match is skipped rather than
// erroring, because a repository legitimately carries tags that are not
// releases.
//
// THE RULES ARE THE EXTENSION'S, DELIBERATELY. editors/vscode/src/install/
// tags.ts parseSemver accepts `v(\d+).(\d+).(\d+)` and nothing else -- no bare
// form, no pre-release, no build suffix -- because that is the shape this
// repository's releases take and DEFAULT_STACK_TAG is spelled that way. The
// installer and the cutter have to agree about what a release is, or the
// cutter creates versions the installer will not offer. TestParseRulesMatchTheExtension
// pins the two together over a shared table.
//
// COMPARISON IS NUMERIC PER COMPONENT, which is the whole reason a max needs
// code: v0.9.2 sorts AFTER v0.17.0 as a string, so a lexicographic max would
// name a nine-month-old release as newest and cut the next version from it --
// silently reissuing versions that already exist.

// releaseTagRe is the ONLY accepted release-tag shape. Anchored at both ends:
// an unanchored pattern would read `v1.2.3` out of `backup-v1.2.3-old` and
// treat a backup ref as a release.
var releaseTagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// version is a parsed release tag.
type version struct{ major, minor, patch int }

// parseReleaseTag reads a vX.Y.Z tag, or reports false for anything else --
// a pre-release, a bare X.Y.Z, a branch-shaped ref, a tag with a suffix.
func parseReleaseTag(tag string) (version, bool) {
	m := releaseTagRe.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	// Each group is \d+ so Atoi cannot fail on shape. It CAN fail on a
	// number too large for an int, which is a tag nobody cut and the
	// honest answer is "not a release tag" rather than a panic.
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	patch, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return version{}, false
	}
	return version{major: major, minor: minor, patch: patch}, true
}

// tag renders the canonical git-tag form, with the leading v.
func (v version) tag() string { return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch) }

// bare renders the form container image tags carry -- no leading v.
//
// The two conventions are memql#4061's rule and they are load-bearing in
// opposite directions: dispatch-engine-images-on-release.yml strips the v
// before dispatching, so an image tagged with the v would be an image the
// installer cannot pull, and a tag without it is a tag the installer cannot
// check out.
func (v version) bare() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

// newer reports whether v sorts after other. Component-wise and numeric.
func (v version) newer(other version) bool {
	if v.major != other.major {
		return v.major > other.major
	}
	if v.minor != other.minor {
		return v.minor > other.minor
	}
	return v.patch > other.patch
}

// bump returns the next version for a part. major and minor ZERO the parts
// below them, which is what makes v1.9.4 --minor land on v1.10.0 rather than
// v1.10.4.
func (v version) bump(part string) (version, error) {
	switch part {
	case "major":
		return version{major: v.major + 1}, nil
	case "minor":
		return version{major: v.major, minor: v.minor + 1}, nil
	case "patch":
		return version{major: v.major, minor: v.minor, patch: v.patch + 1}, nil
	default:
		// No empty-string default. deployversion defaults to patch
		// because its caller is a `logic` body that may legitimately
		// omit the part; here the argument is @required in the DSL and
		// the portal always sends one, so an empty value means
		// something went wrong upstream and guessing "patch" would cut
		// a release the operator did not ask for.
		return version{}, refuse(CodeInvalidBump, "bump must be major, minor or patch, not %q", part)
	}
}

// newestRelease picks the highest vX.Y.Z tag from a repository's tag list,
// skipping every ref that is not a release tag.
//
// Returns false when the list contains no release tag at all. The caller
// refuses with no_release_tags rather than defaulting to v0.0.0: inventing the
// start of somebody's release history is not a decision to make silently, and
// the first release of a repository is a moment for a human to name a version.
func newestRelease(tags []string) (version, bool) {
	var best version
	found := false
	for _, t := range tags {
		v, ok := parseReleaseTag(t)
		if !ok {
			continue
		}
		if !found || v.newer(best) {
			best, found = v, true
		}
	}
	return best, found
}

// normalizeVersion accepts either spelling of a version -- v1.2.3 or 1.2.3 --
// and returns the parsed value.
//
// Only releaseCutStatus takes a version from a caller, and an operator
// reading a row sees the tag form while an operator reading a registry sees
// the bare one. Refusing either spelling would be refusing the same version
// for being written the way the other screen writes it.
func normalizeVersion(s string) (version, bool) {
	if v, ok := parseReleaseTag(s); ok {
		return v, true
	}
	return parseReleaseTag("v" + s)
}
