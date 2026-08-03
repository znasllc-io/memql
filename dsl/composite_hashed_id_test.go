package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageCompiler "github.com/znasllc-io/memql/component/language/compiler"
)

// composite_hashed_id_test.go -- memql#2980.
//
// A mutation deriving its concept id as `hash(concat(a, sep, b))` keys two
// values into one string. That is injective only if the split is recoverable,
// and it is not by default:
//
//	("d:x", "y")  and  ("d", "x:y")   ->  hash("d:x:y")
//
// Two distinct (deployment, nodeType) pairs collapse onto one timeline and
// nodeSpecsForDeployment returns the wrong spec for one of them. Verified
// through the real render path in component/memql's
// TestShortId_DoesNotMakeAColonBearingArgSafe.
//
// Normalising the FK does not close it -- shortId("d:x") is "d:x" -- so the
// rule is about the SEPARATOR, not the normaliser:
//
//	in hash(concat(a, sep, b)), every part after the first must be free of sep.
//
// With the trailing part sep-free the split at the last sep is unique, so
// equal concatenations force equal parts. The leading part may contain sep
// freely, which matters here: deploymentId legitimately accepts the canonical
// "v1:cluster:deployment:<short>" form.
//
// This gate exists because the constraint is one annotation in one file and
// nothing structural holds it there. component/memql's companion test proves
// the pattern WORKS; this proves it is still WRITTEN. Delete the @pattern and
// that test stays green, which is the failure mode memql#2980 is itself an
// instance of -- a rule recorded somewhere and enforced nowhere.
//
// Scope, stated honestly: this checks the two known composite-hashed-id
// mutations by name rather than detecting the shape tree-wide. A general
// detector -- find every `id: hash(concat(...))` and verify its trailing key
// parts are constrained -- is the right long-term gate and is NOT here.
// Adding a third such mutation will not trip this test.
func TestCompositeHashedIdTrailingPartRejectsTheSeparator(t *testing.T) {
	const path = "deployment/mutations.memql"

	fh, err := Tree().Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	raw, err := io.ReadAll(fh)
	fh.Close()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Through the same entry point the tree load uses, so this is the authored
	// construct as the engine sees it and not a regex over source text.
	file, err := languageCompiler.ParseFileSource(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Both mutations derive the id from (deploymentId, nodeType) and their id
	// expressions are required to stay byte-identical (#2885), so the
	// constraint has to be on both or a re-pin forks a second timeline.
	want := map[string]bool{
		"createDeploymentNodeSpec": false,
		"updateDeploymentNodeSpec": false,
	}

	for _, def := range file.Definitions {
		fn, ok := def.(*languageAst.FunctionDef)
		if !ok {
			continue
		}
		if _, tracked := want[fn.Name]; !tracked {
			continue
		}
		want[fn.Name] = true

		var nodeType *languageAst.ArgsField
		if fn.ArgsSchema != nil {
			for _, arg := range fn.ArgsSchema.Fields {
				if arg != nil && arg.Name == "nodeType" {
					nodeType = arg
				}
			}
		}
		if nodeType == nil {
			t.Errorf("%s: no `nodeType` arg. The composite id is "+
				"hash(concat(shortId(deploymentId), \":\", nodeType)); if the trailing key part "+
				"was renamed, this gate needs renaming with it (memql#2980).", fn.Name)
			continue
		}
		if nodeType.Pattern == "" {
			t.Errorf("%s: `nodeType` carries no @pattern. It is the TRAILING part of a hashed "+
				"composite id, so it must be free of the \":\" separator or ("+
				"\"d:x\",\"y\") and (\"d\",\"x:y\") derive the same row (memql#2980). "+
				"authoring-rules.md section 20 states the rule.", fn.Name)
			continue
		}
		// Assert the BEHAVIOUR of the pattern, not its spelling: any regex that
		// admits a plain node type and refuses a colon-bearing one closes the
		// hazard, and pinning the exact string would make a harmless
		// re-spelling look like a regression.
		re, compileErr := regexp.Compile(nodeType.Pattern)
		if compileErr != nil {
			t.Errorf("%s: `nodeType` @pattern %q does not compile: %v -- an uncompilable pattern "+
				"is a load error, so this would not even reach the check it is meant to be.",
				fn.Name, nodeType.Pattern, compileErr)
			continue
		}
		if !re.MatchString("bff") {
			t.Errorf("%s: `nodeType` @pattern %q rejects \"bff\", a node type the tree actually "+
				"deploys. The constraint must forbid the separator and nothing else.",
				fn.Name, nodeType.Pattern)
		}
		if re.MatchString("x:y") {
			t.Errorf("%s: `nodeType` @pattern %q admits \"x:y\". A colon-bearing trailing part "+
				"makes the composite key ambiguous: (\"d\",\"x:y\") then derives the same id as "+
				"(\"d:x\",\"y\") and the two pairs share one timeline (memql#2980).",
				fn.Name, nodeType.Pattern)
		}

		// --- the LEADING part (memql#2981) ---
		//
		// #2980 constrained only the trailing part, because the leading one
		// legitimately accepts a canonical id and so cannot ban colons
		// outright. It still cannot accept ANY colon-bearing value: shortId()
		// strips exactly one canonical prefix, so bare `B` and canonical
		// `v1:cluster:deployment:B` derive one id only when `B` is already a
		// fixed point. Colon- and whitespace-free is exactly that condition,
		// and it covers both of memql#2981's clauses at once.
		var deploymentID *languageAst.ArgsField
		if fn.ArgsSchema != nil {
			for _, arg := range fn.ArgsSchema.Fields {
				if arg != nil && arg.Name == "deploymentId" {
					deploymentID = arg
				}
			}
		}
		if deploymentID == nil {
			t.Errorf("%s: no `deploymentId` arg -- the leading key part was renamed and this "+
				"gate needs renaming with it (memql#2981).", fn.Name)
			continue
		}
		if deploymentID.Pattern == "" {
			t.Errorf("%s: `deploymentId` carries no @pattern. Without one, `v2:x:y:z` and "+
				"`v1:cluster:deployment:v2:x:y:z` derive DIFFERENT ids for one logical "+
				"deployment -- section 20's own prohibited outcome (memql#2981).", fn.Name)
			continue
		}
		dre, compileErr := regexp.Compile(deploymentID.Pattern)
		if compileErr != nil {
			t.Errorf("%s: `deploymentId` @pattern %q does not compile: %v",
				fn.Name, deploymentID.Pattern, compileErr)
			continue
		}
		// Behaviour, not spelling. Every case carries why it is on the list.
		for _, c := range []struct {
			in   string
			want bool
			why  string
		}{
			{"9f8e7d6c-1234-4abc-9def-000000000001", true,
				"the id.NewShortId uuid -- the only shape any in-tree caller sends"},
			{"v1:cluster:deployment:abc123", true,
				"the canonical form memql#2925 landed to support; rejecting it undoes that"},
			{"abc123", true, "a bare short id"},
			{"v1:cluster:deployment:v2:x:y:z", false,
				"forks against bare `v2:x:y:z` -- memql#2981 clause 2"},
			{"a:v9:b:c:d", false, "memql#2981's third measured fork witness"},
			{"v1:cluster:deployment: abc", false,
				"forks against bare ` abc` -- clause 1, the whitespace class that a " +
					"v<digits>-counting rule does not catch"},
			{"  d-abc123  ", false, "padded: BareShortId trims its input but not its output"},
			{"v1:cluster:deployment:", false,
				"empty short id -- BareShortId returns its input unchanged here"},
		} {
			if got := dre.MatchString(c.in); got != c.want {
				t.Errorf("%s: `deploymentId` @pattern %q accepts(%q) = %v, want %v -- %s",
					fn.Name, deploymentID.Pattern, c.in, got, c.want, c.why)
			}
		}
	}

	for name, seen := range want {
		if !seen {
			t.Errorf("mutation %q not found in %s -- it was renamed or moved, and this gate no "+
				"longer covers the composite id it was written for.", name, path)
		}
	}
}

// TestCompositeHashedIdWitnessIsRealWithoutTheConstraint pins the arithmetic
// the whole rule rests on, so a reader does not have to take the concatenation
// argument on trust.
func TestCompositeHashedIdWitnessIsRealWithoutTheConstraint(t *testing.T) {
	// The unconstrained shape: two distinct pairs, one concatenation.
	a := strings.Join([]string{"d:x", "y"}, ":")
	b := strings.Join([]string{"d", "x:y"}, ":")
	if a != b {
		t.Fatalf("the memql#2980 witness no longer collides at the string level (%q vs %q) -- "+
			"if concat() changed, the reasoning behind the nodeType @pattern needs revisiting.", a, b)
	}
	// And why constraining only the TRAILING part is sufficient: with no colon
	// in the last part, the split at the final colon recovers both operands.
	head, tail, found := lastCut(a, ":")
	if !found || tail != "y" || head != "d:x" {
		t.Errorf("splitting %q at its last %q gave (%q, %q, %v); the injectivity argument for "+
			"memql#2980 depends on that recovering the original pair when the trailing part is "+
			"separator-free.", a, ":", head, tail, found)
	}
}

func lastCut(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
