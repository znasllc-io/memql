package campaigns

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/email"
)

// feedback_test.go -- what a bounce does, and who may say so.
//
// The product decision under test: a HARD bounce suppresses the address
// cluster-wide and leaves the audience membership in place; a SOFT bounce
// does neither. The membership is kept because deleting it destroys the
// audit trail AND makes the address resurrectable by the next import,
// which is how a sender ends up re-mailing known-dead addresses.

func adminCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "admin-1", Role: role})
}

func feedbackWorker(engine *fakeEngine) *Worker {
	w := &Worker{
		store:   NewStore(engine),
		logger:  quietLogger(),
		cfg:     Config{UnsubscribeSecret: "s", UnsubscribeBaseURL: "https://example.test"},
		resolve: func() email.Sender { return &recordingSender{} },
	}
	return w
}

func TestHardBounceSuppressesAndKeepsMembership(t *testing.T) {
	engine := &fakeEngine{}
	w := feedbackWorker(engine)

	if _, err := w.handleRecordFeedback(adminCtx(auth.RoleAdmin), map[string]any{
		"email": "dead@example.test",
		"kind":  "hard_bounce",
		"note":  "550 5.1.1 user unknown",
	}, 0); err != nil {
		t.Fatalf("recordFeedback: %v", err)
	}

	sup := engine.mutations("recordSuppression")
	if len(sup) != 1 {
		t.Fatalf("want one suppression write, got %d", len(sup))
	}
	if got := argOf(sup[0].query, "reason"); got != "hard_bounce" {
		t.Errorf("reason = %q, want hard_bounce", got)
	}
	if got := argOf(sup[0].query, "emailDigest"); got != EmailDigest("dead@example.test") {
		t.Errorf("suppression was not keyed by the address digest: %q", got)
	}
	if strings.Contains(sup[0].query, "dead@example.test") {
		t.Error("the plaintext address reached the graph; the whole point of a digest-keyed list is that it does not")
	}
	if got := argOf(sup[0].query, "domain"); got != "example.test" {
		t.Errorf("domain = %q, want example.test -- a deliverability review asks which domains bounce", got)
	}

	// MEMBERSHIP IS NOT DELETED. Nothing here removes a recipient row;
	// the send path converges it to `bounced` on the next run.
	for _, name := range []string{"deleteRecipient", "removeRecipient"} {
		if len(engine.mutations(name)) != 0 {
			t.Errorf("%s was called; a hard bounce must not delete audience membership", name)
		}
	}
}

func TestSoftBounceDoesNotSuppress(t *testing.T) {
	engine := &fakeEngine{}
	w := feedbackWorker(engine)

	if _, err := w.handleRecordFeedback(adminCtx(auth.RoleAdmin), map[string]any{
		"email": "full@example.test",
		"kind":  "soft_bounce",
	}, 0); err != nil {
		t.Fatalf("recordFeedback: %v", err)
	}
	if n := len(engine.mutations("recordSuppression")); n != 0 {
		t.Fatalf("a soft bounce wrote %d suppression rows; suppressing on a transient condition loses real subscribers", n)
	}
}

func TestComplaintSuppresses(t *testing.T) {
	engine := &fakeEngine{}
	w := feedbackWorker(engine)

	if _, err := w.handleRecordFeedback(adminCtx(auth.RoleOwner), map[string]any{
		"email": "angry@example.test",
		"kind":  "complaint",
	}, 0); err != nil {
		t.Fatalf("recordFeedback: %v", err)
	}
	sup := engine.mutations("recordSuppression")
	if len(sup) != 1 || argOf(sup[0].query, "reason") != "complaint" {
		t.Fatalf("complaint did not suppress: %+v", sup)
	}
}

// TestClusterWideWritesRequireAdmin -- the list spans every operator, so
// adding to it is deployment policy. A writer must not be able to
// suppress an address for the whole cluster.
func TestClusterWideWritesRequireAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		engine := &fakeEngine{}
		w := feedbackWorker(engine)
		if _, err := w.handleSuppress(adminCtx(role), map[string]any{"email": "x@example.test"}, 0); err == nil {
			t.Errorf("role %q was allowed to write the cluster-wide suppression list", role)
		}
		if n := len(engine.mutations("recordSuppression")); n != 0 {
			t.Errorf("role %q wrote %d suppression rows despite the gate", role, n)
		}
	}
	// And an unauthenticated call is refused rather than defaulting open.
	engine := &fakeEngine{}
	w := feedbackWorker(engine)
	if _, err := w.handleSuppress(context.Background(), map[string]any{"email": "x@example.test"}, 0); err == nil {
		t.Error("an unauthenticated caller was allowed to write the cluster-wide suppression list")
	}
}

func TestAdminAndOwnerMayWriteTheList(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOwner} {
		engine := &fakeEngine{}
		w := feedbackWorker(engine)
		if _, err := w.handleSuppress(adminCtx(role), map[string]any{"email": "x@example.test"}, 0); err != nil {
			t.Errorf("role %q was refused: %v", role, err)
		}
	}
}

func TestSuppressRejectsAnUnusableAddress(t *testing.T) {
	engine := &fakeEngine{}
	w := feedbackWorker(engine)
	_, err := w.handleSuppress(adminCtx(auth.RoleAdmin), map[string]any{"email": "not-an-address"}, 0)
	if err == nil {
		t.Fatal("an unusable address was accepted; its digest would be empty and would suppress nothing while appearing to work")
	}
	if strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("the rejected address appears verbatim in an error that will be logged: %v", err)
	}
}

// TestRecipientStatusForCoversEverySuppressionReason -- the convergence
// map has to answer for every reason the enum can hold, or a suppressed
// recipient row silently stays `subscribed` in the operator's view.
func TestRecipientStatusForCoversEverySuppressionReason(t *testing.T) {
	for reason, want := range map[string]string{
		"unsubscribed": "unsubscribed",
		"manual":       "unsubscribed",
		"hard_bounce":  "bounced",
		"complaint":    "complained",
	} {
		if got := recipientStatusFor(reason); got != want {
			t.Errorf("recipientStatusFor(%q) = %q, want %q", reason, got, want)
		}
	}
}
