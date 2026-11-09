package app

// routing_rule_renderer_test.go -- the evidence fold's Form and the renderer's
// Form stay the same shape (epic memql#5146, D5).
//
// component/router (a leaf module) mirrors component/routingrules.Form field
// for field, because the leaf cannot import the root module that depends on
// it, and engine_authored.go copies one into the other to call the tree's one
// rule renderer. A field added to one side and not the other would compile and
// be dropped on the way through: the fold would propose a rule, the person
// would approve its hash, and the text would be missing what the evidence put
// in the form. This package is the one place both types are visible, so the
// comparison lives here.

import (
	"reflect"
	"testing"

	routerlib "github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/routingrules"
)

// TestEvidenceFormMirrorsTheRoutingRulesForm: the two Form types declare the
// same fields, in the same order, with the same types.
func TestEvidenceFormMirrorsTheRoutingRulesForm(t *testing.T) {
	fields := func(v any) []string {
		rt := reflect.TypeOf(v)
		out := make([]string, 0, rt.NumField())
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			out = append(out, f.Name+" "+f.Type.String())
		}
		return out
	}
	evidence, renderer := fields(routerlib.Form{}), fields(routingrules.Form{})
	if !reflect.DeepEqual(evidence, renderer) {
		t.Fatalf("component/router.Form and component/routingrules.Form have diverged:\n  router:       %v\n  routingrules: %v\n"+
			"engine_authored.go copies one into the other field by field, so a field on one side only is dropped silently",
			evidence, renderer)
	}
}
