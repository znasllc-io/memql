package dsl

import (
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/pagination"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// itoa is a small wrapper to make exemption-map line refs more readable.
func itoa(i int) string { return strconv.Itoa(i) }

// TestFilterSyntaxCanonical asserts that every filter clause in the
// tree references payload fields via `payload.X`, never via
// `<conceptName>.X` or `?.<conceptName>.X`.
//
// Background: prior to the 2026-05 cleanup, filter clauses mixed
// five syntactic forms for the same operation -- payload.X,
// <conceptName>.X, ?.<conceptName>.X, ?.X, and trait/spec calls.
// The decision recorded in feature/dsl-improvements: payload.X is
// the only legal prefix for payload fields; intrinsics (id, concept,
// createdAt, createdBy, partition, type, schema) stay bare. The `?.`
// optional-chain prefix is itself retired tree-wide (#977; rejected
// by TestNoRetiredOperatorForms) -- arg-conditional predicates now use
// the `when(args.x) { <expr> }` guard. This test still rejects any
// `?.<conceptName>.` LHS so residual `?.` artifacts can't sneak a
// concept-name alias back in.
//
// This test parses each .memql file line-structure-only (no full
// parser), extracts filter clauses, and rejects any predicate whose
// LHS starts with `<conceptName>.` or `?.<conceptName>.` where
// <conceptName> is not "payload" / "args" / "actor" or one of the
// row intrinsics. (`caller` is retired in #221 -- the parser
// rejects the accessor; TestNoCallerVocabulary catches author drift
// earlier with a clear file:line.)
func TestFilterSyntaxCanonical(t *testing.T) {
	intrinsics := map[string]bool{
		"id": true, "concept": true, "createdAt": true,
		"createdBy": true, "partition": true, "type": true,
		"schema": true, "payload": true,
		// reserved engine-side names that may appear bare on the LHS
		"args": true, "actor": true, "now": true,
		"config": true, "trace": true,
	}

	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		// ?.<head>(.<rest>)? or <head>(.<rest>)?
		head, _ := splitFilterRef(pred)
		if head == "" {
			return
		}
		if intrinsics[head] {
			return
		}
		// Heads like "isActiveRecord" (no `.` after) are spec
		// calls, not field refs. Only flag if the predicate has a
		// `.` after the head (so it's <head>.<field>) or an operator
		// that proves it's a comparison.
		if !strings.Contains(pred, ".") && !hasFilterOperator(pred) {
			return
		}
		violations = append(violations, violation{file, lineno, pred})
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates using non-canonical prefix (must be payload.X or bare intrinsic):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.file, v.line, v.text)
		}
	}
}

