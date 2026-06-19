package memql

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/language/ast"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// executor_mutation_readmerge_audit_test.go is the generic, registry-driven
// half of the memql#1709 acceptance: "audit ALL mutations to confirm
// create-vs-update semantics and that omitted fields are preserved on the
// update path."
//
// The behavioural per-family proofs live in
// executor_mutation_readmerge_db_test.go (Postgres-gated). This file is the
// static, no-DB invariant that holds for EVERY mutation in the shipped DSL
// tree, because the read-merge now lives in ONE engine chokepoint
// (executeWrite, reached by both executeInsert and executeUpdate):
//
//   - update-kind mutations always read-merge (the update{} contract).
//   - insert-kind mutations read-merge whenever their target id already
//     names a stored row (memql#1709). A genuinely new id (or an
//     auto-generated id) is a plain create, validated in full.
//
// So the "non-create" class -- update / delete / revoke / transition -- is
// exactly {update-kind} ∪ {insert-kind that targets an explicit id and hits
// an existing row}, and BOTH branches preserve omitted fields without the
// mutation having to re-state every required field. The audit asserts the
// structural precondition for that guarantee across the whole tree: every
// mutation is either insert- or update-kind (no third path that could bypass
// executeWrite), and it enumerates the classification so a regression (e.g.
// a new mutation kind, or leaveSpace/revokeDelegation drifting back to a
// full-record-replace shape) is caught.

func loadAllMutationTemplates(t *testing.T) map[string]*FunctionMutationTemplate {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)

	out := make(map[string]*FunctionMutationTemplate)
	for _, fn := range fnRegistry.List() {
		if fn == nil || fn.MutationTemplate == nil {
			continue
		}
		out[fn.Name] = fn.MutationTemplate
	}
	require.NotEmpty(t, out, "expected the unified tree to register mutation templates")
	return out
}

// hasExplicitID reports whether a mutation template pins an explicit id
// (i.e. targets a specific, known row) rather than letting Create
// auto-generate one. An auto-id mutation is structurally always a create.
func hasExplicitID(tmpl *FunctionMutationTemplate) bool {
	switch v := tmpl.IDTemplate.(type) {
	case nil:
		return false
	case string:
		return v != ""
	default:
		return true
	}
}

// TestReadMergeAudit_EveryMutationRoutesThroughReadMergePath is the
// tree-wide invariant: every shipped mutation is insert- or update-kind, so
// every mutation write reaches the single executeWrite chokepoint where the
// engine-level read-merge lives (memql#1709). A mutation of any other kind
// would silently bypass read-merge -- this fails closed if one ever appears.
func TestReadMergeAudit_EveryMutationRoutesThroughReadMergePath(t *testing.T) {
	templates := loadAllMutationTemplates(t)

	var (
		updateKind   []string // always read-merge (update{} contract)
		insertWithID []string // create-or-upsert: read-merge when the row exists (#1709)
		insertAutoID []string // pure create: fresh id every call
		unknownKind  []string // MUST stay empty -- would bypass executeWrite
	)

	for name, tmpl := range templates {
		switch tmpl.Kind {
		case ast.MutationKindUpdate:
			updateKind = append(updateKind, name)
		case ast.MutationKindInsert, "":
			if hasExplicitID(tmpl) {
				insertWithID = append(insertWithID, name)
			} else {
				insertAutoID = append(insertAutoID, name)
			}
		default:
			unknownKind = append(unknownKind, name+" (kind="+string(tmpl.Kind)+")")
		}
	}
	for _, s := range [][]string{updateKind, insertWithID, insertAutoID, unknownKind} {
		sort.Strings(s)
	}

	require.Empty(t, unknownKind,
		"every mutation must be insert- or update-kind so it routes through executeWrite's "+
			"engine-level read-merge (memql#1709); these bypass it: %v", unknownKind)

	// The "non-create" class the issue cares about (update / delete / revoke /
	// transition) is {update-kind} ∪ {insert-kind onto an existing explicit id}.
	// Both branches read-merge; assert both are non-empty so the audit is
	// actually exercising the tree, and log the full classification.
	nonCreateCount := len(updateKind) + len(insertWithID)
	require.NotZero(t, len(updateKind), "expected update-kind mutations in the tree")
	require.NotZero(t, len(insertWithID), "expected insert-with-explicit-id mutations in the tree")

	t.Logf("mutation read-merge audit (memql#1709): %d total mutations", len(templates))
	t.Logf("  update-kind (always read-merge):            %d", len(updateKind))
	t.Logf("  insert-kind w/ explicit id (upsert merge):  %d", len(insertWithID))
	t.Logf("  insert-kind auto-id (pure create):          %d", len(insertAutoID))
	t.Logf("  non-create class covered by read-merge:     %d", nonCreateCount)
	t.Logf("  update-kind mutations: %v", updateKind)
}

// TestReadMergeAudit_Run4Regressions pins the two mutations the issue calls
// out as still-broken in Run 4 (mutationLeaveSpace, mutationRevokeDelegation).
// They are authored as insert{} with an explicit id and a partial payload --
// the exact shape that used to reject with "” does not validate ... missing
// properties". The engine-level read-merge (memql#1709) is the canonical fix:
// because they target an explicit id, executeWrite read-merges the partial
// onto the stored row. This test asserts they retain that shape (insert-kind
// + explicit id) so the audit documents that they depend on the engine fix
// rather than a per-mutation DSL conversion (which the issue explicitly said
// NOT to apply). The behavioural preservation proofs are
// TestReadMerge_LeaveSpace_* and TestReadMerge_RevokeDelegation_* in the
// DB-gated file.
func TestReadMergeAudit_Run4Regressions(t *testing.T) {
	templates := loadAllMutationTemplates(t)

	for _, name := range []string{"mutationLeaveSpace", "mutationRevokeDelegation"} {
		tmpl, ok := templates[name]
		require.True(t, ok, "%s must be registered from the unified tree", name)
		require.True(t, hasExplicitID(tmpl),
			"%s must target an explicit id so the engine read-merge applies (memql#1709)", name)
		// insert-kind ("" defaults to insert) -- i.e. NOT hand-converted to
		// update{}; the engine merge covers it.
		require.NotEqual(t, ast.MutationKindUpdate, tmpl.Kind,
			"%s is intentionally left as insert{}; the engine-level read-merge (memql#1709) "+
				"is the canonical fix, not a per-mutation update{} conversion", name)
	}
}
