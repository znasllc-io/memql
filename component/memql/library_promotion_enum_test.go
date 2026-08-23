package memql

import (
	"encoding/json"
	"sort"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The Library index's `source` enum must CONTAIN every backing concept's own
// `source` enum (memql#4340 / memql#4342).
//
// WHY THIS IS A GATE AND NOT A COMMENT. The six index*OnCreate promotions bind
// the backing row's `source` off the CDC payload and pass it straight through to
// createArtifact (dsl/library/automations.memql). MemQL validates an enum
// argument at execute time, so a value the BACKING concept can hold and the
// INDEX cannot is not a type error anybody sees at load -- it is a promotion
// that refuses at runtime, for that row only. The row keeps its bytes, gets no
// index row, and never appears in the Library; the automation's failure is a log
// line on a path nobody is watching.
//
// That is exactly how `exported` shipped broken: memql#4340 gave
// v1:library:file the four-value enum the design's section 3.1 specifies
// (uploaded / exported / agent_generated / derived) while the index carried the
// seven values the five older backing concepts needed, and `exported` was in the
// first set and not the second. Every uploaded file promoted, so nothing looked
// wrong.
//
// The containment direction is the whole assertion: the index may hold values no
// backing row uses (it is the union across six concepts), but no backing row may
// hold a value the index cannot.
func TestEveryBackingSourceValueIsPromotable(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	const indexConcept = "v1:library:artifact"
	index := sourceEnumValues(t, indexConcept)
	if len(index) == 0 {
		t.Fatalf("%s declares no `source` enum, so this test would pass while measuring nothing", indexConcept)
	}
	inIndex := make(map[string]bool, len(index))
	for _, v := range index {
		inIndex[v] = true
	}

	// MEMBERSHIP CRITERION: a concept belongs here when its promotion passes
	// the backing row's OWN `source` value through to createArtifact -- written
	// `source,` in the automation's mutation call. Measured, not assumed: of the
	// six index*OnCreate promotions in dsl/library/automations.memql, only these
	// two do. The other four (note, todo, calendarEvent, memory) hardcode
	// `source: "agent_generated"`, so the backing concept's own enum never
	// reaches the index.
	//
	// That distinction is why v1:calendar:calendarEvent is ABSENT even though it
	// declares a `source` enum of its own (native / externalSync). Those values
	// are not in the index's enum and must not be added: nothing passes them,
	// and widening the index to accept values no promotion can produce would
	// make this gate assert less while looking like it asserts more.
	//
	// Adding a seventh promotion means adding its concept here IF it passes
	// source through. A pass-through promotion absent from this list is
	// unmeasured, which is the state that let `exported` through.
	backing := []string{
		"v1:library:file",
		"v1:library:generatedOutput",
	}

	for _, name := range backing {
		values := sourceEnumValues(t, name)
		if len(values) == 0 {
			t.Errorf("%s declares no `source` enum -- if that is deliberate, drop it from `backing`; "+
				"leaving it here measures nothing", name)
			continue
		}
		for _, v := range values {
			if !inIndex[v] {
				t.Errorf("%s.source can be %q, but %s.source cannot hold it.\n"+
					"A row written with that value promotes through createArtifact and is REFUSED at "+
					"execute time, so it never gets an index row and never appears in the Library -- "+
					"and nothing on the failing path says so.\n"+
					"Fix by adding %q to %s.source AND to createArtifact's `source` argument enum in "+
					"dsl/library/mutations.memql (both, or the concept accepts what the mutation "+
					"rejects).\n  index enum: %v\n  %s enum: %v",
					name, v, indexConcept, v, indexConcept, index, name, values)
			}
		}
	}
}

// sourceEnumValues pulls a concept's declared `source` enum values off the
// loaded schema, sorted. Returns nil when the concept is absent or declares no
// such field -- the caller decides whether that is a failure.
func sourceEnumValues(t *testing.T, conceptName string) []string {
	t.Helper()
	c, ok := memorynodes.All()[conceptName]
	if !ok || c == nil {
		t.Fatalf("%s is not in the concept registry", conceptName)
	}
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("%s definition schema: %v", conceptName, err)
	}
	var doc struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s schema: %v", conceptName, err)
	}
	prop, ok := doc.Properties["source"]
	if !ok {
		return nil
	}
	out := append([]string(nil), prop.Enum...)
	sort.Strings(out)
	return out
}
