package dsl

import (
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// agentauthz_stamped_userid_test.go -- memql#3081, extended by memql#3177.
//
// `createAgentAuthorization` used to ACCEPT `userId`, and that field is the key
// the standing computer-use grant is READ by: four Go call sites resolve a
// user's envelope ceiling through `agentAuthorizationsForSelf`. So a
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
// # SCOPE, and how memql#3177 widened it
//
// This file used to gate `createAgentAuthorization` ALONE, and said so: "the
// same field is still caller-writable through updateAgentAuthorization -- see
// the follow-up issue." That follow-up is memql#3177 and it landed, so the
// caveat is now a covered case rather than a known hole.
//
// The update path is the harder one to gate, and it is why this file asserts on
// the PARSED body rather than on source text. `update { id: ...;
// userId: actor.userId; args.payload }` is safe and `update { id: ...;
// args.payload }` is an owner-forgery hole, and the two differ by one line
// whose EFFECT depends on the loader's hoist-and-delete pass populating
// PayloadOverlayTemplate (memql#401). The parsed body is where that line either
// exists or does not; a comment containing it satisfies neither this file nor
// the loader.
//
// The complementary gate over the LOADED templates -- which is what actually
// decides whether the overlay wins -- is
// component/memql/rowauthz_owner_gate_test.go, driven by OwnerFieldProvenance.
// Both are kept: this one names the construct and reads in the DSL tree beside
// what it gates; that one measures the runtime outcome.

// createAgentAuthorizationDef returns the PARSED mutation. Everything below
// asserts on this rather than on file text: the parser drops comments, so a
// `// userId: actor.userId` cannot satisfy a stamp assertion, and an `@actor`
// inside a comment is not an attribute.
func createAgentAuthorizationDef(t *testing.T) *languageAst.FunctionDef {
	t.Helper()
	return parsedMutationDef(t, "createAgentAuthorization")
}

// parsedMutationDef returns the PARSED mutation of the given name, and FAILS
// when it is absent. "Absent" has to be a failure rather than a skip: every
// assertion below is of the form "this construct does X", so a construct the
// loader dropped would make all of them pass by never running.
func parsedMutationDef(t *testing.T, name string) *languageAst.FunctionDef {
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
			if fn.Name == name && fn.Type == languageAst.FunctionTypeMutation {
				return fn
			}
		}
	}
	t.Fatalf("%s not found among %d parsed function definitions", name, seen)
	return nil
}

// parsedMutationPayload returns a mutation's parsed write-block text with
// whitespace collapsed, which is the form PayloadRaw is normalised into.
func parsedMutationPayload(t *testing.T, fn *languageAst.FunctionDef) string {
	t.Helper()
	stmt, ok := fn.Body.(*languageAst.MutationStmt)
	if !ok {
		t.Fatalf("%s's body is %T, not a MutationStmt -- this gate cannot read its write block", fn.Name, fn.Body)
	}
	if strings.TrimSpace(stmt.PayloadRaw) == "" {
		t.Fatalf("%s parsed with an EMPTY write block; every assertion here would pass vacuously", fn.Name)
	}
	return stmt.PayloadRaw
}

// mutationIsActorBound reports whether the parser read an `@actor` attribute.
// The stamp is only real if it did: the actor envelope is not in scope for a
// construct the parser does not read as actor-bound, so `userId: actor.userId`
// would not resolve.
func mutationIsActorBound(fn *languageAst.FunctionDef) bool {
	for _, a := range fn.Attributes {
		if a != nil && a.Name == "actor" {
			return true
		}
	}
	return false
}

