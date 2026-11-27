package auth

import (
	"regexp"
	"strings"
	"testing"
)

// THE ASSIGNMENT RULE, PINNED ACROSS THE TWO LANGUAGES THAT IMPLEMENT IT
// (memql#5236).
//
// WHY THIS GATE EXISTS. `clients/os/src/apps/users/assign.ts` is a
// REIMPLEMENTATION of MayAssignRole, not a caller of it: it shares no symbol
// with this package, so every search for callers of `MayAssignRole` -- which is
// how the change that broke it was scoped -- comes back with two Go call sites
// and no sign of it. The engine learned that a developer may invite an admin
// and the browser kept refusing, and because pickers.tsx feeds the answer to
// `disabled`, the fix shipped live and unreachable with no error anywhere.
//
// WHAT IT ASSERTS, AND WHY IT IS SHAPED THIS WAY. Not "the two answer the same
// for every input" -- that would need a JS runtime here, and the OS's own
// vitest suite is the right place for behaviour (test/users/assign.test.ts
// carries the cross product and the re-role control). What this gate holds is
// the STRUCTURE that the mirror can only get right on purpose:
//
//  1. both sides carry an explicit KIND rather than inferring the seam from an
//     empty target slug -- the inference both sides used, and the reason the
//     bug was expressible at all;
//  2. the people-authority clause is gated on the re-role kind on BOTH sides;
//  3. the refusal vocabularies correspond one-for-one, so a refusal this
//     package can return is one the browser has a word and a sentence for.
//
// A MISSING OR RENAMED FILE IS A FAILURE, NOT A SKIP, for the reason
// TestFleetOnlineWindowMatchesTheClients states: a gate that goes vacuous when
// a file moves goes vacuous exactly when the rule is most likely to have
// drifted.
const osAssignPath = "../../clients/os/src/apps/users/assign.ts"

func TestAssignClientMirrorGatesTheAuthorityClauseOnTheKind(t *testing.T) {
	client := readClientFile(t, osAssignPath)

	// (1) The client declares the kind, and its two values name the two seams.
	if !strings.Contains(client, `export type AssignKind = "invitation" | "reRole"`) {
		t.Errorf("%s declares no AssignKind union.\n"+
			"auth.AssignKind exists because `targetCurrentSlug == \"\"` means THE TARGET HAS NO RUNG, "+
			"which is a different fact from \"this is an invitation\" -- and on the client they come "+
			"apart too, because PersonPage passes `person?.role ?? \"\"`. The mirror must be told which "+
			"seam is asking, not guess.", osAssignPath)
	}

	// (2) The authority clause is reached only on a re-role. This is the one
	// line whose absence was the bug; matched loosely on purpose, so a
	// reformat does not fail the build but a REMOVED condition does.
	//
	// COMMENTS STRIPPED FIRST, and that is not tidiness. This file's own
	// comments discuss `kind === "reRole"` at length, so an unstripped match
	// would be satisfied by the prose that explains the rule while the rule
	// itself was reverted -- a gate that passes on the broken tree, which is
	// worse than no gate. stripComments is the same helper
	// TestAppManifestMirrorsTheEngineFloor uses on the app registry.
	authorityGated := regexp.MustCompile(`kind\s*===\s*"reRole"`)
	if !authorityGated.MatchString(stripComments(client)) {
		t.Errorf("%s never tests `kind === \"reRole\"`.\n"+
			"The people-authority clause (rule 5) must run on the RE-ROLE seam only. Applied to an "+
			"invitation it disables the very rung MayAssignRole now accepts, and because pickers.tsx "+
			"passes this answer to `disabled` the operator gets no control and no sentence.", osAssignPath)
	}

	// The Go side of the same claim, so this gate fails if EITHER half is
	// reverted rather than only the browser.
	if !strings.Contains(assignRuleSource(t), "kind == AssignOnReRole && GrantsPrincipalAuthorityBeyond") {
		t.Errorf("rbac_assignment.go no longer gates GrantsPrincipalAuthorityBeyond on AssignOnReRole.\n" +
			"If that is deliberate, the client mirror and test/users/assign.test.ts move with it -- " +
			"this gate exists so the two cannot move separately.")
	}
}

func TestAssignRefusalVocabularyMatchesTheClient(t *testing.T) {
	client := readClientFile(t, osAssignPath)

	// Every AssignRefusal this package can return, paired with the client's
	// word for it. The empty value is "allowed" on both sides and needs no row.
	for _, pair := range []struct {
		refusal AssignRefusal
		clientW string
	}{
		// AssignUnknownRole covers a typo, a slug that never existed, and a
		// DEACTIVATED role. Only the last can arise in the browser -- the
		// ladder is built by iterating the catalog, so an unknown slug is
		// unreachable there -- and `inactive` is the client's word for it.
		{AssignUnknownRole, "inactive"},
		{AssignNotAUserManager, "notAUserManager"},
		{AssignTargetOutranks, "targetOutranks"},
		{AssignAboveCaller, "aboveCaller"},
		{AssignOwnershipTransfer, "ownershipTransfer"},
		{AssignAuthorityBeyond, "authorityBeyond"},
		{AssignNotAMember, "notAMember"},
	} {
		if !strings.Contains(client, `"`+pair.clientW+`"`) {
			t.Errorf("%s has no %q member, so %s (%q) reaches a ladder with no word for it.\n"+
				"An unmapped refusal renders as a rung that is simply missing, which teaches nobody why.",
				osAssignPath, pair.clientW, pair.refusal, string(pair.refusal))
		}
	}

	// And the reverse direction: a client word for a refusal this package
	// cannot produce is a branch nothing reaches, which reads as coverage.
	//
	// SCOPED TO THE RungRefusal UNION, not the whole file. An unscoped scan
	// also swept up AssignKind's own members ("invitation" | "reRole") and
	// reported them as invented refusals -- a gate that fails on the correct
	// tree teaches people to delete the gate.
	union := regexp.MustCompile(`(?s)export type RungRefusal =(.*?);`).FindStringSubmatch(client)
	if union == nil {
		t.Fatalf("%s declares no RungRefusal union -- this gate cannot check a vocabulary it "+
			"cannot find, and passing quietly would make it vacuous", osAssignPath)
	}
	member := regexp.MustCompile(`"([a-zA-Z]+)"`)
	known := map[string]bool{
		"inactive": true, "notAUserManager": true, "targetOutranks": true,
		"ownershipTransfer": true, "aboveCaller": true, "authorityBeyond": true, "notAMember": true,
	}
	for _, m := range member.FindAllStringSubmatch(union[1], -1) {
		if !known[m[1]] {
			t.Errorf("%s declares refusal %q, which no AssignRefusal in this package produces.\n"+
				"Either the Go side gained a refusal and this table was not updated, or the client "+
				"invented one -- and a branch the server cannot trigger is dead code that reads as cover.",
				osAssignPath, m[1])
		}
	}
}

// assignRuleSource reads this package's own rule file, so the gate can hold
// both halves of the claim rather than only the browser's.
func assignRuleSource(t *testing.T) string {
	t.Helper()
	return readClientFile(t, "rbac_assignment.go")
}
