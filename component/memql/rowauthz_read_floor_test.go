package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CLUSTER-OWNER TIER'S READ FLOOR (memql#5216), proved to move.
//
// The corpus has two declarations of it, so this file running green over the
// tree would be a null result -- it cannot tell a rule that runs from a rule
// that does not. Every case below plants a decl and asserts the answer, and
// each has a control that must stay silent.

func floorDecl(slug string) *langparser.RowAuthzDecl {
	return &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner, RankFloor: slug}
}

// The ladder these run against is the compiled fallback (owner 400, developer
// 300, admin 200, writer 100, reader 50), which is what rankOf resolves against
// with no role catalog readable -- the shape every case here runs under, since
// none of them has a database.
func floorCtx(t *testing.T, e *MemQLEngine, role auth.Role) context.Context {
	t.Helper()
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: role})
	return contextWithRankScopeMemo(ctx, e)
}

func TestTheReadFloorAdmitsAtAndAboveItselfAndRefusesBelow(t *testing.T) {
	// The whole point, in one table. `admin` is the floor v1:bench:* declares,
	// and on this ladder developer (300) outranks admin (200) -- so both clear
	// it, and a writer does not.
	e := &MemQLEngine{}
	decl := floorDecl("admin")
	for _, c := range []struct {
		role  auth.Role
		admit bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleDeveloper, true},
		{auth.RoleAdmin, true},
		{auth.RoleWriter, false},
		{auth.RoleReader, false},
		{auth.Role(""), false},
	} {
		if got := readFloorAdmitsRow(floorCtx(t, e, c.role), decl); got != c.admit {
			t.Errorf("rankFloor=admin, actor %q: admitted=%v, want %v", c.role, got, c.admit)
		}
	}
}

func TestAPlainClusterOwnerTierIsUnaffected(t *testing.T) {
	// The control. Without it a rule that admitted everything would pass the
	// table above, and every clusterOwner concept in the tree would open up.
	e := &MemQLEngine{}
	plain := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleWriter} {
		if readFloorAdmitsRow(floorCtx(t, e, role), plain) {
			t.Fatalf("a clusterOwner tier with no floor admitted %q through the floor branch; "+
				"the branch must contribute nothing unless a floor is declared", role)
		}
	}
}

func TestAnUnresolvableReadFloorAdmitsNobody(t *testing.T) {
	// The same fail-CLOSED property `unowned=` has. The natural spelling
	// (`actorRank >= rankOf(slug)`) reads correctly and fails OPEN, since
	// rankOf answers 0 for a slug it does not know and every rank clears 0 --
	// so one typo would publish an administrative concept to every signed-in
	// caller while the declaration still read like a narrowing.
	e := &MemQLEngine{}
	for _, slug := range []string{"admni", "nosuchrole", "ADMIN"} {
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin} {
			if readFloorAdmitsRow(floorCtx(t, e, role), floorDecl(slug)) {
				t.Fatalf("rankFloor=%q admitted %q -- an unresolvable floor must deny", slug, role)
			}
		}
	}
}

func TestWithNoRankMemoTheReadFloorDeclines(t *testing.T) {
	// The row gate is a package function with no engine; it reads one off the
	// rank memo. A context no entry point stamped therefore declines -- which
	// leaves exactly the pre-floor behaviour, the cluster-owner arm alone.
	ctx := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "u1", Role: auth.RoleAdmin})
	if readFloorAdmitsRow(ctx, floorDecl("admin")) {
		t.Fatal("the read floor admitted with no rank memo installed; it has no engine to " +
			"resolve a ladder from, so the only safe answer is to decline")
	}
}

