package fleet

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Per-row authorization over the fleet domain (epic memql#3852, task memql#3855).
//
// # What this file is actually asserting
//
// Task memql#3855's acceptance criterion is "cross-tenant reads impossible".
// Orbit is a console -- a client -- and a client cannot be the gate. The gate is
// two things, both server-side, and this file checks both:
//
//  1. Every concept carrying customer data declares `@rowAuthz(owner=...)`, so
//     the engine injects the ownership predicate into every read and refuses a
//     write whose target belongs to somebody else.
//  2. Every caller-scoped QUERY additionally names `ownerUserId==actor.userId`
//     as a TOP-LEVEL conjunct.
//
// # Why the second one, when the first already enforces it
//
// Because of WHERE the conjunct sits, not whether it exists. A conjunct inside
// a `when()` guard VANISHES when its argument is absent -- that is what the
// guard is for -- so a query that reads as scoped returns the whole table the
// moment a caller omits an optional argument. That is memql#2883, it has
// happened, and it looks completely correct in review.
//
// The declaration and the conjunct are also read by different things: the
// engine enforces the first, and the per-row-authz CLASSIFIER reads the second.
// A query the classifier cannot see is a query nobody audits.
//
// # Why it lives here rather than in test/dslconformance
//
// The conformance suite walks `dsl.Tree()` -- the embedded tree plus
// plugin-registered subtrees. This bundle is neither: it is a directory under
// deploy/, delivered at runtime through MEMQL_DSL_PATH. So the domain that
// holds every memQL Cloud customer's account, subscription and usage data is
// the one domain the repository's own authz gate does not look at.

var (
	// `@rowAuthz(...)` on the line(s) preceding a concept declaration.
	rowAuthzAttr  = regexp.MustCompile(`@rowAuthz\(([^)]*)\)`)
	conceptDecl   = regexp.MustCompile(`(?m)^concept\s+(\w+)\s*\{`)
	queryDecl     = regexp.MustCompile(`(?m)^query\s+(\w+)\s+(\w+)\s*\{`)
	publicAttr    = regexp.MustCompile(`(?m)^@public\s*$`)
	filterClause  = regexp.MustCompile(`(?m)^\s+filter\s+(.+)$`)
	ownerConjunct = "ownerUserId==actor.userId"
)

func fleetFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundleRoot(t), "fleet", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestEveryCustomerConceptDeclaresAnOwner.
//
// The tier catalog is deliberately public -- it is a price list, read
// unauthenticated by the pricing page, carrying no customer data. Everything
// else in this domain is somebody's account, plan, instance or usage, and each
// must declare an owner so the engine's row-authz layer has a field to gate on.
//
// A concept with NO @rowAuthz is the failure worth naming: it is not "insecure
// by a wrong setting", it is outside the mechanism entirely, and every query
// over it is unscoped no matter how carefully written.
func TestEveryCustomerConceptDeclaresAnOwner(t *testing.T) {
	src := fleetFile(t, "concepts.memql")

	// The price list, and the only concept permitted to be public.
	publicConcepts := map[string]bool{"tierSpec": true}

	lines := strings.Split(src, "\n")
	var pendingAuthz string
	var seen int

	for _, line := range lines {
		if m := rowAuthzAttr.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(line), "//") {
			pendingAuthz = strings.TrimSpace(m[1])
			continue
		}
		m := conceptDecl.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		seen++

		switch {
		case publicConcepts[name]:
			if pendingAuthz != "public" {
				t.Errorf("concept %s is the price list and must be @rowAuthz(public); it declares %q", name, pendingAuthz)
			}
		case pendingAuthz == "":
			t.Errorf("concept %s declares no @rowAuthz. It is not merely mis-scoped -- it is outside the row-authz mechanism entirely, so every read of it is unscoped however carefully the query is written.", name)
		case !strings.HasPrefix(pendingAuthz, `owner=`):
			t.Errorf("concept %s declares @rowAuthz(%s); every customer-facing concept in this domain must be owner-scoped, because a subscriber may only ever see their own rows.", name, pendingAuthz)
		}
		pendingAuthz = ""
	}

	if seen == 0 {
		t.Fatal("parsed no concepts -- either concepts.memql moved or this parse stopped matching, and either way this gate is watching nothing")
	}
}

