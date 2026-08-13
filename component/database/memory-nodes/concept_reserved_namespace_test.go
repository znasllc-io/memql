package memoryNodes

import (
	"strings"
	"testing"
)

// engineNamespaceFields are the eight names memql#3613 added to
// reservedPayloadFields: heads the plan parser resolves to an ENGINE
// NAMESPACE in a filter path, which were nevertheless declarable as concept
// payload properties.
//
// The eight are not interchangeable in consequence, which is why the fix is a
// load-time refusal rather than a lint:
//
//   - `provenance` compiled to `provenance->>'<leaf>'` -- the row's own
//     engine-stamped column -- and the in-process post-filter agreed on that
//     same wrong field. A row whose payload.provenance.kind matched returned
//     match=false, err=nil. Wrong rows, forever, with nothing to indicate it.
//   - `actor` is authorization-relevant. `filter actor.userId == args.v`, the
//     natural spelling of "rows whose actor is the caller", compiled to `? = ?`
//     binding the resolved caller id against a caller-supplied argument --
//     const-folded, true whenever a caller passes their own id. The predicate
//     contributes nothing and the query returns every row of the concept.
//   - `args` / `now` / `config` / `meta` / `row` / `payload` were silent at
//     LOAD and loud only at CALL, since no load-time validation checks that a
//     filter's bare field resolves against the bound concept.
var engineNamespaceFields = []string{
	"provenance", "row", "actor", "args", "now", "config", "trace", "meta",
}

// TestConceptRefusesEngineNamespaceAsPayloadField is the load-time half of the
// memql#3613 fix. A concept declaring any engine namespace as a top-level
// payload property must be REFUSED, not registered with the field intact.
func TestConceptRefusesEngineNamespaceAsPayloadField(t *testing.T) {
	for _, field := range engineNamespaceFields {
		t.Run(field, func(t *testing.T) {
			src := "concept probe {\n  " + field + "  object\n  name  string\n}\n"
			_, err := buildConcept(t, src)
			if err == nil {
				t.Fatalf("concept declaring %q as a payload property loaded successfully; "+
					"it must be refused -- every filter naming %q bare addresses the engine "+
					"namespace, not this field", field, field)
			}
			if !strings.Contains(err.Error(), "reserved property") {
				t.Fatalf("BuildConceptFromDecl error for %q = %v; want the reserved-property refusal", field, err)
			}
		})
	}
}

// The concept-loader gate has always been case-INSENSITIVE (IsReservedPayloadField
// lower-cases before lookup), and it has to stay that way: the plan parser
// lower-cases every filter head it classifies, so `Provenance.kind` and
// `ACTOR.userId` are legal filters that resolve to the namespace. A
// case-sensitive load gate would admit `Provenance` as a payload property and
// leave exactly the defect this issue closes, spelled differently.
func TestConceptReservedFieldGateIsCaseInsensitive(t *testing.T) {
	for _, field := range append([]string{"Id", "CREATEDAT", "Payload"}, engineNamespaceFields...) {
		for _, spelling := range []string{strings.ToUpper(field), upperFirst(field)} {
			src := "concept probe {\n  " + spelling + "  object\n  name  string\n}\n"
			if _, err := buildConcept(t, src); err == nil {
				t.Errorf("concept declaring %q as a payload property loaded; the gate must "+
					"match the parser's case-insensitive head classification", spelling)
			}
		}
	}
}

// An ordinary payload property that merely CONTAINS a reserved name is not
// reserved -- the gate matches whole names, and narrowing it to a prefix or
// substring test would refuse `arguments`, `configuration`, `rowCount`, and
// the `arguments` field this issue renamed v1:observability:invocation.args to.
func TestConceptAdmitsPropertiesThatMerelyResembleReservedNames(t *testing.T) {
	for _, field := range []string{"arguments", "configuration", "rowCount", "actorUserId", "metadata", "nowish", "provenanceNote"} {
		src := "concept probe {\n  " + field + "  string\n}\n"
		if _, err := buildConcept(t, src); err != nil {
			t.Errorf("concept declaring %q was refused: %v -- only whole reserved names are reserved", field, err)
		}
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
