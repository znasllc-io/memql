package identity

// invitation_accept_test.go -- memql#4606.
//
// The write that makes a user invitation single-use has two halves, and each
// one is silent when it breaks. The DSL half stamps the row; the store half is
// the only thing that ever calls it. A regression in either leaves an
// invitation link redeemable for its whole TTL while every test that does not
// read the invitation row stays green -- which is exactly how the gap the issue
// reports survived: `resolveInvitation` has always refused a row whose status
// is not "pending" as `invitation_already_used`, and nothing ever moved a
// kind="user" row off "pending", so the refusal was unreachable.
//
// So these guard both halves at the seam between them:
//
//   - the AUTHORED mutation still stamps status, inviteeId and respondedAt,
//     read from the same embedded tree the engine loads rather than from the
//     source text (memql#2875 -- an @serverOnly the parser does not read as an
//     annotation is not a gate, and a stamp named only in a comment is not a
//     write);
//   - the STORE's call names only args the mutation declares and supplies every
//     one it requires, so a typo'd argument fails here rather than the first
//     time it meets a database (the reason component/identity/http's device
//     tests drive the real store rather than a stubbed interface);
//   - the call carries INTERNAL origin, because the mutation is @serverOnly and
//     origin defaults to client. That refusal does not break the login it sits
//     on -- the redemption still succeeds and only the stamp is lost -- so it
//     is the inert-gate failure test/dslconformance/server_only_callers_stamp_test.go
//     exists for, one layer down;
//   - an incomplete call is REFUSED rather than sent, because an update naming
//     no row succeeds at the engine and reports nothing, which would leave the
//     caller believing an invitation was burned when it was not.
//
// No database: every assertion is about the query the store builds and the
// mutation the tree declares, and both are decided before anything touches
// storage.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

const (
	acceptTestInvitationId = "v1:identity:invitation:inv-1"
	acceptTestInviteeId    = "v1:identity:user:invitee-1"
)

// invitationAcceptEngine records every call the store makes, with the origin it
// was stamped with. `fail` makes the engine refuse, which is how the wrapping
// assertion reaches the error path without a database.
type invitationAcceptEngine struct {
	mu      sync.Mutex
	queries []string
	origins []auth.CallOrigin
	fail    error
}

func (e *invitationAcceptEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, query)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	if e.fail != nil {
		return nil, e.fail
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (e *invitationAcceptEngine) only(t *testing.T) (string, auth.CallOrigin) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queries) != 1 {
		t.Fatalf("the store made %d engine call(s), want exactly 1: %v", len(e.queries), e.queries)
	}
	return e.queries[0], e.origins[0]
}

// namedArgRe reads the `key: "value"` pairs writeKVString emits. Deliberately
// narrow rather than a parser: the point is to see the call the way the engine's
// argument binding will, so a renamed or dropped argument is visible.
var namedArgRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*):\s*"((?:[^"\\]|\\.)*)"`)

func namedArgsOf(query string) map[string]string {
	out := map[string]string{}
	open := strings.Index(query, "(")
	if open < 0 {
		return out
	}
	for _, m := range namedArgRe.FindAllStringSubmatch(query[open:], -1) {
		out[m[1]] = m[2]
	}
	return out
}

// TestMarkUserInvitationAcceptedCallsTheMutationWithBothIds pins the store's
// half: the call names the mutation, carries the invitation it burns, and
// carries the user it credits the acceptance to.
func TestMarkUserInvitationAcceptedCallsTheMutationWithBothIds(t *testing.T) {
	e := &invitationAcceptEngine{}
	s := &Store{Engine: e}

	if err := s.MarkUserInvitationAccepted(context.Background(), acceptTestInvitationId, acceptTestInviteeId); err != nil {
		t.Fatalf("MarkUserInvitationAccepted: %v", err)
	}

	query, _ := e.only(t)
	if !strings.Contains(query, "markUserInvitationAccepted(") {
		t.Fatalf("the store executed %q, which does not call markUserInvitationAccepted -- if the "+
			"mutation was renamed, move this guard with it rather than letting it pass vacuously", query)
	}
	args := namedArgsOf(query)
	if got := args["invitationId"]; got != acceptTestInvitationId {
		t.Errorf("invitationId = %q, want %q -- the write would land on the wrong row, or on none", got, acceptTestInvitationId)
	}
	if got := args["inviteeId"]; got != acceptTestInviteeId {
		t.Errorf("inviteeId = %q, want %q. inviteeId is the whole of what the row records about WHO "+
			"accepted; an accepted invitation that names nobody is the state the concept's "+
			"\"Stamped on acceptance\" description promises cannot happen", got, acceptTestInviteeId)
	}
}

