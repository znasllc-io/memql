package dsl

import (
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// itoa is a small wrapper to make exemption-map line refs more readable.
func itoa(i int) string { return strconv.Itoa(i) }

// TestFilterSyntaxCanonical asserts that every filter clause in the
// tree references payload fields via `payload.X` (or `?.payload.X`
// for arg-conditional predicates), never via `<conceptName>.X` or
// `?.<conceptName>.X`.
//
// Background: prior to the 2026-05 cleanup, filter clauses mixed
// five syntactic forms for the same operation -- payload.X,
// <conceptName>.X, ?.<conceptName>.X, ?.X, and trait/spec calls.
// The decision recorded in feature/dsl-improvements: payload.X is
// the only legal prefix for payload fields; intrinsics (id, concept,
// createdAt, createdBy, partition, type, schema) stay bare; the ?.
// prefix is preserved wherever it carries arg-conditional semantics
// but only over payload.X or a bare intrinsic, never over a
// concept-name alias.
//
// This test parses each .memql file line-structure-only (no full
// parser), extracts filter clauses, and rejects any predicate whose
// LHS starts with `<conceptName>.` or `?.<conceptName>.` where
// <conceptName> is not "payload" / "args" / "actor" / "caller" or
// one of the row intrinsics.
func TestFilterSyntaxCanonical(t *testing.T) {
	intrinsics := map[string]bool{
		"id": true, "concept": true, "createdAt": true,
		"createdBy": true, "partition": true, "type": true,
		"schema": true, "payload": true,
		// reserved engine-side names that may appear bare on the LHS
		"args": true, "actor": true, "caller": true, "now": true,
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
		// Heads like "traitIsActiveRecord" (no `.` after) are spec
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
//	traitIsActiveRecord  ⇔ payload.active == true
//	traitIsNotDeleted    ⇔ payload.deleted != true / payload.deleted == false
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
		{regexp.MustCompile(`payload\.active\s*==\s*true\b`), "traitIsActiveRecord"},
		{regexp.MustCompile(`payload\.active\s*!=\s*false\b`), "traitIsActiveRecord"},
		{regexp.MustCompile(`payload\.deleted\s*==\s*false\b`), "traitIsNotDeleted"},
		{regexp.MustCompile(`payload\.deleted\s*!=\s*true\b`), "traitIsNotDeleted"},
		{regexp.MustCompile(`payload\.status\s*==\s*"active"`), "traitStatusIsActive"},
		{regexp.MustCompile(`payload\.status\s*==\s*"archived"`), "traitStatusIsArchived"},
		{regexp.MustCompile(`payload\.status\s*==\s*"saved"`), "traitStatusIsSaved"},
		{regexp.MustCompile(`payload\.status\s*==\s*"pending"`), "traitStatusIsPending"},
		{regexp.MustCompile(`payload\.status\s*==\s*"running"`), "traitStatusIsRunning"},
		{regexp.MustCompile(`payload\.status\s*==\s*"cancelled"`), "traitIsCancelled"},
		{regexp.MustCompile(`payload\.status\s*==\s*"completed"`), "traitIsCompleted"},
		{regexp.MustCompile(`payload\.status\s*==\s*"inProgress"`), "traitIsInProgress"},
		{regexp.MustCompile(`payload\.status\s*==\s*"open"`), "traitIsOpen"},
		{regexp.MustCompile(`payload\.status\s*==\s*"scheduled"`), "traitIsScheduled"},
		{regexp.MustCompile(`payload\.status\s*!=\s*"archived"`), "traitIsNotArchived"},
		{regexp.MustCompile(`payload\.identityType\s*==\s*"api_key"`), "traitIdentityIsApiKey"},
		{regexp.MustCompile(`payload\.identityType\s*==\s*"worker_token"`), "traitIdentityIsWorkerToken"},
		{regexp.MustCompile(`payload\.deletionScheduledAt\s*!=\s*""`), "traitIsDeletionScheduled"},
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
// (`docs/core/identifiers.md`); the shortId should be the bare unique
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

// TestPerRowAuthzClassification scans every query / mutation in the
// tree and classifies it into one of four buckets:
//
//   - owned:   filter references `actor.userId` (caller-scoped read)
//              or mutation insert/update stamps `ownerUserId`/`userId`
//              from `actor.userId` (caller-scoped write)
//   - admin:   filter or body references `actor.isClusterOwner` or
//              the equivalent `requiresClusterOwner` spec call
//   - public:  carries the `@public` annotation
//   - flagged: none of the above AND the construct references a
//              user-scope field (`payload.ownerUserId`,
//              `payload.userId`, etc.) -- a candidate for caller-
//              scoping that the author forgot
//   - other:   none of the above, no user-scope field referenced
//              (e.g. concept catalogs, cluster topology, system
//              metadata reads)
//
// The test is INFORMATIONAL today -- it logs aggregate counts and a
// per-flagged-construct list so the per-domain follow-up PRs (one
// per domain, per issue #54) can be driven from the output. It does
// NOT fail the build until every domain has been classified and gap-
// closed; flipping to hard-fail lands as the last step of #54.
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
				strings.Contains(body, "requiresClusterOwner") ||
				strings.Contains(body, "caller.isClusterOwner")
			hasOwner := strings.Contains(body, "actor.userId") ||
				strings.Contains(body, "caller.userId")
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
	t.Logf("\n=== Per-row authz classification (informational; see docs/auth/per-row-authz-audit.md) ===")
	t.Logf("%-15s %5s %5s %5s %5s %5s",
		"domain", "owned", "admin", "public", "FLAG", "other")
	for _, d := range domains {
		c := byDomain[d]
		t.Logf("%-15s %5d %5d %5d %5d %5d",
			d, c.owned, c.admin, c.public, c.flagged, c.other)
	}
	t.Logf("")
	t.Logf("Flagged constructs (%d) -- candidates for caller-scope gating:", len(flagged))
	for _, f := range flagged {
		t.Logf("  %s:%d  %s %s", f.file, f.line, f.kind, f.name)
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
//     whose body runs across `;`-separated predicates on the same
//     line and indented continuation lines, terminating on a known
//     end keyword / annotation / blank line.
//
//  2. Procedural-form: a `shape(concept;` call inside a `func`
//     body. Predicates are `;`-separated between `concept;` and
//     the closing `,` before the shape name argument.
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

func splitPredicates(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// splitFilterRef peels the leading identifier (and optional `?.`
// prefix) off a predicate. Returns (head, rest). For
// `?.user.role==args.role` returns ("user", ".role==args.role").
// For `traitIsActiveRecord` returns ("traitIsActiveRecord", "").
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

// Compile-time guarantee that fs is referenced.
var _ fs.FS = (fs.FS)(nil)
