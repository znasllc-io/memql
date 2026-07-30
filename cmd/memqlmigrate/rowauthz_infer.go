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

// constructHeaderRe matches a top-level `query <Concept> <name> {` or
// `mutate <Concept> <name> {` declaration, capturing the kind, the
// bound concept, and the construct name.
//
// The bound concept is the whole point here, so unlike the conformance
// test's own header regex (which makes it a non-capturing optional
// group) this requires it: a construct with no signature concept binds
// nothing and cannot be evidence about a concept.
var constructHeaderRe = regexp.MustCompile(`(?m)^[ \t]*(query|mutate)[ \t]+([\p{L}_][\p{L}\p{Nd}_]*)[ \t]+([\p{L}_][\p{L}\p{Nd}_]*)[ \t]*\{`)

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
	// blocks any tier: the construct reaches rows the tier would
	// exclude, so declaring the tier would assert something this
	// construct disproves.
	//
	// FIRST, so it is the zero value. Under a "block unless proven"
	// rule the safe default has to be the blocking one -- a future
	// `vote{Source: ...}` literal that forgets to set Kind must fail
	// closed, not silently become a vote for the empty tier.
	verdictBlocks verdictKind = iota
	// votes for a tier: its filter guarantees that tier's predicate.
	verdictVote
	// neither: @serverOnly, which is not a client-callable read.
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
		// RECURSIVE, because the loader is. A query under
		// dsl/<domain>/<sub>/*.memql belongs to <domain> (the unified
		// loader's firstPathSegment), so a depth-1 read would silently
		// drop its evidence -- and dropped evidence here means a tier
		// gets declared that a nested query disproves.
		walkErr := filepath.WalkDir(filepath.Join(dslRoot, domain), func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				if path != filepath.Join(dslRoot, domain) &&
					(strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(e.Name(), ".memql") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			files = append(files, domainFile{domain: domain, path: path, src: string(raw)})
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk domain %s: %w", domain, walkErr)
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

	// Pass 2: collect each construct's verdict about the concept it
	// binds.
	for _, f := range files {
		// Headers are located, and bodies sliced, on the comment- and
		// string-blanked view. The blanker is length-preserving, so
		// offsets index the raw source unchanged. Scanning raw source
		// let a `{` inside a string or a comment unbalance the brace
		// walk and run one construct's body into the next -- the same
		// locate-on-one-view/slice-on-another class ConceptHeaders
		// avoids (#2948), and here it would mean a query voting with a
		// DIFFERENT construct's filter.
		blanked := langparser.BlankCommentsAndStrings(f.src)
		// Imports are read off the blanked view too: a `use` inside a
		// block comment is not an import, and treating it as one
		// re-homes every vote in the file to the wrong domain.
		imports := conceptImports(blanked)

		for _, m := range constructHeaderRe.FindAllStringSubmatchIndex(blanked, -1) {
			kind := blanked[m[2]:m[3]]
			conceptName := blanked[m[4]:m[5]]
			constructName := blanked[m[6]:m[7]]

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

			bodyStart, bodyEnd := constructBodyRange(blanked, m[0])
			// Boundary from the raw view (a blanked comment line is
			// indistinguishable from a blank one), content from the
			// blanked view (so a `@public` inside a block comment is
			// prose, not an annotation).
			preamble := blanked[constructPreambleStart(f.src, m[0]):m[0]]
			// The blanker is length-preserving, so one offset pair
			// indexes both views. Classification reads the BLANKED
			// body (a `&&` inside a string must not split a clause);
			// diagnostics quote the RAW body, so the author sees their
			// own text rather than a filter with its string literals
			// blanked to runs of spaces.
			v := classifyConstruct(kind, preamble, blanked[bodyStart:bodyEnd], f.src[bodyStart:bodyEnd])
			v.Source = fmt.Sprintf("%s/%s %s", f.domain, filepath.Base(f.path), constructName)
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
//     `@public` claims are not reliable is separately filed as #2987.
//     (#2918 corrected the audit doc's example LIST and gated it; the
//     reliability of the identity claims themselves is #2987.)
//   - `granted` needs a relationship spec resolved across files, which
//     is Phase 2's job, where the predicate is actually computed.
//
// Both stay undeclared, which is a visible state Phase 2 measures, not
// a silent one.
func classifyConstruct(kind, preamble, body, rawBody string) vote {
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

	// A MUTATION neither votes nor blocks, and the asymmetry with
	// queries is deliberate.
	//
	// It cannot vote: `actor.userId` in a mutation is a stamped VALUE
	// (`ownerUserId: actor.userId`), which records who owns a NEW row
	// rather than which rows the construct may reach.
	//
	// It must not block either, and this is the subtle half. An
	// `update { id: args.x }` does select an existing row, so it looks
	// like a counterexample -- but it is not one. An unscoped QUERY
	// reads other users' rows BY DESIGN (`plansForSpace` is
	// space-scoped because the product needs it that way), which is
	// what makes it evidence against a floor. An ungated update says
	// nothing about intent: it is simply "update the row I name," and
	// the missing owner check is exactly the gap #2803 exists to
	// close. #2803 sequences mutations after reads for that reason --
	// the update path needs a read-then-check that does not exist yet.
	// Treating it as a blocker inverts the argument and would have
	// dropped 6 of 13 correct declarations, including `telephony.call`,
	// #2803's own worked example.
	//
	// The honest consequence is a stated LIMIT rather than a silent
	// one: this inference reads queries, so a concept whose mutations
	// contradict its queries is not detected here. Recorded in the
	// report and in the audit doc; measuring it is Phase 2's job.
	if kind == "mutate" {
		return vote{Kind: verdictExempt}
	}

	clause := ""
	rawClause := ""
	if m := filterClauseRe.FindStringSubmatchIndex(body); m != nil {
		clause = strings.TrimSpace(body[m[2]:m[3]])
		rawClause = strings.TrimSpace(rawBody[m[2]:m[3]])
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
	return vote{Kind: verdictBlocks, Reason: fmt.Sprintf("filters on %q, which does not gate on the caller", rawClause)}
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

// constructBodyRange returns the byte range of the construct whose
// header starts at headerStart, from its opening brace to the matching
// close. Offsets rather than a substring, so the caller can slice the
// blanked view for analysis and the raw view for diagnostics.
//
// Must be given the BLANKED view: a brace inside a string or a comment
// would otherwise unbalance the walk and run one construct's body into
// the next.
func constructBodyRange(blanked string, headerStart int) (int, int) {
	braceIdx := strings.IndexByte(blanked[headerStart:], '{')
	if braceIdx < 0 {
		return headerStart, headerStart
	}
	start := headerStart + braceIdx
	depth := 0
	for i := start; i < len(blanked); i++ {
		switch blanked[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return start, i + 1
			}
		}
	}
	return start, len(blanked)
}

// constructPreambleStart walks back from a construct header over the
// contiguous run of annotation and comment lines that belong to it,
// and returns where that run begins.
//
// It must be given the RAW source. On the blanked view a comment line
// is all spaces, so it trims to "" and the walk stops there -- which
// made the `//` branch dead code and, worse, hid an `@serverOnly`
// sitting above a comment line. The annotation MATCHING still happens
// on the blanked view (see the caller), so a `@public` inside a block
// comment is still prose; only the boundary is computed on raw text.
func constructPreambleStart(raw string, headerStart int) int {
	lineStart := headerStart
	for lineStart > 0 {
		prevEnd := lineStart - 1
		ps := strings.LastIndexByte(raw[:prevEnd], '\n') + 1
		trimmed := strings.TrimSpace(raw[ps:prevEnd])
		if trimmed == "" || (!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "//")) {
			break
		}
		lineStart = ps
	}
	return lineStart
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

	// State the limit. A run that reports only what it covered reads as
	// full coverage, and this one deliberately does not look at
	// mutations or at the granted tier.
	fmt.Fprintln(&b, "\nNOT examined by this inference (stated, not silent):")
	fmt.Fprintln(&b, "  - mutations. `actor.userId` in a mutation is a stamped value, not a row")
	fmt.Fprintln(&b, "    selection, so it cannot establish a tier; and an ungated `update` is the")
	fmt.Fprintln(&b, "    gap #2803 exists to close rather than evidence against a tier. A concept")
	fmt.Fprintln(&b, "    whose mutations contradict its queries is not detected here -- #2803")
	fmt.Fprintln(&b, "    sequences mutations after reads for the same reason.")
	fmt.Fprintln(&b, "  - the `granted` tier, which needs a relationship spec resolved across files.")
	fmt.Fprintln(&b, "  - the `public` tier, which no filter can evidence and which is never inferred.")
	return b.String()
}
