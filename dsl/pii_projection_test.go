package dsl

// pii_projection_test.go -- memql#2883.
//
// Every authorization gate in this package asks how a row is SELECTED.
// TestPerRowAuthzClassification keys on user-scope COLUMNS; the #2840 gate keys
// on the `id` INTRINSIC compared against `args.*`. None of them asks what a
// construct PROJECTS, and that turns out to be the sharper question.
//
//	query user searchUsers {
//	  args   { active boolean }
//	  filter when(args.active) { active==args.active }
//	  shape  userFull
//	}
//
// The only predicate is a `when()` guard, and per the authoring rules a
// `when()` whose arg is absent is DROPPED as if never written -- so calling
// this with no arguments applies no filter at all and returns every user in
// the cluster in `userFull`: every `@pii` field plus the cluster-wide auth
// `role`. It was also on the agent tool surface (`dsl/memql/tools.memql`), so
// a prompt-injected or merely over-eager agent could pull the whole user
// table. Three siblings (`activeUsers`, `usersInDeletionCooldown`,
// `usersScheduledForDeletion`) bound the same shape ungated.
//
// Every existing gate missed all four, and each missed for a DIFFERENT reason
// -- which is why widening one of them was not the fix:
//
//   - the column detector: none of the four names a user-scope column;
//   - the #2840 id-intrinsic detector: none of the four selects by id;
//   - #2860's @serverOnly sweep: scoped to the by-id readers named in #2800;
//   - #2881: scoped to the by-email reader.
//
// So this gate asks the other question, on the other axis: a query projecting
// a shape that carries `@pii` fields must constrain its CALLER, whatever its
// filter says. `searchUsers` proves the filter can be nothing at all.
//
// # Why @pii is the right key
//
// It is not a decorative marker. `@scrubPii` (memql#1711) already drives the
// deletion scrub off it -- the engine enumerates every `@pii` field on the
// bound concept and zeroes it -- so the tree already treats the annotation as
// load-bearing, and adding a field to the set is already understood to have
// runtime consequences. Deriving this gate from the same annotation means a
// newly-marked field widens the gate the day it lands, with no second list to
// keep in sync.
//
// Today `@pii` appears only on `concept user` (8 fields). That is a fact about
// the tree, not a limit of the gate: mark a field on another concept and every
// query projecting it is covered immediately. The gate FAILS LOUDLY if it ever
// scans zero PII-projecting queries, so the set silently emptying -- the
// failure mode that made the previous user-scope detector report a meaningless
// zero (#2799) -- cannot pass as green.
//
// # What clears the gate
//
// The same three signals the #2840 gate accepts, evaluated with the same
// shared helpers so the two cannot drift:
//
//   - an AFFIRMATIVE equality against an actor field, via `clauseGuarantees`
//     (#2832), which walks the filter's boolean STRUCTURE. A gate term inside
//     one arm of a `||` does not clear; a top-level conjunct does.
//   - a CONTEXT-SPEC reference (a spec whose signature binds an `@actor`
//     shape). The set is computed FROM THE TREE, so a newly authored one is
//     recognised immediately.
//   - `@serverOnly` (#2800 / #2860), the origin capability that makes a
//     construct unreachable from the wire.
//
// A `when(...)` guard does NOT clear it, and that is the specific lesson of
// this issue rather than an incidental detail. `clauseGuarantees` sees the
// guard's body as one arm of the predicate, and an arm that vanishes when its
// arg is absent guarantees nothing -- which is exactly how `searchUsers` came
// to return the whole table.
//
// `@public` does not clear it either, for the reason the sibling gate gives:
// it is a parse-only marker with no runtime semantics, so it records an intent
// rather than enforcing one.

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

