package dsl

// caller_arg_selection_test.go -- memql#2840.
//
// TestPerRowAuthzClassification keys on user-scope COLUMNS (`ownerUserId`,
// `requestedBy`, ...). That leaves a second way a construct can be user-scoped
// entirely invisible to it: selecting rows by a caller-supplied ARGUMENT
// compared against a row INTRINSIC.
//
//	filter  row.id==args.userId
//	update { id: args.userId }
//
// No user-scope column appears anywhere, so the column detector correctly says
// nothing and the construct lands in the silent `other` bucket. #2799 fixed
// that gate to see mutations at all and to match the post-#2292 field
// spelling; this class survived both.
//
// # Why this gate is narrow, and what it deliberately does not cover
//
// Selecting a row by its own id is the NORMAL shape for a write, so a blanket
// rule is wrong. Measured on the tree: 122 constructs across 43 bound concepts
// select by `id==args.*`, nearly all of them legitimately (update THIS action,
// expire THAT access request).
//
// The question that separates them is not the spelling, it is what the row IS.
// This gate keeps the concepts where a caller-supplied id names a PERSON:
// `user`, because the row IS one, and the credential concepts (`identity`,
// `authSession`, `delegation`, `magicLinkRequest`, `authCode`, `invitation`),
// because each row is one person's ability to authenticate -- selecting one by
// a caller-supplied id revokes their session, burns their pending login, or
// rewrites their key hash.
//
// The remaining ~95 constructs are the systemic question -- whether the data
// resource enforces anything on the request path at all -- and that is #2802 /
// #2803, not this gate. Widening here before that decision would produce a
// hundred-entry exemption map that means nothing.
//
// # What clears the gate
//
// Two signals, tested STRUCTURALLY rather than by substring. The filter clause
// is run through `clauseGuarantees` (#2832), which walks the boolean structure,
// so `(row.id==args.userId || row.id==actor.userId)` does NOT clear -- one arm
// returns rows the caller does not own -- while `requiresOwnerOrAdmin &&
// (a || b)` DOES, because a top-level conjunct guarantees regardless of the
// disjunction beside it. BOTH comment forms -- `//` and `/* */` -- are blanked
// from the clause first, via parser.BlankComments, so a gate term that is
// commented out cannot clear.
//
// The signals `clauseGuarantees` looks for:
//
//   - an AFFIRMATIVE equality against an actor field, in either operand
//     order. Polarity is load-bearing: `row.createdBy != actor.userId`
//     mentions the caller and scopes nothing -- it returns every row the
//     caller did NOT create -- so `!=` must not clear. adminGateLeaf makes
//     the same point in this package; my first version was a bare
//     strings.Contains and cleared exactly that shape;
//   - a CONTEXT-SPEC reference. A spec bound to an `@actor` shape is the
//     canonical spelling of a caller check, and it carries no literal `actor.`
//     at the call site: `filter row.id==args.userId && requiresOwnerOrAdmin`
//     is fully gated, and a substring test sees nothing. The set is computed
//     FROM THE TREE (specs whose signature binds an `@actor`-annotated shape)
//     rather than hardcoded, so a newly authored one is recognised the day it
//     lands. This is not hypothetical -- #2860 fixed `userById` exactly this
//     way while this gate was being written, and the first version of the
//     clearance check reported it as unguarded;
//   - `@serverOnly` (#2800 / #2860) -- the origin capability that makes a
//     construct unreachable from the wire, and the fix vehicle for most of the
//     entries below. A construct leaves the exemption map by being ANNOTATED
//     rather than by someone remembering to edit this file.
//
// `@public` does NOT clear this gate, unlike in TestPerRowAuthzClassification.
// It is a parse-only marker with no runtime semantics (#2860 surveyed the
// annotation family and found `@public` has no field on `Function` at all), so
// it records an intent rather than enforcing one. That is a reasonable
// classification for an open construct in general; it is not a reason to stop
// asking the question about a row that identifies a PERSON.
//
// It catches three `@public` constructs, and they do NOT land in the same
// place, which is the point of asking: `nodeTokenIdentityById` projects
// `identityFull` -- including `credentials` -- and is tracked as debt below,
// while `userDisplayById` (id + displayName) and `userActiveSpace` (id +
// activePartitionId) are recorded as accepted. A rule that cleared all three
// on sight would have said nothing about any of them.
//
// # Known limits, stated rather than implied
//
// The gate detects selection by the `id` INTRINSIC. It does not detect
// selection by another unique column: `userByEmail` takes an arbitrary
// `primaryEmail` and returns `userFull` -- every `@pii` field -- with no
// caller check, and is invisible both here and to
// TestPerRowAuthzClassification. Widening to "any column compared against
// args" is the 122-construct corpus again, so that is filed separately rather
// than guessed at here (memql#2881).
//
// The gate walks the EMBEDDED tree. A product bundle mounted at
// MEMQL_DSL_PATH is never scanned, and cross-domain concept binding means such
// a bundle can declare constructs against these very concepts.
//
// A context-spec clears the gate whatever it asserts, so a role predicate from
// an unrelated domain (`forgeDeveloper`, `requiresDeveloperOrAbove`) counts as
// a caller check here. That follows from treating "bound to an @actor shape"
// as the definition, which is what keeps the set derivable from the tree
// instead of hardcoded -- but it means the gate answers "is the caller
// constrained at all", not "is the caller constrained to THIS person".

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// personScopedConcepts are the bound concepts whose rows identify a PERSON, so
// that selecting one by a caller-supplied id is an authorization question
// rather than an ordinary row update. See the file header for why the gate
// stops here.
var personScopedConcepts = map[string]bool{
	// The row IS a person.
	"user": true,

	// The row is a credential belonging to exactly one person, so selecting
	// one by a caller-supplied id acts on that person's ability to
	// authenticate. Review pointed out that the first version named only
	// `identity` while the stated criterion covers all of these -- and that
	// `revokeAuthSession` is structurally the same primitive as
	// `bumpUserRevocationEpoch`, which was already exempted here as "a
	// denial-of-service primitive against an arbitrary user". An inconsistent
	// criterion is worse than a narrow one: it reads as audited.
	"identity":         true,
	"authSession":      true,
	"delegation":       true,
	"magicLinkRequest": true,
	"authCode":         true,
	"invitation":       true,
}

