// Command action-upgrade is the Phase 5 verified upgrade-migration tool for
// the action library (#1740, epic #1734).
//
// References pin by default ("act_x@3"); an action improvement is rolled out
// as a CHECKED migration, never a surprise. This tool takes a migration input
// describing every bundle pinned to the old version of an action, plus the
// fingerprint each bundle produced when re-run against the new version, and
// emits the migration plan: it bumps a pin ONLY where the bundle's recorded
// fingerprint still verifies, holds + flags the rest, and exits non-zero when
// any bundle was held -- so CI can gate a library upgrade.
//
// The replay fingerprints are supplied as input (a prior live run against the
// engine computes them); this tool owns the verified bump-or-hold decision via
// component/harness/actionpin. The live library walk that produces the input
// rides the action-reference mechanism (composite/step-type refs) and is the
// remaining integration surface.
//
// Usage:
//
//	action-upgrade -in migration.json [-json]
//
// Input JSON:
//
//	{
//	  "actionId": "act_writeFile",
//	  "newVersion": 4,
//	  "bundles": [
//	    {"bundleId": "b1", "ref": "act_writeFile@3", "recordedFingerprint": "abc", "replayFingerprint": "abc"},
//	    {"bundleId": "b2", "ref": "act_writeFile@3", "recordedFingerprint": "abc", "replayFingerprint": "xyz"}
//	  ]
//	}
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/znasllc-io/memql/component/harness/actionpin"
)

type inputBundle struct {
	BundleID            string `json:"bundleId"`
	Ref                 string `json:"ref"`
	RecordedFingerprint string `json:"recordedFingerprint"`
	ReplayFingerprint   string `json:"replayFingerprint"`
}

type migrationInput struct {
	ActionID   string        `json:"actionId"`
	NewVersion int           `json:"newVersion"`
	Bundles    []inputBundle `json:"bundles"`
}

func main() {
	inPath := flag.String("in", "", "path to the migration input JSON (required)")
	asJSON := flag.Bool("json", false, "emit the migration plan as JSON")
	flag.Parse()

	if *inPath == "" {
		fmt.Fprintln(os.Stderr, "action-upgrade: -in <migration.json> is required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "action-upgrade: read %s: %v\n", *inPath, err)
		os.Exit(2)
	}
	var in migrationInput
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintf(os.Stderr, "action-upgrade: parse %s: %v\n", *inPath, err)
		os.Exit(2)
	}
	if in.NewVersion < 1 {
		fmt.Fprintln(os.Stderr, "action-upgrade: newVersion must be >= 1")
		os.Exit(2)
	}

	// Build the pinned-bundle set + a replay function over the supplied
	// fingerprints (keyed by bundle id).
	replayByBundle := map[string]string{}
	bundles := make([]actionpin.PinnedBundle, 0, len(in.Bundles))
	for _, b := range in.Bundles {
		ref, perr := actionpin.Parse(b.Ref)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "action-upgrade: bundle %q: %v\n", b.BundleID, perr)
			os.Exit(2)
		}
		replayByBundle[b.BundleID] = b.ReplayFingerprint
		bundles = append(bundles, actionpin.PinnedBundle{
			BundleID:            b.BundleID,
			Ref:                 ref,
			RecordedFingerprint: b.RecordedFingerprint,
		})
	}

	results := actionpin.PlanUpgrade(bundles, in.NewVersion, func(b actionpin.PinnedBundle) string {
		return replayByBundle[b.BundleID]
	})
	flagged := actionpin.CountFlagged(results)

	if *asJSON {
		out, _ := json.MarshalIndent(map[string]any{
			"actionId":   in.ActionID,
			"newVersion": in.NewVersion,
			"results":    results,
			"flagged":    flagged,
		}, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("Upgrade migration for %s -> v%d (%d bundle(s))\n", in.ActionID, in.NewVersion, len(results))
		for _, r := range results {
			marker := "BUMP   "
			if r.Flagged {
				marker = "HOLD*  "
			}
			fmt.Printf("  %s %-20s %s\n", marker, r.BundleID, r.NewRef)
		}
		if flagged > 0 {
			fmt.Printf("\n%d bundle(s) HELD (did not verify against v%d) -- left pinned, review required.\n", flagged, in.NewVersion)
		} else {
			fmt.Printf("\nAll bundles verified and bumped to v%d.\n", in.NewVersion)
		}
	}

	if flagged > 0 {
		os.Exit(1) // CI gate: a held bundle fails the upgrade
	}
}