var (
	// conceptDeclRe matches a concept header: `concept <name> {`.
	conceptDeclRe = regexp.MustCompile(`(?m)^[ \t]*concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// piiFieldRe matches a concept field line carrying @pii. The field name is
	// the first token; `@pii` may sit anywhere in the annotation run after the
	// type, so the type is matched loosely rather than enumerated.
	piiFieldRe = regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]+[^\n]*@pii\b`)

	// piiShapeDeclRe matches a concept-bound shape header, `shape <Concept>
	// <name> {`. The single-identifier form is @actor-only and projects no row
	// fields, so it is deliberately not matched.
	piiShapeDeclRe = regexp.MustCompile(`(?m)^[ \t]*shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// piiQueryDeclRe matches a concept-bound query header.
	piiQueryDeclRe = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// piiShapeClauseRe matches a query body's `shape <name>` clause.
	piiShapeClauseRe = regexp.MustCompile(`(?m)^[ \t]*shape[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

	// piiIncludeRe matches a shape body's `include <shapeName>` statement.
	piiIncludeRe = regexp.MustCompile(`(?m)^[ \t]*include[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

	// piiProjectionRe matches a shape body's projected path. Only the terminal
	// segment matters -- that is the key the engine emits and the name a
	// concept field carries.
	piiProjectionRe = regexp.MustCompile(`(?m)^[ \t]*((?:row\.|actor\.)?[A-Za-z_][A-Za-z0-9_.]*)[ \t]*$`)
)

// piiProjectionExemptions records queries that project PII with no caller
// check and are known-outstanding rather than accepted. Keyed
// "<file> <constructName>", exactly as the #2840 gate keys its map.
//
// An entry here is DEBT. The stale-entry check below fails once the construct
// stops matching, so a fixed construct cannot leave a line behind.
var piiProjectionExemptions = map[string]string{}

// piiProjectionAccepted records queries that project a PII-marked field and are
// CORRECT without a caller check. Unlike piiProjectionExemptions, an entry here
// is a decision, not debt.
//
// The criterion is the one callerArgSelectionAccepted uses: the PROJECTION is
// the mitigation. Disclosing the marked field must BE the construct's purpose,
// and the projection must be the minimum that serves it. A construct returning
// contact details, credentials, preferences or role does not qualify however
// narrow its filter.
var piiProjectionAccepted = map[string]string{
	// This gate flags userDisplayById where the #2840 gate also landed on it,
	// and both times the answer is the projection. It is worth stating why
	// rather than narrowing the detector, because the tension is real:
	// `displayName` IS marked @pii on the user concept -- the deletion scrub
	// (@scrubPii, #1711) zeroes it -- while #2860 introduced this construct
	// specifically as the PII-free cross-user read that rosters, mentions and
	// participant lists need.
	//
	// Both are right. `displayName` is personal data for the purpose of
	// erasure, and simultaneously the one field whose cross-user disclosure is
	// the entire point of the construct: you cannot render a name next to an id
	// without returning the name. userDisplayCard is `row.id` + `displayName`
	// and nothing else, so there is no second field riding along.
	//
	// Un-marking the field to silence this would be the wrong repair -- it
	// would quietly narrow the deletion scrub, which is a GDPR path (#1711).
	// Recording the decision here keeps the annotation honest for the scrub and
	// keeps the gate honest about what it saw.
	"identity/queries.memql userDisplayById": "projects userDisplayCard -- `row.id` + `displayName`, nothing else. displayName is @pii for the deletion scrub's purposes, and is also the field this construct exists to return: #2860 split it out of userById precisely so a cross-user lookup could resolve a name WITHOUT the full row. Caller-scoping it would defeat it, since cross-user display is the purpose.",
}

// piiFieldsByConcept maps each concept name to the set of its `@pii` fields.
func piiFieldsByConcept(t *testing.T, sources map[string]string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, src := range sources {
		blanked := blankComments(src)
		for _, m := range conceptDeclRe.FindAllStringSubmatchIndex(blanked, -1) {
			name := blanked[m[2]:m[3]]
			closeIdx := matchingClose(blanked, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := blanked[m[1]:closeIdx]
			for _, fm := range piiFieldRe.FindAllStringSubmatch(body, -1) {
				if out[name] == nil {
					out[name] = map[string]bool{}
				}
				out[name][fm[1]] = true
			}
		}
	}
	return out
}

// shapeInfo is one concept-bound shape: what it binds and what it projects,
// before `include` resolution.
type shapeInfo struct {
	concept  string
	fields   map[string]bool
	includes []string
}

// piiShapes maps shape name -> shapeInfo for every concept-bound shape.
func piiShapes(t *testing.T, sources map[string]string) map[string]*shapeInfo {
	t.Helper()
	out := map[string]*shapeInfo{}
	for _, src := range sources {
		blanked := blankComments(src)
		for _, m := range piiShapeDeclRe.FindAllStringSubmatchIndex(blanked, -1) {
			concept := blanked[m[2]:m[3]]
			name := blanked[m[4]:m[5]]
			closeIdx := matchingClose(blanked, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := blanked[m[1]:closeIdx]
			si := &shapeInfo{concept: concept, fields: map[string]bool{}}
			for _, im := range piiIncludeRe.FindAllStringSubmatch(body, -1) {
				si.includes = append(si.includes, im[1])
			}
			for _, pm := range piiProjectionRe.FindAllStringSubmatch(body, -1) {
				path := pm[1]
				if strings.HasPrefix(path, "actor.") {
					continue
				}
				path = strings.TrimPrefix(path, "row.")
				if i := strings.LastIndexByte(path, '.'); i >= 0 {
					path = path[i+1:]
				}
				si.fields[path] = true
			}
			out[name] = si
		}
	}
	return out
}

// projectedFields resolves a shape's projections transitively through
// `include`. Cycles are impossible in a loading tree (the loader rejects
// them) but the seen-set keeps this terminating regardless.
func projectedFields(name string, shapes map[string]*shapeInfo, seen map[string]bool) map[string]bool {
	out := map[string]bool{}
	if seen[name] {
		return out
	}
	seen[name] = true
	si, ok := shapes[name]
	if !ok {
		return out
	}
	for f := range si.fields {
		out[f] = true
	}
	for _, inc := range si.includes {
		for f := range projectedFields(inc, shapes, seen) {
			out[f] = true
		}
	}
	return out
}

// TestPiiProjectionRequiresCallerGate is the projection-axis gate (memql#2883).
//
// A query projecting a shape that carries `@pii` fields must constrain its
// caller -- with an actor predicate, a context-spec, or `@serverOnly` --
// regardless of what its filter selects on.
func TestPiiProjectionRequiresCallerGate(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

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
	}

	pii := piiFieldsByConcept(t, sources)
	if len(pii) == 0 {
		t.Fatal("found 0 concepts carrying @pii fields -- this gate would then scan nothing and pass vacuously, which is the failure mode that made the previous user-scope detector report a meaningless zero (#2799); check conceptDeclRe/piiFieldRe against dsl/identity/concepts.memql")
	}

	shapes := piiShapes(t, sources)
	if len(shapes) == 0 {
		t.Fatal("found 0 concept-bound shapes -- check piiShapeDeclRe against the tree")
	}

	// A shape is PII-bearing when its bound concept marks at least one of the
	// fields it projects as @pii.
	piiBearing := map[string]bool{}
	for name, si := range shapes {
		conceptPii := pii[si.concept]
		if len(conceptPii) == 0 {
			continue
		}
		for f := range projectedFields(name, shapes, map[string]bool{}) {
			if conceptPii[f] {
				piiBearing[name] = true
				break
			}
		}
	}
	if len(piiBearing) == 0 {
		t.Fatal("found 0 PII-bearing shapes -- with @pii present on a concept this means the shape->concept join or the projection parse is broken, not that the tree is clean")
	}

	contextSpecs := actorBoundSpecNames(t)
	specRefs := make(map[string]*regexp.Regexp, len(contextSpecs))
	for name := range contextSpecs {
		specRefs[name] = regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	}

	// Same leaf predicate as the #2840 gate: an affirmative actor equality, or
	// a context-spec reference. Polarity matters -- `!= actor.userId` mentions
	// the caller and scopes nothing.
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

	var flagged []string
	seen := map[string]bool{}
	scanned := 0

	for _, p := range paths {
		src := sources[p]
		blanked := blankComments(src)
		for _, m := range piiQueryDeclRe.FindAllStringSubmatchIndex(blanked, -1) {
			name := blanked[m[4]:m[5]]
			closeIdx := matchingClose(blanked, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := blanked[m[1]:closeIdx]

			sm := piiShapeClauseRe.FindStringSubmatch(body)
			if sm == nil || !piiBearing[sm[1]] {
				continue
			}
			scanned++

			// No preamble walk any more: the @serverOnly verdict comes from the
			// parsed tree (memql#2875), so this gate no longer needs to read
			// annotation text at all. The walk that used to be here -- on the
			// ORIGINAL source rather than the blanked view, per memql#2868's
			// trap -- went with the regex it fed.

			clause := stripLineComments(filterClauseOf(body))
			// #2875: the @serverOnly verdict comes from the PARSED tree, not from a
			// regex over the preamble. The regex could be satisfied by an
			// `@serverOnly` inside a multi-line annotation string or a block
			// comment opened on an `@`-line -- which EXEMPTED the construct from
			// this gate while Function.ServerOnly stayed false and nothing was
			// enforced at runtime. That exemption happens BEFORE the
			// exemption-map bookkeeping below, so the construct was not even
			// recorded in `seen`.
			gated := serverOnlyConstructs(t)[serverOnlyKey{Path: p, Name: name}] ||
				(strings.TrimSpace(clause) != "" && clauseGuarantees(clause, leaf))
			if gated {
				continue
			}

			key := p + " " + name
			seen[key] = true
			if _, exempt := piiProjectionExemptions[key]; exempt {
				continue
			}
			if _, accepted := piiProjectionAccepted[key]; accepted {
				continue
			}
			flagged = append(flagged, fmt.Sprintf("%s: %s projects %s, which carries @pii fields, with no caller check", p, name, sm[1]))
		}
	}

	if scanned == 0 {
		t.Fatal("scanned 0 queries projecting a PII-bearing shape -- the detector is not measuring what its name says; check piiQueryDeclRe and piiShapeClauseRe against dsl/identity/queries.memql")
	}

	sort.Strings(flagged)
	for _, f := range flagged {
		t.Errorf("%s\n\tThe projection carries personally-identifying fields, so the filter is not the only question -- `searchUsers` returned every user in the cluster behind a `when()` guard that vanishes when its arg is absent (memql#2883). Scope it to actor.*, gate it with a context-spec such as requiresOwnerOrAdmin, mark it @serverOnly if its only caller is server-side, or project a PII-free shape.", f)
	}

	for _, mp := range []struct {
		name    string
		entries map[string]string
	}{
		{"piiProjectionExemptions", piiProjectionExemptions},
		{"piiProjectionAccepted", piiProjectionAccepted},
	} {
		for key := range mp.entries {
			if !seen[key] {
				t.Errorf("%s has a stale entry %q -- the construct no longer matches this detector (renamed, fixed, gated, or deleted). Remove the entry.", mp.name, key)
			}
		}
	}
}