// TestEveryFleetQueryIsCallerScoped.
//
// The load-bearing half. A query is either `@public` -- and then it must bind
// the narrow public projection and touch no customer concept -- or it names
// `ownerUserId==actor.userId` as a TOP-LEVEL conjunct of its filter.
//
// Top-level is checked by splitting on `&&` at depth zero and requiring the
// conjunct to be one of the resulting terms. Inside a `when()` guard it is not
// a top-level term, which is exactly the distinction that matters: the guard
// drops its conjunct when the argument is absent, so a scoped-looking query
// returns everything (memql#2883).
func TestEveryFleetQueryIsCallerScoped(t *testing.T) {
	src := fleetFile(t, "queries.memql")

	// THREE CATEGORIES, and the third is the one worth explaining.
	//
	// PUBLIC -- unauthenticated by intent. The price list, which is what a price
	// list is for. Listed here so that making a NEW fleet read public is a
	// deliberate edit to this test rather than an annotation nobody notices.
	publicQueries := map[string]string{
		"publicTiers": "the public price list; binds tierPublic, which projects no operational field",
		"tierByName":  "one price-list row for the pricing page's detail view",
	}

	// OWNERLESS -- authenticated, but reading a concept that HAS no owner, so
	// there is no ownerUserId to compare against. Exactly one concept qualifies
	// (tierSpec, the price list) and the check below verifies that rather than
	// trusting this list: a query listed here that reads a customer concept
	// fails, so the category cannot be used to excuse an unscoped read of
	// somebody's subscription.
	//
	// The distinction from PUBLIC is real and is why this is its own bucket:
	// `tierSpecByName` returns the operational fields -- which infrastructure
	// profile a tier provisions, which database preset it uses -- that
	// `tierPublic` deliberately withholds from the open internet.
	ownerlessQueries := map[string]string{
		"tierSpecByName": "the price list WITH its operational fields, for the provisioning path and Orbit's upgrade picker; tierSpec is @rowAuthz(public) and carries no owner to scope on",
	}

	// SERVER-ONLY -- an internal sweep or webhook handler, deliberately
	// unscoped, because there is no caller to scope to. A scheduled automation
	// runs with no actor at all, and "find every trial that expired today" is
	// not a question a caller-scoped read can answer: constraining it to
	// actor.userId would return NOTHING while looking perfectly scoped.
	//
	// This is the one bucket that could hide a genuine hole, so it is the one
	// with two independent checks below rather than a list:
	//
	//   the query must carry BOTH @serverOnly and @unbounded. @serverOnly is
	//   what actually refuses a client-originated call; @unbounded is what
	//   stops the pagination gate silently truncating a sweep to its first page
	//   and leaving the tail of the fleet unswept. Either alone is a defect
	//   wearing the other's clothes -- @unbounded without @serverOnly is
	//   precisely the memql#2883 shape, an unconstrained projection of every
	//   subscriber's rows reachable from a browser.
	//
	//   and @unbounded must carry a REASON. The annotation exists to make
	//   somebody write down why reading every row is correct here, and an empty
	//   one converts a deliberate exemption into a syntax that silences a gate.
	serverOnlyAttr := regexp.MustCompile(`(?m)^@serverOnly\s*$`)
	unboundedAttr := regexp.MustCompile(`(?m)^@unbounded\("([^"]*)"\)`)

	// The concepts with no owner. Derived from the same source the concept gate
	// reads, so this cannot drift from what is actually declared.
	ownerless := ownerlessConcepts(t)

	blocks := splitQueryBlocks(src)
	if len(blocks) == 0 {
		t.Fatal("parsed no queries -- either queries.memql moved or this parse stopped matching, and either way this gate is watching nothing")
	}

	var checked int
	for name, body := range blocks {
		isPublic := publicAttr.MatchString(body)

		if reason, listed := publicQueries[name]; listed {
			if !isPublic {
				t.Errorf("query %s is listed as public here but is not annotated @public (%s)", name, reason)
			}
			continue
		}
		if isPublic {
			t.Errorf("query %s is @public but is not in this test's publicQueries list. Making a fleet read unauthenticated is a decision, not an annotation -- add it with its reason, or scope it.", name)
			continue
		}

		if serverOnlyAttr.MatchString(body) {
			m := unboundedAttr.FindStringSubmatch(body)
			if m == nil {
				t.Errorf("query %s is @serverOnly but not @unbounded. The pagination gate will truncate it to its first page, so a sweep silently leaves the tail of the fleet unswept -- and reports success.", name)
				continue
			}
			if strings.TrimSpace(m[1]) == "" {
				t.Errorf("query %s carries @unbounded with an empty reason. The annotation exists to make somebody write down why reading every row is correct here; an empty one is a syntax that silences a gate.", name)
			}
			continue
		}
		if unboundedAttr.MatchString(body) {
			t.Errorf("query %s is @unbounded but NOT @serverOnly. That is the memql#2883 shape exactly: an unconstrained projection of every subscriber's rows, with no caller predicate, reachable from a browser.", name)
			continue
		}

		if reason, listed := ownerlessQueries[name]; listed {
			// The claim is checked, not taken: the query must actually bind a
			// concept that has no owner. Otherwise this bucket becomes a place
			// to park an unscoped read of somebody's subscription.
			concept := queryConcept(body)
			if !ownerless[concept] {
				t.Errorf("query %s is listed as ownerless (%s) but binds concept %q, which declares an owner. A read of an owned concept must be caller-scoped; this listing would excuse a cross-tenant read.", name, reason, concept)
			}
			continue
		}

		m := filterClause.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("query %s declares no filter, so it reads every row in its concept for every caller", name)
			continue
		}
		checked++

		if !hasTopLevelConjunct(m[1], ownerConjunct) {
			t.Errorf("query %s does not name %s as a TOP-LEVEL conjunct of its filter:\n    filter %s\n"+
				"A conjunct inside a when() guard vanishes when its argument is absent, so this query returns the whole table for a caller who omits an optional argument (memql#2883).",
				name, ownerConjunct, strings.TrimSpace(m[1]))
		}
	}

	if checked == 0 {
		t.Fatal("every query was public, so this gate checked no scoping at all")
	}
}

