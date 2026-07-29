package main

// The `row-authz` codemod (memql#2920 Phase 1).
//
// It seeds each concept's `@rowAuthz(...)` declaration from how that
// concept's existing QUERIES already filter, so the tree arrives at
// Phase 2 carrying declared tiers instead of a regex re-inferring one
// per construct on every run.
//
// It is deliberately conservative, and abstention is a first-class
// outcome. A concept is declared only when every query over it that
// carries unambiguous evidence agrees; a concept whose queries
// DISAGREE, or that has no queries at all, is left undeclared. That is
// not a gap -- an undeclared concept is exactly the signal Phase 2's
// shadow mode needs, and a guessed tier would launder a disagreement
// into a false declaration that the measurement then treats as ground
// truth.
//
// The codemod never parses `@rowAuthz` itself: it renders through
// langparser.FormatRowAuthz and the loader reads through
// langparser.ParseRowAuthz, which is the #2621 "one shared detector"
// constraint restated. See component/language/parser/rowauthz_binding.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// queryHeaderRe matches a top-level `query <Concept> <name> {`
// declaration, capturing the bound concept and the construct name.
//
// The bound concept is the whole point here, so unlike the conformance
// test's constructHeaderRe (which makes it a non-capturing optional
// group) this requires it: a query with no signature concept binds
// nothing and cannot be evidence about a concept.
var queryHeaderRe = regexp.MustCompile(`(?m)^[ \t]*query[ \t]+([\p{L}_][\p{L}\p{Nd}_]*)[ \t]+([\p{L}_][\p{L}\p{Nd}_]*)[ \t]*\{`)

// useConceptsRe matches a file-top concepts import,
// `use <ns>.concepts.{ a, b }`, capturing the namespace and the brace
// list.
var useConceptsRe = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([\p{L}_][\p{L}\p{Nd}_:]*)\.concepts\.\{([^}]*)\}`)

// filterClauseRe captures a struct-form `filter` clause. The parser
// rejects a multi-line clause, so one line is the whole clause.
//
// It is only ever run over the comment- and string-blanked view. On
// raw source a trailing `// && ownerUserId==actor.userId` would be
// captured as part of the clause and split into a conjunct by
// topLevelConjuncts, so a COMMENT could manufacture a tier the query
// does not have -- the #2875 "silenced by a sentence" class, running
// in the direction that fabricates a claim rather than suppressing
// one.
var filterClauseRe = regexp.MustCompile(`(?m)^[ \t]*filter[ \t]+(.*)$`)

// Line-anchored, not substring: a comment merely mentioning
// `@public` is prose, and treating it as the annotation is how a gate
// gets silenced by a sentence (the memql#2875 lesson).
var (
	publicAnnotationRe     = regexp.MustCompile(`(?m)^[ \t]*@public\b`)
	serverOnlyAnnotationRe = regexp.MustCompile(`(?m)^[ \t]*@serverOnly\b`)
)

// ownerLeafRe matches a conjunct that scopes rows to the caller:
// `<field>==actor.userId` in either order. The field must be a bare
// payload property (#2292), so a dotted left-hand side is excluded.
var ownerLeafRe = regexp.MustCompile(`^(?:([\p{L}_][\p{L}\p{Nd}_]*)[ \t]*==[ \t]*actor\.userId|actor\.userId[ \t]*==[ \t]*([\p{L}_][\p{L}\p{Nd}_]*))$`)

// adminLeafRe matches a conjunct that gates on the cluster owner,
// either directly or through the shared spec.
var adminLeafRe = regexp.MustCompile(`^(?:actor\.isClusterOwner([ \t]*==[ \t]*true)?|requiresClusterOwner|spec\("requiresClusterOwner"\))$`)

// conceptKey identifies a concept by the domain that declares it plus
// its short name, which is what the rewrite needs in order to edit the
// right `dsl/<domain>/concepts.memql`.
type conceptKey struct {
	Domain string
	Name   string
}

// verdict is what one query says about the concept it binds.
type verdictKind int

const (
	// votes for a tier: its filter guarantees that tier's predicate.
	verdictVote verdictKind = iota
	// blocks any tier: the query reads rows the tier would exclude, so
	// declaring the tier would assert something this query disproves.
	verdictBlocks
	// neither: @serverOnly, which is not a client read at all.
	verdictExempt
)

