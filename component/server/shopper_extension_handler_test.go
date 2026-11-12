package server

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// shopper_extension_handler_test.go -- the two-construct form post (design
// record 2026-09-21, section 4.3).

// extendReviews attaches a client domain's own fields to the fixture's form.
func extendReviews(t *testing.T) {
	t.Helper()
	err := memql.RegisterShopperExtension(memql.ShopperExtension{
		Domain: "fylo", Pack: "reviews", Form: "review",
		Construct: "recordFyloReviewDetail",
		Fields: []memql.ShopperField{
			{Name: "address", Required: true, MaxLength: 200},
			{Name: "ein", MaxLength: 20},
		},
	})
	if err != nil {
		t.Fatalf("setup: the extension was refused: %v", err)
	}
}

var submissionIDPattern = regexp.MustCompile(`submissionId:\s*"([^"]+)"`)

func submissionIDIn(t *testing.T, call string) string {
	t.Helper()
	m := submissionIDPattern.FindStringSubmatch(call)
	if m == nil {
		t.Fatalf("the call carries no submissionId: %s", call)
	}
	return m[1]
}

// THE HAPPY PATH, and the ordering in it is load-bearing: the pack's
// construct holds the gate (applicationsOpen, for wholesale), so it must
// decide before the client's row is written.
func TestAnExtendedFormRunsThePackThenTheExtension(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St&ein=12-3456789", goodStamp())

	if len(exec.calls) != 2 {
		t.Fatalf("Execute was called %d times, want 2 (the pack's construct and the extension's): %v",
			len(exec.calls), exec.calls)
	}
	if !strings.Contains(exec.calls[0], "submitReview") {
		t.Errorf("the FIRST call is not the pack's construct: %s", exec.calls[0])
	}
	if !strings.Contains(exec.calls[1], "recordFyloReviewDetail") {
		t.Errorf("the SECOND call is not the extension's mutation: %s", exec.calls[1])
	}
	// The client's fields reach the client's construct and NOT the pack's --
	// this is the whole point of the seam.
	if strings.Contains(exec.calls[0], "ein") {
		t.Errorf("a client field reached the pack's construct: %s", exec.calls[0])
	}
	if !strings.Contains(exec.calls[1], "12-3456789") {
		t.Errorf("the client's EIN did not reach the client's mutation: %s", exec.calls[1])
	}
	// ONE SUBMISSION, ONE ID. It is what the extension's @relationship
	// points at, and the pack's row id.
	if got, want := submissionIDIn(t, exec.calls[1]), submissionIDIn(t, exec.calls[0]); got != want {
		t.Errorf("the two constructs were stamped different submission ids: %q and %q", want, got)
	}
	// ONE SUBMISSION, ONE ACCOUNT. The client's row is the other half of the
	// pack's, and both act in the site's account: two halves recorded in two
	// accounts would be a submission no single account holds whole.
	for i, half := range []string{"the pack's construct", "the extension's mutation"} {
		if exec.account[i] != "acct-merchant" {
			t.Errorf("%s acts in account %q, want the site's", half, exec.account[i])
		}
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// THE TEST D5 RESTS ON. "Both field sets are validated before anything is
// written" is a property or it is a comment.
func TestABadExtensionFieldWritesNothingAtAll(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)

	// address is required by the EXTENSION and absent. The pack's own
	// fields are all present and valid.
	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	if len(exec.calls) != 0 {
		t.Fatalf("Execute was called %d times; a submission refused by the extension's rules "+
			"must not write the pack's row first: %v", len(exec.calls), exec.calls)
	}
	assertRedirect(t, rec, "/reviews/problem", "invalid")
}

// D5, the owner's own answer: the application stands and the shopper is not
// sent to an error page from which they would resubmit.
func TestAFailedExtensionWriteLeavesThePackRowAndRedirectsOK(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)
	exec.errForCall = func(n int, _ string) error {
		if n == 1 {
			return errors.New("transient")
		}
		return nil
	}

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St", goodStamp())

	if len(exec.calls) != 2 {
		t.Fatalf("Execute was called %d times, want 2", len(exec.calls))
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// A refusal by the PACK ends the request: the client's row must not be
// written against an application that does not exist.
func TestAPackRefusalStopsTheExtension(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)
	exec.errForCall = func(n int, _ string) error {
		if n == 0 {
			return errors.New("applications are closed")
		}
		return nil
	}

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("Execute was called %d times, want 1: the extension must not run after the "+
			"pack refused: %v", len(exec.calls), exec.calls)
	}
	assertRedirect(t, rec, "/reviews/problem", "failed")
}

