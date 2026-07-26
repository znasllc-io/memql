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
// rule is wrong. Measured on the tree: 120 constructs across 42 bound concepts
// select by `id==args.*`, nearly all of them legitimately (update THIS action,
// expire THAT access request).
//
// The question that separates them is not the spelling, it is what the row IS.
// This gate keeps the concepts where a caller-supplied id names a PERSON:
//
//	user      -- the row IS a person. Selecting by a caller-supplied id means
//	             reading or writing an arbitrary human's record.
//	identity  -- the row is a credential owned by exactly one person. Selecting
//	             by a caller-supplied id means acting on an arbitrary human's
//	             credentials (revoke their PAT, their worker token, ...).
//
// The other 100 constructs are the systemic question -- whether the data
// resource enforces anything on the request path at all -- and that is #2802 /
// #2803, not this gate. Widening here before that decision would produce a
// hundred-entry exemption map that means nothing.
//
// # What clears the gate
//
// Three things, and the middle one is the one worth explaining:
//
//   - an `actor.` term in the body -- the construct compares against its
//     caller directly;
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
// asking the question about a row that identifies a PERSON. The one construct
// this catches -- `nodeTokenIdentityById`, `@public` and projecting
// `identityFull`, which includes `credentials` -- is exactly the case worth
// not silencing.

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
	"user":     true,
	"identity": true,
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
// Every entry below is fixed the same way, by #2860's `@serverOnly`: each has
// a server-side caller (the identity resolver, the admin app, the token
// lifecycle) and no legitimate wire caller. When that annotation lands and is
// applied, the construct clears this gate on its own and its line here should
// be deleted.
var callerArgSelectionExemptions = map[string]string{
	// --- user: the row IS a person ------------------------------------------
	"identity/mutations.memql updateUser":              "writes an UNRESTRICTED caller-supplied payload to any userId -- `role` is in scope, so this is the sharpest entry here. Admin-app caller only; see #2840 for the reachability analysis.",
	"identity/mutations.memql deleteUserHard":          "hard-deletes any userId. Administrative by nature; needs origin gating, not caller scoping.",
	"identity/mutations.memql scheduleAccountDeletion": "schedules deletion of any userId. Same shape as deleteUserHard.",
	"identity/mutations.memql cancelScheduledDeletion": "cancels deletion for any userId.",
	"identity/mutations.memql bumpUserRevocationEpoch": "invalidates every session for any userId -- a denial-of-service primitive against an arbitrary user.",
	"identity/mutations.memql bumpUserDataExport":      "stamps the data-export marker on any userId.",
	"identity/mutations.memql recordLegalAcceptance":   "records legal acceptance on behalf of any userId -- attributing consent to a person who did not give it.",
	"identity/mutations.memql setUserActiveSpace":      "sets any userId's active space.",
	"identity/queries.memql userActiveSpace":           "returns any userId's active space. #2860 landed @serverOnly and fixed its sibling userById (now gated by the requiresOwnerOrAdmin context-spec) but left this one ungated.",

	// --- identity: the row is one person's credential ------------------------
	"identity/mutations.memql updateIdentity":            "updates any identity row by id.",
	"identity/mutations.memql revokePATIdentity":         "revokes any user's personal access token.",
	"identity/mutations.memql revokeWorkerTokenIdentity": "revokes any user's worker token.",
	"identity/mutations.memql revokeBadgeIdentity":       "revokes any user's badge identity.",
	"identity/mutations.memql revokeNodeTokenIdentity":   "revokes any node token identity.",
	"identity/mutations.memql bumpPATLastUsedAt":         "stamps last-used on any PAT; server-side token path.",
	"identity/mutations.memql touchBadgeLastUsed":        "stamps last-used on any badge identity; server-side token path.",
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
	"identity/queries.memql userDisplayById": "projects userDisplayCard, which is `row.id` + `displayName` and nothing else. #2860 introduced it precisely so a caller can render another user's name without userById's full row; cross-user display IS the construct's purpose, so caller-scoping it would defeat it.",
}

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

			// Anything that constrains the caller clears the gate.
			gated := strings.Contains(body, "actor.") || strings.Contains(preamble, "@serverOnly")
			for _, ref := range specRefs {
				if gated {
					break
				}
				gated = ref.MatchString(body)
			}
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
