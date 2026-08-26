package identity

// invitation_binding_test.go -- memql#4601.
//
// First-touch binding is a control with two silent failure modes, and neither
// one shows up as a broken flow:
//
//  1. THE PROJECTION. `bindingHash` can exist on the concept, be written by the
//     mutation, and still never reach Go, because the query the store runs
//     selects an explicit field list through a shape. A field the shape omits
//     comes back as the zero value, and for this field the zero value is not
//     "unknown" -- it reads as "no browser has bound this row", which is the
//     state the accept path is required to admit. So the whole control fails
//     OPEN, and it does so with every test that stubs the engine still green.
//     That is not hypothetical: `active` and `inviteeRole` were lifted by
//     LookupInvitationByTokenHash while invitationFull omitted them, for the
//     same reason and with the same silence.
//
//  2. THE NO-OVERWRITE RULE. It is not expressible in the mutation body, so it
//     is Go, so it is exactly as durable as a test that pins it. Without one, a
//     later refactor that "simplifies" BindUserInvitation into a plain write
//     leaves a control that looks present and holds nothing -- a row a second
//     browser can re-bind is an unbound row.
//
// The projection tests therefore run end to end from the store's OWN emitted
// query text: the query name is read back out of the call, the tree is asked
// what shape that query declares, and the shape is asked whether it carries the
// field. A rename anywhere along that chain fails here rather than reading as an
// unbound row in production.

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	bindTestInvitationId = "v1:identity:invitation:inv-4601"
	bindTestHash         = "b1a5f00d" + "cafe"
	bindTestOtherHash    = "0ther8r0wser"
)

// bindingEngine serves invitationById from one in-memory row and records every
// call. `row` nil means the lookup finds nothing.
type bindingEngine struct {
	mu      sync.Mutex
	row     map[string]string
	queries []string
	origins []auth.CallOrigin
	fail    error
}

func (e *bindingEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, query)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	if e.fail != nil {
		return nil, e.fail
	}
	if strings.Contains(query, "invitationById(") || strings.Contains(query, "InvitationByTokenHash(") {
		if e.row == nil {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		fields := map[string]any{}
		for k, v := range e.row {
			fields[k] = v
		}
		payload, err := structpb.NewStruct(fields)
		if err != nil {
			return nil, err
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id:      e.row["id"],
			Payload: payload,
		}}}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (e *bindingEngine) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.queries)
}

// writes returns the mutation calls only, which is what every no-overwrite
// assertion is actually about: the lookup is expected, the write is not.
func (e *bindingEngine) writes() []string {
	var out []string
	for _, q := range e.calls() {
		if strings.HasPrefix(strings.TrimSpace(q), "mutation ") {
			out = append(out, q)
		}
	}
	return out
}

func (e *bindingEngine) originOf(t *testing.T, fragment string) auth.CallOrigin {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, q := range e.queries {
		if strings.Contains(q, fragment) {
			return e.origins[i]
		}
	}
	t.Fatalf("no call containing %q was made; calls: %v", fragment, e.queries)
	return auth.OriginClient
}

func boundRow(bindingHash string) map[string]string {
	row := map[string]string{
		"id":           bindTestInvitationId,
		"kind":         "user",
		"status":       "pending",
		"inviteeEmail": "invited@example.com",
	}
	if bindingHash != "" {
		row["bindingHash"] = bindingHash
	}
	return row
}