// callerArgIdSelection matches row SELECTION by the `id` intrinsic against a
// caller-supplied arg, in either spelling. Rule 22 moved filter intrinsics to
// the `row.` namespace, so both are accepted -- matching only the current
// spelling is how the previous detector came to report a meaningless zero
// (see the note on userScopeFieldRe).
var callerArgIdSelection = regexp.MustCompile(`(?m)(?:^[ \t]*id:[ \t]*args\.|(?:\brow\.)?\bid[ \t]*==[ \t]*args\.)[A-Za-z_]`)

// callerArgSelectionExemptions records constructs that select a person-scoped
// row by a caller-supplied id with no caller check, and are known-outstanding
// rather than accepted. Keyed "<file> <constructName>".
//
// An entry here is DEBT, not a blessing. It keeps the gate green for findings
// already tracked elsewhere while still failing on anything NEW -- which is
// the point: this class grew to twenty constructs precisely because nothing
// counted it.
//
// Most entries below want the same fix -- `@serverOnly`, which has LANDED
// (#2860): each has a server-side caller (the identity resolver, the admin
// app, the token lifecycle) and no obvious wire caller. #2860 applied it to
// the three constructs #2800 named and stopped there, so the rest of this list
// is the same work, not new work. Annotating one clears this gate on its own,
// and the stale-entry check below then FAILS until its line here is deleted --
// so a fixed construct cannot leave debt behind.
//
// "Wants @serverOnly" is a judgement about each construct's callers, not a
// verified fact for all of them; a few may instead want a caller-scoping
// context-spec, as `userById` got.
var callerArgSelectionExemptions = map[string]string{
	// --- user: the row IS a person ------------------------------------------
	"identity/mutations.memql updateUser":              "writes an UNRESTRICTED caller-supplied payload to any userId -- `role` is in scope, so this is the sharpest entry here. Admin-app caller only; see #2840 for the reachability analysis.",
	"identity/mutations.memql deleteUserHard":          "hard-deletes any userId. Administrative by nature; needs origin gating, not caller scoping.",
	"identity/mutations.memql scheduleAccountDeletion": "schedules deletion of any userId. Same shape as deleteUserHard.",
	"identity/mutations.memql cancelScheduledDeletion": "cancels deletion for any userId.",
	"identity/mutations.memql bumpUserRevocationEpoch": "sets revocationEpoch to a caller-supplied int on any userId. RAISING it invalidates every live session (denial of service); LOWERING it re-validates previously revoked tokens, because the verifier rejects only claims BELOW the stored value (see the field @description). Both directions matter.",
	"identity/mutations.memql bumpUserDataExport":      "stamps the data-export marker on any userId.",
	"identity/mutations.memql recordLegalAcceptance":   "records legal acceptance on behalf of any userId -- attributing consent to a person who did not give it.",
	"identity/mutations.memql setUserActiveSpace":      "sets any userId's active space.",

	// --- identity: the row is one person's credential ------------------------
	"identity/mutations.memql rotateAuthSession":         "sets refreshTokenHash to a CALLER-SUPPLIED value on an arbitrary session id -- an attacker who can name a session can install a refresh token they control. The sharpest entry in this map after updateUser.",
	"cognition/mutations.memql touchSession":             "unrestricted `payload object!` spread onto an arbitrary authSession row.",
	"identity/mutations.memql revokeAuthSession":         "revokes an arbitrary session by id -- the same denial-of-service primitive as bumpUserRevocationEpoch, one session at a time.",
	"identity/mutations.memql revokeDelegation":          "revokes an arbitrary delegation by id.",
	"identity/mutations.memql consumeAuthCode":           "marks an arbitrary auth code consumed, burning a pending login before its owner can use it.",
	"identity/mutations.memql consumeMagicLinkRequest":   "marks an arbitrary magic-link request consumed -- burning a pending login, and magic-link IS the primary login path.",
	"identity/queries.memql invitationById":              "returns invitationFull for an arbitrary invitation id.",
	"identity/mutations.memql updateIdentity":            "unrestricted `payload object!` spread onto an arbitrary CREDENTIAL row: can rewrite `userId` (re-pointing a credential at another person) and `credentials.keyHash`. Structurally identical to updateUser.",
	"identity/mutations.memql revokePATIdentity":         "revokes any user's personal access token.",
	"identity/mutations.memql revokeWorkerTokenIdentity": "revokes any user's worker token.",
	"identity/mutations.memql revokeBadgeIdentity":       "revokes any user's badge identity.",
	"identity/mutations.memql revokeNodeTokenIdentity":   "revokes any node token identity.",
	"identity/mutations.memql bumpPATLastUsedAt":         "stamps last-used on any PAT; server-side token path.",
	"identity/mutations.memql touchBadgeLastUsed":        "MISNAMED, and sharper than its name: it takes `credentials object!` and spreads it, so it overwrites the ENTIRE credentials block -- keyHash included -- of an arbitrary identity from caller input. Badge-credential substitution, not a timestamp stamp.",
	"identity/mutations.memql stampNodeTokenBootstrap":   "stamps bootstrap state on any node token identity; server-side node path.",
	"identity/queries.memql patIdentityById":             "returns a PAT identity row by id; server-side token verification path.",
	"identity/queries.memql nodeTokenIdentityById":       "returns a node token identity row by id; server-side node auth path. Additionally marked @public while projecting identityFull, which includes `credentials` -- a credential row classified as intentionally open. @public carries no runtime semantics, so the classification is the defect rather than the exposure, but it should be reconsidered rather than inherited.",
}

