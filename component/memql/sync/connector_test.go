package sync

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubConnector is a Connector that serves nothing. Every contract
// method answers NotImplemented, which is what a connector filling in
// one direction at a time looks like.
type stubConnector struct{ name string }

func (s stubConnector) Name() string          { return s.name }
func (s stubConnector) Domains() []DomainSpec { return nil }
func (s stubConnector) Apply(context.Context, InboundRequest) ([]MirrorWrite, error) {
	return nil, NotImplemented(s.name, "Apply")
}
func (s stubConnector) Backfill(context.Context, string, string) (BackfillPage, error) {
	return BackfillPage{}, NotImplemented(s.name, "Backfill")
}
func (s stubConnector) Reconcile(context.Context, string, time.Time) (ReconcileReport, error) {
	return ReconcileReport{}, NotImplemented(s.name, "Reconcile")
}
func (s stubConnector) Propagate(context.Context, OutboxEntry) (PropagateResult, error) {
	return PropagateResult{}, NotImplemented(s.name, "Propagate")
}
func (s stubConnector) EnsureSubscriptions(context.Context) error {
	return NotImplemented(s.name, "EnsureSubscriptions")
}

// The two halves are separate facts, and the boot check reads the one
// that is answerable before integrations are wired.
func TestDeclareIsAnswerableBeforeAnythingIsBound(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	Declare("shopify")

	if !IsDeclared("shopify") {
		t.Fatal("IsDeclared(\"shopify\") is false after Declare -- the boot check runs before integrations are wired, so declaration is the only fact available to it")
	}
	if _, ok := Lookup("shopify"); ok {
		t.Error("Lookup found an implementation nothing bound")
	}
	if names := DeclaredNames(); len(names) != 1 || names[0] != "shopify" {
		t.Errorf("DeclaredNames() = %v, want [shopify]", names)
	}
	if names := BoundNames(); len(names) != 0 {
		t.Errorf("BoundNames() = %v, want none", names)
	}
}

func TestBindRefusesANameNothingDeclared(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	err := Bind(stubConnector{name: "shopify"})
	if err == nil {
		t.Fatal("Bind accepted an undeclared connector -- a connector reachable at runtime but invisible to the boot check would let a concept name it and still refuse boot")
	}
	for _, want := range []string{"never declared", "Declare", "shopify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestBindRefusesASecondImplementationForOneName(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	Declare("shopify")
	if err := Bind(stubConnector{name: "shopify"}); err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	if err := Bind(stubConnector{name: "shopify"}); err == nil {
		t.Fatal("Bind accepted a second implementation for one name -- which of the two serves a write is then unanswerable")
	}
	if _, ok := Lookup("shopify"); !ok {
		t.Error("Lookup lost the first implementation after the second was refused")
	}
}

func TestBindRefusesNilAndNamelessImplementations(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	if err := Bind(nil); err == nil {
		t.Error("Bind(nil) was accepted")
	}
	Declare("shopify")
	if err := Bind(stubConnector{name: "  "}); err == nil {
		t.Error("Bind accepted an implementation with a blank Name()")
	}
}

// Declaring twice asserts nothing different and must not be a failure;
// two implementations under one name is the ambiguity that is.
func TestDeclareIsIdempotent(t *testing.T) {
	t.Cleanup(resetForTest)
	resetForTest()

	Declare("shopify")
	Declare("shopify")
	Declare("")

	if names := DeclaredNames(); len(names) != 1 || names[0] != "shopify" {
		t.Errorf("DeclaredNames() = %v, want exactly [shopify] -- a blank name declares nothing and a repeat declares the same thing", names)
	}
}

// A connector that serves one direction of one domain must be able to
// say so without every unserved call looking like a delivery failure.
func TestNotImplementedIsDistinguishableFromADeliveryFailure(t *testing.T) {
	c := stubConnector{name: "shopify"}
	_, err := c.Propagate(context.Background(), OutboxEntry{})
	if err == nil {
		t.Fatal("the stub connector reported success for a capability it does not serve")
	}
	if !IsNotImplemented(err) {
		t.Errorf("IsNotImplemented(%v) = false -- the drain worker would dead-letter every entry instead of reporting an unconfigured capability", err)
	}
	for _, want := range []string{"shopify", "Propagate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q -- an operator has to learn WHICH capability is missing", err, want)
		}
	}
	if IsNotImplemented(context.Canceled) {
		t.Error("IsNotImplemented matched an unrelated error")
	}
}
