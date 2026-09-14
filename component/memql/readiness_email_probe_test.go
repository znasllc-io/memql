package memql

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

func emailModule() envregistry.Module {
	return envregistry.Module{
		Name: "email", Core: true, Description: "d",
		Evaluator: envregistry.EvaluatorIntegrationPrefix + "email",
	}
}

// integrations/email's statusAuthorized, restated over the same context surface
// it reads.
//
// A RESTATEMENT RATHER THAN AN IMPORT, because integrations/email requires
// component/memql and the reverse edge is a cycle -- the same module direction
// component/memql/readiness/inference.go argues at length.
//
// AND A RESTATEMENT IS A COPY, WHICH DRIFTS. This comment used to claim the
// case below "fails loudly if the real one grows stricter", and that was not
// true of anything here: a copy does not change when its original does, so a
// stricter real gate would leave every assertion in this file passing while
// the probe it stands for started refusing in production. What actually holds
// the two together is TestEmailStatusFloorMatchesTheIntegration below, which
// reads the real function out of its source file. Keep them in that order:
// this is the shape, that is the gate.
func statusAuthorizedRule(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return errors.New("email.status: no authenticated caller")
	}
	switch ac.Role {
	case auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper:
		return nil
	}
	return errors.New("email.status: role may not read integration configuration")
}

// WHY `email` READ notApplicable ON EVERY NODE OF EVERY CLUSTER.
//
// The design record recorded the cause as a lazy-sender timing problem -- "the
// rows are written before the lazy mail sender has resolved". IT IS NOT ONE.
// integrations/email/status.go's describer is deliberately a REPRODUCTION of
// the resolution algorithm ("neither trigger that resolution early nor be
// answered by a cache that predates a credential the operator has since
// seeded"), so it never materializes a sender and timing cannot reach it.
//
// The cause is the ACTOR. app/run.go's boot write passed context.Background(),
// the EVALUATION ran on that caller context, statusAuthorized refuses a context
// with no AccessContext at all, and evaluateModule mapped an errored probe to
// notApplicable. The mapping is `unknown` now (D1 of the 2026-09-14
// readiness-convergence record) -- a probe that could not run says nothing
// about whether a person did the setup, and "not hosted here" was the wrong
// word for it -- but the actor half of the bug is unchanged by that.
//
// Both halves are pinned here: an errored probe is never a verdict, and the
// evaluation context must not be the one that produces the error.
func TestAnUnauthorizedProbeIsWhatMadeEmailNotApplicable(t *testing.T) {
	// (1) The old shape. A bare context reaches the probe, the probe refuses,
	// and the verdict was notApplicable -- on every node, forever.
	refusing := readinessResolvers{
		IntegrationState: func(ctx context.Context, _ string) (string, bool, bool, error) {
			if err := statusAuthorizedRule(ctx); err != nil {
				return "", false, true, err
			}
			return "configured", true, true, nil
		},
	}
	// A PROBE THAT REFUSES MAPS TO unknown, which is the half of this that is
	// right: a probe that could not run says nothing about whether a person
	// did the setup, `unconfigured` would send them to a form they may not
	// need, and `notApplicable` -- what it used to read -- claimed the node
	// does not host the integration. Asserted with a resolver that refuses for
	// a reason no actor can fix, because the ACTOR reason is no longer
	// reachable through this entry point -- see (2).
	alwaysRefuses := readinessResolvers{
		IntegrationState: func(context.Context, string) (string, bool, bool, error) {
			return "", false, true, errors.New("email.status: the probe itself failed")
		},
	}
	if got := evaluateModule(context.Background(), alwaysRefuses, emailModule(), "n", "bff", inferenceNow); got.State != readiness.Unknown {
		t.Fatalf("a refused probe read %s. The mapping this test pins has changed, so the "+
			"regression pinned below can no longer be reproduced -- read evaluateModule's "+
			"integration arm before touching this test.", got.State)
	}

	// (2) The fix, through the SAME gate, from a BARE context.
	//
	// This is the assertion that moved when the evaluation actor moved. It used
	// to be applied by the caller, so this case had to hand it in by name; it
	// is applied inside `evaluateModule` now, at the one place a context
	// reaches a resolver, which means the old bug is no longer reachable
	// through this function AT ALL rather than merely not taken. Passing
	// `context.Background()` here is the whole point: the bare context that
	// produced notApplicable on every node of every cluster now produces the
	// right answer, because nothing between here and the probe can forget.
	got := evaluateModule(context.Background(), refusing, emailModule(), "n", "bff", inferenceNow)
	if got.State == readiness.Unknown || got.State == readiness.NotApplicable {
		t.Fatalf("a node carrying the email plug-in reports %s after boot: its own probe refused it. "+
			"evaluateModule must clear integrations/email's statusAuthorized, which "+
			"admits owner, admin or developer and nothing else.", got.State)
	}
	if got.State != readiness.Configured {
		t.Fatalf("email reports %s, want configured", got.State)
	}
}