// callerArgSelectionAccepted records constructs that select a person-scoped row
// by a caller-supplied id and are CORRECT that way. Unlike
// callerArgSelectionExemptions, an entry here is a decision, not debt.
//
// The criterion is that the PROJECTION is the mitigation: the construct returns
// so little about the person that handing it an arbitrary id discloses nothing
// worth gating. Anything returning contact details, credentials, preferences or
// role does not qualify -- that is what the userById / userByIdSystem /
// userDisplayById split exists for.
var callerArgSelectionAccepted = map[string]string{
	"identity/queries.memql userActiveSpace": "projects userActiveSpaceProjection, which is `row.id` + `activePartitionId` and nothing else -- no PII, as its own doc comment states. Carries @public deliberately. Review caught this one filed as debt with a description (\"ungated\") that described the caller check it lacks rather than the projection that makes it safe.",
	"identity/queries.memql userDisplayById": "projects userDisplayCard, which is `row.id` + `displayName` and nothing else. #2860 introduced it precisely so a caller can render another user's name without userById's full row; cross-user display IS the construct's purpose, so caller-scoping it would defeat it.",
}

// stripLineComments removes `//` comment tails, ignoring `//` inside a string
// literal. Both halves are load-bearing and both were learned the hard way:
//
//   - without stripping, a construct with NO gate at all was cleared by
//     `// TODO: gate with requiresOwnerOrAdmin later`;
//   - stripping naively, `avatarUrl=="https://x/y"` truncated the clause at
//     the URL's `//` and threw away the real gate term after it, turning a
//     properly-gated construct into a false flag.
func stripLineComments(s string) string { return blankComments(s) }

