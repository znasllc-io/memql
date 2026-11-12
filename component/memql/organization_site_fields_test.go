package memql

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestEverySiteFieldHasAPart holds siteFieldParts to the concept: every
// v1:platform:site field in the committed field snapshot is listed exactly once,
// and nothing is listed that the concept does not declare. A field added
// without a decision would otherwise be ordinary data, and a member holding Edit
// on Data could change it on a live site with no part at all.
func TestEverySiteFieldHasAPart(t *testing.T) {
	raw, err := os.ReadFile("../conceptfields/concept-fields.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Concepts []struct {
			Concept string   `json:"concept"`
			Fields  []string `json:"fields"`
		} `json:"concepts"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, c := range snap.Concepts {
		if c.Concept == conceptPlatformSite {
			for _, f := range c.Fields {
				declared[f] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("the snapshot names no %s fields", conceptPlatformSite)
	}
	listed := map[string]int{}
	for _, f := range siteFieldParts {
		listed[f.field]++
	}
	var missing, extra, twice []string
	for f := range declared {
		if listed[f] == 0 {
			missing = append(missing, f)
		}
	}
	for f, n := range listed {
		if !declared[f] {
			extra = append(extra, f)
		}
		if n > 1 {
			twice = append(twice, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(twice)
	if len(missing)+len(extra)+len(twice) > 0 {
		t.Fatalf("siteFieldParts disagrees with the concept: not listed %v, not declared %v, listed twice %v -- decide which deployable part a change to each field asks (organization_action_capabilities.go)", missing, extra, twice)
	}
}