// vote is one query's verdict about the concept it binds.
type vote struct {
	Kind   verdictKind
	Decl   langparser.RowAuthzDecl // set only when Kind == verdictVote
	Reason string                  // set only when Kind == verdictBlocks
	Source string                  // "<domain>/<file> <constructName>", for the report
}

// rowAuthzInference is the whole-tree result: what each concept should
// declare, plus why the rest were left alone.
type rowAuthzInference struct {
	// Tiers is keyed by domain, then concept name -- the shape
	// RewriteRowAuthz consumes for one file.
	Tiers map[string]map[string]langparser.RowAuthzDecl
	// Abstained records, per concept, why no tier was inferred.
	Abstained map[conceptKey]string
	// Counts is the tier distribution, for the run report.
	Counts map[langparser.RowAuthzTier]int
}

// inferRowAuthz walks a dsl/ tree and works out what each concept
// should declare.
func inferRowAuthz(dslRoot string) (*rowAuthzInference, error) {
	domains, err := os.ReadDir(dslRoot)
	if err != nil {
		return nil, fmt.Errorf("read dsl root %s: %w", dslRoot, err)
	}

	declared := map[conceptKey]bool{} // every concept in the tree
	votes := map[conceptKey][]vote{}  // evidence, per concept
	abstained := map[conceptKey]string{}

	type domainFile struct {
		domain string
		path   string
		src    string
	}
	var files []domainFile

	for _, d := range domains {
		if !d.IsDir() || strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
			// `_`/`.` prefixes are the tree's soft-disable and hidden
			// conventions; the loader skips them and so must this.
			continue
		}
		domain := d.Name()
		entries, readErr := os.ReadDir(filepath.Join(dslRoot, domain))
		if readErr != nil {
			return nil, fmt.Errorf("read domain %s: %w", domain, readErr)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".memql") {
				continue
			}
			path := filepath.Join(dslRoot, domain, e.Name())
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, fmt.Errorf("read %s: %w", path, readErr)
			}
			files = append(files, domainFile{domain: domain, path: path, src: string(raw)})
		}
	}

	// Pass 1: every declared concept, so a vote can be matched to one.
	for _, f := range files {
		if filepath.Base(f.path) != "concepts.memql" {
			continue
		}
		for _, h := range langparser.ConceptHeaders(f.src) {
			declared[conceptKey{Domain: f.domain, Name: h.Name}] = true
		}
	}

	// Pass 2: collect each query's verdict about the concept it binds.
	for _, f := range files {
		imports := conceptImports(f.src)
		// Headers are located, and bodies sliced, on the comment- and
		// string-blanked view. The blanker is length-preserving, so
		// offsets index the raw source unchanged. Scanning raw source
		// let a `{` inside a string or a comment unbalance the brace
		// walk and run one construct's body into the next -- the same
		// locate-on-one-view/slice-on-another class ConceptHeaders
		// avoids (#2948), and here it would mean a query voting with a
		// DIFFERENT construct's filter.
		blanked := langparser.BlankCommentsAndStrings(f.src)
		for _, m := range queryHeaderRe.FindAllStringSubmatchIndex(blanked, -1) {
			conceptName := blanked[m[2]:m[3]]
			constructName := blanked[m[4]:m[5]]

			key := conceptKey{Domain: f.domain, Name: conceptName}
			if ns, ok := imports[conceptName]; ok {
				key.Domain = ns
			}
			if !declared[key] {
				// Binds a concept this tree does not declare (a
				// product-bundle concept, or a namespace this codemod
				// cannot resolve). Not evidence about anything here.
				continue
			}

			body := constructBody(blanked, m[0])
			preamble := constructPreamble(blanked, m[0])
			source := fmt.Sprintf("%s/%s %s", f.domain, filepath.Base(f.path), constructName)

			v := classifyQuery(preamble, body)
			v.Source = source
			votes[key] = append(votes[key], v)
		}
	}

	out := &rowAuthzInference{
		Tiers:     map[string]map[string]langparser.RowAuthzDecl{},
		Abstained: abstained,
		Counts:    map[langparser.RowAuthzTier]int{},
	}

	// A tier is a FLOOR: the predicate that will eventually be AND-ed
	// into every access of the concept. So a tier is only true of the
	// concept if EVERY query over it already satisfies that predicate.
	//
	// The consequence is the rule below, and it is the whole
	// correctness of this codemod: a query that carries no evidence is
	// not a neutral bystander, it is a COUNTEREXAMPLE. It reads rows
	// the floor would exclude, so declaring the floor would assert
	// something that query disproves. Counting only the positive votes
	// -- "one caller-scoped query, therefore the concept is owned" --
	// declares `planner.plan` owned off 2 of 10 queries while the
	// primary user-facing read is space-scoped, and declares
	// `library.artifact` owned when `libraryWorkspaceLiveSources`
	// documents its rows as having no owner at all.
	//
	// Only @serverOnly is exempt: it is not a client-callable read, and
	// #2803's design decision 4 reserves an explicit system actor for
	// exactly that path.
	for key := range declared {
		vs := votes[key]
		var agreed langparser.RowAuthzDecl
		var agreedSource string
		haveVote := false
		reason := ""

		for _, v := range vs {
			switch v.Kind {
			case verdictExempt:
				continue
			case verdictBlocks:
				if reason == "" {
					reason = fmt.Sprintf("%s %s", v.Source, v.Reason)
				}
			case verdictVote:
				if !haveVote {
					agreed, agreedSource, haveVote = v.Decl, v.Source, true
					continue
				}
				if v.Decl != agreed && reason == "" {
					reason = fmt.Sprintf("queries disagree: %s says %s, %s says %s",
						agreedSource, describe(agreed), v.Source, describe(v.Decl))
				}
			}
		}

		switch {
		case reason != "":
			abstained[key] = reason
		case !haveVote:
			abstained[key] = "no query over this concept establishes a tier"
		default:
			if out.Tiers[key.Domain] == nil {
				out.Tiers[key.Domain] = map[string]langparser.RowAuthzDecl{}
			}
			out.Tiers[key.Domain][key.Name] = agreed
			out.Counts[agreed.Tier]++
		}
	}

	return out, nil
}

