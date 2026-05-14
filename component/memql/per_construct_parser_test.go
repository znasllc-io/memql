package memql

import "testing"

// TestPerConstructParser_RejectsUnknownAnnotation locks the
// dedicated-parser allow-list. A query that carries @bogusAnnotation
// must surface a hard error at parse time -- not silently drop the
// annotation onto FunctionDef.Attributes.
func TestPerConstructParser_RejectsUnknownAnnotation(t *testing.T) {
	cases := []struct {
		kind   string
		source string
	}{
		{
			kind: "query",
			source: `@description("...")
@bogusAnnotation
@useConcept(participant)
query queryFoo {
  filter participant.spaceId == "x"
  shape participantFull
}`,
		},
		{
			kind: "mutation",
			source: `@description("...")
@bogusAnnotation
@useConcept(participant)
mutation mutationFoo {
  args { id string @required }
  insert {
    id: args.id
  }
}`,
		},
		{
			kind: "logic",
			source: `@description("...")
@bogusAnnotation
logic logicFoo {
  body {
    return 1
  }
}`,
		},
		{
			kind: "automation",
			source: `@description("...")
@bogusAnnotation
@trigger(event="system.startup")
automation autoFoo {
  step noop { logic noopLogic { } }
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var fn *Function
			var err error
			switch tc.kind {
			case "query":
				fn, err = parseQueryMemQL("queryFoo", "test", tc.source, nil)
			case "mutation":
				fn, err = parseMutationMemQL("mutationFoo", "test", tc.source, nil)
			case "logic":
				fn, err = parseLogicMemQL("logicFoo", "test", tc.source, nil)
			case "automation":
				fn, err = parseAutomationMemQL("autoFoo", "test", tc.source, nil)
			}
			if err == nil {
				t.Fatalf("%s: expected unknown-annotation error, got nil (fn=%v)", tc.kind, fn)
			}
			if !containsStr(err.Error(), "@bogusAnnotation") {
				t.Errorf("%s: error should mention @bogusAnnotation, got %v", tc.kind, err)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