// TestNoInlineTraitablePredicates asserts that no filter clause
// inlines a payload comparison that an existing trait spec covers.
// Today's traits in dsl/common/traits.memql:
//
//	isActiveRecord  ⇔ payload.active == true
//	isNotDeleted    ⇔ payload.deleted != true / payload.deleted == false
//	traitIsArchived      ⇔ payload.archived == true / payload.archivedAt != null
//	traitIsSaved         ⇔ payload.saved == true
//
// Authors must call the trait, not inline the comparison, so the
// definition of "active" / "deleted" / etc. lives in one place.
// Concept-specific predicates (payload.ownerUserId==args.userId)
// remain legal inline -- only the traitable predicates are
// rejected here.
func TestNoInlineTraitablePredicates(t *testing.T) {
	// Each pattern: matcher regex + suggested trait
	type rule struct {
		re   *regexp.Regexp
		hint string
	}
	rules := []rule{
		{regexp.MustCompile(`payload\.active\s*==\s*true\b`), "isActiveRecord"},
		{regexp.MustCompile(`payload\.active\s*!=\s*false\b`), "isActiveRecord"},
		{regexp.MustCompile(`payload\.deleted\s*==\s*false\b`), "isNotDeleted"},
		{regexp.MustCompile(`payload\.deleted\s*!=\s*true\b`), "isNotDeleted"},
		{regexp.MustCompile(`payload\.status\s*==\s*"active"`), "statusIsActive"},
		{regexp.MustCompile(`payload\.status\s*==\s*"archived"`), "statusIsArchived"},
		{regexp.MustCompile(`payload\.status\s*==\s*"saved"`), "statusIsSaved"},
		{regexp.MustCompile(`payload\.status\s*==\s*"pending"`), "statusIsPending"},
		{regexp.MustCompile(`payload\.status\s*==\s*"running"`), "statusIsRunning"},
		{regexp.MustCompile(`payload\.status\s*==\s*"cancelled"`), "isCancelled"},
		{regexp.MustCompile(`payload\.status\s*==\s*"completed"`), "isCompleted"},
		{regexp.MustCompile(`payload\.status\s*==\s*"inProgress"`), "isInProgress"},
		{regexp.MustCompile(`payload\.status\s*==\s*"open"`), "isOpen"},
		{regexp.MustCompile(`payload\.status\s*==\s*"scheduled"`), "isScheduled"},
		{regexp.MustCompile(`payload\.status\s*!=\s*"archived"`), "isNotArchived"},
		{regexp.MustCompile(`payload\.identityType\s*==\s*"api_key"`), "identityIsApiKey"},
		{regexp.MustCompile(`payload\.identityType\s*==\s*"worker_token"`), "identityIsWorkerToken"},
		{regexp.MustCompile(`payload\.deletionScheduledAt\s*!=\s*""`), "isDeletionScheduled"},
	}

	type violation struct {
		file string
		line int
		text string
		hint string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		for _, r := range rules {
			if r.re.MatchString(pred) {
				violations = append(violations, violation{file, lineno, pred, r.hint})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("found %d filter predicates that should use a trait spec:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s   → use %s", v.file, v.line, v.text, v.hint)
		}
	}
}

// TestNoShortIdConceptPrefix asserts that no .memql file constructs
// a node-id shortId with a concept-name (or sub-type discriminator)
// prefix. The canonical id format is `{partition}:{concept}:{shortId}`
// (`docs/public/concepts/identifiers.md`); the shortId should be the bare unique
// part (uuid / hash / slug), never `concat("<conceptName>-", ...)`
// or any equivalent string-concatenation pattern.
//
// Background (issue #53): pre-fix, the tree had `concat("ga-",
// hash(email))` for General Assistant agentIds and `concat("session-",
// id)` for sessionIds. Both duplicate information already in the
// canonical position of the id (concept name) or in a payload field
// (e.g. the agent's `role="assistant"` discriminator). The cleanup
// stripped the prefixes and moved the discrimination, where needed,
// into payload fields.
//
// The test fails on any new occurrence of `concat("<knownPrefix>-",
// ...)` in a .memql file. New names should be added to the list when
// they're identified as anti-patterns.
func TestNoShortIdConceptPrefix(t *testing.T) {
	bannedPrefixes := []string{
		// concept-name prefixes
		"agent-", "user-", "session-", "role-", "space-", "plan-",
		"task-", "delegation-", "partition-",
		// sub-type discriminators (move into payload fields instead)
		"ga-", "specialist-",
		// legacy identity-side prefixes (now redundant with `:user:` /
		// `:identity:` concept names + the polymorphic concept's
		// `identityType` discriminator field)
		"pat-", "wkr-", "wpc-", "sess-", "ml-", "ar-", "ac-",
	}
	type violation struct {
		file   string
		line   int
		text   string
		prefix string
	}
	var violations []violation
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	// Known exemptions tied to follow-up issues. Lines listed here use
	// the prefix legitimately for partition-naming (the partition row
	// stores per-user partition state); these go away wholesale with
	// the partitioning removal in issue #56. Until then they're an
	// acknowledged hole, not a regression.
	exemptions := map[string]bool{
		"identity/logic.memql:412": true, // partition name (issue #56 will remove the partition concept entirely)
		"identity/logic.memql:426": true, // partition lookup-by-name (same; issue #56)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for lineno, line := range strings.Split(string(raw), "\n") {
			// Skip line comments + block-comment spans (line-level only).
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, prefix := range bannedPrefixes {
				needle := `concat("` + prefix
				if strings.Contains(line, needle) {
					locKey := p + ":" + itoa(lineno+1)
					if exemptions[locKey] {
						continue
					}
					violations = append(violations, violation{
						file:   p,
						line:   lineno + 1,
						text:   strings.TrimSpace(line),
						prefix: prefix,
					})
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("found %d shortId-prefix anti-patterns (issue #53):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %q in: %s", v.file, v.line, v.prefix, v.text)
		}
		t.Logf("\nThe canonical id format is `{partition}:{concept}:{shortId}`; the\nshortId should be the bare unique part (uuid/hash/slug), never\nprefixed with the concept name or a sub-type discriminator. If the\nprefix is a real discriminator (e.g. 'ga-' vs specialists), move it\ninto a payload field (e.g. agent.role='assistant').")
	}
}

// TestRelationshipTargetsUseImports pins memql#1067: a concept's
// @relationship target must reference an imported concept by its bare name
// (resolved through a file-top `use <ns>.concepts.{ name }` import, or from
// the owning file for same-namespace targets) -- NOT a hardcoded canonical-ID
// string. The canonical-string form (`target="v1:..."`) is retired; this gate
// hard-fails any reintroduction so relationships stay on the same import model
// as mutations/queries/shapes. The engine resolves the bare name to the
// canonical id at load time (component/memql.LoadUnifiedConcepts).
func TestRelationshipTargetsUseImports(t *testing.T) {
	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for lineno, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !strings.Contains(line, "@relationship") {
				continue
			}
			// Retired form: a quoted canonical-ID target (target="v1:...").
			if strings.Contains(line, `target="`) {
				violations = append(violations, violation{
					file: p,
					line: lineno + 1,
					text: strings.TrimSpace(line),
				})
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("found %d @relationship target(s) using the retired canonical-ID string form (memql#1067):", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d  %s", v.file, v.line, v.text)
		}
		t.Logf("\nUse the bare imported concept name instead of a canonical-ID string:\n" +
			"  use identity.concepts.{ user }\n" +
			"  @relationship(type=\"parent\", field=\"ownerUserId\", target=user, direction=\"outgoing\")\n" +
			"Same-namespace targets resolve from the owning file (no import needed).")
	}
}

// TestPerRowAuthzClassification scans every query / mutation in the
// tree and classifies it into one of four buckets:
//
//   - owned:   filter references `actor.userId` (caller-scoped read)
//     or mutation insert/update stamps `ownerUserId`/`userId`
//     from `actor.userId` (caller-scoped write)
//   - admin:   filter or body references `actor.isClusterOwner` or
//     the equivalent `requiresClusterOwner` spec call
//   - public:  carries the `@public` annotation
//   - flagged: none of the above AND the construct references a
//     user-scope field (`payload.ownerUserId`,
//     `payload.userId`, etc.) -- a candidate for caller-
//     scoping that the author forgot
//   - other:   none of the above, no user-scope field referenced
//     (e.g. concept catalogs, cluster topology, system
//     metadata reads)
//
// The test HARD-FAILS on any flagged construct -- the per-domain
// follow-up PRs for #54 closed every existing gap, and any new
// user-scope read/write needs to either include an actor-check
// (`actor.userId` reference, or an `isClusterOwner` admin gate)
// or carry an explicit `@public` annotation acknowledging the
// intent.
func TestPerRowAuthzClassification(t *testing.T) {
	type counts struct {
		owned   int
		admin   int
		public  int
		flagged int
		other   int
	}
	byDomain := map[string]*counts{}
	type flag struct {
		file string
		line int
		name string
		kind string
	}
	var flagged []flag

	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	headerRe := regexp.MustCompile(`(?m)^[ \t]*(query|mutation)[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)
		matches := headerRe.FindAllStringSubmatchIndex(src, -1)
		domain := strings.SplitN(p, "/", 2)[0]
		if byDomain[domain] == nil {
			byDomain[domain] = &counts{}
		}
		for _, m := range matches {
			openIdx := m[1] - 1
			closeIdx := matchingClose(src, openIdx)
			if closeIdx < 0 {
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
				if line == "" {
					break
				}
				break
			}
			preamble := src[preambleStart:m[0]]
			body := src[m[1]:closeIdx]
			kind := src[m[2]:m[3]]
			name := src[m[4]:m[5]]

			hasPublic := strings.Contains(preamble, "@public")
			hasAdmin := strings.Contains(body, "actor.isClusterOwner") ||
				strings.Contains(body, "requiresClusterOwner")
			hasOwner := strings.Contains(body, "actor.userId")
			referencesUserScope := strings.Contains(body, "payload.ownerUserId") ||
				strings.Contains(body, "payload.userId") ||
				strings.Contains(body, "payload.actorUserId") ||
				strings.Contains(body, "payload.targetId") ||
				strings.Contains(body, "payload.createdBy")
			lineNo := strings.Count(src[:m[0]], "\n") + 1

			switch {
			case hasPublic:
				byDomain[domain].public++
			case hasOwner:
				byDomain[domain].owned++
			case hasAdmin:
				byDomain[domain].admin++
			case referencesUserScope:
				byDomain[domain].flagged++
				flagged = append(flagged, flag{p, lineNo, name, kind})
			default:
				byDomain[domain].other++
			}
		}
	}

	var domains []string
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	t.Logf("\n=== Per-row authz classification (informational; see docs/public/operate/auth/per-row-authz-audit.md) ===")
	t.Logf("%-15s %5s %5s %5s %5s %5s",
		"domain", "owned", "admin", "public", "FLAG", "other")
	for _, d := range domains {
		c := byDomain[d]
		t.Logf("%-15s %5d %5d %5d %5d %5d",
			d, c.owned, c.admin, c.public, c.flagged, c.other)
	}
	t.Logf("")
	if len(flagged) > 0 {
		t.Errorf("found %d flagged constructs that reference user-scope fields without a caller-check or @public annotation:", len(flagged))
		for _, f := range flagged {
			t.Errorf("  %s:%d  %s %s", f.file, f.line, f.kind, f.name)
		}
		t.Logf("\nResolution options:\n" +
			"  (1) add a caller-scope filter: `args.X == actor.userId` (or the canonical caller-id check for the domain)\n" +
			"  (2) add an admin gate: reference `actor.isClusterOwner` or a `requiresClusterOwner` spec\n" +
			"  (3) add `@public` to the construct's annotations with a comment explaining why no caller-check applies\n" +
			"See docs/public/operate/auth/per-row-authz-audit.md for the bucket definitions + the audit history.")
	}
}

// matchingClose walks src from openIdx (position of `{`) and returns
// the index of the matching `}`. String + line-comment aware.
func matchingClose(src string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(src) || src[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if inString {
			if c == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				nl := strings.IndexByte(src[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// visitFilterPredicates walks every .memql file in the unified tree,
// extracts filter-clause lines, splits on `;`, and invokes f for
// each predicate. Files under _reference/ are skipped -- they are
// documentation, not loaded.
func visitFilterPredicates(t *testing.T, f func(file string, lineno int, pred string)) {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		walkFilterPredicates(p, string(raw), f)
	}
}

// walkFilterPredicates scans src line-by-line.
//
// Two contexts emit predicates:
//
//  1. Struct-form: a line beginning with `filter ` opens a clause
//     whose body runs across `&&`-joined predicates (the canonical
//     AND operator, #977; the `;` AND separator is retired and
//     rejected tree-wide by TestNoRetiredOperatorForms) on the same
//     line and indented continuation lines, terminating on a known
//     end keyword / annotation / blank line.
//
//  2. Procedural-form: a legacy `shape(concept;` call inside a `func`
//     body (the author-side procedural form is retired -- this branch
//     only matches any residual artifact). Predicates there are
//     `;`-separated between `concept;` and the closing `,` before the
//     shape name argument.
//
// @filter(...) annotations on automations are intentionally NOT
// walked: that annotation uses a different (event-trigger)
// evaluator that doesn't recognize trait spec calls, so the same
// rules don't apply.
func walkFilterPredicates(path, src string, emit func(file string, lineno int, pred string)) {
	inFilter := false
	inShapeCall := false
	for lineno, raw := range strings.Split(src, "\n") {
		line := raw
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		trim := strings.TrimSpace(line)

		// Procedural-form `shape(` body — emit each ;-piece until
		// we see the closing `,` + shape name + `)`.
		if inShapeCall {
			if strings.Contains(trim, ")") {
				inShapeCall = false
			}
			for _, p := range splitPredicates(strim(trim, ',')) {
				p = strings.TrimSpace(p)
				if p == "" || p == "concept" {
					continue
				}
				// drop the trailing shape-name string arg if it's on this line
				if strings.HasPrefix(p, "\"") {
					continue
				}
				emit(path, lineno+1, p)
			}
			continue
		}
		if m := procShapeCallStart(line); m {
			inShapeCall = !strings.Contains(trim, ")")
			continue
		}

		if trim == "" {
			inFilter = false
			continue
		}
		if strings.HasPrefix(trim, "filter ") || strings.HasPrefix(trim, "filter\t") {
			inFilter = true
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
			for _, p := range splitPredicates(rest) {
				if p != "" {
					emit(path, lineno+1, p)
				}
			}
			continue
		}
		if !inFilter {
			continue
		}
		end := false
		for _, kw := range []string{"shape", "insert", "update", "return", "concept", "args", "use", "}", ")"} {
			if strings.HasPrefix(trim, kw+" ") || trim == kw || strings.HasPrefix(trim, kw+"\t") {
				end = true
				break
			}
		}
		if strings.HasPrefix(trim, "@") {
			end = true
		}
		if end {
			inFilter = false
			continue
		}
		for _, p := range splitPredicates(trim) {
			if p != "" {
				emit(path, lineno+1, p)
			}
		}
	}
}

// procShapeCallStart returns true if the line opens a procedural-
// form `shape(concept;` call. We look for the `shape(` token
// followed (possibly across whitespace) by `concept;`.
func procShapeCallStart(line string) bool {
	idx := strings.Index(line, "shape(")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(line[idx+len("shape("):])
	return strings.HasPrefix(rest, "concept;") || rest == "concept"
}

// strim trims the trailing rune `r` off a string if present.
// Mirrors strings.TrimRight for a single byte.
func strim(s string, r byte) string {
	for len(s) > 0 && s[len(s)-1] == r {
		s = s[:len(s)-1]
	}
	return s
}

// splitPredicates splits a filter clause into its AND-ed predicates on the
// canonical `&&` operator (#977), respecting paren/brace/string nesting so the
// `&&` inside a `when(args.x) { a && b }` guard block is not split. Each
// `when(args.x) { <inner> }` guard is unwrapped to its inner predicate so the
// canonical-prefix / traitable checks apply to the real comparison.
func splitPredicates(s string) []string {
	var raw []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case depth == 0 && c == '&' && i+1 < len(s) && s[i+1] == '&':
			raw = append(raw, s[start:i])
			i++
			start = i + 1
		}
	}
	raw = append(raw, s[start:])

	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, unwrapWhenPredicate(p))
	}
	return out
}

// unwrapWhenPredicate returns the inner predicate of a `when(args.x) { <inner> }`
// guard, or the predicate unchanged when it is not a guard.
func unwrapWhenPredicate(p string) string {
	if !strings.HasPrefix(p, "when(") {
		return p
	}
	open := strings.Index(p, "{")
	closeIdx := strings.LastIndex(p, "}")
	if open < 0 || closeIdx <= open {
		return p
	}
	return strings.TrimSpace(p[open+1 : closeIdx])
}

// splitFilterRef peels the leading identifier (and optional `?.`
// prefix) off a predicate. Returns (head, rest). For
// `?.user.role==args.role` returns ("user", ".role==args.role").
// For `isActiveRecord` returns ("isActiveRecord", "").
// For `id==args.userId` returns ("id", "==args.userId").
func splitFilterRef(pred string) (string, string) {
	s := pred
	if strings.HasPrefix(s, "?.") {
		s = s[2:]
	}
	end := 0
	for end < len(s) {
		c := s[end]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && end > 0)) {
			break
		}
		end++
	}
	if end == 0 {
		return "", ""
	}
	return s[:end], s[end:]
}

func hasFilterOperator(pred string) bool {
	for _, op := range []string{"==", "!=", "<=", ">=", " has ", " in ", " not "} {
		if strings.Contains(pred, op) {
			return true
		}
	}
	for _, c := range pred {
		if c == '<' || c == '>' {
			return true
		}
	}
	return false
}

// TestNoCallerVocabulary is the #221 conformance guardrail: every
// loaded .memql file must use the canonical `actor.X` auth-context
// accessor and the `@actor` shape kind annotation. The retired
// `caller.X` / `@caller` spellings are rejected by both parsers, so
// any drift would already break loading -- but a static-grep test
// surfaces the violation with a clean file:line and a migration
// hint rather than relying on log-scraping the load failure.
//
// _reference/ files are documentation templates not loaded by the
// engine; this test treats them as out-of-scope (matches the
// existing per-row-authz classification test's exclusion).
func TestNoCallerVocabulary(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	// Match the accessor or annotation, not the English word "caller"
	// in comments / @description text. We surface only the structural
	// forms that survive into the parsed body: `caller.<ident>` (any
	// position) and `@caller` (start of line / after whitespace).
	accessorRe := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])caller\.[A-Za-z_]`)
	annotationRe := regexp.MustCompile(`(?m)^[ \t]*@caller\b`)

	type hit struct {
		file string
		line int
		form string
	}
	var hits []hit
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		// Strip /* */ block comments and // line comments so an
		// English `caller.` inside a @description literal or a doc
		// header doesn't trip the check. We do this at line granularity
		// since the DSL doesn't use /* */ blocks; line stripping is
		// enough.
		for i, line := range strings.Split(string(raw), "\n") {
			stripped := stripLineComment(line)
			if accessorRe.MatchString(stripped) {
				hits = append(hits, hit{p, i + 1, "caller.X"})
			}
			if annotationRe.MatchString(stripped) {
				hits = append(hits, hit{p, i + 1, "@caller"})
			}
		}
	}
	if len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].file != hits[j].file {
				return hits[i].file < hits[j].file
			}
			return hits[i].line < hits[j].line
		})
		var sb strings.Builder
		sb.WriteString("epic #218 / #221: caller. and @caller are retired -- use actor. and @actor.\n")
		for _, h := range hits {
			sb.WriteString("  " + h.file + ":" + itoa(h.line) + " " + h.form + "\n")
		}
		t.Errorf("%s", sb.String())
	}
}

func stripLineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' && (i == 0 || line[i-1] != '\\') {
			inStr = !inStr
			continue
		}
		if !inStr && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			return line[:i]
		}
	}
	return line
}

// TestQueryConceptMatchesShapeConcept asserts that every struct-form query
// is bound (in its `query <Concept> <name>` signature) to the SAME concept as
// the `@row` shape it projects.
//
// Background (memql#1634): a query's signature concept names the table the
// engine SCANS; the shape only projects fields. When the two disagree the
// query scans the wrong concept and returns [] for rows that were written
// under the other concept -- silently, with no load-time or parse-time error.
// The 2026-05 signature-bound-concept migration (commit f7b6ab3) SWAPPED the
// bindings for two planner queries: taskStateById became `query plan`
// (reads taskState via taskStateFull) and tasksForPlan became
// `query taskState` (reads task via taskFull). Both returned [] right after
// successful creates. This invariant catches that class at test time.
//
// Only `@row` (concept-bound) shapes are checked. `@actor`-only shapes carry
// no concept in their signature (single identifier after `shape`) and are
// skipped -- a query never scans an actor envelope.
func TestQueryConceptMatchesShapeConcept(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	readFile := func(p string) string {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		return string(raw)
	}

	// `shape <Concept> <name> {` -> concept-bound (@row / mixed). The
	// single-identifier `shape <name> {` form is @actor-only (no concept).
	shapeDeclRe := regexp.MustCompile(`^\s*shape\s+(\w+)\s+(\w+)\s*\{`)
	// `query <Concept> <name> {`
	queryDeclRe := regexp.MustCompile(`^\s*query\s+(\w+)\s+(\w+)\s*\{`)
	// `shape <shapeName>` clause inside a query body.
	shapeClauseRe := regexp.MustCompile(`^\s*shape\s+(\w+)\s*$`)

	// Exempted (query -> shape) pairs: a shape deliberately REUSED across two
	// sibling concepts with an identical field layout. The query's scan
	// concept is correct (a mutation writes rows under it); only the projection
	// borrows a shape whose signature names the other sibling. This is NOT the
	// #1634 wrong-scan bug. variableFull projects {key,value,...} shared by
	// platform's globalVariable + partitionVariable.
	sharedShapeExempt := map[string]string{
		"globalVariable":  "variableFull",
		"globalVariables": "variableFull",
	}

	// Pass 1: global shapeName -> concept (concept-bound shapes only).
	shapeConcept := map[string]string{}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") || !strings.HasSuffix(p, "shapes.memql") {
			continue
		}
		for _, line := range strings.Split(readFile(p), "\n") {
			if m := shapeDeclRe.FindStringSubmatch(stripLineComment(line)); m != nil {
				shapeConcept[m[2]] = m[1]
			}
		}
	}

	// Pass 2: walk queries; compare each query's concept to its shape's concept.
	type violation struct {
		file, query, queryConcept, shape, shapeConcept string
	}
	var violations []violation
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") || !strings.HasSuffix(p, "queries.memql") {
			continue
		}
		var curQuery, curConcept string
		for _, raw := range strings.Split(readFile(p), "\n") {
			line := stripLineComment(raw)
			if m := queryDeclRe.FindStringSubmatch(line); m != nil {
				curConcept, curQuery = m[1], m[2]
				continue
			}
			m := shapeClauseRe.FindStringSubmatch(line)
			if m == nil || curQuery == "" {
				continue
			}
			shapeName := m[1]
			sc, ok := shapeConcept[shapeName]
			if !ok {
				// @actor-only shape or unknown -> nothing to compare.
				curQuery = ""
				continue
			}
			if sc != curConcept && sharedShapeExempt[curQuery] != shapeName {
				violations = append(violations, violation{p, curQuery, curConcept, shapeName, sc})
			}
			curQuery = ""
		}
	}

	for _, v := range violations {
		t.Errorf("%s: query %q is bound to concept %q but projects @row shape %q bound to concept %q -- the query will scan %q and return [] for rows written under %q (memql#1634)",
			v.file, v.query, v.queryConcept, v.shape, v.shapeConcept, v.queryConcept, v.shapeConcept)
	}
}