func describe(d langparser.RowAuthzDecl) string {
	line, err := langparser.FormatRowAuthz(d)
	if err != nil {
		return string(d.Tier)
	}
	return line
}

// classifyQuery reads one query's evidence. The second return is false
// when the query abstains -- it is server-only, it is annotated
// @public, or its filter does not unambiguously establish a tier.
//
// ONLY `owned` AND `clusterOwner` ARE EVER INFERRED. Both rest on a
// filter term that demonstrably narrows the row set, so declaring them
// restates what the tree already does. The other two tiers are left to
// a human on purpose:
//
//   - `public` is a WIDENING claim, and no filter can evidence it. The
//     nearest thing in the tree, a construct-level `@public`, answers a
//     different question -- "this CALL is intentionally unscoped" --
//     and carries no runtime semantics, so it is an author's
//     acknowledgement rather than a fact. Promoting it to "these ROWS
//     are globally readable by intent" would re-create exactly the
//     silent permissiveness this tier exists to end (#2803: "'no
//     annotation' and 'declared public' are different states"), and it
//     would do so unreviewed. Inferring it from `@public` on
//     `dsl/identity/queries.memql` would have declared `identity.user`
//     -- email, phone, birthdate -- publicly readable. That those
//     `@public` claims are not reliable is separately filed as #2918.
//   - `granted` needs a relationship spec resolved across files, which
//     is Phase 2's job, where the predicate is actually computed.
//
// Both stay undeclared, which is a visible state Phase 2 measures, not
// a silent one.
func classifyQuery(preamble, body string) vote {
	// @serverOnly says "not callable by a client at all", so a
	// caller-scope filter is neither present nor meaningful there
	// (#2800). It is the one verdict that neither votes nor blocks.
	if serverOnlyAnnotationRe.MatchString(preamble) {
		return vote{Kind: verdictExempt}
	}
	// @public says this call is intentionally unscoped, which is a
	// direct counterexample to any floor that narrows rows.
	if publicAnnotationRe.MatchString(preamble) {
		return vote{Kind: verdictBlocks, Reason: "is @public, so it reads rows any narrowing tier would exclude"}
	}

	clause := ""
	if m := filterClauseRe.FindStringSubmatch(body); m != nil {
		clause = strings.TrimSpace(m[1])
	}
	if clause == "" {
		return vote{Kind: verdictBlocks, Reason: "has no filter, so it reads every row of the concept"}
	}

	// Only a TOP-LEVEL CONJUNCT establishes anything. A term inside a
	// parenthesised `||` group does not narrow the result set -- the
	// other arm still returns rows the term would exclude, which is
	// the memql#2832 defect where one permissive disjunct made a gate
	// read as scoped.
	conjuncts := topLevelConjuncts(clause)
	for _, conjunct := range conjuncts {
		if m := ownerLeafRe.FindStringSubmatch(conjunct); m != nil {
			field := m[1]
			if field == "" {
				field = m[2]
			}
			return vote{Kind: verdictVote, Decl: langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: field}}
		}
	}
	for _, conjunct := range conjuncts {
		if adminLeafRe.MatchString(conjunct) {
			return vote{Kind: verdictVote, Decl: langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}}
		}
	}
	return vote{Kind: verdictBlocks, Reason: fmt.Sprintf("filters on %q, which does not gate on the caller", clause)}
}

