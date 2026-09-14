package conformance

// corpus_test.go -- the verdict runner for the DSL conformance corpus (epic
// memql#5356, task memql#5361; D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The corpus is data: one directory per edition (test/conformance/2026/), and
// under it a directory per case group holding .memql case files, an optional
// fixture.memql loaded beside every case in the directory, and one expect.json
// naming each case's verdict. The layout and the file format are described in
// test/conformance/2026/README.md; this file is the one thing that reads them.
//
// FIVE VERDICTS, decided by the engine rather than by a copy of it:
//
//   - refuse_parse: the edition's front end plus the parser the loaders use
//     (compiler.ParseFileSource) refuse the file.
//   - load_ok / refuse_load: the file parses, and the engine's own boot-time
//     validation -- MemQLEngine.Init over the embedded tree with the case
//     mounted as an overlay domain, via memql.LintUnifiedTree, plus the
//     automations loader for an automation and the capability catalog and
//     action loader for a capability or an action (corpusActionProblems) --
//     accepts it, or refuses it.
//   - lower / evaluate: the expression lowers to SQL containing the expected
//     text, or evaluates against a row to the expected value, through the
//     adapter in engine_adapter_test.go.
//
// A refusal is matched on its MESSAGE (text the diagnostic must contain) and,
// when the refusal carries one, its CODE (a stable rule id such as
// annotation_unknown). The wording is part of the contract: a refusal is how
// the language tells an author, or a model, what to write instead.
//
// Cases are loaded in batches -- boots for the cases that must load, boots for
// the cases that must be refused at load, one for every expression case --
// because a boot costs over a second and the corpus holds hundreds of cases.
// Each case gets its own overlay domain, and no two cases of one directory
// share a boot, since each mounts the directory's fixture (corpusRounds).
//
// A diagnostic is claimed by the one case it names: by the case file it was
// filed under, else by the one case domain its message names, else by the one
// case whose construct it quotes by name (corpusDomainOf, corpusRunNamed). A
// diagnostic that names no case, or names two, is claimed by nobody, and the
// runner falls back to one boot per case for that batch -- slower, and never a
// guess.
//
// Some refusals STOP Init where they are found (an invalid @relationship
// type, an unresolvable connector, a CQS violation), and every check Init
// would have run after that point then runs for nobody in the boot. So a case
// that must load never shares a boot with a case that must be refused, and a
// case that drew no diagnostic from a boot in which another case was refused
// is loaded again without that case (corpusLoadUntilSettled): a case judged
// quiet was quiet in a boot where every case was.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const (
	verdictLoadOK      = "load_ok"
	verdictRefuseParse = "refuse_parse"
	verdictRefuseLoad  = "refuse_load"
	verdictLower       = "lower"
	verdictEvaluate    = "evaluate"
)

// corpusExpectFile is one directory's expect.json.
type corpusExpectFile struct {
	Cases []corpusCase `json:"cases"`
}

