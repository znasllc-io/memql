package calls

import (
	"encoding/json"
	"slices"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// TestTheSeatConceptHoldsAKindToTheRulesSeatKinds: a seat's kind is one of the rules
// module's four (calls.SeatKind: person, agent, avatar, sampler) and nothing else, held by
// the concept itself. The kind decides what a seat counts against (people against
// maxPeople, agents against maxAgents) and which grant its pass carries (calls.GrantFor),
// so a free string would let a misspelt kind take a seat that counts against neither.
//
// An enum is a narrowing, which is safe only while no stored row can hold another value:
// no release carries calls yet, the service mints only calls.KindPerson (service.go), and
// no seed writes a seat. Adding a kind later is a widening and needs no migration.
func TestTheSeatConceptHoldsAKindToTheRulesSeatKinds(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.Get(seatConcept)
	if err != nil || c == nil {
		t.Fatalf("%s is not in the loaded tree: %v", seatConcept, err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
		t.Fatalf("the seat concept's schema does not parse: %v", err)
	}
	got := slices.Clone(schema.Properties["kind"].Enum)
	slices.Sort(got)
	want := []string{string(callrules.KindAgent), string(callrules.KindAvatar), string(callrules.KindPerson), string(callrules.KindSampler)}
	if !slices.Equal(got, want) {
		t.Fatalf("v1:calls:seat.kind accepts %v, want exactly the rules module's seat kinds %v", got, want)
	}
}
