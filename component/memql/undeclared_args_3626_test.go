package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// undeclared_args_3626_test.go -- memql#3626.
//
// declared_usage_validator.go refused "args field declared but never
// referenced" and nothing else. The opposite direction -- a body READING an
// `args.X` that the args block never declared -- loaded clean, and covered two
// different failures with the same silence:
//
//   - the author typo (`args.userld`), whose field is simply ABSENT from the
//     written payload;
//   - the caller-supplied undeclared name, which is BOUND AND WRITTEN while
//     bypassing @required / type / @enum / @pattern, because
//     validateFunctionArgs iterates DECLARED fields and rejectUnknownArgs is
//     gated behind the MCP boundary.

func undeclaredArgsRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:identity:user": {Name: "v1:identity:user"},
	})
}

// The issue's fixture, verbatim in shape: one declared arg, one that is not.
func TestUndeclaredArgsReferenceIsRefusedAtLoad(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeUndecl {
  args {
    userId string!
  }
  insert {
    id: args.userId
    name: args.neverDeclared
    active: true
  }
}`
	_, err := tryParseNewFunctionSyntax("zzargProbeUndecl", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry())
	if err == nil {
		t.Fatal("the mutation loaded clean. Called with {userId}, `name` is silently absent; " +
			"called with an extra `neverDeclared`, the value is BOUND AND WRITTEN having " +
			"passed no declared-schema check at all (memql#3626).")
	}
	if !strings.Contains(err.Error(), "neverDeclared") {
		t.Errorf("the error must name the undeclared reference; got: %v", err)
	}
	if !strings.Contains(err.Error(), "userId") {
		t.Errorf("the error should list what IS declared, so a typo is visible side by side; got: %v", err)
	}
}

// The author-typo route to the same outcome, which is what makes this worth
// refusing rather than merely warning.
func TestTypodArgsReferenceIsRefusedAtLoad(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeTypo {
  args {
    userId string!
  }
  insert {
    id: args.userId
    name: args.userld
    active: true
  }
}`
	_, err := tryParseNewFunctionSyntax("zzargProbeTypo", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry())
	if err == nil {
		t.Fatal("`args.userld` (lowercase L for I) loaded clean and writes nothing -- the " +
			"memql#3605 outcome reached by a different route")
	}
	if !strings.Contains(err.Error(), "userld") {
		t.Errorf("got: %v", err)
	}
}

// A construct with NO args block at all reading `args.X` is the same defect
// with a different starting point, and gets its own message because the fix is
// different (add a block, not a field).
func TestArgsReferenceWithNoArgsBlockIsRefused(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeNoBlock {
  insert {
    id: args.userId
    active: true
  }
}`
	_, err := tryParseNewFunctionSyntax("zzargProbeNoBlock", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry())
	if err == nil {
		t.Fatal("a body reading args.userId with no args block loaded clean")
	}
	if !strings.Contains(err.Error(), "declares no args block") {
		t.Errorf("the error must say the block is missing entirely; got: %v", err)
	}
}

// The direction that keeps the change honest: a fully-declared construct still
// loads, and the reverse rule (declared-but-unused) still fires, so the two
// checks together describe a contract rather than one of them swallowing the
// other.
func TestFullyDeclaredArgsStillLoad(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeOk {
  args {
    userId string!
    name   string
  }
  insert {
    id: args.userId
    name: args.name
    active: true
  }
}`
	if _, err := tryParseNewFunctionSyntax("zzargProbeOk", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry()); err != nil {
		t.Fatalf("a fully-declared mutation must still load: %v", err)
	}
}

