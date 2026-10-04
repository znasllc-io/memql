package memql

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/readiness"
)

// THE SHELL FOLDS WHAT THE SHAPE PROJECTS, AND NOTHING ELSE. The OS reads
// moduleReadinessAll's shaped rows (clients/os/src/live/readiness.tsx), so a
// report field the shape omits arrives there as undefined -- which the feed
// turns into false or an empty list, a VALUE and not an error. The Go read
// takes the bundle and sees every field, so no engine-side test notices.
//
// The field list is read off readiness.NodeReport's own JSON tags rather than
// restated here: the report IS the row, and a list in this file would be a
// third copy of the same fact.
func TestTheReadinessShapeProjectsEveryReportField(t *testing.T) {
	raw, err := os.ReadFile("../../dsl/platform/shapes.memql")
	if err != nil {
		t.Fatalf("read dsl/platform/shapes.memql: %v", err)
	}
	body := constructBody(t, string(raw), "shape moduleReadiness moduleReadinessFull")

	rt := reflect.TypeOf(readiness.NodeReport{})
	checked := 0
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		checked++
		if !projectsField(body, name) {
			t.Errorf("shape moduleReadinessFull does not project %q, which every readiness row carries.\n"+
				"The shell's fold would read it as absent. Add it to dsl/platform/shapes.memql.", name)
		}
	}
	// A REACHABLE POSITIVE: a struct read that found no tags would pass the
	// loop above having checked nothing.
	if checked < 10 {
		t.Fatalf("checked %d report fields; NodeReport carries at least ten", checked)
	}

	// And the query has to NAME that shape, or the projection above is the
	// projection of a shape nobody reads.
	queries, err := os.ReadFile("../../dsl/platform/queries.memql")
	if err != nil {
		t.Fatalf("read dsl/platform/queries.memql: %v", err)
	}
	q := constructBody(t, string(queries), "query moduleReadiness moduleReadinessAll")
	if !regexp.MustCompile(`(?m)^\s*shape\s+moduleReadinessFull\s*$`).MatchString(q) {
		t.Fatalf("query moduleReadinessAll does not declare `shape moduleReadinessFull`; its body was:\n%s", q)
	}
}