func attributeNames(fn *languageAst.FunctionDef) []string {
	names := make([]string, 0, len(fn.Attributes))
	for _, a := range fn.Attributes {
		if a != nil {
			names = append(names, a.Name)
		}
	}
	return names
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

// ---------------------------------------------------------------------------
// memql#3177 -- the UPDATE path
// ---------------------------------------------------------------------------

// TestUpdateAgentAuthorizationReStampsUserIdOverItsSplat is the gate the
// memql#3081 header used to name as missing.
//
// `updateAgentAuthorization` splats `args.payload` into the write block. A bare
// splat lands VERBATIM -- memql#401's overlay-wins protection is populated only
// from EXPLICIT block fields -- so before memql#3177 a caller could send
// `{userId: "<victim>"}` and take a grant they had legitimately created, keeping
// its `computerUseScope: "full"` through the read-merge, and hand the victim a
// standing ceiling they never approved. Two calls, no privileged access, on the
// concept that declares `@rowAuthz(owner="userId")`.
//
// The remedy is updateNote's idiom: an explicit `userId: actor.userId` ALONGSIDE
// the splat, which the loader hoists into PayloadOverlayTemplate and applies
// AFTER it. So this gate demands BOTH halves in the parsed body -- the splat
// alone is the hole, and the stamp alone would silently drop every caller field
// the SPA's "Approve & always allow this tier" action sends.
func TestUpdateAgentAuthorizationReStampsUserIdOverItsSplat(t *testing.T) {
	fn := parsedMutationDef(t, "updateAgentAuthorization")
	raw := parsedMutationPayload(t, fn)
	payload := strings.ReplaceAll(raw, " ", "")

	if !strings.Contains(payload, "userId:actor.userId") {
		t.Errorf("updateAgentAuthorization does not re-stamp `userId: actor.userId` over its "+
			"payload splat.\nThat field is this concept's declared @rowAuthz owner, so an "+
			"un-restamped splat lets a caller REASSIGN a grant they own to somebody else and "+
			"raise that user's standing computer-use ceiling (memql#3138, closed by "+
			"memql#3177).\n  parsed write block: %s", raw)
	}
	if !strings.Contains(payload, "args.payload") {
		t.Errorf("updateAgentAuthorization no longer splats `args.payload`.\nIf that is "+
			"deliberate, this gate needs rewriting rather than deleting -- the SPA's 'Approve "+
			"& always allow this tier' action writes the whole skillTierAllowlist array back "+
			"through it (pack#38, memql#169).\n  parsed write block: %s", raw)
	}
}

// The stamp above is only real if the parser reads the construct as actor-bound:
// `actor.userId` does not resolve in a construct with no @actor attribute, so
// without this the gate above would pass on a mutation whose stamp evaluates to
// nothing.
func TestUpdateAgentAuthorizationIsActorBound(t *testing.T) {
	fn := parsedMutationDef(t, "updateAgentAuthorization")
	if !mutationIsActorBound(fn) {
		t.Errorf("updateAgentAuthorization stamps actor.userId but carries no parsed @actor "+
			"attribute, so the stamp would not resolve (memql#3177).\n  attributes: %v",
			attributeNames(fn))
	}
}

// The other half of "a caller cannot set userId": it must not come back as a
// declared ARG either. A stamp plus an accepted arg is the shape that reads as
// safe and is not -- the loader would hoist the arg into the same write.
func TestUpdateAgentAuthorizationDoesNotAcceptUserId(t *testing.T) {
	fn := parsedMutationDef(t, "updateAgentAuthorization")
	var argNames []string
	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			if f == nil {
				continue
			}
			argNames = append(argNames, f.Name)
			if f.Name == "userId" {
				t.Errorf("`userId` is a declared arg of updateAgentAuthorization. It is the "+
					"concept's @rowAuthz owner field and must be stamped from actor.userId "+
					"alone (memql#3177).\n  args: %v", argNames)
			}
		}
	}
	if len(argNames) == 0 {
		t.Fatal("updateAgentAuthorization parsed with no args at all -- this gate would pass " +
			"for the wrong reason")
	}
}

// `revokeAgentAuthorization` is the OTHER caller-supplied-target mutation on this
// concept, and it deliberately carries no owner stamp: its write block is
// `{id, active:false}`, so `userId` is not among the keys it can set and the
// read-merge inherits the stored value. What stops a stranger revoking a grant
// is the engine (the declared tier + memql#3174's write guard), not this body.
//
// The gate is therefore the inverse shape: the body must NOT grow a payload
// splat. A splat here would make `userId` caller-writable again through a second
// door, and the failure would be silent -- OwnerFieldProvenance would catch it in
// component/memql, but this file is where a reader of the DSL looks.
func TestRevokeAgentAuthorizationWritesNoCallerPayload(t *testing.T) {
	fn := parsedMutationDef(t, "revokeAgentAuthorization")
	raw := parsedMutationPayload(t, fn)
	payload := strings.ReplaceAll(raw, " ", "")

	if strings.Contains(payload, "args.payload") {
		t.Errorf("revokeAgentAuthorization now splats a caller payload.\nThat reopens "+
			"memql#3138 through a second mutation: the splat lands verbatim, so `userId` -- "+
			"the concept's declared @rowAuthz owner -- becomes caller-writable. Re-stamp it "+
			"from actor.userId as updateAgentAuthorization does, or keep the write block "+
			"field-by-field.\n  parsed write block: %s", raw)
	}
	if !strings.Contains(payload, "active:false") {
		t.Errorf("revokeAgentAuthorization no longer sets `active: false`; it is a soft-revoke "+
			"and this gate is measuring the wrong construct.\n  parsed write block: %s", raw)
	}
}

// `updateAgentAuthScope` (dsl/worker/mutations.memql) is the third caller-supplied
// -target mutation on this concept and the second half of memql#3138's
// escalation: it sets computerUseScope by row id with no owner predicate, so once
// a userId rewrite existed it was the step that turned a hijacked row into `full`.
//
// Same inverse shape as revoke, and it is checked here rather than in a worker-
// namespace file because the invariant belongs to the CONCEPT: every mutation
// that writes v1:agents:agentAuthorization either stamps the owner or writes no
// caller payload at all.
func TestUpdateAgentAuthScopeWritesNoCallerPayload(t *testing.T) {
	fn := parsedMutationDef(t, "updateAgentAuthScope")
	raw := parsedMutationPayload(t, fn)
	payload := strings.ReplaceAll(raw, " ", "")

	if strings.Contains(payload, "args.payload") {
		t.Errorf("updateAgentAuthScope now splats a caller payload, which makes `userId` -- "+
			"the concept's declared @rowAuthz owner -- caller-writable through it "+
			"(memql#3177).\n  parsed write block: %s", raw)
	}
	if !strings.Contains(payload, "args.computerUseScope") {
		t.Errorf("updateAgentAuthScope no longer writes computerUseScope from a named arg; "+
			"this gate is measuring the wrong construct.\n  parsed write block: %s", raw)
	}
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
