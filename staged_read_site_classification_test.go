package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// staged_read_site_classification_test.go -- epic memql#3974, task memql#3984.
//
// ===========================================================================
// THE INVERSION: THIS GATE CLASSIFIES, IT DOES NOT REQUIRE
// ===========================================================================
// The staged-DATA tier withholds a promoted concept's rows from the ordinary
// read path (memql#3980 for the state, memql#3983 for the DSL seams). memql#3983
// covers everything that goes through the engine's parser and filter path. It
// does NOT cover a hand-rolled `SELECT ... FROM "MemoryNodes"` in integrations/,
// because nothing is injected into a statement the engine never parsed. Those
// sites are what this gate is about.
//
// The issue that opened this task asked for the predicate at every such read.
// The inventory measured the tree and found that instruction is a BUG REPORT at
// 41% of the sites -- 24 entries covering 29 functions out of 58 MUST NOT be
// gated, several of them in ways that destroy data:
//
//   - component/backup's eachRow. Staged rows omitted from an export are
//     DESTROYED on restore, silently and permanently. And gating CountRows
//     beside it disables the "refuse a non-empty target" check that would have
//     caught it -- one gate causes the loss and hides it.
//   - authoring_concept_retire's countConceptRows decides RETIRE vs REMOVE. A
//     concept holding ten thousand staged rows counts as zero and the concept is
//     removed outright. authoring_concept_diff's countLatestConceptRows decides
//     whether a BREAKING schema change is refused; gated, it reports nothing
//     affected, the change lands, and the damage arrives later when the staged
//     rows publish into a schema with no place for their values.
//   - agent_lock_validation fails OPEN: not-found returns (nil, nil), so an
//     agent bound to a staged role receives the tools its role lock exists to
//     withhold. agent_role_slug_unique_validation is worse -- the gate CREATES
//     the uniqueness violation it would then be unable to detect.
//
// So a gate that fails on an UNGATED read would manufacture that list. This one
// fails on an UNCLASSIFIED read instead. Every hand-rolled MemoryNodes read must
// carry a verdict a human wrote, and the build stays red until one is there.
//
// That is the ShadowUndecidable verdict transposed. rowauthz_enforce_gate_test.go
// (component/memql) does not fail on a query that lacks an ownership conjunct; it
// fails on one it cannot DECIDE about, and adjudicating the entry is the fix. The
// subject here is a Go SQL site rather than a MemQL construct, but the shape of
// the demand is identical: the tree may not contain a read nobody has ruled on.
//
// # Why it cannot be an extension of rowauthz_enforce_gate_test.go
//
// That gate re-derives its answer at run time from three inputs -- a loaded
// FunctionRegistry, fn.BoundConcept, and an analyzable MemQL AST for
// AnalyzeShadow. A Go SQL site has none of them: there is no registry entry, no
// bound concept, and the filter is a string. What transfers is its SHAPE, and
// this file takes four things from it deliberately:
//
//   - RE-DERIVE at build time rather than maintain a list. The site inventory
//     below is computed from the tree on every run; nothing here is a copy of
//     what was true when the issue was written.
//   - Fail on UNDECIDABLE, not only on known-bad (above).
//   - Ship a self-test that MUTATES A REAL SITE to prove the gate is not blind
//     (TestStagedReadSiteGateCatchesAStrippedVerdict, modelled on
//     TestGateCatchesAStrippedOwnershipConjunct).
//   - Guard the vacuous pass. "A gate that measures nothing passes forever" is
//     that file's phrase and this one inherits the failure mode exactly: a
//     detector that silently stops matching reports a clean tree.
//
// # Why the ROOT package
//
// The sites live in component/** and integrations/**, which are ~48 separate
// modules. Three facts settle the placement:
//
//   - Root `go test ./...` covers the ROOT MODULE only (memql#4032), so a gate in
//     component/memql is invisible to the command every contributor runs.
//   - component/memql's suite is db-gated: without MEMQL_REQUIRE_DB=1 its cases
//     SKIP and the package reports ok. A gate whose default outcome is "skipped
//     quietly" is the vacuous pass wearing a different hat.
//   - The root package runs uncached under RUN_GO *and* RUN_GATES, and any .go
//     change anywhere sets the `go` bucket -- which is exactly the change class
//     that adds a read site.
//
// PR memql#4031 put its tree sweep here for the same reason and its neighbours
// (TestNoEnvironmentBranchingInEngineCode, TestEngineIsProductNeutral) are the
// house pattern for a gate whose subject is the tree rather than a package.
//
// # Two vestigial traps, recorded because a future reader will hit them
//
// component/database/memory-nodes/repository.go's ListRecentMemoryNodes,
// LoadMemoryNode and FindMemoryNodes have ZERO in-tree callers, and
// component/harness's BunAgentRoster had no in-tree constructor call either --
// and that module is now retired outright (work spine A1), so only the first
// half of this trap survives. A gate that demanded the predicate everywhere
// would demand edits to code nothing runs. Classification handles it honestly:
// the repository trio is MUST-NOT-GATE (the storage module cannot see engine
// state, so a gate there would necessarily be a SECOND source of truth),
// because its constructor is exported API a product wires and a required
// parameter gives a downstream caller a compile error rather than a silent hole.

