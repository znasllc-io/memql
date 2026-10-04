//go:build agent

package worker

import (
	"fmt"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
)

// EVERY APP GATE CODE HAS WORDS ON THE DECISION ROW. A refused app door is
// read in Settings > Decisions through its `considered` line, which the
// router writes as the refusal's code and the router's own words -- never the
// refusal's message, which names people (component/router failure_reason.go).
// The router keeps those words keyed by these codes, so a code added or
// renamed here without its words there would print a bare code. The refusal
// is wrapped exactly as AppInference.Call returns it.
func TestEveryAppGateRefusalHasWordsOnTheDecisionRow(t *testing.T) {
	for _, code := range []string{AppGateNoOwner, AppGateKillSwitchEngaged, AppGateKillSwitchUnreadable, AppGateNotNamedByOwner} {
		refusal := &appGateRefusal{code: code, message: "a message naming alice@example.com"}
		line := router.FailureReason(fmt.Errorf("%w: %w", memqlengine.ErrAppUnavailable, refusal))
		if !strings.HasPrefix(line, "failed when called: "+code+": ") {
			t.Errorf("the decision row's line for %s is %q, want the code and the router's words for it", code, line)
		}
		if strings.Contains(line, "alice@example.com") {
			t.Errorf("the decision row's line for %s carries the refusal's message: %q", code, line)
		}
	}
}