// corpusCase is one case: a file and the verdict the engine must reach on it.
type corpusCase struct {
	// File is the case file, in the same directory as expect.json.
	File string `json:"file"`
	// Verdict is one of load_ok, refuse_parse, refuse_load, lower, evaluate.
	Verdict string `json:"verdict"`
	// Code is the stable refusal code a refuse_* diagnostic must carry, when
	// the refusal has one.
	Code string `json:"code,omitempty"`
	// Message is text a refuse_* diagnostic must contain. Required for a
	// refusal: the wording is part of the contract.
	Message string `json:"message,omitempty"`
	// Position is the tiers.Position a lower / evaluate expression is written
	// in. Under expr/<position>/ it defaults to the directory's position.
	Position string `json:"position,omitempty"`
	// Concept is the concept, by bare name, a lower / evaluate expression is
	// over; the directory's fixture.memql declares it.
	Concept string `json:"concept,omitempty"`
	// Row, Args and Actor are the values a lower / evaluate expression sees.
	Row   map[string]any `json:"row,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	Actor map[string]any `json:"actor,omitempty"`
	// SQL is text the lowered SQL must contain (lower).
	SQL string `json:"sql,omitempty"`
	// Expect is the value the expression must evaluate to (evaluate).
	Expect json.RawMessage `json:"expect,omitempty"`
	// Note says why the case exists. Not checked.
	Note string `json:"note,omitempty"`
}

// corpusVersionManifest is an edition directory's manifest.json.
type corpusVersionManifest struct {
	Edition  string `json:"edition"`
	Language string `json:"language"`
	// Status is "draft" until the freeze epic (dsl-v1-freeze) flips it to
	// "frozen".
	Status string `json:"status"`
}

// corpusRun is one case as the runner carries it through the batches.
type corpusRun struct {
	rel     string // case file, relative to test/conformance
	dir     string // its directory, relative to test/conformance
	index   int    // position of the case in its expect.json
	edition string
	c       corpusCase
	src     string
	fixture string // the directory's fixture.memql, "" when it has none
	// sidecars are the directory's other files -- a prompt's or a seed's
	// @templateFile -- mounted beside the case under the same names.
	sidecars map[string][]byte
	domain   string // the overlay domain the case loads in
	line     dslfs.Manifest

	parseErr  error
	loadDiags []string
	loadRan   bool
	got       any
	gotErr    error
}

func (r *corpusRun) name() string {
	return fmt.Sprintf("%s#%d(%s)", r.rel, r.index+1, r.c.Verdict)
}

// TestCorpusVerdicts runs every case in every edition directory and fails each
// case whose verdict the engine does not reach, naming the file and the
// verdict.
func TestCorpusVerdicts(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "") // strict, as a node boots
	runs := discoverCorpus(t)
	if len(runs) == 0 {
		t.Fatal("the corpus holds no cases -- the runner is reading nothing, so every gate over it would pass by matching nothing")
	}

	for _, r := range runs {
		if r.c.Verdict == verdictLower || r.c.Verdict == verdictEvaluate {
			continue
		}
		r.parseErr = corpusParse(r.line.Edition, r.src)
	}
	var loads, probes []*corpusRun
	for _, r := range runs {
		switch r.c.Verdict {
		case verdictLoadOK, verdictRefuseLoad:
			if r.parseErr == nil {
				loads = append(loads, r)
			}
		case verdictLower, verdictEvaluate:
			probes = append(probes, r)
		}
	}
	corpusLoad(t, loads)
	corpusProbe(t, probes)

	for _, r := range runs {
		r := r
		t.Run(r.name(), func(t *testing.T) {
			if msg := corpusJudge(r); msg != "" {
				t.Errorf("%s: verdict %s: %s", r.rel, r.c.Verdict, msg)
			}
		})
	}
}

// corpusJudge compares what the engine did with what the case says it must do.
func corpusJudge(r *corpusRun) string {
	switch r.c.Verdict {
	case verdictRefuseParse:
		if r.parseErr == nil {
			return "the file parsed; the case says the parser refuses it"
		}
		return corpusMatchRefusal(r.c, []string{r.parseErr.Error()})
	case verdictLoadOK:
		if r.parseErr != nil {
			return "the parser refused the file: " + r.parseErr.Error()
		}
		if !r.loadRan {
			return "the load batch did not run this case"
		}
		if len(r.loadDiags) > 0 {
			return "the engine refused the file at load:\n    " + strings.Join(r.loadDiags, "\n    ")
		}
	case verdictRefuseLoad:
		if r.parseErr != nil {
			return "the parser refused the file, but the case says it parses and is refused at load: " + r.parseErr.Error()
		}
		if !r.loadRan {
			return "the load batch did not run this case"
		}
		if len(r.loadDiags) == 0 {
			return "the engine loaded the file; the case says it is refused"
		}
		return corpusMatchRefusal(r.c, r.loadDiags)
	case verdictLower:
		if r.gotErr != nil {
			return "did not lower: " + r.gotErr.Error()
		}
		sql, _ := r.got.(string)
		if !strings.Contains(sql, r.c.SQL) {
			return fmt.Sprintf("lowered to %q, which does not contain %q", sql, r.c.SQL)
		}
	case verdictEvaluate:
		if r.gotErr != nil {
			return "did not evaluate: " + r.gotErr.Error()
		}
		var want any
		_ = json.Unmarshal(r.c.Expect, &want)
		gotJSON, _ := json.Marshal(r.got)
		wantJSON, _ := json.Marshal(want)
		if !bytes.Equal(gotJSON, wantJSON) {
			return fmt.Sprintf("evaluated to %s, want %s", gotJSON, wantJSON)
		}
	}
	return ""
}

// corpusMatchRefusal is empty when one diagnostic carries the case's message
// (and its code, when it names one).
func corpusMatchRefusal(c corpusCase, diags []string) string {
	for _, d := range diags {
		if strings.Contains(d, c.Message) && (c.Code == "" || strings.Contains(d, c.Code)) {
			return ""
		}
	}
	want := fmt.Sprintf("message %q", c.Message)
	if c.Code != "" {
		want = fmt.Sprintf("code %q and %s", c.Code, want)
	}
	return fmt.Sprintf("refused, but no diagnostic carries %s; the diagnostics were:\n    %s", want, strings.Join(diags, "\n    "))
}

// corpusParse is the parse half of a load: the edition's front end, then the
// parser every loader uses.
func corpusParse(edition, src string) error {
	fe, err := langparser.FrontEndFor(edition)
	if err != nil {
		return err
	}
	prepared, err := fe.Prepare(src)
	if err != nil {
		return err
	}
	_, err = compiler.ParseFileSource(prepared)
	return err
}

// ---- discovery ----------------------------------------------------------------

var corpusEditionDir = regexp.MustCompile(`^[0-9]{4}$`)

// discoverCorpus reads every edition directory under test/conformance. It
// fails the test on anything malformed -- an unknown verdict, a refusal with no
// message, a case naming a missing file, a .memql file no case names -- because
// a corpus that silently skips a file is a corpus whose gates pass by matching
// nothing.
func discoverCorpus(t *testing.T) []*corpusRun {
	t.Helper()
	root := os.DirFS(".")
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		t.Fatalf("read test/conformance: %v", err)
	}
	var runs []*corpusRun
	domains := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || !corpusEditionDir.MatchString(e.Name()) {
			continue
		}
		edition := e.Name()
		vm := readCorpusManifest(t, root, edition)
		line := dslfs.Manifest{Language: vm.Language, Edition: vm.Edition}
		err := fs.WalkDir(root, edition, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() || path.Base(p) != "expect.json" {
				return nil
			}
			dir := path.Dir(p)
			runs = append(runs, readCorpusDir(t, root, edition, dir, line, domains)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", edition, err)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].rel < runs[j].rel })
	return runs
}

func readCorpusManifest(t *testing.T, root fs.FS, edition string) corpusVersionManifest {
	t.Helper()
	data, err := fs.ReadFile(root, edition+"/manifest.json")
	if err != nil {
		t.Fatalf("edition %s has no manifest.json: %v", edition, err)
	}
	var vm corpusVersionManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vm); err != nil {
		t.Fatalf("%s/manifest.json: %v", edition, err)
	}
	if vm.Edition != edition {
		t.Fatalf("%s/manifest.json declares edition %q; the directory is the edition", edition, vm.Edition)
	}
	return vm
}

func readCorpusDir(t *testing.T, root fs.FS, edition, dir string, line dslfs.Manifest, domains map[string]string) []*corpusRun {
	t.Helper()
	data, err := fs.ReadFile(root, dir+"/expect.json")
	if err != nil {
		t.Fatalf("read %s/expect.json: %v", dir, err)
	}
	var ef corpusExpectFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ef); err != nil {
		t.Fatalf("%s/expect.json: %v", dir, err)
	}
	if len(ef.Cases) == 0 {
		t.Fatalf("%s/expect.json lists no cases", dir)
	}
	fixture := ""
	if b, err := fs.ReadFile(root, dir+"/fixture.memql"); err == nil {
		fixture = string(b)
	}
	entries, _ := fs.ReadDir(root, dir)
	var sidecars map[string][]byte
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "expect.json" || strings.HasSuffix(name, ".memql") {
			continue
		}
		b, err := fs.ReadFile(root, dir+"/"+name)
		if err != nil {
			t.Fatalf("read %s/%s: %v", dir, name, err)
		}
		if sidecars == nil {
			sidecars = map[string][]byte{}
		}
		sidecars[name] = b
	}
	base := corpusDomainName(strings.TrimPrefix(dir, edition+"/"))
	if prev, taken := domains[base]; taken && prev != dir {
		t.Fatalf("%s and %s map to the same overlay domain %q; rename one", prev, dir, base)
	}
	domains[base] = dir

	named := map[string]bool{"fixture.memql": true}
	var runs []*corpusRun
	for i, c := range ef.Cases {
		where := fmt.Sprintf("%s/expect.json case %d (%s)", dir, i+1, c.File)
		if msg := corpusValidateCase(dir, c); msg != "" {
			t.Fatalf("%s: %s", where, msg)
		}
		src, err := fs.ReadFile(root, dir+"/"+c.File)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		named[c.File] = true
		if c.Position == "" {
			c.Position = corpusDefaultPosition(dir)
		}
		runs = append(runs, &corpusRun{
			rel: dir + "/" + c.File, dir: dir, index: i, edition: edition,
			c: c, src: string(src), fixture: fixture, sidecars: sidecars, line: line,
			domain: fmt.Sprintf("%s_%d", base, i+1),
		})
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".memql") && !named[e.Name()] {
			t.Fatalf("%s/%s is in the corpus but no case in expect.json names it", dir, e.Name())
		}
	}
	return runs
}

func corpusValidateCase(dir string, c corpusCase) string {
	switch c.Verdict {
	case verdictLoadOK:
	case verdictRefuseParse, verdictRefuseLoad:
		if c.Message == "" {
			return "a refusal names the text its diagnostic must contain (message); the wording is part of the contract"
		}
	case verdictLower, verdictEvaluate:
		if c.Concept == "" {
			return "an expression case names the concept it is over (concept)"
		}
		if c.Verdict == verdictLower && c.SQL == "" {
			return "a lower case names the text the SQL must contain (sql)"
		}
		if c.Verdict == verdictEvaluate && len(c.Expect) == 0 {
			return "an evaluate case names the value it must produce (expect)"
		}
		pos := c.Position
		if pos == "" {
			pos = corpusDefaultPosition(dir)
		}
		if !corpusKnownPosition(pos) {
			return fmt.Sprintf("position %q is not a tiers.Position (write one, or put the case under expr/<position>/)", pos)
		}
	default:
		return fmt.Sprintf("verdict %q is not one of load_ok, refuse_parse, refuse_load, lower, evaluate", c.Verdict)
	}
	if c.File == "" || c.File == "fixture.memql" || strings.Contains(c.File, "/") || !strings.HasSuffix(c.File, ".memql") {
		return "file names a .memql case in this directory, other than fixture.memql"
	}
	return ""
}

// corpusDefaultPosition is the position a case under expr/<position>/ is
// written in; "" elsewhere.
func corpusDefaultPosition(dir string) string {
	parts := strings.Split(dir, "/")
	for i, p := range parts {
		if p == "expr" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func corpusKnownPosition(p string) bool {
	for _, known := range tiers.Positions() {
		if string(known) == p {
			return true
		}
	}
	return false
}

var corpusNonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// corpusDomainName turns a case directory into an overlay domain name: the
// path, lower-cased, with every run of other characters folded to `_`.
func corpusDomainName(dir string) string {
	name := strings.Trim(corpusNonIdent.ReplaceAllString(strings.ToLower(dir), "_"), "_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "c_" + name
	}
	return name
}

// ---- the load batch -------------------------------------------------------------

var corpusQuiet = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

// corpusDeclaresAutomation reports whether a case file declares an automation,
// which the automations loader reads only from a domain's automations.memql.
var corpusAutomationDecl = regexp.MustCompile(`(?m)^automation\s`)

// corpusTree builds the overlay tree for a set of cases: one domain each,
// holding the corpus's language line, the directory's fixture and the case.
func corpusTree(runs []*corpusRun) fstest.MapFS {
	tree := fstest.MapFS{}
	for _, r := range runs {
		tree[r.domain+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(r.line.Render())}
		if r.fixture != "" {
			tree[r.domain+"/fixture.memql"] = &fstest.MapFile{Data: []byte(r.fixture)}
		}
		for name, data := range r.sidecars {
			tree[r.domain+"/"+name] = &fstest.MapFile{Data: data}
		}
		name := "case.memql"
		if corpusAutomationDecl.MatchString(r.src) {
			name = "automations.memql"
		}
		tree[r.domain+"/"+name] = &fstest.MapFile{Data: []byte(r.src)}
	}
	return tree
}

// corpusLoad runs every load case through the engine: the cases that must load
// in boots of their own, the cases that must be refused in another (see the
// file comment for why they never share one).
func corpusLoad(t *testing.T, runs []*corpusRun) {
	t.Helper()
	var accepting, refusing []*corpusRun
	for _, r := range runs {
		if r.c.Verdict == verdictLoadOK {
			accepting = append(accepting, r)
		} else {
			refusing = append(refusing, r)
		}
	}

	for _, round := range corpusRounds(accepting) {
		corpusLoadUntilSettled(t, round)
	}
	for _, round := range corpusRounds(refusing) {
		corpusLoadUntilSettled(t, round)
	}
}

// corpusRounds splits runs so that no two cases of one directory share a
// boot. A directory's fixture is mounted beside each of its cases, so two of
// them in one boot declare every construct in the fixture twice, and a bare
// name two domains declare is ambiguous to every lookup by bare name -- the
// boot would refuse the fixture rather than the case.
func corpusRounds(runs []*corpusRun) [][]*corpusRun {
	seen := map[string]int{}
	var rounds [][]*corpusRun
	for _, r := range runs {
		i := seen[r.dir]
		seen[r.dir]++
		for len(rounds) <= i {
			rounds = append(rounds, nil)
		}
		rounds[i] = append(rounds[i], r)
	}
	return rounds
}

// corpusLoadUntilSettled loads a group, then loads the cases that drew no
// diagnostic again in a boot without the ones that did, until a boot holds
// only quiet cases or only refused ones.
//
// What it guarantees is about the QUIET: a boot in which any case was refused
// may have stopped at that refusal, so "no diagnostic" is taken as evidence
// only from a boot in which every case was quiet -- a case that must load
// never passes on less than a whole Init, and a case that must be refused is
// never judged quiet because another case's refusal stopped Init first.
//
// A case that DID draw a diagnostic keeps the list from the boot it drew it in,
// and that boot may have stopped early, so the list can be short of what a
// whole Init would add. That can fail a refusal whose expected diagnostic comes
// after another case's stopping refusal; it cannot pass one.
func corpusLoadUntilSettled(t *testing.T, group []*corpusRun) {
	t.Helper()
	for pending := group; len(pending) > 0; {
		corpusLoadGroup(t, pending)
		var quiet []*corpusRun
		for _, r := range pending {
			if len(r.loadDiags) == 0 {
				quiet = append(quiet, r)
			}
		}
		if len(quiet) == len(pending) || len(quiet) == 0 {
			return
		}
		pending = quiet
	}
}

// corpusLoadGroup runs a group of load cases through the engine in one boot,
// falling back to one boot per case when a diagnostic no single case claims
// makes the group unattributable.
func corpusLoadGroup(t *testing.T, runs []*corpusRun) {
	t.Helper()
	if len(runs) == 0 {
		return
	}
	unclaimed := corpusLoadBatch(t, runs)
	if len(unclaimed) == 0 {
		return
	}
	if len(runs) == 1 {
		runs[0].loadDiags = append(runs[0].loadDiags, unclaimed...)
		return
	}
	// One boot per case is slow, so say what forced it: a diagnostic that
	// names no case, or names two, is a problem this batch cannot attribute.
	t.Logf("%d diagnostic(s) name no single case; loading these %d cases one boot each:\n    %s",
		len(unclaimed), len(runs), strings.Join(unclaimed, "\n    "))
	for _, r := range runs {
		r.loadDiags, r.loadRan = nil, false
		if rest := corpusLoadBatch(t, []*corpusRun{r}); len(rest) > 0 {
			r.loadDiags = append(r.loadDiags, rest...)
		}
	}
}

// corpusLoadBatch loads runs together and attributes every diagnostic to the
// one run it names (see the file comment). It returns the diagnostics no
// single run claims.
func corpusLoadBatch(t *testing.T, runs []*corpusRun) []string {
	t.Helper()
	tree := corpusTree(runs)
	byDomain := map[string]*corpusRun{}
	for _, r := range runs {
		byDomain[r.domain] = r
		r.loadRan = true
	}

	byName := corpusDeclaredNames(runs)
	var unclaimed []string
	claim := func(file, msg string) {
		if r, ok := byDomain[corpusDomainOf(file, msg, byDomain)]; ok {
			r.loadDiags = append(r.loadDiags, msg)
			return
		}
		if r, ok := corpusRunNamed(msg, byName); ok {
			r.loadDiags = append(r.loadDiags, msg)
			return
		}
		unclaimed = append(unclaimed, msg)
	}

	diags, _, err := memql.LintUnifiedTree(corpusQuiet, tree)
	if err != nil {
		// The concept build stopped (an id error, a refused @namespace). The
		// error names the file it stopped in when it can, and is claimed by
		// that case like any other diagnostic.
		claim("", err.Error())
	}
	for _, d := range diags {
		claim(d.File, d.Message)
	}

	// The automations loader is its own pass at boot (app/engine.go), so it is
	// its own pass here, over the same mounted tree.
	if corpusAnyAutomation(runs) {
		for _, line := range corpusAutomationProblems(t, tree) {
			claim("", line)
		}
	}
	if corpusAnyActionConstruct(runs) {
		for _, line := range corpusActionProblems(tree) {
			claim("", line)
		}
	}
	return unclaimed
}

// corpusActionDecl matches a top-level `action` or `capability` declaration.
var corpusActionDecl = regexp.MustCompile(`(?m)^(action|capability)\s`)

func corpusAnyActionConstruct(runs []*corpusRun) bool {
	for _, r := range runs {
		if corpusActionDecl.MatchString(r.src) || corpusActionDecl.MatchString(r.fixture) {
			return true
		}
	}
	return false
}

// corpusPinProcessActions loads component/actions' process-wide capability
// catalog and action registry from the embedded tree, before any test in the
// binary runs -- so before any case, or any other test's overlay, is mounted.
//
// Init validates capabilities and authored actions through those two, and
// each loads ONCE per process (a sync.Once over dsl.Tree()), from whatever
// tree is mounted the first time anything asks. A node boots once per process,
// so there it is the node's own tree. A test binary boots many engines: which
// tree the singletons hold would depend on which test asked first -- a case's
// overlay if the corpus asked first (and then a refused case's error would
// answer every later boot in the binary), the embedded tree otherwise (and
// then no case's action was checked at all). Pinning them at init makes the
// answer the same in every run and every test order, and
// corpusActionProblems checks each batch's own capabilities and actions with
// the same loaders, uncached.
func corpusPinProcessActions() {
	actions.DefaultCatalog()
	actions.Default()
}

// An init rather than a line in TestCorpusVerdicts: a test that runs earlier
// in the binary and mounts a tree of its own could otherwise fix the
// singletons from that tree first.
func init() { corpusPinProcessActions() }

// corpusActionProblems mounts the tree and loads its capability catalog and
// its authored actions the way Init does (reconciliation against the Go
// vocabulary, then strict capability arg-typing), without the process-wide
// cache, and returns one line per problem. Each loader stops at its first
// problem; corpusLoadUntilSettled reloads the rest without the case it names.
//
// One reach this does not have: an action's arguments are typed against the
// process catalog (component/actions reads DefaultCatalog there), so an action
// calling a capability a case itself declares is held to the capability's
// namespace, not to its declared arguments.
func corpusActionProblems(tree fs.FS) []string {
	_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, tree)
	defer unmount()
	var out []string
	if _, err := actions.LoadCatalogFromFS(memqldsl.Tree()); err != nil {
		out = append(out, "capability catalog reconciliation failed: "+err.Error())
	}
	if _, err := actions.NewRegistry().LoadFromFS(memqldsl.Tree()); err != nil {
		out = append(out, "authored action load (strict capability arg-typing) failed: "+err.Error())
	}
	return out
}

// corpusDomainOf finds the one run a diagnostic belongs to: the case file it
// was filed under, when that is a case's, else the one case domain its message
// names. A message names a domain as a path (`<domain>/case.memql`), inside a
// canonical id (`v1:<domain>:ticket`), or as the qualifier of a construct name
// (`<domain>.openTickets`, the form the @requiresRank and @requiresCapability
// gates print) -- always as a whole word, so a domain whose name ends another
// one's is not read into it (corpusNamesDomain).
//
// A message that names two case domains belongs to neither: the answer is ""
// rather than a pick between them, whatever order the map yields, and the
// batch falls back to one boot per case. A file that is not a case's (the
// rule loader files an order problem under dsl/rules/rules.memql) is looked
// past, to the message.
func corpusDomainOf(file, msg string, byDomain map[string]*corpusRun) string {
	if file != "" {
		f := strings.TrimPrefix(file, "unified:")
		if i := strings.IndexByte(f, '/'); i > 0 {
			if _, ok := byDomain[f[:i]]; ok {
				return f[:i]
			}
		}
	}
	found := ""
	for d := range byDomain {
		if !corpusNamesDomain(msg, d) {
			continue
		}
		if found != "" {
			return "" // two cases named: not a choice the runner makes
		}
		found = d
	}
	return found
}

// corpusNamesDomain reports whether msg names domain as a whole word followed
// by `/`, `:` or `.`: the path, canonical-id and qualified-name forms.
func corpusNamesDomain(msg, domain string) bool {
	for from := 0; ; {
		i := strings.Index(msg[from:], domain)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(domain)
		startsWord := i == 0 || !corpusIdentByte(msg[i-1])
		if startsWord && end < len(msg) && strings.IndexByte("/:.", msg[end]) >= 0 {
			return true
		}
		from = i + 1
	}
}

// corpusConstructHeader matches a construct declaration and captures its name:
// `query ticket openTickets {`, `capability fs.list {`, `seed skill a-b {`.
var corpusConstructHeader = regexp.MustCompile(`(?m)^[ \t]*(?:query|mutate|logic|automation|action|capability|spec|trait|tool|builtin|prompt|provider|shape|policy|rule|seed|concept)[ \t]+(?:[A-Za-z_][\w]*[ \t]+)?([A-Za-z_][\w.-]*)[ \t]*\{`)

// corpusDeclaredNames maps every construct name the runs declare, in their
// cases and fixtures, to the one run that declares it. A name more than one
// run declares maps to nil: it cannot say whose a diagnostic is.
func corpusDeclaredNames(runs []*corpusRun) map[string]*corpusRun {
	out := map[string]*corpusRun{}
	for _, r := range runs {
		for _, src := range []string{r.src, r.fixture} {
			for _, m := range corpusConstructHeader.FindAllStringSubmatch(src, -1) {
				if prev, seen := out[m[1]]; seen && prev != r {
					out[m[1]] = nil
					continue
				}
				out[m[1]] = r
			}
		}
	}
	return out
}

var corpusQuoted = regexp.MustCompile(`"([A-Za-z_][\w.-]*)"`)

// corpusRunNamed claims a diagnostic that names no domain -- a whole-tree
// refusal such as a policy entry that does not expand -- for the one run whose
// construct it quotes by name. The construct names the corpus looks up by bare
// name are unique across it (README), so a quoted name has one owner; a
// diagnostic quoting the constructs of two runs (an aggregated refusal that
// lists several prompts) is claimed by neither, and the batch falls back.
func corpusRunNamed(msg string, byName map[string]*corpusRun) (*corpusRun, bool) {
	var found *corpusRun
	for _, m := range corpusQuoted.FindAllStringSubmatch(msg, -1) {
		r := byName[m[1]]
		if r == nil {
			continue
		}
		if found != nil && found != r {
			return nil, false
		}
		found = r
	}
	return found, found != nil
}

func corpusIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func corpusAnyAutomation(runs []*corpusRun) bool {
	for _, r := range runs {
		if corpusAutomationDecl.MatchString(r.src) {
			return true
		}
	}
	return false
}

// corpusAutomationProblems mounts the tree, loads the automations the way a
// node does, and returns one line per problem the strict loader refuses.
func corpusAutomationProblems(t *testing.T, tree fs.FS) []string {
	t.Helper()
	_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, tree)
	defer func() {
		unmount()
		memoryNodes.ReplaceAll(nil)
		_, _ = memql.LoadUnifiedConcepts(corpusQuiet)
	}()
	if _, err := memql.LoadUnifiedConcepts(corpusQuiet); err != nil {
		return []string{"loading concepts for the automations pass: " + err.Error()}
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: corpusQuiet, Registry: memoryNodes.DefaultRegistry()})
	_, err := loader.LoadAll()
	if err == nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "- ") {
			lines = append(lines, strings.TrimPrefix(l, "- "))
		}
	}
	if len(lines) == 0 {
		lines = []string{err.Error()}
	}
	return lines
}

// ---- the expression batch -------------------------------------------------------

// corpusProbe boots one engine with every expression directory's fixture
// mounted and runs each lower / evaluate case through the adapter.
func corpusProbe(t *testing.T, runs []*corpusRun) {
	t.Helper()
	if len(runs) == 0 {
		return
	}
	tree := fstest.MapFS{}
	fixtureDomain := map[string]string{}
	for _, r := range runs {
		d := corpusDomainName(strings.TrimPrefix(r.dir, r.edition+"/"))
		fixtureDomain[r.dir] = d
		tree[d+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(r.line.Render())}
		tree[d+"/fixture.memql"] = &fstest.MapFile{Data: []byte(r.fixture)}
	}
	eng, stop, err := corpusProbeEngine(tree)
	if err != nil {
		for _, r := range runs {
			r.gotErr = fmt.Errorf("the expression fixtures do not load: %w", err)
		}
		return
	}
	defer stop()
	for _, r := range runs {
		env := ExprEnv{
			Row: r.c.Row, Args: r.c.Args, Actor: r.c.Actor,
			Concept: "v1:" + fixtureDomain[r.dir] + ":" + r.c.Concept,
			Engine:  eng,
		}
		expr := strings.TrimSpace(r.src)
		pos := tiers.Position(r.c.Position)
		if r.c.Verdict == verdictLower {
			r.got, r.gotErr = lower(pos, expr, env)
		} else {
			r.got, r.gotErr = evaluate(pos, expr, env)
		}
	}
}

// corpusProbeEngine mounts the fixtures and boots an engine over them. The
// returned stop restores the global tree and concept registry.
func corpusProbeEngine(tree fs.FS) (*memql.MemQLEngine, func(), error) {
	_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, tree)
	stop := func() {
		unmount()
		memoryNodes.ReplaceAll(nil)
		_, _ = memql.LoadUnifiedConcepts(corpusQuiet)
	}
	if _, err := memql.LoadUnifiedConcepts(corpusQuiet); err != nil {
		stop()
		return nil, nil, err
	}
	eng, err := memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		stop()
		return nil, nil, err
	}
	eng.Logger = corpusQuiet
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		stop()
		return nil, nil, err
	}
	return eng, stop, nil
}

// TestCorpusAttributionNamesOneCase pins the claim rules: a diagnostic goes to
// the one case it names, and to nobody when it names none or two -- whatever
// order the domain map yields.
func TestCorpusAttributionNamesOneCase(t *testing.T) {
	a := &corpusRun{domain: "cells_query_cache_1"}
	b := &corpusRun{domain: "cells_query_cache_10"}
	c := &corpusRun{domain: "cells_mutation_actor_1"}
	byDomain := map[string]*corpusRun{a.domain: a, b.domain: b, c.domain: c}
	for _, tc := range []struct {
		file, msg, want string
	}{
		{"cells_query_cache_10/case.memql", "anything", "cells_query_cache_10"},
		{"dsl/rules/rules.memql", "rule order: cells_mutation_actor_1/case.memql ties", "cells_mutation_actor_1"},
		{"", "concept \"v1:cells_query_cache_1:ticket\" DROPPED", "cells_query_cache_1"},
		{"", "cells_query_cache_10.openTickets declares @requiresRank(\"x\")", "cells_query_cache_10"},
		{"", "read cells_query_cache_10/x.tmpl: no such file", "cells_query_cache_10"}, // not read into _1
		{"", "cells_query_cache_1/case.memql and cells_mutation_actor_1/case.memql", ""},
		{"", "a whole-tree refusal naming no case", ""},
	} {
		for i := 0; i < 20; i++ { // map order varies run to run; the answer may not
			if got := corpusDomainOf(tc.file, tc.msg, byDomain); got != tc.want {
				t.Fatalf("corpusDomainOf(%q, %q) = %q, want %q", tc.file, tc.msg, got, tc.want)
			}
		}
	}

	x := &corpusRun{src: "policy anyLocalModel { }", domain: "d1"}
	y := &corpusRun{src: "prompt summariseA {\n}\n", domain: "d2"}
	z := &corpusRun{src: "prompt summariseB {\n}\n", domain: "d3"}
	byName := corpusDeclaredNames([]*corpusRun{x, y, z})
	if r, ok := corpusRunNamed(`policy "anyLocalModel": policy entry "fleet:*" is retired`, byName); !ok || r != x {
		t.Errorf("a refusal quoting one case's construct is that case's")
	}
	if _, ok := corpusRunNamed(`2 prompt(s): prompt "summariseA": ...; prompt "summariseB": ...`, byName); ok {
		t.Errorf("an aggregated refusal quoting two cases' constructs was claimed by one of them")
	}
}
