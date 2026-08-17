package chat

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowauthz_repack_gate_4029_test.go -- memql#4029.
//
// Every capability in recent_chat.go is a REPACK: it reads real graph rows and
// returns ONE synthetic node stamped `chat.recentChat` carrying a summary of
// them -- for the utterance reads, the conversation text itself.
//
// Both of the engine's row-authz mechanisms resolve the tier from a CONCEPT
// (filter injection from plan.BoundConcept; the row gate from the row's own
// concept), so the gate that runs on this capability's OUTPUT is asked about
// `chat.recentChat`. That concept declares no tier, so it admits -- and the tier
// v1:cognition:utterance / space / participant declares is never consulted,
// because no row bearing those concepts ever leaves this package. The gate runs,
// finds nothing declared, and says yes.
//
// The repair is to gate the SOURCE rows, which still carry their real concept,
// id and payload, before they are folded into the summary. These tests pin that
// the gate is applied, that it is applied in the right ORDER relative to the
// per-id fold, and that it cannot be quietly removed.
//
// NO DATABASE IS REQUIRED and that is deliberate: the seams under test are the
// pure ones either side of the bun call, so this suite runs in `make test`
// rather than only in the db-gated lane -- which is where a gate that feeds LLM
// context should be checked.

func denyAll(context.Context, memorynodes.MemoryNode) bool { return false }

func participantRow(id, status, name string) memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"status": status, "displayName": name, "participantType": "human",
	})
	return memorynodes.MemoryNode{
		ID:      id,
		Concept: memorynodes.ConceptCognitionParticipant,
		Payload: payload,
	}
}

// TestAdmittedDropsDeniedSourceRows is the rule, with its control.
//
// Both directions are asserted in one test on purpose: a gate that denied
// everything and a gate that admitted everything would each pass one half.
func TestAdmittedDropsDeniedSourceRows(t *testing.T) {
	ctx := context.Background()
	rows := []memorynodes.MemoryNode{
		participantRow("p1", "active", "Ana"),
		participantRow("p2", "active", "Bo"),
	}

	denied := NewIntegration(nilBun, neverStaged, denyAll)
	got, err := denied.admitted(ctx, rows)
	if err != nil {
		t.Fatalf("admitted (deny-all) errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("admitted (deny-all) kept %d rows, want 0 -- the source rows are the only "+
			"point on this path where the row gate can resolve the real tier", len(got))
	}

	openGate := NewIntegration(nilBun, neverStaged, admitAll)
	got, err = openGate.admitted(ctx, rows)
	if err != nil {
		t.Fatalf("admitted (admit-all) errored: %v", err)
	}
	if len(got) != len(rows) {
		t.Errorf("admitted (admit-all) kept %d rows, want %d -- a gate that withholds "+
			"unconditionally is not a gate, it is an outage", len(got), len(rows))
	}
}

// TestAdmittedRefusesWhenTheGateIsMissing: an unwired gate is a boot
// misconfiguration and must be refused, not read as "yes". Mirrors
// TestRecentChatRefusesWhenTheStagedPredicateIsMissing for the sibling gate.
func TestAdmittedRefusesWhenTheGateIsMissing(t *testing.T) {
	c := NewIntegration(nilBun, neverStaged, nil)
	if _, err := c.admitted(context.Background(), []memorynodes.MemoryNode{
		participantRow("p1", "active", "Ana"),
	}); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("admitted with no gate = %v, want a refusal naming the unwired gate", err)
	}
	if _, err := c.foldActiveParticipants(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "not wired") {
		t.Errorf("foldActiveParticipants with no gate = %v, want the same refusal", err)
	}
}

