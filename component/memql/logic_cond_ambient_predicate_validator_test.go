package memql

import (
	"strings"
	"testing"
)

// The half of memql#2962 the arg-substitution fix structurally cannot reach.
//
// Expansion resolves an `args.`-rooted comparison predicate because it is
// handed the args map. It is handed nothing else, so an ambient comparison --
// `actor.`, `config.`, `partition`, `trace` -- falls through, resolves against
// a nil lambda scope, and takes the else branch for every input. That is the
// reported defect surviving in the namespace the report's own motivation is
// about: a role gate open or closed by accident rather than by the role.
//
// The near miss is in the tree. dsl/deployment/logic.memql's owner-only
// rollback gate reads
//
//	role := actor.role ?? ""
//	allowed := cond(role == "owner", true, false)
//
// and is correct ONLY because the ambient value is bound to a local first.
// Inlining those two lines -- the obvious simplification, and one a reviewer
// would wave through -- silently disables the gate.
//
// #2962's definition of done says an unsupported shape must be a load-time
// error rather than a silent constant. Supporting it properly means plumbing
// the actor envelope and config through arg expansion, which is wider than
// this issue; until then the shape is refused at load.
func loadCondAmbientPredicateProbe(pred string) error {
	src := strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"cond ambient predicate probe\")",
		"logic condAmbientPredProbe {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    return cond(" + pred + ", \"elevated\", \"plain\")",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condAmbientPredProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	return err
}

func TestLogicCondAmbientPredicate_RejectedAtLoad(t *testing.T) {
	for name, pred := range map[string]string{
		"actor-role":    `actor.role == "owner"`,
		"actor-isowner": `actor.isClusterOwner == true`,
		// No actor.claims case: the actor envelope is a closed set (#2623) and
		// an unknown member is already rejected by that gate before this one,
		// so it would be testing the wrong validator.
		"config-value":   `config.someFlag == "on"`,
		"partition-root": `partition == "default"`,
		"not-equal":      `actor.role != "owner"`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadCondAmbientPredicateProbe(pred)
			if err == nil {
				t.Fatalf("cond(%s, ...) loaded green. Arg substitution receives only args, so "+
					"this predicate is never resolved -- it takes the ELSE branch for every "+
					"input, silently, which is what memql#2962 is about. It must be a load "+
					"error until the ambient namespaces are plumbed through expansion.", pred)
			}
			if !strings.Contains(err.Error(), "2962") {
				t.Errorf("the rejection should cite the issue so the reason is findable, got: %v", err)
			}
		})
	}
}

// The counterpart. A rejection that fires on the shapes the tree actually uses
// would break boot, and this validator sits on the load path for every binary.
func TestLogicCondAmbientPredicate_LeavesLegitimateShapesAlone(t *testing.T) {
	// The live shape: the ambient value is bound to a local first, and the
	// comparison is over that local. Every real cond role gate in dsl/ is
	// written this way and must keep loading.
	src := strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"local-bound ambient probe\")",
		"logic condAmbientLocalProbe {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    role := actor.role ?? \"\"",
		"    allowed := cond(role == \"owner\", true, false)",
		"    return allowed",
		"  }",
		"}",
	}, "\n")
	if _, err := tryParseNewFunctionSyntax("condAmbientLocalProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry()); err != nil {
		t.Fatalf("binding the ambient value to a local first is the CORRECT authoring, and it is "+
			"what dsl/deployment/logic.memql and dsl/forge/logic.memql already do. Rejecting it "+
			"would break boot: %v", err)
	}

	// And the case the substitution fix handles: an args-rooted predicate must
	// still load, since it now evaluates correctly.
	if err := loadCondAmbientPredicateProbe(`args.a == "owner"`); err != nil {
		t.Fatalf("an args-rooted comparison predicate is resolved by expansion and must load: %v", err)
	}
}