// --- the verdicts -----------------------------------------------------------

// stagedVerdictMarker matches the classification a site must carry. Exact
// casing on the `staged-data:` prefix, so ordinary prose about the tier
// ("the staged-DATA visibility question") cannot be mistaken for a ruling.
//
// GATE: the read reaches somewhere staged rows must not appear, and the gate is
// applied -- here, or at a named place the comment points to.
//
// MUST-NOT-GATE: applying the gate here causes a NAMED bug. The comment has to
// name it; "exempt" on its own tells the next person nothing and invites them
// to helpfully add the gate back.
//
// INDIFFERENT: the gate would neither prevent a bug nor cause one.
//
// INDIFFERENT has NO instances in the tree today, and that is a real finding
// rather than a gap: every direct read here either feeds something that must
// not see staged rows, or is an input to a decision that gating corrupts. Keep
// the value anyway. Deleting it would leave an adjudicator with only two
// options for a read that genuinely does not matter, and the ruling they would
// then write -- MUST-NOT-GATE with an invented bug, or GATE with a gate nobody
// needs -- is worse than the honest third answer. The vacuous-pass check below
// deliberately does not require it to be populated.
var stagedVerdictMarker = regexp.MustCompile(`staged-data:\s+(GATE|MUST-NOT-GATE|INDIFFERENT)\b`)

// --- the detector -----------------------------------------------------------

// memoryNodesReadSite is one HOLDER -- a top-level func, or a file-scope
// const/var -- that issues or carries a direct read against the row tables.
type memoryNodesReadSite struct {
	File     string
	Holder   string
	Line     int
	Evidence string
	Verdicts []string
}

// stagedScanSkipPrefixes are paths the sweep does not classify.
//
// component/database/dbtest is a TEST-HELPER package whose non-_test.go files
// exist to build fixtures; its reads are inputs to assertions, not production
// read paths, and the inventory excluded it for the same reason.
var stagedScanSkipPrefixes = []string{
	"component/database/dbtest/",
}

