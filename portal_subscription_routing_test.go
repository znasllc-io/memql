package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/node"
)

// THE ROUTING CONFORMANCE GATE (memql#4543).
//
// # The class this closes
//
// Cross-node event routing is default-deny (component/node/routing.go). A
// concept with no forward rule is DARK in the mesh: written on one replica,
// invisible to a browser subscribed through another, with no error anywhere.
// The surface is correct when it loads and frozen thereafter -- which the
// routing file itself calls "the worst of the three possible behaviours
// because it looks like it is working".
//
// It has now been found by a human four times. memql#4349 was the Fleet
// pages. memql#4542 was saved views, the Agents view, the Library, Nexus's
// whole construct half, and every cognition DELETE. Each fix was correct and
// none of them closed the class, because the class is not "these concepts
// were missed" -- it is "nothing checks".
//
// # What this gate asserts
//
// Every concept a portal surface subscribes to, for every verb it subscribes
// with, either forwards across the mesh or is a RECORDED exclusion with a
// reason. A new subscription against an unrouted concept fails the build and
// names both ways out.
//
// A "subscription" is either a direct `subscribeGraph({concept, actions})` or
// a `useLive(...)` collection spec declaring the same two fields (memql#4539).
// The second is now the normal shape: the fold lives in the SDK, and the spec
// object is exactly what the SDK passes to subscribeGraph.
//
// # It queries the real tables, never a copy
//
// The evaluation goes through node.ForwardsGraphEvent, which calls the same
// evaluateRouting the event bridge calls, over the same defaultRoutingRules
// plus whatever this binary's packages registered. A gate holding a string
// copy of the table would be asserting about its copy: the wildcard rules
// alone are enough to make a plausible re-implementation disagree, since
// `v1:planner:*` is an INTRA-segment glob (a concept id contains no dots)
// rather than the segment wildcard the pattern looks like.
//
// # What it does NOT reach, stated plainly
//
//   - SDK call sites outside clients/portal. Product SPAs live in downstream
//     repos and this gate cannot see them; a product that subscribes to its
//     own concepts needs its own rules, registered from its own pack through
//     node.RegisterRoutingRule.
//   - The generic concept browser, which subscribes to WHATEVER concept the
//     operator navigated to. It is declared in unboundSubscriptions below
//     with its reason; enumerating every concept in the registry is not a
//     routing table, it is a broadcast of everything.
//   - Whether a forwarded event is one the surface can actually READ. Row
//     admission decides that separately (memql#4309), and a subscription
//     that receives an id-only notification it then fails to resolve is a
//     correct outcome here.
//   - Non-graph subscription kinds (TELEMETRY / MESSAGE / AI_STREAM). They
//     carry node-level events with no concept id to route by.
//
// # Where it lives, and why not in vitest
//
// The same reasoning as portal_render_path_test.go and
// portal_view_composition_test.go: a guard inside clients/portal can be
// deleted by the same change that breaks it, and the portal lane stays
// green. This is a .go file outside that tree.

const portalSrcDir = "clients/portal/src"

// unboundSubscriptions declares the subscription sites whose concept cannot
// be resolved statically, each with the reason it cannot be.
//
// A site here is NOT exempt from thought -- it is a site where the routing
// question has a different answer than "add a rule", and the reason has to
// say which. Adding an entry to silence a failure you have not understood is
// the misuse this map invites; the reason string is where that shows.
var unboundSubscriptions = map[string]string{
	"clients/portal/src/cluster/useConceptRows.ts": "The GENERIC concept browser (/concepts/:conceptId). Its concept comes " +
		"from the route, so the set is every concept in the registry and " +
		"enumerating it would not be a routing table -- it would be a mesh " +
		"broadcast of everything, including the invocation-class volumes the " +
		"exclusions exist to keep off the wire. The pane is honest about the " +
		"consequence: its live band is best-effort for concepts outside the " +
		"forward table, its Refresh is the answer, and useConceptRows.ts says " +
		"so in its header. An operator surface that must be live belongs in " +
		"the table by name, the way the Fleet and Nexus pages are.",
}