// TestMarkUserInvitationAcceptedStampsInternalOrigin is the half that breaks
// SILENTLY.
//
// markUserInvitationAccepted is @serverOnly and origin defaults to client, so an
// unstamped call is refused at execute. Nothing about the redemption fails when
// that happens -- the user is created, the session is issued, the person is
// signed in -- and only the stamp is lost, which puts the invitation straight
// back to being reusable for its whole TTL.
//
// Failing-first: drop auth.ContextWithInternalOrigin from the store method and
// this reports OriginClient while every other test in this file still passes.
func TestMarkUserInvitationAcceptedStampsInternalOrigin(t *testing.T) {
	e := &invitationAcceptEngine{}
	s := &Store{Engine: e}

	if err := s.MarkUserInvitationAccepted(context.Background(), acceptTestInvitationId, acceptTestInviteeId); err != nil {
		t.Fatalf("MarkUserInvitationAccepted: %v", err)
	}

	_, origin := e.only(t)
	if !origin.IsInternal() {
		t.Errorf("the call ran with origin %v, not internal. markUserInvitationAccepted is "+
			"@serverOnly, so the engine refuses it -- the invitation stays pending and stays "+
			"redeemable. Route the call through auth.ContextWithInternalOrigin.", origin)
	}
}

// TestMarkUserInvitationAcceptedRefusesAnIncompleteCall pins that a missing id
// is an ERROR rather than a query.
//
// inviteeId is the half the engine cannot refuse: required-arg validation tests
// PRESENCE, not content (component/memql/function_validator.go), so "" satisfies
// `inviteeId string!` and the row lands stamped accepted while naming nobody.
// invitationId the engine does reject, and refusing it here anyway is what keeps
// the two halves of one call symmetrical.
func TestMarkUserInvitationAcceptedRefusesAnIncompleteCall(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		invitationId, inviteeId string
	}{
		{"no invitation id", "", acceptTestInviteeId},
		{"blank invitation id", "   ", acceptTestInviteeId},
		{"no invitee id", acceptTestInvitationId, ""},
		{"blank invitee id", acceptTestInvitationId, "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &invitationAcceptEngine{}
			s := &Store{Engine: e}

			err := s.MarkUserInvitationAccepted(context.Background(), tc.invitationId, tc.inviteeId)
			if err == nil {
				t.Fatal("the store reported success. A caller reads that as \"the invitation is " +
					"burned\", and it is not")
			}
			if !strings.HasPrefix(err.Error(), "identity.store: ") {
				t.Errorf("error %q does not carry the identity.store: prefix every other method in "+
					"this file uses", err)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.queries) != 0 {
				t.Errorf("the store executed %v. A refused call must not reach the engine at all: "+
					"the point of refusing early is that the caller is told which argument was "+
					"missing, by the layer that knows what the call was for", e.queries)
			}
		})
	}
}

// TestMarkUserInvitationAcceptedWrapsAnEngineFailure keeps the failure legible
// AND inspectable: the prefix names the layer, and errors.Is still reaches the
// engine's own error, so a caller can tell a transport failure from a refusal.
func TestMarkUserInvitationAcceptedWrapsAnEngineFailure(t *testing.T) {
	boom := errors.New("engine unavailable")
	e := &invitationAcceptEngine{fail: boom}
	s := &Store{Engine: e}

	err := s.MarkUserInvitationAccepted(context.Background(), acceptTestInvitationId, acceptTestInviteeId)
	if err == nil {
		t.Fatal("the engine refused and the store reported success")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q does not wrap the engine's own error", err)
	}
	if !strings.HasPrefix(err.Error(), "identity.store: mark user invitation accepted: ") {
		t.Errorf("error %q does not name the operation the way the neighbouring methods do", err)
	}
}

// authoredMarkUserInvitationAccepted returns the mutation as the ENGINE reads
// it: parsed out of the embedded tree, not grepped out of the source.
//
// The distinction is memql#2875's: an `@serverOnly` inside a comment or a
// multi-line annotation string satisfies a regex and is not an annotation, and
// the same is true of a stamp named only in prose. Reading the parsed tree is
// what makes "the mutation still stamps this" a fact rather than a hope.
func authoredMarkUserInvitationAccepted(t *testing.T) *languageAst.FunctionDef {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v -- the tree did not parse, so nothing below measures the "+
			"mutation. Fix the parse failure rather than reading this as \"the mutation is gone\".", err)
	}
	for path, file := range tree.Files {
		if file == nil || path != "identity/mutations.memql" {
			continue
		}
		for _, def := range file.Definitions {
			d, ok := def.(*languageAst.FunctionDef)
			if !ok || d.Name != "markUserInvitationAccepted" {
				continue
			}
			return d
		}
	}
	t.Fatal("identity/mutations.memql declares no markUserInvitationAccepted. Without it the " +
		"status check in resolveInvitation tests a value nothing can change, and a user " +
		"invitation is redeemable for its whole TTL (memql#4606).")
	return nil
}

