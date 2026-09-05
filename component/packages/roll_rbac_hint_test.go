package packages

import (
	"testing"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// The roll's 403 hint names an env var this package owns, and names it as a
// LITERAL because the dependency runs the other way (memql#4933).
//
// `component/packages` imports `component/deploycontrol` -- the roller calls
// RestartDeployment -- so deploycontrol cannot import RollTargetsEnv back
// without closing a cycle. It therefore spells the name itself, and this is
// what keeps the two copies equal. A renamed variable with a stale hint sends
// an operator to set something that does not exist, at the exact moment they
// are already looking at a 403 from the last stage of a deploy that otherwise
// succeeded.
//
// This test can live here and not there for the same reason the literal has to
// exist: only this side can see both.
func TestRollTargetsEnvNameMatchesTheOwner(t *testing.T) {
	if got := deploycontrol.RollTargetsEnvNameForHint(); got != RollTargetsEnv {
		t.Fatalf("deploycontrol's 403 hint names %q; this package's variable is %q",
			got, RollTargetsEnv)
	}
}
