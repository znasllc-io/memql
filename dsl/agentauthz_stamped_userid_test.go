package dsl

import (
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// agentauthz_stamped_userid_test.go -- memql#3081.
//
// `createAgentAuthorization` used to ACCEPT `userId`, and that field is the key
// the standing computer-use grant is READ by: four Go call sites resolve a
// user's envelope ceiling through `agentAuthorizationsForUser(userId:X)`. So a
// caller-supplied value let a row be written as
// `{userId: <someone else>, computerUseScope: "full"}`, raising THAT user's
// standing ceiling on a grant they never approved -- on the concept whose own
// field doc says "Only the granting user can revoke".
//
// Nothing else could catch it, and each reason is load-bearing elsewhere: the
// mutation grammar cannot express an owner predicate (memql#3079), the field
// carries neither @internal nor @serverSet so validateMutationCallerArgs never
// inspects it, and the mutation is on the client surface via the generated SDK
// so @serverOnly was not in play.
//
// Asserted against the PARSED tree (dslimports.Load(Tree())), in the idiom of
// dsl/server_only_parsed_test.go: a stamp the parser does not read as a stamp is
// not a stamp (memql#2875's lesson). The first version of this file grepped the
// source and passed with the stamp line commented out.
//
// SCOPE: this gates createAgentAuthorization only. updateAgentAuthorization
// splats a caller payload with no owner re-stamp, so the same field is still
// caller-writable through it -- see the PR thread and the follow-up issue.

// createAgentAuthorizationDef returns the PARSED mutation. Everything below
// asserts on this rather than on file text: the parser drops comments, so a
// `// userId: actor.userId` cannot satisfy a stamp assertion, and an `@actor`
// inside a comment is not an attribute.
func createAgentAuthorizationDef(t *testing.T) *languageAst.FunctionDef {
	t.Helper()
	tree, err := dslimports.Load(Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	seen := 0
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			fn, ok := def.(*languageAst.FunctionDef)
			if !ok {
				continue
			}
			seen++
			if fn.Name == "createAgentAuthorization" && fn.Type == languageAst.FunctionTypeMutation {
				return fn
			}
		}
	}
	t.Fatalf("createAgentAuthorization not found among %d parsed function definitions", seen)
	return nil
}

// TestAgentAuthorizationUserIdIsStampedNotAccepted is the ratchet.
func TestAgentAuthorizationUserIdIsStampedNotAccepted(t *testing.T) {
	fn := createAgentAuthorizationDef(t)

	var argNames []string
	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			if f == nil {
				continue
			}
			argNames = append(argNames, f.Name)
			if f.Name == "userId" {
				t.Errorf("`userId` is back in createAgentAuthorization's declared args.\n"+
					"That field is the key the standing computer-use grant is READ by -- "+
					"agentAuthorizationsForUser(userId:X) resolves a user's envelope ceiling from it -- "+
					"so accepting it lets a caller raise ANOTHER user's ceiling on a grant they never "+
					"approved. Stamp it from actor.userId instead (memql#3081).\n  args: %v",
					argNames)
			}
		}
	}
	if len(argNames) == 0 {
		t.Fatal("createAgentAuthorization parsed with no args at all -- this gate would pass for the wrong reason")
	}

	stmt, ok := fn.Body.(*languageAst.MutationStmt)
	if !ok {
		t.Fatalf("createAgentAuthorization's body is %T, not a MutationStmt -- this gate cannot read its stamp", fn.Body)
	}
	// PayloadRaw is parser-normalised (whitespace collapsed, comments dropped),
	// so compare against the normalised form.
	payload := strings.ReplaceAll(stmt.PayloadRaw, " ", "")
	if !strings.Contains(payload, "userId:actor.userId") {
		t.Errorf("createAgentAuthorization no longer stamps `userId: actor.userId`.\n"+
			"The granting user is the caller by definition; anything else is a caller-supplied "+
			"attribution on a security-relevant key (memql#3081).\n  parsed payload: %s",
			stmt.PayloadRaw)
	}
}

// The stamp is only real if the parser reads the construct as actor-bound.
func TestCreateAgentAuthorizationIsActorBound(t *testing.T) {
	fn := createAgentAuthorizationDef(t)
	for _, a := range fn.Attributes {
		if a != nil && a.Name == "actor" {
			return
		}
	}
	names := make([]string, 0, len(fn.Attributes))
	for _, a := range fn.Attributes {
		if a != nil {
			names = append(names, a.Name)
		}
	}
	t.Errorf("createAgentAuthorization stamps actor.userId but carries no parsed @actor attribute.\n"+
		"The actor envelope is only in scope for an @actor-bound construct, so the stamp "+
		"would not resolve (memql#3081).\n  attributes: %v", names)
}

// mutationsSource reads agents/mutations.memql and REFUSES an empty result.
//
// The shared readTreeFile returns "" when the open fails rather than failing
// the test. Every assertion in this file is "X is not present", so an empty
// source would make all of them pass vacuously -- the exact shape of a gate
// that reports green while measuring nothing.
func mutationsSource(t *testing.T) string {
	t.Helper()
	src := readTreeFile(t, "agents/mutations.memql")
	if strings.TrimSpace(src) == "" {
		t.Fatal("agents/mutations.memql read as empty; this gate would pass vacuously")
	}
	return src
}

// constructBody returns the text of the construct opened by header, from the
// header to its matching close brace.
func constructBody(t *testing.T, src, header string) string {
	t.Helper()
	i := strings.Index(src, header)
	if i < 0 {
		t.Fatalf("construct %q not found in the tree", header)
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : j+1]
			}
		}
	}
	t.Fatalf("construct %q is unbalanced", header)
	return ""
}

// acceptAndStampOf splits a write block's `accept { ... }` and `stamp { ... }`.
func acceptAndStampOf(t *testing.T, body string) (accept, stamp string) {
	t.Helper()
	accept = subBlock(body, "accept")
	stamp = subBlock(body, "stamp")
	if accept == "" && stamp == "" {
		t.Fatalf("neither an accept nor a stamp block found in:\n%s", body)
	}
	return accept, stamp
}

func subBlock(body, name string) string {
	i := strings.Index(body, name)
	if i < 0 {
		return ""
	}
	open := strings.Index(body[i:], "{")
	if open < 0 {
		return ""
	}
	open += i
	depth := 0
	for j := open; j < len(body); j++ {
		switch body[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[open : j+1]
			}
		}
	}
	return ""
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
