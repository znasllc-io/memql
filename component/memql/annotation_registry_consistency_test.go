package memql

import (
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// TestAnnotationReceiverGateConsistency is the #991 anti-drift guard: for the
// four function constructs the editor's annotation surface
// (sense.AnnotationsByReceiver) must be identical to the parser's load-time
// allow-list gate (constructAnnotationAllowLists). The three annotation tiers
// the audit (#964) flagged drifted repeatedly (the post-#989/#990 Tool/Query
// drift was the latest); this test makes a divergence a hard failure instead of
// a silent editor-vs-loader mismatch.
func TestAnnotationReceiverGateConsistency(t *testing.T) {
	cases := []struct {
		receiver string
		ft       languageParser.FunctionType
	}{
		{"Query", languageParser.FunctionTypeQuery},
		{"Mutation", languageParser.FunctionTypeMutation},
		{"Logic", languageParser.FunctionTypeLogic},
		{"Automation", languageParser.FunctionTypeAutomation},
	}
	for _, c := range cases {
		gate, ok := constructAnnotationAllowLists[c.ft]
		if !ok {
			t.Fatalf("no parser gate allow-list for %s", c.receiver)
		}
		senseSet := map[string]bool{}
		for _, a := range sense.AnnotationsByReceiver[c.receiver] {
			senseSet[a] = true
		}
		for name := range gate {
			if !senseSet[name] {
				t.Errorf("%s: parser gate allows @%s but the editor (sense.AnnotationsByReceiver) omits it", c.receiver, name)
			}
		}
		for name := range senseSet {
			if !gate[name] {
				t.Errorf("%s: editor lists @%s but the parser gate rejects it at load", c.receiver, name)
			}
		}
	}
}
