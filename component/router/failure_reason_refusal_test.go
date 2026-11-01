package router

// failure_reason_refusal_test.go -- the router's OWN refusal is named by its
// code in a failed-attempt line.
//
// FailureReason names a failure by a stable code and the router's words, and
// never by the error's text. The typed refusals the app door raises expose
// that code through Code(); the router's own refusal (every_door_shut,
// ceiling_reached) keeps it in a FIELD called Code, so it answers through the
// door report's RefusalCode() instead. Before this, such an error fell through
// to the ledger's categories and read "failed when called: other", which
// dropped the one fact the line exists to give -- and a reader that matches the
// code out of the line's text (work.InferenceRefusalCode) found none.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

func TestTheRoutersOwnRefusalIsNamedByItsCode(t *testing.T) {
	// A door's reason can quote whatever a machine or a vendor said, a
	// hostname-shaped reported name included.
	const hostname = "Joses-MacBook-Pro"
	doors := []work.DoorReport{{Name: "fleet:strongest", Door: "fleet", Reason: hostname + ": refused before start"}}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"no door is open", &InferenceUnavailable{Code: work.RefusalEveryDoorShut, Doors: doors},
			"failed when called: every_door_shut: no door to a model is open for this call"},
		{"the cost ceiling is reached", &InferenceUnavailable{Code: work.RefusalCeilingReached, Doors: doors, CeilingReason: hostname + " spent it"},
			"failed when called: ceiling_reached: a paid provider could have served this call and the cost ceiling is reached"},
		{"under whatever a caller wrapped it in", fmt.Errorf("independent review: %w", &InferenceUnavailable{Code: work.RefusalEveryDoorShut, Doors: doors}),
			"failed when called: every_door_shut: no door to a model is open for this call"},
	} {
		got := FailureReason(c.err)
		if got != c.want {
			t.Errorf("%s: FailureReason = %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, hostname) {
			t.Errorf("%s: the line carries a door's text: %q", c.name, got)
		}
		// The line must still carry the code a message matcher looks for.
		if code, ok := work.InferenceRefusalCode(got); !ok || !strings.Contains(c.want, code) {
			t.Errorf("%s: InferenceRefusalCode(%q) = %q, %v", c.name, got, code, ok)
		}
	}
}
