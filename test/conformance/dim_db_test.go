package conformance

// dim_db_test.go -- the DB-backed contract dimensions, driven through the MCP
// run_query / run_mutation meta-tools against a seeded engine. These SKIP when
// no Postgres is reachable and RUN in CI where the conformance job provides one.
//
// The data record family (v1:data:record) is the representative lifecycle
// vehicle: it has a clean create/read/update/delete quad plus a state-filtered
// query, so one seeded family exercises lifecycle, inclusion/exclusion,
// partial-update preservation, and the canonical envelope at once. The PII
// dimension uses the user concept (the @pii-annotated concept hard-delete
// scrubs). Coverage of the FULL tool surface for the schema dimension is in
// dim_schema_test.go; see coverageReport in conformance_test.go for the gap log.

import (
	"fmt"
	"strings"
	"testing"
)

// seedRecord creates one data record via the MCP run_mutation path and returns
// its canonical stored id.
func seedRecord(t *testing.T, e *Env, recordId, partitionId, label string, data map[string]any) string {
	t.Helper()
	out := e.runMutation(t, "createRecord", map[string]any{
		"recordId":     recordId,
		"partitionId":  partitionId,
		"recordType":   "vehicle",
		"label":        label,
		"data":         data,
		"importSource": "manual",
		"importedBy":   "identity-conformance",
	})
	return storedID(t, out)
}

// storedID pulls the written node id out of a mutation's MCP result, tolerating
// the shapes the engine emits (array of node objects, single node object, or a
// {nodes:[...]} bundle).
func storedID(t *testing.T, out any) string {
	t.Helper()
	pick := func(m map[string]any) string {
		if s, ok := m["id"].(string); ok && s != "" {
			return s
		}
		return ""
	}
	switch v := out.(type) {
	case map[string]any:
		if id := pick(v); id != "" {
			return id
		}
		if nodes, ok := v["nodes"].([]any); ok && len(nodes) > 0 {
			if m, ok := nodes[0].(map[string]any); ok {
				if id := pick(m); id != "" {
					return id
				}
			}
		}
	case []any:
		if len(v) > 0 {
			if m, ok := v[0].(map[string]any); ok {
				if id := pick(m); id != "" {
					return id
				}
			}
		}
	}
	t.Fatalf("could not extract a stored node id from mutation result: %#v", out)
	return ""
}

// idOf returns the id field of a result row.
func rowID(m any) string {
	row, _ := m.(map[string]any)
	if row == nil {
		return ""
	}
	s, _ := row["id"].(string)
	return s
}

// --- the suite contract: per-tool create -> read -> update -> delete ---

func lifecycleCheck() check {
	return check{
		Issue:   "#1715",
		Dim:     "lifecycle-create-read-update-delete",
		NeedsDB: true,
		Run:     runLifecycle,
	}
}

func runLifecycle(t *testing.T, e *Env) {
	space := "space-lifecycle-" + uniqueSuffix("crud")

	// CREATE (>=2 distinct inputs).
	idA := seedRecord(t, e, "rec-A-"+uniqueSuffix("crud"), space, "2024 Toyota Camry",
		map[string]any{"make": "Toyota", "model": "Camry"})
	idB := seedRecord(t, e, "rec-B-"+uniqueSuffix("crud"), space, "2023 Honda Accord",
		map[string]any{"make": "Honda", "model": "Accord"})

	// READ -- both are present, active, in the space.
	got := asArray(t, e.runQuery(t, "recordsByState", map[string]any{"partitionId": space}))
	if !containsID(got, idA) || !containsID(got, idB) {
		t.Fatalf("lifecycle READ: expected both %s and %s in %v", idA, idB, idIDs(got))
	}

	// UPDATE -- a minimal label change must succeed and persist.
	e.runMutation(t, "updateRecord", map[string]any{
		"recordId": idA,
		"label":    "2024 Toyota Camry (updated)",
	})
	pa := e.latestPayload(t, "v1:data:record", idA)
	if pa["label"] != "2024 Toyota Camry (updated)" {
		t.Fatalf("lifecycle UPDATE: label not persisted; got %v", pa["label"])
	}

	// DELETE -- flips active=false; the active-filtered read no longer returns it.
	e.runMutation(t, "deleteRecord", map[string]any{"recordId": idA})
	pa = e.latestPayload(t, "v1:data:record", idA)
	if pa["active"] != false {
		t.Fatalf("lifecycle DELETE: active not flipped to false; got %v", pa["active"])
	}
	after := asArray(t, e.runQuery(t, "recordsByState", map[string]any{"partitionId": space}))
	if containsID(after, idA) {
		t.Fatalf("lifecycle DELETE: deleted record %s still returned by active query: %v", idA, idIDs(after))
	}
	if !containsID(after, idB) {
		t.Fatalf("lifecycle DELETE: sibling %s wrongly dropped: %v", idB, idIDs(after))
	}
	t.Logf("#1715 lifecycle: create(x2) -> read -> update -> delete verified on the record family")
}

// --- #1708: inclusion/exclusion for filtered queries ---

func filterCheck() check {
	return check{
		Issue:   "#1708",
		Dim:     "filter-inclusion-exclusion",
		NeedsDB: true,
		Run:     runFilter,
	}
}