// boundConceptOf returns the signature-bound concept from a construct header,
// or "" when the signature omits one (`logic`, and the pre-signature forms).
//
// It reads the matched header text rather than adding a capture group to
// constructHeaderRe, which is shared by three test files -- a new group would
// shift every index they use.
func boundConceptOf(header string) string {
	fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(header), "{"))
	if len(fields) >= 3 {
		return fields[1]
	}
	return ""
}

// actorEqualityRe matches an AFFIRMATIVE equality against an actor field, in
// either operand order. `!=` is excluded deliberately: a negative comparison
// against the caller widens the result set rather than narrowing it to the
// caller's own rows.
var actorEqualityRe = regexp.MustCompile(`(?:\bactor\.[A-Za-z_][A-Za-z0-9_.]*[ \t]*==)|(?:==[ \t]*actor\.[A-Za-z_])`)

var (
	// actorShapeDeclRe matches an `@actor` shape declaration -- the annotation,
	// then the header, allowing other annotations and doc comments between.
	actorShapeDeclRe = regexp.MustCompile(`(?m)^@actor[ \t]*\r?\n(?:[ \t]*(?:@[^\n]*|//[^\n]*)\r?\n)*[ \t]*shape[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	specDeclRe       = regexp.MustCompile(`(?m)^[ \t]*spec[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
)

// actorBoundSpecNames returns every spec whose signature binds an `@actor`
// shape -- the context-specs. They are the canonical way to express a caller
// check and carry no literal `actor.` where they are used, so a gate that
// substring-tests for `actor.` reports them as unguarded.
func actorBoundSpecNames(t *testing.T) map[string]bool {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	actorShapes := map[string]bool{}
	sources := make(map[string]string, len(paths))
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		sources[p] = string(raw)
		for _, m := range actorShapeDeclRe.FindAllStringSubmatch(sources[p], -1) {
			actorShapes[m[1]] = true
		}
	}

	specs := map[string]bool{}
	for _, src := range sources {
		for _, m := range specDeclRe.FindAllStringSubmatch(src, -1) {
			if actorShapes[m[1]] {
				specs[m[2]] = true
			}
		}
	}
	if len(specs) == 0 {
		t.Fatal("found 0 context-specs -- the clearance check would then report every spec-gated construct as unguarded, which is a false FLAG rather than a false clear, but still means this gate is not measuring what it says; check actorShapeDeclRe/specDeclRe against the tree")
	}
	return specs
}

func TestCallerSuppliedRowSelectionOnPersonScopedConcepts(t *testing.T) {
	contextSpecs := actorBoundSpecNames(t)
	specRefs := make(map[string]*regexp.Regexp, len(contextSpecs))
	for name := range contextSpecs {
		specRefs[name] = regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	}

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var flagged []string
	seen := map[string]bool{}
	scanned := 0

	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)

		for _, m := range constructHeaderRe.FindAllStringSubmatchIndex(src, -1) {
			concept := boundConceptOf(src[m[0]:m[1]])
			if !personScopedConcepts[concept] {
				continue
			}
			closeIdx := matchingClose(src, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			name := src[m[4]:m[5]]
			body := src[m[1]:closeIdx]
			scanned++

			// Only the parts that SELECT rows -- the filter clause and an
			// update block's `id:` line. Matching the whole body would flag
			// every insert that stamps an id it was handed, which is a
			// different question (and always legitimate).
			if !callerArgIdSelection.MatchString(rowSelectionSurface(body)) {
				continue
			}

			preambleStart := m[0]
			for k := m[0] - 1; k >= 0; k-- {
				lineStart := strings.LastIndexByte(src[:k], '\n') + 1
				line := strings.TrimSpace(strings.TrimRight(src[lineStart:k+1], "\r\n"))
				if strings.HasPrefix(line, "@") || strings.HasPrefix(line, "//") {
					preambleStart = lineStart
					k = lineStart - 1
					continue
				}
				break
			}
			preamble := src[preambleStart:m[0]]

			// A caller check counts only where it can actually constrain the
			// selection, and only if the predicate's STRUCTURE guarantees it.
			// Both halves are reused from the package rather than re-derived:
			//
			//   - clauseGuarantees (#2832) walks the boolean structure, so
			//     `(row.id==args.userId || row.id==actor.userId)` does not
			//     clear -- one arm returns rows the caller does not own --
			//     while `requiresOwnerOrAdmin && (a || b)` DOES, because a
			//     top-level conjunct guarantees regardless of the disjunction
			//     beside it. My first attempt refused to clear on any `||` at
			//     all, which got the second case wrong and would have pushed
			//     authors into writing bogus exemption entries.
			//   - serverOnlyAnnotationRe is LINE-ANCHORED. Substring-testing
			//     the preamble let `/// TODO: mark this @serverOnly` clear the
			//     gate with no annotation present. The sibling gate already
			//     carries that lesson in a comment; I repeated the mistake
			//     anyway.
			//
			// Comments are stripped from the clause first, so a gate term
			// named only in a comment cannot clear.
			// POLARITY is load-bearing, and a bare strings.Contains does not
			// carry it -- the same point adminGateLeaf makes in this package.
			// `row.createdBy != actor.userId` mentions the actor and scopes
			// NOTHING: it returns every user row the caller did not create.
			// So the leaf demands an affirmative equality against an actor
			// field, on a string-blanked predicate so `note=="actor.userId"`
			// cannot pass either.
			leaf := func(pred string) bool {
				pred = structureOf(pred)
				if actorEqualityRe.MatchString(pred) {
					return true
				}
				for _, ref := range specRefs {
					if ref.MatchString(pred) {
						return true
					}
				}
				return false
			}
			clause := stripLineComments(filterClauseOf(body))
			gated := serverOnlyAnnotationRe.MatchString(preamble) ||
				(strings.TrimSpace(clause) != "" && clauseGuarantees(clause, leaf))
			if gated {
				continue
			}

			key := p + " " + name
			seen[key] = true
			if _, exempt := callerArgSelectionExemptions[key]; exempt {
				continue
			}
			if _, accepted := callerArgSelectionAccepted[key]; accepted {
				continue
			}
			flagged = append(flagged, fmt.Sprintf("%s: %s selects a %s row by a caller-supplied id with no caller check", p, name, concept))
		}
	}

	if scanned == 0 {
		t.Fatal("scanned 0 person-scoped constructs -- the detector is not measuring what its name says (the failure mode that made the previous user-scope detector report a meaningless zero); check constructHeaderRe and personScopedConcepts against the tree")
	}

	sort.Strings(flagged)
	for _, f := range flagged {
		t.Errorf("%s\n\tThe row names a PERSON, so a caller-supplied id means acting on an arbitrary human's record or credentials. Scope it to actor.userId, gate it with @serverOnly (#2860) if its only caller is server-side, or add it to callerArgSelectionExemptions with the issue that tracks it.", f)
	}

	// A stale exemption is worse than a missing one: it reports that a finding
	// is tracked when the construct it names no longer exists, so the next
	// author trusts a line that measures nothing.
	for _, m := range []struct {
		name    string
		entries map[string]string
	}{
		{"callerArgSelectionExemptions", callerArgSelectionExemptions},
		{"callerArgSelectionAccepted", callerArgSelectionAccepted},
	} {
		for key := range m.entries {
			if !seen[key] {
				t.Errorf("%s has a stale entry %q -- the construct no longer matches this detector (renamed, fixed, gated, or deleted). Remove the entry.", m.name, key)
			}
		}
	}
}