// A post the pack's construct ACCEPTED AND DID NOT WRITE (memql.ShopperOutcome)
// is answered as an accepted one, and the client's row is not written about a
// submission that does not exist.
func TestAnAcceptedPostThePackDidNotWriteStopsTheExtension(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)
	exec.onCall = func(n int, ctx context.Context) {
		if n == 0 {
			memql.MarkShopperSubmissionUnwritten(ctx)
		}
	}

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("Execute was called %d times, want 1: the extension must not run after the "+
			"pack wrote nothing: %v", len(exec.calls), exec.calls)
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// The stamp is the server's. A form that could supply its own submission id
// could aim a client row at another shopper's application.
func TestAFormSuppliedSubmissionIdCannotWin(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	extendReviews(t)

	shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St&submissionId=someone-elses", goodStamp())

	if len(exec.calls) == 0 {
		t.Fatal("nothing was executed")
	}
	for i, call := range exec.calls {
		if strings.Contains(call, "someone-elses") {
			t.Errorf("call %d carries the caller's own submission id: %s", i, call)
		}
	}
}

// THE REGRESSION. A route nobody extends behaves exactly as it did.
func TestAnUnextendedFormRunsOneConstruct(t *testing.T) {
	h, exec, _ := shopperFixture(t)

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("Execute was called %d times, want 1: %v", len(exec.calls), exec.calls)
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// extendReviewsWithAccount is extendReviews for an extension whose mutation
// declares an accountId argument.
func extendReviewsWithAccount(t *testing.T) {
	t.Helper()
	err := memql.RegisterShopperExtension(memql.ShopperExtension{
		Domain: "fylo", Pack: "reviews", Form: "review",
		Construct:      "recordFyloReviewDetail",
		Fields:         []memql.ShopperField{{Name: "address", Required: true, MaxLength: 200}},
		AcceptsAccount: true,
	})
	if err != nil {
		t.Fatalf("setup: the extension was refused: %v", err)
	}
}

// AN EXTENSION THAT ASKS FOR THE ACCOUNT IS HANDED THE SITE'S, so the client's
// own row is readable by the same organization as the pack's.
func TestTheHandlerStampsTheAccountIntoAnExtensionThatAcceptsIt(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	sites.site.AccountID = "acct-north"
	extendReviewsWithAccount(t)

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St&accountId=acct-other", goodStamp())

	if len(exec.calls) != 2 {
		t.Fatalf("Execute was called %d times, want 2: %v", len(exec.calls), exec.calls)
	}
	if !strings.Contains(exec.calls[1], `accountId: "acct-north"`) {
		t.Fatalf("the extension was not handed the site's organization: %s", exec.calls[1])
	}
	if strings.Contains(exec.calls[1], "acct-other") {
		t.Fatalf("a form-supplied accountId reached the extension: %s", exec.calls[1])
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// AN EXTENSION THAT DECLARES NO accountId IS HANDED NONE and still writes: a
// client mutation written before organizations reached the shopper path keeps
// working unchanged.
func TestAnExtensionWithoutAnAccountArgumentIsNotHandedOne(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	sites.site.AccountID = "acct-north"
	extendReviews(t)

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&address=12+Main+St", goodStamp())

	if len(exec.calls) != 2 {
		t.Fatalf("Execute was called %d times, want 2: %v", len(exec.calls), exec.calls)
	}
	if strings.Contains(exec.calls[1], "accountId") {
		t.Fatalf("an extension that declares no accountId was handed one: %s", exec.calls[1])
	}
	// Each asks for itself: the review form does not ask either, and is
	// handed none; both still act in the site's account.
	if strings.Contains(exec.calls[0], "accountId") {
		t.Fatalf("the form's construct was handed an accountId it did not ask for: %s", exec.calls[0])
	}
	for i, account := range exec.account {
		if account != "acct-north" {
			t.Fatalf("call %d acts in account %q, want the site's", i, account)
		}
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}
