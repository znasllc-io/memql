package auth

import (
	"context"
	"testing"
)

// The mark says one thing about one context: this is a person's authority lent
// to a visitor's request. It must not leak to the context it was made from, it
// must survive being derived from, and it must change nothing about who acts.
func TestVisitorBorrowMarksOnlyItsOwnContext(t *testing.T) {
	owner := ContextWithUserActor(context.Background(), "u-merchant")
	if BorrowedForVisitor(owner) {
		t.Fatal("a context nobody marked reads as a visitor's")
	}

	visitor := ContextWithVisitorBorrow(owner)
	if !BorrowedForVisitor(visitor) {
		t.Fatal("the marked context does not read as a visitor's")
	}
	if BorrowedForVisitor(owner) {
		t.Fatal("marking a context marked the one it was made from")
	}

	// Still the owner acting, with nothing else changed.
	ac, ok := AccessFromContext(visitor)
	if !ok || ac == nil || ac.UserId != "u-merchant" || ac.Synthetic {
		t.Fatalf("the marked context acts as %+v, want the owner unchanged", ac)
	}

	// A context derived from it is the same request.
	derived, cancel := context.WithCancel(ContextWithCallAccount(visitor, "acct-1"))
	defer cancel()
	if !BorrowedForVisitor(derived) {
		t.Fatal("a context derived from a visitor's lost the mark")
	}

	if BorrowedForVisitor(nil) { //nolint:staticcheck // a nil context is the case under test
		t.Fatal("a nil context reads as a visitor's")
	}
	if !BorrowedForVisitor(ContextWithVisitorBorrow(nil)) { //nolint:staticcheck // as above
		t.Fatal("marking a nil context made no mark")
	}
}
