package parser

import "testing"

// TestActorMemberRefsMatchesTheLexersIdentifiers is the regression evidence
// for memql#2809. The member class was ASCII-only (`[A-Za-z_][A-Za-z0-9_]*`)
// while the lexer admits identifiers via unicode.IsLetter / unicode.IsDigit,
// so the pattern stopped at the first non-ASCII byte.
//
// The VERDICT was never wrong -- the actor envelope is a closed set of ASCII
// names (#2623), so a member containing a non-ASCII letter is invalid either
// way and the rule still flags it. What broke is the author's ability to act
// on it: the diagnostic named a token that does not appear in their file and
// underlined the wrong span, which reads as the tool being confused rather
// than as a real finding.
func TestActorMemberRefsMatchesTheLexersIdentifiers(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"filter x == actor.userId\n", "userId"},
		// Truncated at the first non-ASCII byte before the fix: "na" / "r".
		{"filter x == actor.naïveProp\n", "naïveProp"},
		{"filter x == actor.rôle\n", "rôle"},
		{"filter x == actor.日本語\n", "日本語"},
		{"filter x == actor.ident_2\n", "ident_2"},
	}
	for _, tc := range cases {
		refs := ActorMemberRefs(tc.src)
		if len(refs) != 1 {
			t.Errorf("ActorMemberRefs(%q) returned %d refs, want 1", tc.src, len(refs))
			continue
		}
		if refs[0].Name != tc.want {
			t.Errorf("ActorMemberRefs(%q) named %q, want %q -- a diagnostic must name the token the author actually wrote",
				tc.src, refs[0].Name, tc.want)
		}
	}
}

// TestActorGuardRejectsNonASCIIIdentifierContext is the more serious half, and
// it is not a message defect: the leading guard used `\w`, which is ASCII-only
// in Go, so a non-ASCII letter immediately before `actor.` counted as a word
// BOUNDARY. `mïactor.userId` -- one ordinary identifier -- therefore read as
// an actor-envelope reference.
//
// Under the #2621 used-but-undeclared rule that is a LOAD ERROR on valid DSL:
// the file would be rejected for failing to declare `@actor` it never used.
func TestActorGuardRejectsNonASCIIIdentifierContext(t *testing.T) {
	notReads := []string{
		"filter x == myactor.userId\n", // ASCII identifier ending in "actor"
		"filter x == mïactor.userId\n", // the same, with a non-ASCII letter
		"filter x == naïve.actor.id\n", // dotted context: the event envelope
		"filter x == event.actor.id\n", // the shape the guard was written for
		"filter x == a1actor.userId\n", // digit before "actor"
		"filter x == a_actor.userId\n", // underscore before "actor"
	}
	for _, src := range notReads {
		if refs := ActorMemberRefs(src); len(refs) != 0 {
			t.Errorf("ActorMemberRefs(%q) = %+v, want none -- this is an identifier or a different envelope, not an actor read", src, refs)
		}
		if ActorRefInSource(src) {
			t.Errorf("ActorRefInSource(%q) = true, want false; a false positive here load-rejects valid DSL for a missing @actor it never needed", src)
		}
	}

	reads := []string{
		"filter x == actor.userId\n",
		"filter x == (actor.userId)\n",
		"filter x == actor.naïveProp\n",
	}
	for _, src := range reads {
		if !ActorRefInSource(src) {
			t.Errorf("ActorRefInSource(%q) = false, want true; this IS an actor read and must still require @actor", src)
		}
	}
}

// TestActorMemberOffsetPointsAtTheMember keeps the offset contract while the
// class widens: editors underline from it, so a name fix that shifted the
// anchor would move every squiggle.
func TestActorMemberOffsetPointsAtTheMember(t *testing.T) {
	for _, src := range []string{
		"filter x == actor.userId\n",
		"filter x == actor.naïveProp\n",
	} {
		refs := ActorMemberRefs(src)
		if len(refs) != 1 {
			t.Fatalf("ActorMemberRefs(%q) returned %d refs, want 1", src, len(refs))
		}
		got := src[refs[0].Offset : refs[0].Offset+len(refs[0].Name)]
		if got != refs[0].Name {
			t.Errorf("offset %d in %q points at %q, but the ref is named %q", refs[0].Offset, src, got, refs[0].Name)
		}
	}
}

// TestActorMemberFollowsTheLexerOnHyphens covers the character an earlier
// revision of this fix excluded on a FALSE premise: that the lexer joins `-`
// only before an alphanumeric, so including it would swallow subtraction.
//
// The lexer joins `-` unconditionally (isIdentifierCharNoColon; the
// "only when the next rune is alphanumeric" clause in that comment governs
// `:` and `.`). Excluding it left BOTH directions of this bug alive, and
// subtraction was never at risk because it needs surrounding spaces.
func TestActorMemberFollowsTheLexerOnHyphens(t *testing.T) {
	// `actor.userId-1` lexes as ONE identifier. Truncating the member to
	// `userId` named a member that IS in the closed set, so an invalid one
	// passed the validator silently -- a false NEGATIVE on a security check.
	refs := ActorMemberRefs("filter x == actor.userId-1\n")
	if len(refs) != 1 || refs[0].Name != "userId-1" {
		t.Errorf("ActorMemberRefs(actor.userId-1) = %+v, want member %q; the lexer produces one identifier, and naming only %q would let an invalid member pass the closed-set check",
			refs, "userId-1", "userId")
	}

	// Subtraction requires spaces, and the patterns already stop there.
	spaced := ActorMemberRefs("filter x == actor.userId - 1\n")
	if len(spaced) != 1 || spaced[0].Name != "userId" {
		t.Errorf("ActorMemberRefs(actor.userId - 1) = %+v, want member %q; arithmetic must not be absorbed", spaced, "userId")
	}

	// The false-POSITIVE direction: one ordinary hyphenated identifier is not
	// an actor read, and treating it as one load-rejects a valid file.
	if refs := ActorMemberRefs("filter x == my-actor.userId\n"); len(refs) != 0 {
		t.Errorf("ActorMemberRefs(my-actor.userId) = %+v, want none; `my-actor.userId` is a single identifier token", refs)
	}
	if ActorRefInSource("filter x == my-actor.userId\n") {
		t.Error("ActorRefInSource(my-actor.userId) = true; this load-rejects valid DSL for a missing @actor it never needed")
	}
}