// classifyMemoryNodesReadSites finds every direct MemoryNodes read holder in one
// Go source file and reports the verdict markers each carries, plus any marker
// that sits in no holder at all.
//
// Split out from the sweep, and taking SOURCE rather than a path, precisely so
// the self-test can drive it on a mutated copy of a real file without writing
// that copy anywhere the sweep would then find it.
//
// # The two detection rules, and why each is drawn where it is
//
// RULE A -- a string literal containing `MemoryNodes` AND (case-insensitively)
// `FROM`. The FROM requirement is what separates a READ from the other reasons
// this tree spells the table's name: a bun struct tag (`bun:"table:MemoryNodes"`),
// a table-name constant, a readiness probe's table list, the migration DDL. All
// of those name the table without reading rows out of it, and demanding a
// verdict on each would train a reviewer to stamp INDIFFERENT reflexively --
// which is how a gate acquires the exemptions that hollow it out. `INSERT INTO`
// and `UPDATE ... SET` have no FROM either, and writes are memql#3985's subject,
// not this tier's. A `DELETE FROM` does match, and that over-approximation is
// kept on purpose: purgeChunksForSource's DELETE carries a SELECT subquery over
// the same table, and the cost of a spurious verdict is one comment while the
// cost of a missed read is a published row.
//
// RULE B -- a bun `.Model(` / `.ModelTableExpr(` call whose RECEIVER CHAIN
// reaches `NewSelect()`. Chain-rooted rather than function-scoped, because
// `.Model(` is not a bun-only spelling: component/memql's AI providers call
// `.Model(` on request builders, and timescaledb.go issues a `NewInsert().Model(...)`
// inside a function that separately runs a NewSelect. Walking the chain
// distinguishes them exactly.
//
// # Honest limits
//
//   - Rule B follows a fluent chain. A site that parked the query in a local
//     (`q := db.NewSelect(); q = q.Model(...)`) is invisible to it. No site in
//     the tree does that today; if one appears, the rule needs the local's
//     assignment tracked rather than a wider net.
//   - `_test.go` files are not swept. Test fixtures build rows to assert on and
//     are not a production read path.
//   - It classifies the CLAIM, not the behaviour. A holder marked GATE whose
//     gate was later deleted still passes. What measures behaviour is the
//     per-site test beside each gate; this gate is the only thing that scales
//     with the tree.
func classifyMemoryNodesReadSites(rel string, src []byte) ([]memoryNodesReadSite, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	type span struct {
		start, end token.Pos
		name       string
	}
	var spans []span
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				name = stagedReceiverName(decl.Recv.List[0].Type) + "." + name
			}
			start := decl.Pos()
			if decl.Doc != nil {
				start = decl.Doc.Pos()
			}
			spans = append(spans, span{start, decl.End(), name})
		case *ast.GenDecl:
			if decl.Tok != token.CONST && decl.Tok != token.VAR {
				continue
			}
			start := decl.Pos()
			if decl.Doc != nil {
				start = decl.Doc.Pos()
			}
			name := "(unnamed decl)"
			for _, sp := range decl.Specs {
				if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
					name = string(decl.Tok.String()) + " " + vs.Names[0].Name
					break
				}
			}
			spans = append(spans, span{start, decl.End(), name})
		}
	}
	holderAt := func(p token.Pos) (string, bool) {
		for _, s := range spans {
			if p >= s.start && p < s.end {
				return s.name, true
			}
		}
		return "", false
	}

	found := map[string]*memoryNodesReadSite{}
	record := func(p token.Pos, evidence string) {
		holder, ok := holderAt(p)
		if !ok {
			// A read outside any top-level declaration cannot happen in valid
			// Go, but returning silently on a shape we did not anticipate is
			// how a detector goes quiet. Attribute it to the file.
			holder = "(file scope)"
		}
		if existing, ok := found[holder]; ok {
			existing.Evidence += "+" + evidence
			return
		}
		found[holder] = &memoryNodesReadSite{
			File:     rel,
			Holder:   holder,
			Line:     fset.Position(p).Line,
			Evidence: evidence,
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(node.Value)
			if uerr != nil {
				v = node.Value
			}
			if strings.Contains(v, "MemoryNodes") && strings.Contains(strings.ToUpper(v), "FROM") {
				record(node.Pos(), "sql")
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Model" && sel.Sel.Name != "ModelTableExpr" {
				return true
			}
			if stagedChainReachesNewSelect(sel.X) {
				record(node.Pos(), "bun")
			}
		}
		return true
	})

	// Attach verdicts, and collect markers that landed in no holder.
	var strays []string
	for _, group := range f.Comments {
		for _, c := range group.List {
			m := stagedVerdictMarker.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}
			holder, ok := holderAt(c.Pos())
			site := (*memoryNodesReadSite)(nil)
			if ok {
				site = found[holder]
			}
			if site == nil {
				strays = append(strays, fmt.Sprintf("%s:%d (%s)", rel, fset.Position(c.Pos()).Line, m[1]))
				continue
			}
			if !stagedContains(site.Verdicts, m[1]) {
				site.Verdicts = append(site.Verdicts, m[1])
			}
		}
	}

	out := make([]memoryNodesReadSite, 0, len(found))
	for _, s := range found {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, strays, nil
}

func stagedContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// stagedChainReachesNewSelect walks a fluent method chain's receiver looking for
// NewSelect(). See RULE B on classifyMemoryNodesReadSites for why this is
// chain-rooted rather than function-scoped.
func stagedChainReachesNewSelect(e ast.Expr) bool {
	for {
		switch t := e.(type) {
		case *ast.CallExpr:
			sel, ok := t.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			if sel.Sel.Name == "NewSelect" {
				return true
			}
			e = sel.X
		case *ast.SelectorExpr:
			e = t.X
		default:
			return false
		}
	}
}

func stagedReceiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return stagedReceiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return stagedReceiverName(t.X)
	case *ast.IndexListExpr:
		return stagedReceiverName(t.X)
	}
	return "?"
}

// --- the sweep --------------------------------------------------------------

// stagedTrackedGoFiles lists the git-tracked, non-test Go files the sweep reads.
//
// git-tracked rather than a directory walk, so a local scratch file, a nested
// worktree or a vendored copy is not repo content. The WHOLE tree rather than
// component/ + integrations/, which is where the sites happen to be today: the
// two-directory scope would silently develop a hole the first time a read landed
// in app/ or cmd/, and the wider scan finds exactly the same set at no cost.
func stagedTrackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		skip := false
		for _, prefix := range stagedScanSkipPrefixes {
			if strings.HasPrefix(rel, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files
}

// TestEveryDirectMemoryNodesReadIsClassified is the gate.
//
// It fails when a hand-rolled read against the row tables carries no verdict,
// when one carries two different verdicts, or when a verdict marker is left
// behind in a file whose reads are gone -- that last one because a stale marker
// pre-authorizes whatever lands beside it next, which is the failure mode
// memql#3987's allow-list staleness check exists to prevent.
//
// It does NOT fail on an ungated read. See this file's header for the four
// data-destroying bugs that demand would have manufactured.
func TestEveryDirectMemoryNodesReadIsClassified(t *testing.T) {
	files := stagedTrackedGoFiles(t)

	var (
		sites        []memoryNodesReadSite
		strays       []string
		verdictCount = map[string]int{}
	)
	for _, rel := range files {
		src, err := os.ReadFile(rel)
		if err != nil {
			continue // deleted-in-worktree etc.
		}
		fileSites, fileStrays, perr := classifyMemoryNodesReadSites(rel, src)
		if perr != nil {
			t.Errorf("%s: parse: %v", rel, perr)
			continue
		}
		sites = append(sites, fileSites...)
		// A stray marker only counts when the file holds no read at all. A
		// gate is often applied one function away from the statement it
		// protects -- actionsearch's searchHandler guards a const searchSQL --
		// and a marker at the gate is exactly the comment a reader wants. What
		// must not survive is a marker in a file whose reads have all gone.
		if len(fileSites) == 0 {
			strays = append(strays, fileStrays...)
		}
	}

	var unclassified, conflicting []string
	for _, s := range sites {
		switch len(s.Verdicts) {
		case 0:
			unclassified = append(unclassified, fmt.Sprintf("%s:%d %s [%s]", s.File, s.Line, s.Holder, s.Evidence))
		case 1:
			verdictCount[s.Verdicts[0]]++
		default:
			sort.Strings(s.Verdicts)
			conflicting = append(conflicting,
				fmt.Sprintf("%s:%d %s carries %s", s.File, s.Line, s.Holder, strings.Join(s.Verdicts, " and ")))
			verdictCount[s.Verdicts[0]]++
		}
	}

	t.Logf("\n=== direct MemoryNodes read sites, re-derived at this tree (memql#3984) ===\n"+
		"  go files swept      %d\n"+
		"  read-site holders   %d\n"+
		"    GATE              %d\n"+
		"    MUST-NOT-GATE     %d\n"+
		"    INDIFFERENT       %d\n"+
		"    unclassified      %d\n",
		len(files), len(sites),
		verdictCount["GATE"], verdictCount["MUST-NOT-GATE"], verdictCount["INDIFFERENT"],
		len(unclassified))

	// --- the vacuous-pass guards ---------------------------------------------
	//
	// Every one of these is a way for the sweep to report a clean tree having
	// measured nothing: run from the wrong directory, run without git, a
	// detection rule that quietly stopped matching, or a marker regexp that
	// stopped recognising its own vocabulary. A gate that measures nothing
	// passes forever.
	if len(files) < 300 {
		t.Fatalf("swept only %d tracked non-test Go files; this gate cannot pass vacuously", len(files))
	}
	if len(sites) < 40 {
		t.Fatalf("found only %d direct MemoryNodes read holders, and the tree had 56 when this gate "+
			"landed. Either the detector stopped matching or the reads moved somewhere it does not "+
			"look -- both report a clean tree while measuring nothing", len(sites))
	}
	if verdictCount["GATE"] == 0 || verdictCount["MUST-NOT-GATE"] == 0 {
		t.Errorf("the sweep recognised no %s verdict at all. Both halves of the split are supposed to "+
			"exist -- the whole finding of memql#3984 is that 41%% of these sites must NOT be gated -- "+
			"so a vocabulary with only one live value means the marker regexp and the tree have "+
			"drifted apart",
			map[bool]string{true: "GATE", false: "MUST-NOT-GATE"}[verdictCount["GATE"] == 0])
	}

	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("these direct reads against \"MemoryNodes\" carry no staged-DATA verdict. A hand-rolled "+
			"read does not go through the engine's parser or filter path, so memql#3983 injects nothing "+
			"into it and this site is the whole of the enforcement.\n"+
			"ADJUDICATE it -- do not reflexively add the predicate. Of the 58 sites the memql#3984 "+
			"inventory measured, 24 entries covering 29 functions MUST NOT be gated, and gating them "+
			"destroys exports on restore, removes concepts that hold rows, lands breaking schema "+
			"changes over rows about to publish, and fails an agent's role lock OPEN.\n"+
			"Write ONE of these in a comment on the holder (or at the gate, in the same file):\n"+
			"  // staged-data: GATE -- <what is withheld, and where the check is>\n"+
			"  // staged-data: MUST-NOT-GATE -- <the bug gating would cause; NAME it>\n"+
			"  // staged-data: INDIFFERENT -- <why the gate changes nothing here>\n"+
			"%d unclassified:\n  %s",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}

	sort.Strings(conflicting)
	if len(conflicting) > 0 {
		t.Errorf("these holders carry more than one verdict, so the tree does not record a decision -- "+
			"it records a disagreement. Split the holder, or say which reading wins:\n  %s",
			strings.Join(conflicting, "\n  "))
	}

	sort.Strings(strays)
	if len(strays) > 0 {
		t.Errorf("these staged-DATA verdicts sit in files that no longer contain a direct MemoryNodes "+
			"read. Delete them: a verdict outliving its read pre-authorizes whatever lands in that file "+
			"next, and the next reader has no way to tell it apart from a live ruling.\n  %s",
			strings.Join(strays, "\n  "))
	}
}

// TestStagedReadSiteGateCatchesAStrippedVerdict is the gate's own gate.
//
// The measurement above is worthless if the detector cannot see a real site, and
// "it passed" is indistinguishable from "it looked at nothing". So this takes a
// REAL file out of the tree, strips its verdict markers in memory, and asserts
// the holder goes unclassified -- the same construction as
// TestGateCatchesAStrippedOwnershipConjunct (component/memql), which strips an
// ownership conjunct out of a real construct rather than hand-building an AST.
//
// Both halves matter, and they fail in opposite directions. Without the first,
// the detector could match nothing and every run would be green. Without the
// second, the marker regexp could match anything and every run would be green
// too.
func TestStagedReadSiteGateCatchesAStrippedVerdict(t *testing.T) {
	// A real, gated, single-concept read site: the chat integration's utterance
	// reads, which feed LLM context. Chosen over a MUST-NOT-GATE site so the
	// mutation removes a POSITIVE claim -- the direction a careless edit
	// actually goes.
	const victim = "integrations/chat/recent_chat.go"

	src, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read %s: %v", victim, err)
	}

	// HALF ONE: the detector sees the real file, and every holder is ruled on.
	sites, strays, err := classifyMemoryNodesReadSites(victim, src)
	if err != nil {
		t.Fatalf("classify %s: %v", victim, err)
	}
	if len(sites) < 5 {
		t.Fatalf("%s: detector found %d read holders, want at least 5 (readRecent, readByKeyword, "+
			"readByTime, getSpaceContext, listParticipants). A detector that stopped seeing this file "+
			"would report the whole tree clean", victim, len(sites))
	}
	if len(strays) > 0 {
		t.Errorf("%s: unexpected stray verdicts %v", victim, strays)
	}
	for _, s := range sites {
		if len(s.Verdicts) != 1 || s.Verdicts[0] != "GATE" {
			t.Fatalf("%s:%d %s: verdicts %v, want exactly [GATE] -- the mutation below is only "+
				"meaningful against a site that starts out ruled on", victim, s.Line, s.Holder, s.Verdicts)
		}
	}

	// HALF TWO: strip the rulings and watch every one of those holders go
	// unclassified. Only the marker is removed -- the gate code, the SQL and
	// the surrounding prose stay -- so what this proves is that the gate keys
	// on the ruling and not on some incidental property of the file.
	stripped := stagedVerdictMarker.ReplaceAllString(string(src), "(verdict stripped by the gate's self-test)")
	mutated, _, err := classifyMemoryNodesReadSites(victim, []byte(stripped))
	if err != nil {
		t.Fatalf("classify mutated %s: %v", victim, err)
	}
	if len(mutated) != len(sites) {
		t.Fatalf("stripping the verdicts changed the holder count (%d -> %d); the mutation was supposed "+
			"to touch the ruling only", len(sites), len(mutated))
	}
	for _, s := range mutated {
		if len(s.Verdicts) != 0 {
			t.Errorf("%s:%d %s still reads as classified (%v) after its verdict was stripped -- the "+
				"gate would not catch the very change it exists to catch", victim, s.Line, s.Holder, s.Verdicts)
		}
	}
}

