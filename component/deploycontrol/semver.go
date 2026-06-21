// semver.go is the version-arithmetic helper behind the "cut a version"
// flow (#1877). It parses a version, bumps a chosen part, and compares
// two versions to find the highest succeeded deployment.
//
// memQL release tags are a three-part numeric version. Two schemes are
// in use and BOTH fit one parser: classic semver (0.9.9, 1.2.3) and the
// date-ish CalVer the concept doc mentions (2026.6.21) -- each is three
// dot-separated non-negative integers. That is exactly what the release
// tooling validates: scripts/release/release.sh enforces
// `^[0-9]+\.[0-9]+\.[0-9]+$` for the immutable image tag. We follow the
// same contract here -- no pre-release / build suffix -- so a cut version
// is always a clean X.Y.Z that release.sh will accept downstream.
package deploycontrol

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is a three-part numeric version (major.minor.patch). It models
// both classic semver and the CalVer (YYYY.M.D) scheme -- both are three
// non-negative integers.
type semver struct {
	major, minor, patch int
}

// parseSemver parses a clean three-part numeric version. It rejects
// anything release.sh would: a wrong segment count, empty/negative/
// non-numeric segments, or a pre-release/build suffix (the '-'/'+' make
// the segment non-numeric and fail).
func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver{}, fmt.Errorf("invalid version %q: empty", s)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid version %q: want three-part X.Y.Z (got %d segments)", s, len(parts))
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return semver{}, fmt.Errorf("invalid version %q: empty segment", s)
		}
		// strconv.Atoi rejects "+1" and any non-digit; "-1" parses to a
		// negative, caught below. This is the same shape release.sh's
		// numeric-only regex accepts.
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("invalid version %q: segment %q is not a non-negative integer", s, p)
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2]}, nil
}

// String renders the canonical X.Y.Z form (leading zeros dropped).
func (v semver) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// bump returns the version with one part incremented. "major" and
// "minor" zero the lower parts (2.4.7 -> major 3.0.0, minor 2.5.0);
// "patch" (the default, including the empty string) increments the
// patch (2.4.7 -> 2.4.8). Any other part is an error.
func (v semver) bump(part string) (semver, error) {
	switch part {
	case "major":
		return semver{major: v.major + 1}, nil
	case "minor":
		return semver{major: v.major, minor: v.minor + 1}, nil
	case "", "patch":
		return semver{major: v.major, minor: v.minor, patch: v.patch + 1}, nil
	default:
		return semver{}, fmt.Errorf("invalid bump %q: want major, minor, or patch", part)
	}
}

// compare returns -1 / 0 / +1 as v is less than / equal to / greater
// than o, comparing major then minor then patch.
func (v semver) compare(o semver) int {
	switch {
	case v.major != o.major:
		return sign(v.major - o.major)
	case v.minor != o.minor:
		return sign(v.minor - o.minor)
	case v.patch != o.patch:
		return sign(v.patch - o.patch)
	default:
		return 0
	}
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