func TestTheFloorIsORedOntoTheTierRatherThanReplacingIt(t *testing.T) {
	// orRankScope's rule, and the reason: a cluster owner whose rank cannot be
	// resolved must keep reading their own administrative rows. Replacing the
	// tier's term would make an owner's access depend on a second lookup.
	base := &ComparisonExpression{
		Field:    FieldReference{Raw: "actor.isClusterOwner", Parts: []string{"actor", "isClusterOwner"}},
		Operator: OpEq,
		Value:    true,
	}
	got := orReadFloor(base, floorDecl("admin"))
	logical, ok := got.(*LogicalExpression)
	if !ok {
		t.Fatalf("orReadFloor produced %T, want a disjunction", got)
	}
	if logical.Op != LogicalOr {
		t.Errorf("the floor must be OR-ed onto the tier, got op %v", logical.Op)
	}
	if logical.Left != ExpressionNode(base) {
		t.Error("the tier's own term must survive on the left; replacing it would make a " +
			"cluster owner's access depend on resolving a ladder")
	}
	if _, isFloor := logical.Right.(*ReadFloorExpression); !isFloor {
		t.Errorf("the right arm is %T, want the symbolic floor node", logical.Right)
	}
}

func TestAPlainTierGetsNoExtraTerm(t *testing.T) {
	// The control for the case above. A rule that always wrapped would change
	// every clusterOwner concept's plan.
	base := &ComparisonExpression{Operator: OpEq}
	plain := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}
	if got := orReadFloor(base, plain); got != ExpressionNode(base) {
		t.Fatalf("orReadFloor wrapped a tier with no floor: %T", got)
	}
	granted := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "s", RankFloor: "admin"}
	if got := orReadFloor(base, granted); got != ExpressionNode(base) {
		t.Fatal("orReadFloor acted on the granted tier; the formatter refuses that " +
			"declaration, and the injector must not honour one that slipped through")
	}
}

func TestTheSymbolicFloorIsLoweredAndNotLeftInThePlan(t *testing.T) {
	// A symbolic placeholder reaching the SQL compiler is the one failure this
	// mechanism must not have, and it is what happens if treeHasRankScope does
	// not know the node -- the lowering walk is skipped entirely.
	node := &ReadFloorExpression{Floor: "admin"}
	if !treeHasRankScope(node) {
		t.Fatal("treeHasRankScope does not recognise the read floor, so the lowering walk is " +
			"skipped and the symbolic node reaches the compiler unlowered")
	}
	tree := &LogicalExpression{Op: LogicalOr, Left: &ComparisonExpression{}, Right: node}
	if !treeHasRankScope(tree) {
		t.Fatal("treeHasRankScope does not find the floor under a disjunction, which is the " +
			"only shape the injector ever produces")
	}

	e := &MemQLEngine{}
	lowered := e.lowerRankScope(floorCtx(t, e, auth.RoleAdmin), tree)
	logical, ok := lowered.(*LogicalExpression)
	if !ok {
		t.Fatalf("lowering produced %T, want the disjunction back", lowered)
	}
	constant, ok := logical.Right.(*constantBoolExpression)
	if !ok {
		t.Fatalf("the floor lowered to %T, want a constant -- it compares nothing on the row, "+
			"so there is no column to push down against", logical.Right)
	}
	if !constant.value {
		t.Error("an admin did not clear the admin floor after lowering")
	}

	below := e.lowerRankScope(floorCtx(t, e, auth.RoleWriter), tree)
	belowConstant, ok := below.(*LogicalExpression).Right.(*constantBoolExpression)
	if !ok || belowConstant.value {
		t.Error("a writer cleared the admin floor after lowering")
	}
}

// ---------------------------------------------------------------------
// The declaration surface
// ---------------------------------------------------------------------

func parseFloor(t *testing.T, args map[string]any) (*langparser.RowAuthzDecl, error) {
	t.Helper()
	return langparser.ParseRowAuthz(&langparser.Attribute{
		Name: langparser.RowAuthzAnnotation,
		Args: args,
	})
}

func TestTheReadFloorParsesAndRoundTrips(t *testing.T) {
	decl, err := parseFloor(t, map[string]any{"clusterOwner": true, "rankFloor": "admin"})
	if err != nil {
		t.Fatalf(`@rowAuthz(clusterOwner, rankFloor="admin") did not parse: %v`, err)
	}
	if decl.Tier != langparser.RowAuthzClusterOwner || decl.RankFloor != "admin" {
		t.Fatalf("parsed to %+v", *decl)
	}
	rendered, err := langparser.FormatRowAuthz(*decl)
	if err != nil {
		t.Fatalf("FormatRowAuthz: %v", err)
	}
	if rendered != `@rowAuthz(clusterOwner, rankFloor="admin")` {
		t.Fatalf("round-trip rendered %q", rendered)
	}
}