// TestAuthoredUserInvitationAcceptStampsTheRow is the assertion the issue asks
// for in so many words: the row is stamped, so a refactor that drops the stamp
// fails loudly instead of quietly restoring a reusable credential.
func TestAuthoredUserInvitationAcceptStampsTheRow(t *testing.T) {
	d := authoredMarkUserInvitationAccepted(t)

	if d.Type != languageAst.FunctionTypeMutation {
		t.Fatalf("markUserInvitationAccepted parses as a %s, not a mutation", d.Type)
	}
	serverOnly := false
	for _, a := range d.Attributes {
		if a != nil && a.Name == "serverOnly" {
			serverOnly = true
		}
	}
	if !serverOnly {
		t.Error("markUserInvitationAccepted lost @serverOnly. Client-reachable it is a burn " +
			"primitive: any authenticated caller could name an arbitrary invitation id and spend " +
			"it before its holder ever clicked. Its expected-set entry in " +
			"test/dslconformance/server_only_parsed_test.go carries the full argument.")
	}

	stmt, ok := d.Body.(*languageAst.MutationStmt)
	if !ok {
		t.Fatalf("markUserInvitationAccepted's body parses as %T, not a mutation statement", d.Body)
	}
	if stmt.Kind != languageAst.MutationKindUpdate {
		t.Errorf("markUserInvitationAccepted is a %q, want an update -- an insert would mint a "+
			"second invitation row rather than spending the one presented", stmt.Kind)
	}
	if stmt.Concept != "invitation" {
		t.Errorf("markUserInvitationAccepted writes concept %q, want invitation", stmt.Concept)
	}
	ref, ok := stmt.IDTemplate.(*languageAst.ArgRefExpr)
	if !ok || ref.Path != "invitationId" {
		t.Errorf("the update selects %#v, want args.invitationId -- the row it lands on is the "+
			"whole point", stmt.IDTemplate)
	}

	// The payload is compared field by field rather than whole: the three that
	// matter are named here with what breaks without each, so a failure says
	// which property was lost rather than that a string changed.
	for _, want := range []struct {
		fragment, why string
	}{
		{`status:"accepted"`, "resolveInvitation refuses anything that is not \"pending\" as " +
			"invitation_already_used, so this assignment IS the single-use property"},
		{"inviteeId:args.inviteeId", "the row records who accepted; without it an accepted " +
			"invitation names nobody and the concept's \"Stamped on acceptance\" description is false"},
		{"respondedAt:now", "the acceptance timestamp, and the only record of WHEN the credential " +
			"was spent -- an audit trail with no time on it cannot answer whether a leaked link " +
			"was used before or after it leaked"},
	} {
		if !strings.Contains(stmt.PayloadRaw, want.fragment) {
			t.Errorf("the write no longer carries %s (payload: %s).\n\t%s",
				want.fragment, stmt.PayloadRaw, want.why)
		}
	}

	// active is NOT written, deliberately. It means "not soft-cancelled", which
	// is what revokeUserInvitation writes, and collapsing the two would cost the
	// redeeming page the difference between "somebody cancelled this" and "you
	// already used this" -- two different next steps for the person holding the
	// link, as revokeUserInvitation's own doc comment records.
	if strings.Contains(stmt.PayloadRaw, "active") {
		t.Errorf("the write now touches `active` (payload: %s). Acceptance is not cancellation; "+
			"keeping them distinct on the row is what lets the redeeming page tell the holder "+
			"which of the two happened", stmt.PayloadRaw)
	}
}

// TestMarkUserInvitationAcceptedCallMatchesTheAuthoredArgs closes the seam
// between the two halves in both directions.
//
// An arg the store sends that the mutation does not declare is dropped or
// refused at binding; a required arg the store omits refuses the call. Neither
// is visible to a test that only reads the query text, and neither is visible to
// one that only reads the tree -- which is why this compares them against each
// other rather than against a hardcoded list.
func TestMarkUserInvitationAcceptedCallMatchesTheAuthoredArgs(t *testing.T) {
	d := authoredMarkUserInvitationAccepted(t)
	if d.ArgsSchema == nil || len(d.ArgsSchema.Fields) == 0 {
		t.Fatal("markUserInvitationAccepted declares no args block, so the comparison below would " +
			"pass vacuously")
	}

	e := &invitationAcceptEngine{}
	s := &Store{Engine: e}
	if err := s.MarkUserInvitationAccepted(context.Background(), acceptTestInvitationId, acceptTestInviteeId); err != nil {
		t.Fatalf("MarkUserInvitationAccepted: %v", err)
	}
	query, _ := e.only(t)
	sent := namedArgsOf(query)

	declared := map[string]bool{}
	for _, f := range d.ArgsSchema.Fields {
		if f == nil {
			continue
		}
		declared[f.Name] = true
		if f.Optional {
			continue
		}
		if _, ok := sent[f.Name]; !ok {
			t.Errorf("the mutation requires %q and the store's call does not carry it -- the engine "+
				"refuses the whole write, so the invitation stays pending", f.Name)
		}
	}
	for name := range sent {
		if !declared[name] {
			t.Errorf("the store sends %q, which markUserInvitationAccepted does not declare. This is "+
				"the typo class: the call looks right, the argument goes nowhere, and the field it "+
				"was meant to stamp keeps its old value", name)
		}
	}
}
