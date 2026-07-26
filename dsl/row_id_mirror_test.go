package dsl

// row_id_mirror_test.go -- memql#2784.
//
// Two concepts stamped their own row id into a payload field and then filtered
// QUERIES on the copy rather than on the id:
//
//	mutate deployment createDeployment {
//	  insert {
//	    id:           args.deploymentId
//	    deploymentId: args.deploymentId   // <- the mirror
//	    ...
//
//	query deployment deploymentById {
//	  filter deploymentId == args.deploymentId   // <- reads the mirror
//
// The justification was a comment claiming the row id "would need
// partition+concept prefixing" -- stale on both counts: the {partition}: prefix
// was retired in #56, and the concept prefix is composed by the ENGINE
// (resolveFullId), not by the caller.
//
// # Why this needs a gate rather than a corrected comment
//
// The two filters are equivalent ONLY while every writer keeps the two fields
// in step. Nothing enforces that, and the failure is silent in the worst
// direction: a by-id query that stops matching returns ZERO ROWS rather than
// erroring. `oAuthClientByClientId` sits on the unauthenticated /oauth/token
// path; `deploymentById` is how the deploy controller reads its own state.
//
// The silence is also why the obvious tests do not discriminate. A round-trip
// test over resolveFullId / BuildNodeId pins the ID COMPOSITION, not the
// filter, and passes whichever field the filter names. A database test that
// creates a row through the normal mutation cannot tell the two apart either,
// because that mutation stamps the mirror from the same argument -- so the
// mirror and the id agree by construction and both filters return the same
// single row. Pre-fix and post-fix are indistinguishable to every test that
// goes through the front door.
//
// So the discriminating check has to be structural, on the authored text. That
// is this file.
//
// # What it detects
//
// The general class, not the two known instances:
//
//  1. In each mutation's write block, find the argument the row `id` is stamped
//     from. Any OTHER payload field stamped from that same argument is a
//     self-mirror of the row id. `accept { clientId }` is desugared, since it
//     is shorthand for `clientId: args.clientId` and is how the oauthClient
//     instance is spelled.
//  2. Any query bound to that concept whose filter compares the mirror field
//     against an arg is reading the copy where the intrinsic is available.
//
// A new concept that adopts the same shape is caught the day it lands, which is
// the point -- the class grew to two precisely because nothing counted it.
//
// # Deliberately NOT flagged
//
// The mirror FIELD itself is left alone. Both instances have real readers that
// are not "look the row up by its own id": `component/node/reconciler.go` reads
// `deploymentId` off the payload, `deploymentFull` projects it, and deleting a
// projected field is a consumer-visible change this gate has no business
// forcing. The defect is querying THROUGH the mirror, not storing it.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

var (
	// mirrorMutateHeaderRe matches a concept-bound mutation header.
	mirrorMutateHeaderRe = regexp.MustCompile(`(?m)^[ \t]*mutate[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// mirrorQueryHeaderRe matches a concept-bound query header.
	mirrorQueryHeaderRe = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	// mirrorStampRe matches a `<field>: args.<arg>` stamp line, tolerating a
	// trailing `?? default`.
	mirrorStampRe = regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*:[ \t]*args\.([A-Za-z_][A-Za-z0-9_]*)`)

	// mirrorAcceptRe matches an `accept { a, b, c }` block. Each named field is
	// shorthand for `<name>: args.<name>`.
	mirrorAcceptRe = regexp.MustCompile(`(?s)accept[ \t]*\{([^}]*)\}`)
)

// rowIdMirrorExemptions records queries that filter a self-mirror of the row id
// and are known-outstanding rather than accepted. Keyed "<file> <constructName>".
//
// Empty, and it should stay that way: the fix is one token (`row.id`), and the
// engine composes the concept prefix on both sides, so there is no migration to
// stage.
var rowIdMirrorExemptions = map[string]string{}

