package routingrules

// owner_ids_test.go -- a routing rule's bundle id is its owner's own, and the
// writes that bring the bundle and its construct into existence are the
// server's (memql#6022). The Postgres half -- a second person can neither
// create a bundle at that id nor block it by arming the same name first -- is
// app/routing_rule_owner_ids_db_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// TestABundleIdIsItsOwnersOwn: one person derives one id for a name, however
// their id is spelled and however the name is cased; two people derive two;
// and neither is the name-only id the derivation used to be.
func TestABundleIdIsItsOwnersOwn(t *testing.T) {
	alice, bob := "v1:identity:user:alice", "v1:identity:user:bob"
	id := BundleIdFor(alice, "myRule")
	if BundleIdFor("alice", "myRule") != id || BundleIdFor(alice, "MYRULE") != id {
		t.Fatalf("one owner's rule derives more than one id: %s, %s, %s",
			id, BundleIdFor("alice", "myRule"), BundleIdFor(alice, "MYRULE"))
	}
	if BundleIdFor(bob, "myRule") == id {
		t.Fatalf("two owners' rules of one name derive one id, %s", id)
	}
	if !strings.HasPrefix(id, "v1:authoring:bundle:routingrule-myrule-") {
		t.Fatalf("the id %s does not carry the rule's name", id)
	}
	if id == "v1:authoring:bundle:routingrule-myrule" {
		t.Fatalf("the id is still derived from the name alone: %s", id)
	}
}

// recordingEngine accepts every call and records its text and origin.
type recordingEngine struct {
	calls    []string
	internal []bool
}

func (e *recordingEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	e.calls = append(e.calls, q)
	e.internal = append(e.internal, auth.OriginFromContext(ctx).IsInternal())
	return &memql.ExecuteResult{}, nil
}

func (e *recordingEngine) ActivateApprovedBundle(context.Context, string, string, memql.AuthoredRuntimeDeps) (memql.ActivationResult, error) {
	return memql.ActivationResult{}, nil
}

func (e *recordingEngine) RetireActiveBundle(context.Context, string, string, memql.AuthoredRuntimeDeps) error {
	return nil
}

// TestTheBundleAndItsConstructAreWrittenAsTheServer: the bundle, its construct
// and Gate 1's verdict on it are @serverOnly writes, so each stamps internal
// origin, and each is written to the id derived for the owner.
func TestTheBundleAndItsConstructAreWrittenAsTheServer(t *testing.T) {
	e := &recordingEngine{}
	a := NewActivator(e, func() memql.AuthoredRuntimeDeps { return memql.AuthoredRuntimeDeps{} }, shipped)
	res, _ := a.Activate(ctxAs(auth.RoleOwner), "u-1", validForm())
	if want := BundleIdFor("u-1", validForm().Name); res.BundleId != want {
		t.Fatalf("the rule's bundle is %q, want the id derived for its owner, %q", res.BundleId, want)
	}
	for _, write := range []string{"createAuthoringBundle(", "createAuthoringConstruct(", "recordBundleValidation("} {
		found := false
		for i, q := range e.calls {
			if !strings.HasPrefix(q, write) {
				continue
			}
			found = true
			if !e.internal[i] {
				t.Errorf("%s ran without internal origin; it is @serverOnly, so it would be refused:\n  %s", write, q)
			}
			if !strings.Contains(q, res.BundleId) {
				t.Errorf("%s does not write to the owner's bundle %s:\n  %s", write, res.BundleId, q)
			}
		}
		if !found {
			t.Errorf("%s was never written", write)
		}
	}
}