// callNameRe reads the construct name back out of a query the store built.
var callNameRe = regexp.MustCompile(`^\s*(?:query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

func calledConstruct(t *testing.T, query string) string {
	t.Helper()
	m := callNameRe.FindStringSubmatch(query)
	if m == nil {
		t.Fatalf("could not read a construct name out of %q", query)
	}
	return m[1]
}

// shapePathsOfQuery answers "what does this query actually return", by asking
// the tree rather than the author: query -> declared shape -> projected paths.
func shapePathsOfQuery(t *testing.T, queryName string) []string {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}
	var shapeName string
	shapes := map[string][]string{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				if d.Name != queryName || d.Type != languageAst.FunctionTypeQuery {
					continue
				}
				se, ok := d.Body.(*languageAst.ShapeExpr)
				if !ok {
					t.Fatalf("%s does not project through a shape (body %T), so this gate cannot "+
						"tell what it returns", queryName, d.Body)
				}
				shapeName = se.TemplateName
			case *languageAst.ShapeDecl:
				shapes[d.Name] = d.Paths
			}
		}
	}
	if shapeName == "" {
		t.Fatalf("the tree declares no query %q -- the store calls it, so this is a rename that "+
			"already broke the lookup", queryName)
	}
	paths, ok := shapes[shapeName]
	if !ok {
		t.Fatalf("%s projects shape %q, which the tree does not declare", queryName, shapeName)
	}
	return paths
}

// TestInvitationLookupsProjectTheBindingHash is the end-to-end projection gate.
//
// It does not name a query or a shape: it runs the store, reads the construct
// the store actually called, and follows that call into the tree. So it keeps
// holding when the query is renamed or re-pointed -- which is precisely what
// happened while this was being written (LookupInvitationByTokenHash moved from
// invitationByTokenHash to userInvitationByTokenHash, memql#4612).
func TestInvitationLookupsProjectTheBindingHash(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(s *Store)
	}{
		{"LookupInvitationByTokenHash", func(s *Store) {
			_, _ = s.LookupInvitationByTokenHash(context.Background(), "a-token-hash")
		}},
		{"LookupInvitationById", func(s *Store) {
			_, _ = s.LookupInvitationById(context.Background(), bindTestInvitationId)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &bindingEngine{}
			tc.call(&Store{Engine: e})
			calls := e.calls()
			if len(calls) != 1 {
				t.Fatalf("%s made %d engine call(s), want 1: %v", tc.name, len(calls), calls)
			}
			name := calledConstruct(t, calls[0])
			paths := shapePathsOfQuery(t, name)
			if !slices.Contains(paths, "bindingHash") {
				t.Errorf("%s runs %s, whose shape projects %v -- no bindingHash.\n\t"+
					"The lift in firstInvitationRow then reads the empty string, which does not "+
					"mean \"unknown\": it means \"no browser has bound this row\", and the accept "+
					"path admits an unbound row. The control fails OPEN and nothing says so.",
					tc.name, name, paths)
			}
		})
	}
}

// TestInvitationLookupsLiftTheBindingHash is the other half of the same chain:
// the shape may carry the field and the Go projection still drop it.
func TestInvitationLookupsLiftTheBindingHash(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(s *Store) (*InvitationRow, error)
	}{
		{"LookupInvitationByTokenHash", func(s *Store) (*InvitationRow, error) {
			return s.LookupInvitationByTokenHash(context.Background(), "a-token-hash")
		}},
		{"LookupInvitationById", func(s *Store) (*InvitationRow, error) {
			return s.LookupInvitationById(context.Background(), bindTestInvitationId)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &bindingEngine{row: boundRow(bindTestHash)}
			row, err := tc.call(&Store{Engine: e})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if row == nil {
				t.Fatalf("%s returned no row", tc.name)
			}
			if row.BindingHash != bindTestHash {
				t.Errorf("%s lifted BindingHash = %q, want %q. An empty one reads as an unbound "+
					"row, which is the state the accept path lets through", tc.name, row.BindingHash, bindTestHash)
			}
		})
	}
}

// TestBindUserInvitationStampsAnUnboundRow is the happy path: first touch
// writes, through the @serverOnly gate's internal origin.
func TestBindUserInvitationStampsAnUnboundRow(t *testing.T) {
	e := &bindingEngine{row: boundRow("")}
	s := &Store{Engine: e}

	if err := s.BindUserInvitation(context.Background(), bindTestInvitationId, bindTestHash); err != nil {
		t.Fatalf("BindUserInvitation: %v", err)
	}

	writes := e.writes()
	if len(writes) != 1 {
		t.Fatalf("made %d write(s), want 1: %v", len(writes), writes)
	}
	if got := calledConstruct(t, writes[0]); got != "bindUserInvitation" {
		t.Errorf("the store called %q, not bindUserInvitation", got)
	}
	args := namedArgsOf(writes[0])
	if args["invitationId"] != bindTestInvitationId || args["bindingHash"] != bindTestHash {
		t.Errorf("write carried %v, want the invitation id and the digest", args)
	}
	if origin := e.originOf(t, "bindUserInvitation("); !origin.IsInternal() {
		t.Errorf("the write ran with origin %v, not internal. bindUserInvitation is @serverOnly, "+
			"so the engine refuses it -- and the row is then simply never bound, which no part of "+
			"the sign-in surfaces", origin)
	}
}

// TestBindUserInvitationRefusesToMoveAnExistingBinding is THE security
// assertion of this file.
//
// A second browser must not be able to take a binding the first one holds. If
// it could, a forwarded invitation would be redeemable by the forwardee, which
// is the entire thing first-touch binding exists to stop.
//
// Failing-first: delete the existing-binding branch from BindUserInvitation and
// this reports a write that should not have happened.
func TestBindUserInvitationRefusesToMoveAnExistingBinding(t *testing.T) {
	e := &bindingEngine{row: boundRow(bindTestHash)}
	s := &Store{Engine: e}

	err := s.BindUserInvitation(context.Background(), bindTestInvitationId, bindTestOtherHash)
	if !errors.Is(err, ErrInvitationBoundElsewhere) {
		t.Fatalf("err = %v, want ErrInvitationBoundElsewhere. A caller cannot tell a taken row "+
			"from a fresh one otherwise", err)
	}
	if writes := e.writes(); len(writes) != 0 {
		t.Errorf("the store wrote %v. The row already carried another browser's binding, so this "+
			"call just handed the invitation to whoever made it", writes)
	}
}

// TestBindUserInvitationIsIdempotentForTheSameBrowser keeps the refusal above
// from being a trap for the ordinary case: a reload, a back button or a
// prefetch re-touches the link from the SAME browser, and that must not read as
// an attack.
func TestBindUserInvitationIsIdempotentForTheSameBrowser(t *testing.T) {
	e := &bindingEngine{row: boundRow(bindTestHash)}
	s := &Store{Engine: e}

	if err := s.BindUserInvitation(context.Background(), bindTestInvitationId, bindTestHash); err != nil {
		t.Fatalf("a re-touch by the same browser was refused: %v", err)
	}
	if writes := e.writes(); len(writes) != 0 {
		t.Errorf("the store re-wrote an identical binding (%v). Harmless today, but it makes the "+
			"write path depend on the value rather than on the row being empty", writes)
	}
}

// TestBindUserInvitationRefusesAMissingRowAndIncompleteArguments covers the
// refusals that must not reach the engine.
func TestBindUserInvitationRefusesAMissingRowAndIncompleteArguments(t *testing.T) {
	t.Run("no such invitation", func(t *testing.T) {
		e := &bindingEngine{}
		s := &Store{Engine: e}
		err := s.BindUserInvitation(context.Background(), bindTestInvitationId, bindTestHash)
		if !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("err = %v, want ErrInvitationNotFound", err)
		}
		if writes := e.writes(); len(writes) != 0 {
			t.Errorf("the store wrote %v against a row that does not exist", writes)
		}
	})

	for _, tc := range []struct {
		name                   string
		invitationId, bindHash string
	}{
		{"no invitation id", "", bindTestHash},
		{"blank invitation id", "  ", bindTestHash},
		{"no binding hash", bindTestInvitationId, ""},
		{"blank binding hash", bindTestInvitationId, "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &bindingEngine{row: boundRow("")}
			s := &Store{Engine: e}
			err := s.BindUserInvitation(context.Background(), tc.invitationId, tc.bindHash)
			if err == nil {
				t.Fatal("the store reported success. An empty digest binds the row to nobody while " +
					"reading as bound to everybody that checks it")
			}
			if !strings.HasPrefix(err.Error(), "identity.store: ") {
				t.Errorf("error %q does not carry the identity.store: prefix", err)
			}
			if calls := e.calls(); len(calls) != 0 {
				t.Errorf("the store reached the engine at all (%v) for a call it had already "+
					"refused", calls)
			}
		})
	}
}

// TestAuthoredBindUserInvitationWritesOnlyTheBinding pins the mutation as the
// engine reads it: it stamps the digest onto the row named by the argument, and
// touches NOTHING else -- a bind is not an acceptance and must not move status,
// active or respondedAt.
func TestAuthoredBindUserInvitationWritesOnlyTheBinding(t *testing.T) {
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}
	var d *languageAst.FunctionDef
	for path, file := range tree.Files {
		if file == nil || path != "identity/mutations.memql" {
			continue
		}
		for _, def := range file.Definitions {
			fn, ok := def.(*languageAst.FunctionDef)
			if ok && fn.Name == "bindUserInvitation" {
				d = fn
			}
		}
	}
	if d == nil {
		t.Fatal("identity/mutations.memql declares no bindUserInvitation (memql#4601)")
	}

	serverOnly := false
	for _, a := range d.Attributes {
		if a != nil && a.Name == "serverOnly" {
			serverOnly = true
		}
	}
	if !serverOnly {
		t.Error("bindUserInvitation lost @serverOnly. It writes a caller-supplied digest that " +
			"decides which browser may accept, so on the wire any authenticated caller could bind " +
			"an invitation they know the id of to a cookie they hold.")
	}

	stmt, ok := d.Body.(*languageAst.MutationStmt)
	if !ok {
		t.Fatalf("bindUserInvitation's body parses as %T, not a mutation statement", d.Body)
	}
	if stmt.Kind != languageAst.MutationKindUpdate || stmt.Concept != "invitation" {
		t.Errorf("bindUserInvitation is a %q on %q, want an update on invitation", stmt.Kind, stmt.Concept)
	}
	if ref, ok := stmt.IDTemplate.(*languageAst.ArgRefExpr); !ok || ref.Path != "invitationId" {
		t.Errorf("the update selects %#v, want args.invitationId", stmt.IDTemplate)
	}
	if !strings.Contains(stmt.PayloadRaw, "bindingHash:args.bindingHash") {
		t.Errorf("the write does not carry bindingHash (payload: %s), so the binding is never "+
			"recorded and the accept path admits every browser", stmt.PayloadRaw)
	}
	for _, forbidden := range []string{"status", "active", "respondedAt", "inviteeId"} {
		if strings.Contains(stmt.PayloadRaw, forbidden) {
			t.Errorf("the write touches %q (payload: %s). Opening a link is not responding to an "+
				"invitation: a bind that moved the lifecycle would spend the invitation for a "+
				"person who has not accepted anything yet", forbidden, stmt.PayloadRaw)
		}
	}
}