// TestStagedVerdictMarkerFiresAndSpares pins the marker vocabulary in both
// directions.
//
// The false-positive half is as load-bearing as the other: a marker regexp loose
// enough to match ordinary prose about the tier would classify sites nobody
// ruled on, and the gate would pass while measuring nothing. Every "spare" line
// below is real text from this change.
func TestStagedVerdictMarkerFiresAndSpares(t *testing.T) {
	mustFire := []string{
		"// staged-data: GATE -- utterances go straight into LLM context.",
		"// staged-data: MUST-NOT-GATE -- staged rows omitted here are DESTROYED on restore.",
		"//	staged-data: INDIFFERENT -- no caller, and no consumer to reach.",
		"// staged-data:   GATE -- extra spacing is still a ruling",
	}
	for _, line := range mustFire {
		if !stagedVerdictMarker.MatchString(line) {
			t.Errorf("marker missed a real verdict:\n  %s", line)
		}
	}

	mustSpare := []string{
		"// SetStagedConceptPredicate injects the staged-DATA visibility question.",
		"// stagedConcept answers the staged-DATA visibility question (memql#3984).",
		"// staged-data enforcement rides immediately after it (memql#3983).",
		"// The staged-DATA tier withholds a promoted concept's rows.",
		"// staged-data: reviewed",
		"// staged-data: GATEWAY -- not a verdict",
	}
	for _, line := range mustSpare {
		if stagedVerdictMarker.MatchString(line) {
			t.Errorf("marker fired on prose that is not a verdict -- this is how a gate starts "+
				"classifying sites nobody ruled on:\n  %s", line)
		}
	}
}