// TestFoldDropsAnIdWhoseLatestVersionIsDenied is the ORDERING rule, and the
// reason foldActiveParticipants exists as its own function.
//
// The rows arrive newest-first with several versions per id. If the gate were
// applied to the slice BEFORE the per-id fold, a denied LATEST version would be
// removed and the fold would then treat the older, admitted version as the
// latest -- handing the caller a stale row instead of no row. That is the
// quietest failure available here, because a plausible answer is returned rather
// than an empty one.
//
// `p1` is denied on its latest version and admitted on its older one, so a
// pre-filter implementation returns "Ana (old)" and the correct one returns
// nothing for p1 at all. `p2` is the control: an ordinary admitted participant
// still comes back, so an implementation that simply dropped everything cannot
// pass.
func TestFoldDropsAnIdWhoseLatestVersionIsDenied(t *testing.T) {
	latestDenied := func(_ context.Context, n memorynodes.MemoryNode) bool {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		name, _ := p["displayName"].(string)
		return name != "Ana (latest)"
	}
	c := NewIntegration(nilBun, neverStaged, latestDenied)

	// createdAt DESC order, exactly as listParticipants' query returns them.
	items, err := c.foldActiveParticipants(context.Background(), []memorynodes.MemoryNode{
		participantRow("p1", "active", "Ana (latest)"),
		participantRow("p1", "active", "Ana (old)"),
		participantRow("p2", "active", "Bo"),
	})
	if err != nil {
		t.Fatalf("foldActiveParticipants: %v", err)
	}

	names := map[string]bool{}
	for _, it := range items {
		dn, _ := it["displayName"].(string)
		names[dn] = true
	}
	if names["Ana (old)"] {
		t.Error("a DENIED latest version fell through to an ADMITTED older one. " +
			"The fold must mark the id seen BEFORE gating it, so a denial drops the id " +
			"outright rather than revealing its predecessor (memql#4029)")
	}
	if names["Ana (latest)"] {
		t.Error("the denied row itself was emitted -- the gate is not being applied in the fold")
	}
	if !names["Bo"] {
		t.Error("the admitted participant is missing: this is the control, and without it a " +
			"fold that dropped everything would pass the assertions above")
	}
	if len(items) != 1 {
		t.Errorf("got %d participants, want exactly 1 (p2); items=%v", len(items), items)
	}
}

// TestFoldGatesBeforeReadingStatus: the gate runs before any payload field is
// read for domain filtering. `status` decides whether a participant is listed;
// deciding that from a row the caller may not see is the wrong way round even
// when nothing is emitted from it.
func TestFoldGatesBeforeReadingStatus(t *testing.T) {
	var gated []string
	c := NewIntegration(nilBun, neverStaged, func(_ context.Context, n memorynodes.MemoryNode) bool {
		gated = append(gated, n.ID)
		return false
	})
	if _, err := c.foldActiveParticipants(context.Background(), []memorynodes.MemoryNode{
		participantRow("inactive-1", "inactive", "Ana"),
	}); err != nil {
		t.Fatalf("foldActiveParticipants: %v", err)
	}
	if len(gated) != 1 {
		t.Errorf("the gate saw %d rows, want 1: an INACTIVE row must still reach the gate, "+
			"because skipping it first means the status field was read off an unauthorized row", len(gated))
	}
}

// TestEveryReadAppliesTheRowGate walks recent_chat.go's AST and asserts that
// each of the five capability functions reaches the gate.
//
// A source-level assertion, matching the engine's own house pattern for its
// enforcement seams (component/memql/rowauthz_toplevel_builtin_test.go pins
// `filterRowAuthzBuiltinNodes(` the same way). It earns its keep here for a
// specific reason: the behavioural tests above cover the pure helpers, but the
// wiring FROM each read INTO those helpers sits on the far side of a bun call
// that needs a database. Without this, a read could quietly stop calling the
// gate and every test in `make test` would still pass.
func TestEveryReadAppliesTheRowGate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "recent_chat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse recent_chat.go: %v", err)
	}

	// readRecent / readByKeyword / readByTime / getSpaceContext reach the gate
	// through `admitted`; listParticipants reaches it through the fold, which
	// carries its own ordering rule and its own tests above.
	want := map[string]string{
		"readRecent":       "admitted",
		"readByKeyword":    "admitted",
		"readByTime":       "admitted",
		"getSpaceContext":  "admitted",
		"listParticipants": "foldActiveParticipants",
	}

	found := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		gate, tracked := want[fn.Name.Name]
		if !tracked {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == gate {
				found[fn.Name.Name] = true
			}
			return true
		})
	}

	for name, gate := range want {
		if !found[name] {
			t.Errorf("%s does not call c.%s, so the rows it repacks reach the caller having "+
				"passed no per-row authorization.\n"+
				"This is the memql#4029 defect: the engine's row gate resolves a tier from the "+
				"returned node's concept, and everything this file returns is stamped "+
				"%q -- which declares no tier and admits.", name, gate, recentChatKind)
		}
	}
}
