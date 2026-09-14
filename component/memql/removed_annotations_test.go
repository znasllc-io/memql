package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// TestRemovedAnnotationsRejected locks in #989: the recognized-but-unused and
// retired annotations are gone from the function receivers, so each is
// refused -- since memql#5359 at parse time, by the annotation registry --
// instead of slipping through to the generic attribute map. Zero in-tree
// occurrences (#964) backs the hard cutover -- there is nothing to migrate.
func TestRemovedAnnotationsRejected(t *testing.T) {
	cases := []struct {
		annotation string
		receiver   annotations.Receiver
	}{
		// Query removals.
		{"deprecated", annotations.Query},
		{"cacheTTL", annotations.Query},
		{"timeout", annotations.Query},
		{"rateLimit", annotations.Query},
		{"retry", annotations.Query},
		{"audit", annotations.Query},
		{"role", annotations.Query},
		{"permission", annotations.Query},
		// Mutation removals.
		{"idempotent", annotations.Mutation},
		{"destructive", annotations.Mutation},
		{"requiresConfirmation", annotations.Mutation},
		{"audit", annotations.Mutation},
		// Logic removal.
		{"deprecated", annotations.Logic},
		// Automation removals.
		{"async", annotations.Automation},
		{"deprecated", annotations.Automation},
	}

	for _, tc := range cases {
		t.Run(string(tc.receiver)+"/@"+tc.annotation, func(t *testing.T) {
			err := runReceiverGate(tc.receiver, receiverFixture(tc.receiver, "", "@"+tc.annotation))
			code := refusalCode(err)
			if code == "" {
				t.Fatalf("expected @%s on %s to be refused by the registry, got: %v", tc.annotation, tc.receiver.Phrase(), err)
			}
			// @role (#2709) and @permission (#2713) graduated from the generic
			// unknown-annotation error to the pointed BURY message, the #989
			// removals (and @async, refused on automations since #2712) to a
			// retirement naming that history (memql#5360), and epic memql#5375
			// retired @deprecated / @latestMode / @enabled / @rateLimit /
			// @scopes / @schedule and their siblings. The rejection this test
			// locks in is preserved either way -- sharper, not weaker.
			//
			// The branch asks the registry's TABLES rather than a literal list,
			// so the next retirement needs no edit here: whatever the registry
			// says is retired must refuse with CodeRetired and carry its hint.
			if hint, isRetired := annotations.RetiredHint(tc.receiver, tc.annotation); isRetired {
				if code != annotations.CodeRetired {
					t.Fatalf("expected @%s on %s to be refused as retired, got %s: %v", tc.annotation, tc.receiver.Phrase(), code, err)
				}
				if !strings.Contains(err.Error(), hint) {
					t.Fatalf("expected @%s to refuse with its registry hint (%q), got: %v", tc.annotation, hint, err)
				}
				return
			}
			// @rateLimit / @destructive / @requiresConfirmation are live on a
			// tool, so they are misplaced here rather than unknown.
			if code != annotations.CodeUnknown && code != annotations.CodeMisplaced {
				t.Fatalf("expected @%s on %s to be unknown or misplaced, got %s: %v", tc.annotation, tc.receiver.Phrase(), code, err)
			}
		})
	}
}

// TestKeptAnnotationsStillAccepted guards against over-removal: the surviving
// construct annotations still pass the gate.
func TestKeptAnnotationsStillAccepted(t *testing.T) {
	cases := []struct {
		annotation string
		receiver   annotations.Receiver
	}{
		{`@description("d")`, annotations.Query},
		{"@public", annotations.Query},
		{"@actor", annotations.Mutation},
		{`@trigger(event="x")`, annotations.Automation},
		{"@filter(row => row.a == 1)", annotations.Automation},
		// @schedule reinstated on automations (#2712): LIVE (folds to the
		// honored AutomationDef.Schedule / cron scheduler).
	}

	for _, tc := range cases {
		t.Run(string(tc.receiver)+"/"+tc.annotation, func(t *testing.T) {
			if err := runReceiverGate(tc.receiver, receiverFixture(tc.receiver, "", tc.annotation)); err != nil {
				t.Fatalf("expected %s on %s to be accepted, got: %v", tc.annotation, tc.receiver.Phrase(), err)
			}
		})
	}
}