// TestPaginationAuthoringRule is the pagination authoring gate (epic 5,
// issue 5.1 / memql#1965). The rule: every LIST-RETURNING query (a shape
// projecting a row set without a unique-key `id ==` filter) must declare
// how it is bounded -- `paginate` / `sort`, a `count` aggregate, or an
// explicit `@unbounded("reason")` opt-out.
//
// SEQUENCING (owner decision, 2026-06-22): the repo-wide HARD-FAIL was
// COUPLED to the issue 5.3 backfill. 5.3 (memql#1967) marked every list
// query in the tree (`paginate`/`sort` or `@unbounded("reason")`) and, in
// the same merge, flipped this gate from report-only to ENFORCING. So the
// test now HARD-FAILS on any unmarked-list finding -- a freshly-authored
// list query that declares no bound trips it. It asserts three things:
//
//  1. every list-returning query in the tree declares its bound (the
//     repo-wide hard-fail; zero unmarked-list findings allowed),
//  2. the classifier DETECTS a freshly-authored unmarked list query
//     (proves the enforcement mechanism is present and correct), and
//  3. the @unbounded-marked set is internally consistent (every mark
//     carries a non-empty reason).
//
// The audit report (scripts/audit-pagination) reads the SAME classifier,
// so its UNMARKED list and this gate can never drift.
func TestPaginationAuthoringRule(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var findings []pagination.QueryFinding
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") || !strings.HasSuffix(p, "queries.memql") {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		findings = append(findings, pagination.ScanSource(p, string(raw))...)
	}
	pagination.SortFindings(findings)

	byClass := map[pagination.Classification]int{}
	var unmarked []pagination.QueryFinding
	for _, f := range findings {
		byClass[f.Class]++
		if f.Class == pagination.UnmarkedList {
			unmarked = append(unmarked, f)
		}
		// (3) consistency: an @unbounded mark must carry a reason. The
		// rewriter rejects a reason-less @unbounded at load time, so this
		// is a belt-and-suspenders check on the classifier's capture path.
		if f.Class == pagination.UnboundedMarked && f.UnboundedReason == "" {
			t.Errorf("%s:%d query %q is @unbounded but carries no reason -- @unbounded(\"reason\") requires a non-empty justification", f.File, f.Line, f.Name)
		}
	}

	// Classification breakdown (for the test log).
	t.Logf("\n=== Pagination audit (memql#1965; ENFORCING since the 5.3 backfill) ===")
	t.Logf("%-16s %d", "single-row", byClass[pagination.SingleRow])
	t.Logf("%-16s %d", "aggregate", byClass[pagination.Aggregate])
	t.Logf("%-16s %d", "bounded-list", byClass[pagination.BoundedList])
	t.Logf("%-16s %d", "unbounded-marked", byClass[pagination.UnboundedMarked])
	t.Logf("%-16s %d", "unmarked-list", byClass[pagination.UnmarkedList])

	// (1) Repo-wide HARD-FAIL: every list-returning query must declare its
	// bound. A new unmarked list query (no `paginate`/`sort`, no
	// `@unbounded("reason")`) fails here. Backfilled to zero by issue 5.3.
	for _, f := range unmarked {
		t.Errorf("%s:%d query %q is an unmarked list query -- declare a bound: add `sort`/`paginate` "+
			"(a consumer-facing or growable list) or `@unbounded(\"reason\")` (a legitimate full-set read). "+
			"See authoring-rules.md #23; run `go run ./scripts/audit-pagination --unmarked` for the live list.",
			f.File, f.Line, f.Name)
	}

	// (2) Prove the gate's enforcement mechanism: a freshly-authored
	// unmarked list query MUST be detected. The repo-wide assertion above
	// relies on this classifier; the synthetic query locks its correctness
	// in even if the tree ever reached zero list queries.
	newQuery := `query widget queryEveryWidget {
  filter  payload.kind=="gizmo"
  shape   widgetFull
}`
	got := pagination.ScanSource("synthetic/queries.memql", newQuery)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 finding for the synthetic query, got %d", len(got))
	}
	if !got[0].IsUnmarkedList() {
		t.Fatalf("checker FAILED to detect a new unmarked list query: classified as %s, want unmarked-list -- the pagination authoring gate is broken", got[0].Class)
	}
}

// Compile-time guarantee that fs is referenced.
var _ fs.FS = (fs.FS)(nil)
