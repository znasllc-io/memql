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
			// @role (#2709) and @permission (#2713) graduated from the
			// generic unknown-annotation error to the pointed BURY
			// message, and the #989 removals (and @async, refused on
			// automations since #2712) to a retirement naming that history
			// (memql#5360) -- the rejection this test locks in is
			// preserved, sharper.
			retired := map[string]string{
				"role": "#2709", "permission": "#2713",
				"timeout": "memql#989", "retry": "memql#989", "audit": "memql#989", "idempotent": "memql#989",
				"async": "memql#2712",
			}
			if ticket := retired[tc.annotation]; ticket != "" {
				if code != annotations.CodeRetired || !strings.Contains(err.Error(), ticket) {
					t.Fatalf("expected @%s retired with its history (%s), got: %v", tc.annotation, ticket, err)
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
		{"@filter(a == 1)", annotations.Automation},
		// @schedule reinstated on automations (#2712): LIVE (folds to the
		// honored AutomationDef.Schedule / cron scheduler).
		{`@schedule(cron="0 5 9 * * *")`, annotations.Automation},
	}

	for _, tc := range cases {
		t.Run(string(tc.receiver)+"/"+tc.annotation, func(t *testing.T) {
			if err := runReceiverGate(tc.receiver, receiverFixture(tc.receiver, "", tc.annotation)); err != nil {
				t.Fatalf("expected %s on %s to be accepted, got: %v", tc.annotation, tc.receiver.Phrase(), err)
			}
		})
	}
}