func TestDeclaredButUnusedArgIsStillRefused(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeUnused {
  args {
    userId string!
    unused string
  }
  insert {
    id: args.userId
    active: true
  }
}`
	_, err := tryParseNewFunctionSyntax("zzargProbeUnused", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry())
	if err == nil {
		t.Fatal("the pre-existing declared-but-unused rule must still fire -- the new direction " +
			"is additive, not a replacement")
	}
	if !strings.Contains(err.Error(), "declared but never referenced") {
		t.Errorf("got: %v", err)
	}
}

// Prose is not a reference. Comments and string literals are blanked before
// the scan, so a doc comment discussing an argument cannot fail a load -- the
// cheapest way for a used-requires-declared rule to become a nuisance.
func TestCommentAndStringMentionsAreNotReferences(t *testing.T) {
	src := `use identity.concepts.{ user }

@description("probe")
mutate user zzargProbeProse {
  args {
    userId string!
  }
  insert {
    // callers used to pass args.legacyName here; it is gone
    id: args.userId
    name: "see args.somethingElse in the runbook"
    active: true
  }
}`
	if _, err := tryParseNewFunctionSyntax("zzargProbeProse", "mutation", src, "identity.mutations.memql", undeclaredArgsRegistry()); err != nil {
		t.Fatalf("an args.X mentioned only in a comment or a string literal is not a reference: %v", err)
	}
}

// A Logic reading an undeclared `event` keeps its OWN diagnosis
// (validateLogicEventBinding, memql#1706), which names the runner-threading
// reason and the exact line to add. Pinned so the generic rule cannot quietly
// take that case over and make the message worse.
func TestLogicUndeclaredEventKeepsItsOwnDiagnosis(t *testing.T) {
	src := `@description("reads the event but never declares it")
logic zzargProbeLogicEvent {
  body {
    return ensureDailySpaceForUser( userId: args.event.payload.id )
  }
}`
	_, err := tryParseNewFunctionSyntax("zzargProbeLogicEvent", "logic", src, "test.memql", undeclaredArgsRegistry())
	if err == nil {
		t.Fatal("a logic reading an undeclared event must still be refused")
	}
	if !strings.Contains(err.Error(), "1706") {
		t.Errorf("the memql#1706 message is the better one for this case and must survive; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The corpus sweep -- the evidence that this stays clean
// ---------------------------------------------------------------------------

// TestCorpusHasNoUndeclaredArgsReferences walks every function-shaped slice in
// the tree the way LoadUnifiedFunctions does and asserts none reads an
// undeclared `args.X`. This is the issue's "engine corpus is clean (0
// constructs)" claim turned into a standing guard, so the hole cannot be
// re-opened by a construct that merely happens not to be exercised by a test.
func TestCorpusHasNoUndeclaredArgsReferences(t *testing.T) {
	var offenders []string
	seen := 0

	for _, raw := range baseloader.ReadAll(nil) {
		for _, slice := range ExtractFunctionSlices(raw.Content) {
			src := slice.Source
			if rewritten, rerr := languageParser.NormaliseAll(src); rerr == nil {
				src = rewritten
			}
			lexer := languageParser.NewLexer(src)
			tokens, lerr := lexer.Tokenize()
			if lerr != nil {
				continue
			}
			p := languageParser.NewParser(tokens)
			p.SetDocComments(lexer.DocComments())
			p.SetSource(src)
			parsed, perr := p.Parse()
			if perr != nil {
				continue
			}
			file, ok := parsed.(*languageParser.File)
			if !ok {
				continue
			}
			for _, def := range file.Definitions {
				fd, ok := def.(*languageParser.FunctionDef)
				if !ok {
					continue
				}
				seen++
				if err := validateArgsReferencesAreDeclared(extractFunctionBody(src), fd); err != nil {
					offenders = append(offenders, raw.Path+": "+err.Error())
				}
			}
		}
	}

	if seen == 0 {
		t.Fatal("no function-shaped constructs walked -- the sweep would assert nothing")
	}
	if len(offenders) > 0 {
		t.Fatalf("%d construct(s) read an undeclared args.X:\n  %s", len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("swept %d function-shaped constructs; none reads an undeclared args.X", seen)
}
