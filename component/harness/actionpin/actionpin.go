// Package actionpin implements Phase 5 of the action library (#1740, epic
// #1734): version pinning of action references and the verified upgrade
// migration.
//
// References pin by default ("act_writeFile@3") -- reproducibility is the whole
// promise, so a referenced action cannot change underneath a bundle unless the
// bundle is deliberately, verifiably migrated. Floating ("act_fmt") is an
// explicit opt-in for actions trusted to always improve.
//
// The migration is deliberate and checked: when an action gets a better
// version, the upgrade walks every bundle pinned to the old version, re-runs
// each against the bundle's recorded fingerprint, and bumps the pin ONLY where
// it still verifies. Non-verifying bundles stay pinned and are flagged. The
// re-run itself is the caller's (it needs the live replay path); this package
// owns the parse/format of pins and the bump-or-hold decision, which is the
// part that must be exactly right.
package actionpin

import (
	"fmt"
	"strconv"
	"strings"
)

// Ref is a parsed action reference: an action id, optionally pinned to a
// version. Floating refs track the latest version.
type Ref struct {
	ID       string
	Version  int // 0 when floating
	Floating bool
}

// Parse parses an action reference: "act_x@3" pins version 3; "act_x" floats.
func Parse(ref string) (Ref, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Ref{}, fmt.Errorf("actionpin: empty action ref")
	}
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return Ref{ID: ref, Floating: true}, nil
	}
	id := ref[:at]
	v, err := strconv.Atoi(ref[at+1:])
	if err != nil || v < 1 || id == "" {
		return Ref{}, fmt.Errorf("actionpin: invalid pinned ref %q", ref)
	}
	return Ref{ID: id, Version: v}, nil
}

// Format renders a pinned ref as "id@version"; a floating ref is just the id.
func Format(r Ref) string {
	if r.Floating || r.Version < 1 {
		return r.ID
	}
	return fmt.Sprintf("%s@%d", r.ID, r.Version)
}

// UpgradeDecision is the per-bundle verdict of an upgrade migration.
type UpgradeDecision string

const (
	// Bump: the candidate new version reproduced the recorded fingerprint --
	// the pin is bumped (a verified upgrade).
	Bump UpgradeDecision = "bump"
	// Hold: the new version did not verify -- the bundle stays pinned and is
	// flagged for review.
	Hold UpgradeDecision = "hold"
)

// Decide returns Bump iff the candidate new version reproduced the reference's
// recorded fingerprint on re-run; otherwise Hold. An empty recorded
// fingerprint is unverifiable, so it holds (we never silently bump a pin we
// can't check).
func Decide(recordedFingerprint, replayFingerprint string) UpgradeDecision {
	if recordedFingerprint != "" && recordedFingerprint == replayFingerprint {
		return Bump
	}
	return Hold
}

// PinnedBundle is a bundle's reference to an action version, with the
// fingerprint the bundle recorded for that action.
type PinnedBundle struct {
	BundleID            string
	Ref                 Ref
	RecordedFingerprint string
}

// MigrationResult is the per-bundle outcome of an upgrade migration.
type MigrationResult struct {
	BundleID    string
	FromVersion int
	ToVersion   int
	Decision    UpgradeDecision
	NewRef      string
	Flagged     bool // true when held (non-verifying); needs review
}

// PlanUpgrade computes the migration to upgrade each pinned bundle to
// newVersion. replayFingerprint re-runs a bundle against the new version and
// returns the resulting fingerprint (the caller's live replay). Pin-by-
// default: a bundle is bumped ONLY where its recorded fingerprint still
// verifies; non-verifying bundles stay on their old version and are flagged.
// Floating refs already track latest, so they are reported as bumped without a
// re-run.
func PlanUpgrade(bundles []PinnedBundle, newVersion int, replayFingerprint func(PinnedBundle) string) []MigrationResult {
	out := make([]MigrationResult, 0, len(bundles))
	for _, b := range bundles {
		if b.Ref.Floating {
			out = append(out, MigrationResult{
				BundleID:    b.BundleID,
				FromVersion: 0,
				ToVersion:   newVersion,
				Decision:    Bump,
				NewRef:      b.Ref.ID,
			})
			continue
		}
		fp := ""
		if replayFingerprint != nil {
			fp = replayFingerprint(b)
		}
		res := MigrationResult{BundleID: b.BundleID, FromVersion: b.Ref.Version, Decision: Decide(b.RecordedFingerprint, fp)}
		if res.Decision == Bump {
			res.ToVersion = newVersion
			res.NewRef = Format(Ref{ID: b.Ref.ID, Version: newVersion})
		} else {
			res.ToVersion = b.Ref.Version // stays pinned
			res.NewRef = Format(b.Ref)
			res.Flagged = true
		}
		out = append(out, res)
	}
	return out
}

// CountFlagged returns how many results were held (non-verifying) -- a CI
// upgrade gate can fail when this is non-zero.
func CountFlagged(results []MigrationResult) int {
	n := 0
	for _, r := range results {
		if r.Flagged {
			n++
		}
	}
	return n
}