// topLevelConjuncts splits a filter clause on `&&` at paren depth 0 and
// returns each conjunct trimmed of surrounding whitespace and of one
// layer of redundant parentheses.
//
// A clause containing a top-level `||` yields NO conjuncts: the whole
// expression is a disjunction, so nothing in it is guaranteed.
func topLevelConjuncts(clause string) []string {
	depth := 0
	var parts []string
	start := 0
	for i := 0; i < len(clause); i++ {
		switch clause[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '&':
			if depth == 0 && i+1 < len(clause) && clause[i+1] == '&' {
				parts = append(parts, clause[start:i])
				i++
				start = i + 1
			}
		case '|':
			if depth == 0 && i+1 < len(clause) && clause[i+1] == '|' {
				return nil
			}
		}
	}
	parts = append(parts, clause[start:])

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		for strings.HasPrefix(p, "(") && strings.HasSuffix(p, ")") && balanced(p[1:len(p)-1]) {
			p = strings.TrimSpace(p[1 : len(p)-1])
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// balanced reports whether every paren in s closes inside s.
func balanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// conceptImports maps each concept short-name a file imports to the
// namespace it came from, so `query space listSpaces` in
// dsl/library/queries.memql resolves to the cognition concept rather
// than to a library one that does not exist.
func conceptImports(src string) map[string]string {
	out := map[string]string{}
	for _, m := range useConceptsRe.FindAllStringSubmatch(src, -1) {
		ns := m[1]
		for _, name := range strings.Split(m[2], ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				out[name] = ns
			}
		}
	}
	return out
}

// constructBody returns the source of the construct whose header
// starts at headerStart, from its opening brace to the matching close.
func constructBody(src string, headerStart int) string {
	braceIdx := strings.IndexByte(src[headerStart:], '{')
	if braceIdx < 0 {
		return ""
	}
	depth := 0
	for i := headerStart + braceIdx; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[headerStart+braceIdx : i+1]
			}
		}
	}
	return src[headerStart+braceIdx:]
}

// constructPreamble returns the contiguous run of annotation and
// comment lines immediately above a construct header.
func constructPreamble(src string, headerStart int) string {
	lineStart := headerStart
	for lineStart > 0 {
		prevEnd := lineStart - 1
		ps := strings.LastIndexByte(src[:prevEnd], '\n') + 1
		trimmed := strings.TrimSpace(src[ps:prevEnd])
		if trimmed == "" || (!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//")) {
			break
		}
		lineStart = ps
	}
	return src[lineStart:headerStart]
}

// Report renders the run's tier distribution and the abstentions, so a
// codemod run states what it did and what it deliberately did not do.
// Silent truncation reads as "covered everything" when it did not.
func (r *rowAuthzInference) Report() string {
	var b strings.Builder
	total := 0
	tiers := []langparser.RowAuthzTier{
		langparser.RowAuthzOwned,
		langparser.RowAuthzClusterOwner,
		langparser.RowAuthzPublic,
		langparser.RowAuthzGranted,
	}
	fmt.Fprintln(&b, "row-authz inference:")
	for _, t := range tiers {
		fmt.Fprintf(&b, "  %-13s %d\n", t, r.Counts[t])
		total += r.Counts[t]
	}
	fmt.Fprintf(&b, "  %-13s %d\n", "undeclared", len(r.Abstained))
	fmt.Fprintf(&b, "  %-13s %d\n", "TOTAL", total+len(r.Abstained))

	if len(r.Abstained) > 0 {
		keys := make([]conceptKey, 0, len(r.Abstained))
		for k := range r.Abstained {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Domain != keys[j].Domain {
				return keys[i].Domain < keys[j].Domain
			}
			return keys[i].Name < keys[j].Name
		})
		fmt.Fprintln(&b, "\nleft undeclared (Phase 2 measures these rather than inheriting a guess):")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s.%s -- %s\n", k.Domain, k.Name, r.Abstained[k])
		}
	}
	return b.String()
}