func TestABareReadFloorIsRefused(t *testing.T) {
	// `@rowAuthz(clusterOwner, rankFloor)` stores `true`, which would lower to
	// a floor of "true" -- rank 0, cleared by everyone. The fail-OPEN direction
	// rankFloorAdmits exists to make impossible at runtime, arriving through
	// the parser instead.
	_, err := parseFloor(t, map[string]any{"clusterOwner": true, "rankFloor": true})
	if err == nil {
		t.Fatal(`a bare rankFloor parsed; it would lower to a floor of "true", which ranks 0`)
	}
	if !strings.Contains(err.Error(), "needs a role slug") {
		t.Errorf("the message must say what is missing: %v", err)
	}
}

func TestTheFloorIsRefusedOnEveryOtherTier(t *testing.T) {
	// Refused rather than dropped, for FormatRowAuthz's standing round-trip
	// reason. The owned tier takes it too since design record 2026-11-24, so
	// only the public and granted tiers are left to refuse it.
	for _, d := range []langparser.RowAuthzDecl{
		{Tier: langparser.RowAuthzPublic, RankFloor: "admin"},
		{Tier: langparser.RowAuthzGranted, Spec: "s", RankFloor: "admin"},
	} {
		if _, err := langparser.FormatRowAuthz(d); err == nil {
			t.Errorf("FormatRowAuthz accepted a rankFloor on tier %q", d.Tier)
		}
	}
}

func TestTheCompositeTierStillReachesItsOwnParser(t *testing.T) {
	// `owner=` beside `clusterOwner` is the COMPOSITE tier (memql#4312), which
	// the owned parser owns. The clusterOwner-with-modifiers parser must
	// DECLINE that shape rather than claim it, or the composite would start
	// parsing as a bare cluster-owner tier and every composite concept in the
	// tree would silently lose its owner arm.
	decl, err := parseFloor(t, map[string]any{"owner": "ownerUserId", "clusterOwner": true})
	if err != nil {
		t.Fatalf("the composite tier stopped parsing: %v", err)
	}
	if decl.Tier != langparser.RowAuthzOwned || decl.Owner != "ownerUserId" || !decl.ClusterOwnerBypass {
		t.Fatalf("the composite parsed to %+v, want the owned tier with the bypass", *decl)
	}
}

// THE OWNER TIER'S READ FLOOR (design record 2026-11-24), through the row gate
// of a planted concept: a developer reads a row somebody else owns, an admin
// does not, a role scoped to one organization does not however high its rank,
// and nobody WRITES through the floor.
func TestTheOwnerTierFloorAdmitsReadsFromItsRungAndNoWrite(t *testing.T) {
	const concept = "v1:rowauthzfixture:ownerfloor"
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true, RankFloor: "developer"}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{concept: {Name: concept, NodeType: "ownerfloor", RowAuthz: decl}})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })
	// staffCatalog (rowauthz_account_test.go) scopes "client-lead-350" to one
	// organization and ranks it above developer.
	auth.SetCapabilityCatalog(staffCatalog{})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })

	e := &MemQLEngine{}
	someoneElses := []byte(`{"ownerUserId":"someone-else"}`)
	for _, c := range []struct {
		role auth.Role
		read bool
	}{
		{auth.RoleDeveloper, true},
		{auth.RoleAdmin, false},
		{auth.Role("client-lead-350"), false},
	} {
		ctx := floorCtx(t, e, c.role)
		if got := rowAuthzAdmits(ctx, concept, "row-1", someoneElses) == rowAuthzAdmit; got != c.read {
			t.Errorf("%s reading somebody else's row: admitted=%v, want %v", c.role, got, c.read)
		}
		if rowAuthzAdmitsWrite(ctx, concept, "row-1", someoneElses) == rowAuthzAdmit {
			t.Errorf("%s was admitted to WRITE somebody else's row; the floor relaxes reads only", c.role)
		}
	}
	if _, ored := orReadFloor(&ComparisonExpression{}, decl).(*LogicalExpression); !ored {
		t.Error("the owner tier's floor is not OR-ed into the read predicate, so SQL and the row gate would disagree")
	}
}