func TestEveryPortalSubscriptionIsRoutedOrExcluded(t *testing.T) {
	sites, consts := portalSubscriptionSites(t)

	// COVERAGE FIRST. A gate that silently examined nothing would pass, and
	// its pass would be a claim about the tool rather than about the code.
	if len(sites) == 0 {
		t.Fatalf("no subscribeGraph call sites found under %s.\n"+
			"Either the portal stopped subscribing (unlikely) or the extractor stopped matching (likely -- "+
			"check whether the call was renamed or wrapped). A gate that finds nothing passes forever.", portalSrcDir)
	}
	if len(consts) == 0 {
		t.Fatalf("no concept-id constants found under %s -- the resolver cannot work, so every site would read as dynamic", portalSrcDir)
	}
	t.Logf("examined %d subscribeGraph call site(s) against %d concept-id constant(s)", len(sites), len(consts))

	resolvedAny := false
	for _, s := range sites {
		if len(s.concepts) == 0 {
			reason, declared := unboundSubscriptions[s.file]
			if !declared {
				t.Errorf("%s:%d subscribes to a concept this gate cannot resolve (expression: %s).\n"+
					"  Three ways out, in preference order:\n"+
					"    1. name the concept with a literal or a module-level `const X = \"v1:...\"` so the gate can see it;\n"+
					"    2. if the hook forwards a caller's argument, keep the parameter name -- the gate resolves call sites;\n"+
					"    3. if the concept is genuinely unbounded, add %q to unboundSubscriptions in this file WITH the reason.",
					s.file, s.line, s.expr, s.file)
				continue
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is declared unbound with an empty reason -- an unbound site without a why is indistinguishable from one nobody looked at", s.file)
			}
			continue
		}
		resolvedAny = true

		for _, conceptId := range s.concepts {
			for _, verb := range s.verbs {
				topic := node.GraphEventTopic(verb, conceptId)
				if node.ForwardsGraphEvent(topic) {
					continue
				}
				if ex, ok := node.ExcludedFromForwarding(topic); ok {
					if strings.TrimSpace(ex.Reason) == "" {
						t.Errorf("%s is excluded by pattern %q with an EMPTY reason. An exclusion without a why is a hole with paperwork.", topic, ex.Pattern)
					}
					continue
				}
				t.Errorf("%s:%d subscribes to %s, and %s is neither forwarded nor excluded.\n"+
					"  With two replicas per mesh node (the default topology) this surface is correct on load and frozen afterwards -- no error, nothing in a log.\n"+
					"  Two ways out, and both are a one-line change with a reason:\n"+
					"    1. add a forward rule in component/node/routing.go naming who writes the row and who reads it;\n"+
					"    2. add an entry to node.RoutingExclusions() saying why the mesh should not carry it.",
					s.file, s.line, conceptId, topic)
			}
		}
	}

	if !resolvedAny {
		t.Fatal("every subscription site resolved to zero concepts -- the resolver is broken, and every assertion above was vacuous")
	}
}

// TestPortalSubscriptionExtractorFindsTheKnownSites is the extractor's own
// negative control (repo memory: a null result proves nothing until you show
// the instrument could have moved).
//
// The list is deliberately SHORT and deliberately not exhaustive: it names
// sites whose disappearance would mean the extractor broke rather than that
// the portal changed. A refactor that renames these files should update this
// list; a refactor that makes the extractor stop seeing them must not pass.
func TestPortalSubscriptionExtractorFindsTheKnownSites(t *testing.T) {
	sites, _ := portalSubscriptionSites(t)

	found := map[string][]string{}
	for _, s := range sites {
		found[s.file] = append(found[s.file], s.concepts...)
	}

	for _, want := range []struct{ file, concept string }{
		// A module-level constant declared in the same file, in a useLive spec.
		{"clients/portal/src/compose/useSavedViews.ts", "v1:portalviews:view"},
		// A constant imported from another file, in a useLive spec.
		{"clients/portal/src/fleet/useMachines.ts", "v1:worker:registration"},
		// A hook parameter, resolved at the hook's call sites.
		{"clients/portal/src/home/useHomeTiles.ts", "v1:identity:auditEvent"},
		// A second useLive spec, in a different tree -- the extractor is not
		// keyed on any one file's shape.
		{"clients/portal/src/people/usePendingInvitations.ts", "v1:identity:invitation"},
	} {
		got, ok := found[want.file]
		if !ok {
			t.Errorf("the extractor found no subscription in %s -- it resolves one of the four shapes this gate depends on", want.file)
			continue
		}
		if !listHasConcept(got, want.concept) {
			t.Errorf("the extractor did not resolve %s in %s (got %v) -- one of the four resolution shapes stopped working",
				want.concept, want.file, got)
		}
	}
}

// ---- extraction --------------------------------------------------------

// portalFile is one comment-stripped portal source file.
type portalFile struct {
	path string
	src  string
}