// THE PARITY GATE THE RESTATEMENT ABOVE NEEDS (memql#5118).
//
// `statusAuthorizedRule` is a copy of a function in another module, and a copy
// is worth exactly as much as whatever holds it equal. Nothing did: the roles
// were written out here once, and integrations/email could have narrowed its
// floor to owner-only in a later release with every test in this file still
// green and the readiness probe silently refusing on every node again -- the
// exact bug this file exists to pin, returning by the one route the file did
// not watch.
//
// READ AT TEST TIME, which is the repo's answer wherever a boundary forbids
// the import (component/proving/capability reads the printf formats out of
// scripts/lib/capability.sh for the same reason). The source is the authority;
// this asserts the copy agrees with it.
func TestEmailStatusFloorMatchesTheIntegration(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own file, so the source cannot be read")
	}
	src := filepath.Join(filepath.Dir(thisFile), "..", "..", "integrations", "email", "status.go")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("cannot read %s: %v. The gate this file rests on is the real statusAuthorized; "+
			"if it moved, point this at its new home rather than dropping the check.", src, err)
	}

	// The function, from its signature to the closing brace at column 0.
	text := string(body)
	start := strings.Index(text, "func statusAuthorized(ctx context.Context) error {")
	if start < 0 {
		t.Fatal("integrations/email/status.go no longer declares statusAuthorized(ctx) error. " +
			"The readiness probe's authorization floor is whatever replaced it -- find it and " +
			"re-point this gate, because statusAuthorizedRule above is now a copy of nothing.")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("statusAuthorized has no closing brace at column 0; this reader cannot bound it")
	}
	fn := text[start : start+end]

	// EVERY ROLE THE LADDER HAS, checked in both directions. Naming only the
	// three admitted ones would pass a gate that had ALSO started admitting
	// `writer` -- looser is a change too, and a probe that suddenly runs for
	// everybody is a different report from one that runs for nobody.
	admits := map[auth.Role]bool{auth.RoleOwner: true, auth.RoleAdmin: true, auth.RoleDeveloper: true}
	for _, role := range auth.ValidRoles() {
		named := strings.Contains(fn, "auth.Role"+strings.ToUpper(string(role)[:1])+string(role)[1:])
		if named != admits[role] {
			t.Errorf("integrations/email's statusAuthorized %s %q, and statusAuthorizedRule in this "+
				"file %s it. The copy has drifted from the original, which is the one failure a "+
				"restatement can have: every case here would go on passing while the real probe "+
				"answered differently on every node.",
				map[bool]string{true: "names", false: "does not name"}[named], role,
				map[bool]string{true: "admits", false: "refuses"}[admits[role]])
		}
	}
}

// The gate this leans on, stated as its own case so a reader can see WHICH
// property of the evaluation context does the work. RoleReader -- what
// readinessWriteContext deliberately carries -- would not clear it, which is
// why the two contexts are two.
func TestTheEvaluationActorClearsTheIntegrationStatusFloor(t *testing.T) {
	if err := statusAuthorizedRule(readinessEvaluateContext(context.Background())); err != nil {
		t.Fatalf("the evaluation actor is refused by the integration status floor: %v", err)
	}
	if err := statusAuthorizedRule(readinessWriteContext(context.Background())); err == nil {
		t.Fatal("the WRITE context clears the integration status floor. It carries RoleReader on " +
			"purpose -- it writes a public concept behind @serverOnly plus internal origin and needs " +
			"nothing more -- so if it now clears this, the two contexts have converged and the " +
			"argument for having two has gone with them.")
	}
}