// selfMirrorFields returns concept -> {mirrorField -> mutation that stamps it},
// where a mirror field is a payload field written from the SAME argument the
// row `id` is written from.
func selfMirrorFields(t *testing.T, sources map[string]string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}

	for _, src := range sources {
		blanked := blankComments(src)
		for _, m := range mirrorMutateHeaderRe.FindAllStringSubmatchIndex(blanked, -1) {
			concept := blanked[m[2]:m[3]]
			mutation := blanked[m[4]:m[5]]
			closeIdx := matchingClose(blanked, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := blanked[m[1]:closeIdx]

			// Which arg does the row id come from?
			var idArg string
			stamps := map[string]string{}
			for _, sm := range mirrorStampRe.FindAllStringSubmatch(body, -1) {
				field, arg := sm[1], sm[2]
				if field == "id" {
					idArg = arg
					continue
				}
				stamps[field] = arg
			}
			if idArg == "" {
				continue
			}

			// `accept { x, y }` is shorthand for `x: args.x, y: args.y`.
			for _, am := range mirrorAcceptRe.FindAllStringSubmatch(body, -1) {
				for _, raw := range strings.Split(am[1], ",") {
					name := strings.TrimSpace(raw)
					if name == "" || name == "id" {
						continue
					}
					stamps[name] = name
				}
			}

			for field, arg := range stamps {
				if arg != idArg {
					continue
				}
				if out[concept] == nil {
					out[concept] = map[string]string{}
				}
				out[concept][field] = mutation
			}
		}
	}
	return out
}

// TestByIdQueriesFilterTheRowIdNotASelfMirror is the memql#2784 gate.
//
// A query must not filter on a payload field that duplicates the row id. The
// intrinsic is authoritative, the copy is only as good as every writer that
// maintains it, and a drift between them returns zero rows silently.
func TestByIdQueriesFilterTheRowIdNotASelfMirror(t *testing.T) {
	paths, err := dslfs.WalkMemqlFiles(Tree())
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	sources := make(map[string]string, len(paths))
	for _, p := range paths {
		sources[p] = readTreeFile(t, p)
	}

	mirrors := selfMirrorFields(t, sources)
	if len(mirrors) == 0 {
		t.Fatal("found 0 self-mirroring concepts -- this gate would then pass vacuously. " +
			"createDeployment (id + deploymentId from args.deploymentId) and createOAuthClient " +
			"(stamp id from args.clientId + accept clientId) are both in the tree, so zero means " +
			"mirrorStampRe / mirrorAcceptRe stopped matching the authored form, not that the tree is clean")
	}

	var flagged []string
	seen := map[string]bool{}

	for _, p := range paths {
		blanked := blankComments(sources[p])
		for _, m := range mirrorQueryHeaderRe.FindAllStringSubmatchIndex(blanked, -1) {
			concept := blanked[m[2]:m[3]]
			name := blanked[m[4]:m[5]]
			fields := mirrors[concept]
			if len(fields) == 0 {
				continue
			}
			closeIdx := matchingClose(blanked, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			clause := filterClauseOf(blanked[m[1]:closeIdx])
			if strings.TrimSpace(clause) == "" {
				continue
			}

			for field, mutation := range fields {
				// The mirror read BARE (`deploymentId == args.x`). `row.id` is
				// the fix, and a payload field is never written `row.<field>`,
				// so requiring the bare spelling cannot match the fixed form.
				bare := regexp.MustCompile(`(?:^|[^.\w])` + regexp.QuoteMeta(field) + `[ \t]*==[ \t]*args\.`)
				if !bare.MatchString(clause) {
					continue
				}
				key := p + " " + name
				seen[key] = true
				if _, exempt := rowIdMirrorExemptions[key]; exempt {
					continue
				}
				flagged = append(flagged, fmt.Sprintf(
					"%s: %s filters on %q, which %s stamps from the same argument as the row id -- filter `row.id` instead",
					p, name, field, mutation))
			}
		}
	}

	sort.Strings(flagged)
	for _, f := range flagged {
		t.Errorf("%s\n\tThe payload field is a COPY of the row id (memql#2784). The two agree only "+
			"while every writer keeps them in step, and when they drift the query returns zero rows "+
			"rather than erroring -- silently, on the unauthenticated /oauth/token path and on the "+
			"deploy controller's read of its own state. The engine composes the concept prefix on "+
			"both sides (resolveFullId vs id.BuildNodeId), so `row.id == args.X` with the same bare "+
			"argument is a drop-in replacement.", f)
	}

	for key := range rowIdMirrorExemptions {
		if !seen[key] {
			t.Errorf("rowIdMirrorExemptions has a stale entry %q -- the construct no longer matches. Remove the entry.", key)
		}
	}
}