// ownerlessConcepts returns the concepts in this domain that declare no owner
// -- today just the price list.
//
// DERIVED from concepts.memql rather than listed, so the ownerless-query bucket
// above cannot drift from what is actually declared. If somebody makes a
// customer concept public, this set grows and the queries over it stop being
// excusable in one step rather than two.
func ownerlessConcepts(t *testing.T) map[string]bool {
	t.Helper()
	src := fleetFile(t, "concepts.memql")
	out := map[string]bool{}

	var pendingAuthz string
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := rowAuthzAttr.FindStringSubmatch(line); m != nil {
			pendingAuthz = strings.TrimSpace(m[1])
			continue
		}
		if m := conceptDecl.FindStringSubmatch(line); m != nil {
			if !strings.HasPrefix(pendingAuthz, "owner=") {
				out[m[1]] = true
			}
			pendingAuthz = ""
		}
	}
	return out
}

// queryConcept extracts the concept a query binds from its signature,
// `query <Concept> <name> {`.
func queryConcept(body string) string {
	if m := queryDecl.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// splitQueryBlocks returns each query's name mapped to its source, including
// the annotation lines above the declaration (which is where @public sits).
func splitQueryBlocks(src string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(src, "\n")

	var preamble []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		m := queryDecl.FindStringSubmatch(line)
		if m == nil {
			trimmed := strings.TrimSpace(line)
			// Keep the annotations attached to the next declaration; reset on a
			// blank line so a stray @public far above cannot be misattributed.
			if trimmed == "" {
				preamble = nil
			} else {
				preamble = append(preamble, line)
			}
			continue
		}
		body := append(append([]string{}, preamble...), line)
		depth := strings.Count(line, "{") - strings.Count(line, "}")
		for i+1 < len(lines) && depth > 0 {
			i++
			body = append(body, lines[i])
			depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		}
		out[m[2]] = strings.Join(body, "\n")
		preamble = nil
	}
	return out
}

// hasTopLevelConjunct reports whether `want` is one of the filter's `&&` terms
// at bracket depth zero -- i.e. not nested inside a when() guard, a parenthesis
// group, or a sub-expression.
func hasTopLevelConjunct(filter, want string) bool {
	var depth int
	var cur strings.Builder
	var terms []string

	for i := 0; i < len(filter); i++ {
		c := filter[i]
		switch c {
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		}
		if depth == 0 && c == '&' && i+1 < len(filter) && filter[i+1] == '&' {
			terms = append(terms, cur.String())
			cur.Reset()
			i++ // consume the second '&'
			continue
		}
		cur.WriteByte(c)
	}
	terms = append(terms, cur.String())

	for _, term := range terms {
		if strings.ReplaceAll(strings.TrimSpace(term), " ", "") == want {
			return true
		}
	}
	return false
}

// TestEveryOwnedMutationStampsTheOwnerFromTheActor.
//
// The write half of the same property. A mutation that takes `ownerUserId` as
// an ARGUMENT lets a caller create rows owned by somebody else; one that omits
// it on an UPDATE leaves the field to the read-merge, and a row whose ownership
// depends on a merge is a row whose ownership can drift.
//
// So every insert and update in this domain must contain the literal
// `ownerUserId: actor.userId`, and none may declare `ownerUserId` in its args.
func TestEveryOwnedMutationStampsTheOwnerFromTheActor(t *testing.T) {
	src := fleetFile(t, "mutations.memql")

	blocks := regexp.MustCompile(`(?m)^mutate\s+(\w+)\s+(\w+)\s*\{`).FindAllStringSubmatchIndex(src, -1)
	if len(blocks) == 0 {
		t.Fatal("parsed no mutations -- this gate is watching nothing")
	}

	for i, m := range blocks {
		name := src[m[4]:m[5]]
		end := len(src)
		if i+1 < len(blocks) {
			end = blocks[i+1][0]
		}
		body := src[m[0]:end]

		if !strings.Contains(body, "ownerUserId: actor.userId") {
			t.Errorf("mutation %s does not stamp `ownerUserId: actor.userId`. On an insert that lets a caller create rows owned by somebody else; on an update it leaves ownership to the read-merge, where it can drift.", name)
		}

		// `ownerUserId` must not appear as a declared argument. Checked against
		// the args block only, since the stamp above legitimately mentions it.
		if args := regexp.MustCompile(`(?s)args\s*\{(.*?)\}`).FindStringSubmatch(body); args != nil {
			if regexp.MustCompile(`(?m)^\s*ownerUserId\s`).MatchString(args[1]) {
				t.Errorf("mutation %s declares `ownerUserId` as an argument. Ownership is stamped from the actor, never accepted from a caller -- a caller who can name the owner can write rows they do not own.", name)
			}
		}
	}
}