type subscriptionSite struct {
	file     string
	line     int
	expr     string   // the concept expression as written, for diagnostics
	concepts []string // resolved; empty means unresolved
	verbs    []string
}

var (
	constDeclRe = regexp.MustCompile(`(?m)^\s*(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*"(v1:[^"]+)"`)
	// A Record whose entries are concept ids or constants naming them.
	recordDeclRe   = regexp.MustCompile(`(?s)(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::\s*Record<[^>]*>)?\s*=\s*\{(.*?)\n\}`)
	localFromMapRe = regexp.MustCompile(`const\s+%s\s*=\s*([A-Za-z_$][\w$]*)\s*\[`)
	// TWO SHAPES, because there are two (memql#4539). A surface either calls
	// `subscriptions.subscribeGraph({concept, actions})` directly, or -- since
	// the fold moved into the SDK -- declares the same two fields in a
	// `useLive(...)` collection spec. The spec object is what the SDK hands to
	// subscribeGraph, so the concept and the verbs are the same expressions in
	// the same shape and the resolver below is unchanged.
	//
	// The optional `<...>` is the type argument every call site carries
	// (`useLive<Row>(`). Without it this matched nothing after the sweep and
	// the coverage test is what said so.
	subscribeRe  = regexp.MustCompile(`(?:subscribeGraph|useLive)(?:<[^>()]*>)?\s*\(`)
	conceptArgRe = regexp.MustCompile(`(?s)\bconcept\s*:\s*([^,\n}]+)`)
	actionsArgRe = regexp.MustCompile(`(?s)\bactions\s*:\s*\[([^\]]*)\]`)
	stringLitRe  = regexp.MustCompile(`"([a-z]+)"`)
	// `export function useX(`, `function useX(`, `export const useX = (`,
	// `const useX = (` -- the four shapes a portal hook is declared in.
	funcDeclRe = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:function\s+([A-Za-z_$][\w$]*)\s*\(|const\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?\()`)
)

// portalSubscriptionSites walks the portal source and returns every
// subscribeGraph call site with its concept ids resolved as far as they can
// be, plus the constant table it built (returned so the caller can assert
// the extractor did any work at all).
func portalSubscriptionSites(t *testing.T) ([]subscriptionSite, map[string]string) {
	t.Helper()

	var files []portalFile
	err := filepath.Walk(portalSrcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Every dot-prefixed directory, which covers `.claude` by
			// construction -- see walkerExemptions in
			// repo_walkers_share_one_skiplist_test.go.
			if strings.HasPrefix(info.Name(), ".") && info.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files = append(files, portalFile{path: filepath.ToSlash(path), src: stripComments(string(b))})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", portalSrcDir, err)
	}

	// Pass 1 -- the constant table, portal-wide. Imports are not resolved:
	// a name is a name, and two files declaring the same constant name with
	// different values would be a portal bug of its own.
	consts := map[string]string{}
	for _, f := range files {
		for _, m := range constDeclRe.FindAllStringSubmatch(f.src, -1) {
			consts[m[1]] = m[2]
		}
	}

	// Pass 2 -- record tables, so `MAP[key]` resolves to the set of values.
	records := map[string][]string{}
	for _, f := range files {
		for _, m := range recordDeclRe.FindAllStringSubmatch(f.src, -1) {
			var vals []string
			for _, line := range strings.Split(m[2], "\n") {
				i := strings.Index(line, ":")
				if i < 0 {
					continue
				}
				if v, ok := resolveConceptExpr(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[i+1:]), ",")), consts); ok {
					vals = append(vals, v)
				}
			}
			if len(vals) > 0 {
				records[m[1]] = vals
			}
		}
	}

	// Pass 3 -- the call sites.
	var sites []subscriptionSite
	for _, f := range files {
		for _, loc := range subscribeRe.FindAllStringIndex(f.src, -1) {
			arg, ok := balancedArgs(f.src, loc[1]-1)
			if !ok {
				continue
			}
			cm := conceptArgRe.FindStringSubmatch(arg)
			if cm == nil {
				continue
			}
			expr := strings.TrimSpace(cm[1])

			var verbs []string
			if am := actionsArgRe.FindStringSubmatch(arg); am != nil {
				for _, v := range stringLitRe.FindAllStringSubmatch(am[1], -1) {
					verbs = append(verbs, v[1])
				}
			}
			if len(verbs) == 0 {
				// An omitted `actions` means every verb, so assert all three:
				// assuming fewer would make the gate quieter than the truth.
				verbs = []string{"created", "updated", "deleted"}
			}

			sites = append(sites, subscriptionSite{
				file:     f.path,
				line:     strings.Count(f.src[:loc[0]], "\n") + 1,
				expr:     expr,
				verbs:    verbs,
				concepts: resolveSiteConcepts(expr, f.src, loc[0], consts, records, files),
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		return sites[i].line < sites[j].line
	})
	return sites, consts
}

// resolveSiteConcepts walks the resolution ladder for one call site's
// concept expression. Each rung is a shape the portal actually uses; the
// extractor test above is what keeps that claim honest.
func resolveSiteConcepts(expr, src string, at int, consts map[string]string, records map[string][]string, files []portalFile) []string {
	// 1. A literal, or a module-level constant.
	if v, ok := resolveConceptExpr(expr, consts); ok {
		return []string{v}
	}
	if !isIdentifier(expr) {
		return nil
	}

	// 2. A local `const expr = SOMEMAP[...]`.
	if re, err := regexp.Compile(fmt.Sprintf(localFromMapRe.String(), regexp.QuoteMeta(expr))); err == nil {
		if m := re.FindStringSubmatch(src); m != nil {
			if vals, ok := records[m[1]]; ok {
				return vals
			}
		}
	}

	// 3. A parameter of the enclosing function -- a hook forwarding its
	//    caller's argument. Resolve the CALL SITES instead. This is what
	//    keeps the Home tiles inside the gate without the portal having to
	//    maintain a second list of what it subscribes to.
	name, argIndex, ok := enclosingFunctionParam(src, at, expr)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
	for _, f := range files {
		for _, loc := range callRe.FindAllStringIndex(f.src, -1) {
			args, ok := balancedArgs(f.src, loc[1]-1)
			if !ok {
				continue
			}
			parts := splitTopLevel(args)
			if argIndex >= len(parts) {
				continue
			}
			if v, ok := resolveConceptExpr(strings.TrimSpace(parts[argIndex]), consts); ok && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out
}

// enclosingFunctionParam finds the function declaration nearest above `at`
// and reports the position of `param` in its parameter list.
func enclosingFunctionParam(src string, at int, param string) (name string, index int, ok bool) {
	locs := funcDeclRe.FindAllStringSubmatchIndex(src[:at], -1)
	if len(locs) == 0 {
		return "", 0, false
	}
	last := locs[len(locs)-1]
	m := funcDeclRe.FindStringSubmatch(src[last[0]:last[1]])
	name = m[1]
	if name == "" {
		name = m[2]
	}
	if name == "" {
		return "", 0, false
	}
	params, ok := balancedArgs(src, strings.LastIndex(src[:last[1]], "("))
	if !ok {
		return "", 0, false
	}
	for i, p := range splitTopLevel(params) {
		// `conceptId: string`, `conceptId`, `conceptId = x` all name the
		// parameter first.
		field := strings.TrimSpace(p)
		for _, cut := range []string{":", "="} {
			if j := strings.Index(field, cut); j >= 0 {
				field = strings.TrimSpace(field[:j])
			}
		}
		if field == param {
			return name, i, true
		}
	}
	return "", 0, false
}

func resolveConceptExpr(expr string, consts map[string]string) (string, bool) {
	expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(expr), ","))
	if len(expr) >= 2 && expr[0] == '"' && expr[len(expr)-1] == '"' {
		inner := expr[1 : len(expr)-1]
		if strings.HasPrefix(inner, "v1:") {
			return inner, true
		}
		return "", false
	}
	if v, ok := consts[expr]; ok {
		return v, true
	}
	return "", false
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// balancedArgs returns the text between the paren at `open` and its match.
func balancedArgs(s string, open int) (string, bool) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits on commas that are not nested inside brackets.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// stripComments removes // and /* */ comments while respecting string and
// template literals.
//
// Naive comment stripping is what made the FIRST version of this extractor
// report a subscription in ClusterProvider.tsx, which has none -- the file
// mentions `undefined.subscribeGraph()` in prose. A gate reporting a call
// site that does not exist is worse than one missing a real one: the reader
// goes looking for a bug in a file that never had one.
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					b.WriteByte('\n')
				}
				i++
			}
			i++
		case c == '"' || c == '\'' || c == '`':
			quote := c
			b.WriteByte(c)
			i++
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					b.WriteByte(src[i])
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(src[i])
				if src[i] == quote {
					break
				}
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func listHasConcept(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