func runFilter(t *testing.T, e *Env) {
	spaceX := "space-filter-X-" + uniqueSuffix("flt")
	spaceY := "space-filter-Y-" + uniqueSuffix("flt")

	inX1 := seedRecord(t, e, "rec-X1-"+uniqueSuffix("flt"), spaceX, "X one", map[string]any{"k": 1})
	inX2 := seedRecord(t, e, "rec-X2-"+uniqueSuffix("flt"), spaceX, "X two", map[string]any{"k": 2})
	outY := seedRecord(t, e, "rec-Y1-"+uniqueSuffix("flt"), spaceY, "Y one", map[string]any{"k": 3})

	res := asArray(t, e.runQuery(t, "recordsByState", map[string]any{"partitionId": spaceX}))

	// Inclusion: both spaceX records present.
	if !containsID(res, inX1) || !containsID(res, inX2) {
		t.Fatalf("#1708 inclusion: spaceX records missing; got %v", idIDs(res))
	}
	// Exclusion: the spaceY record must NOT leak into a spaceX-filtered query
	// (the dual assertion that guards against a too-broad predicate).
	if containsID(res, outY) {
		t.Fatalf("#1708 exclusion: spaceY record %s leaked into spaceX filter: %v", outY, idIDs(res))
	}

	// recordType is an additional ANDed predicate -- a non-matching type excludes.
	none := asArray(t, e.runQuery(t, "recordsByState", map[string]any{
		"partitionId": spaceX, "recordType": "no-such-type",
	}))
	if containsID(none, inX1) || containsID(none, inX2) {
		t.Fatalf("#1708 exclusion: recordType predicate not ANDed; got %v", idIDs(none))
	}
	t.Logf("#1708: inclusion + exclusion (space + recordType predicates) verified")
}

// --- #1709: partial update preserves omitted sibling fields ---

func partialUpdateCheck() check {
	return check{
		Issue:   "#1709",
		Dim:     "partial-update-preserves-siblings",
		NeedsDB: true,
		Run:     runPartialUpdate,
	}
}

func runPartialUpdate(t *testing.T, e *Env) {
	space := "space-readmerge-" + uniqueSuffix("rm")
	recID := seedRecord(t, e, "rec-rm-"+uniqueSuffix("rm"), space, "Original Label",
		map[string]any{"make": "Subaru", "model": "Outback"})
	before := e.latestPayload(t, "v1:data:record", recID)

	// MINIMAL delete -- id only, no required record fields. Pre-#1709 this
	// rejected ("missing properties") or wiped omitted fields; now it read-merges.
	e.runMutation(t, "deleteRecord", map[string]any{"recordId": recID})

	after := e.latestPayload(t, "v1:data:record", recID)
	if after["active"] != false {
		t.Fatalf("#1709: delete must flip active=false; got %v", after["active"])
	}
	for _, f := range []string{"label", "recordType", "partitionId", "importSource", "data"} {
		if fmt.Sprintf("%v", after[f]) != fmt.Sprintf("%v", before[f]) {
			t.Errorf("#1709: omitted field %q not preserved across partial update: before=%v after=%v", f, before[f], after[f])
		}
	}
	t.Logf("#1709: partial (id-only) mutation preserved every omitted sibling field")
}

// --- #1710: canonical array envelope for 0 / 1 / many ---

func envelopeCheck() check {
	return check{
		Issue:   "#1710",
		Dim:     "canonical-array-envelope-0-1-many",
		NeedsDB: true,
		Run:     runEnvelope,
	}
}

func runEnvelope(t *testing.T, e *Env) {
	// 0 results: a never-seeded space.
	zero := e.runQuery(t, "recordsByState", map[string]any{"partitionId": "space-empty-" + uniqueSuffix("env")})
	z := asArray(t, zero)
	if len(z) != 0 {
		t.Fatalf("#1710 (0): expected empty array, got %v", z)
	}

	// 1 result.
	oneSpace := "space-one-" + uniqueSuffix("env")
	seedRecord(t, e, "rec-one-"+uniqueSuffix("env"), oneSpace, "solo", map[string]any{"k": 1})
	one := asArray(t, e.runQuery(t, "recordsByState", map[string]any{"partitionId": oneSpace}))
	if len(one) != 1 {
		t.Fatalf("#1710 (1): expected a single-element ARRAY (not a bare object), got %d elems: %v", len(one), one)
	}

	// many results.
	manySpace := "space-many-" + uniqueSuffix("env")
	for i := 0; i < 3; i++ {
		seedRecord(t, e, fmt.Sprintf("rec-many-%d-%s", i, uniqueSuffix("env")), manySpace, fmt.Sprintf("m%d", i), map[string]any{"k": i})
	}
	many := asArray(t, e.runQuery(t, "recordsByState", map[string]any{"partitionId": manySpace}))
	if len(many) != 3 {
		t.Fatalf("#1710 (many): expected 3 elems, got %d: %v", len(many), many)
	}
	t.Logf("#1710: query result is a canonical array for 0, 1, and many rows")
}

// --- helpers shared by the record-family dimensions ---

func containsID(rows []any, id string) bool {
	for _, r := range rows {
		got := rowID(r)
		if got == id || strings.HasSuffix(got, id) || strings.HasSuffix(id, got) {
			return true
		}
	}
	return false
}

func idIDs(rows []any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowID(r))
	}
	return out
}
