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
//     automations loader for an automation -- accepts it, or refuses it.
//   - lower / evaluate: the expression lowers to SQL containing the expected
//     text, or evaluates against a row to the expected value, through the
//     adapter in engine_adapter_test.go.
//
// A refusal is matched on its MESSAGE (text the diagnostic must contain) and,
// when the refusal carries one, its CODE (a stable rule id such as
// annotation_unknown). The wording is part of the contract: a refusal is how
// the language tells an author, or a model, what to write instead.
//
// Cases are loaded in batches -- one engine boot for every load case, one for
// every expression case -- because a boot costs over a second and the corpus
// holds hundreds of cases. Each case gets its own overlay domain, so a problem
// is attributed to the case by the domain it names. A boot error no domain
// claims (a whole-tree failure) makes the runner fall back to one boot per case
// for that batch, so attribution never guesses.

import (
	"bytes"
	"encoding/json"
	"errors"
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

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
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
	// Calls answers the construct calls an evaluate expression makes, by
	// "<kind> <name>" ("query openTickets"): the value the call returns. The
	// corpus boots no database, so a call's answer is the case's to give;
	// the construct must still be one the directory's fixture declares.
	Calls map[string]json.RawMessage `json:"calls,omitempty"`
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
	domain  string // the overlay domain the case loads in
	line    dslfs.Manifest

	file      *ast.File // the parsed case file, for a case that is a whole file
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
		r.file, r.parseErr = corpusParseFile(r.line.Edition, r.src)
	}
	corpusCheckConstructNames(t, runs)
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
	corpusCheckRegistered(t, loads)
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
		if rule := corpusRefusalRule(r.parseErr); rule != "" && r.c.Code == "" {
			return fmt.Sprintf("the refusal carries the rule id %q; name it in code -- the id is the part of the contract a reworded message keeps", rule)
		}
		return corpusMatchRefusal(r.c, []string{corpusRefusalText(r.parseErr)})
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

// corpusRefusalRule is the stable rule id a parse refusal carries -- today the
// retired-form refusals of the edition-2026 expression grammar -- or "".
func corpusRefusalRule(err error) string {
	var rf *langparser.RetiredFormError
	if errors.As(err, &rf) {
		return rf.Form.Rule
	}
	return ""
}

// corpusRefusalText is a parse refusal as a case's code and message are
// matched against it: the error's text, prefixed by its rule id when it has
// one.
func corpusRefusalText(err error) string {
	if rule := corpusRefusalRule(err); rule != "" {
		return "[" + rule + "] " + err.Error()
	}
	return err.Error()
}

// corpusParse is the parse half of a load: the edition's front end, then the
// parser every loader uses.
func corpusParse(edition, src string) error {
	_, err := corpusParseFile(edition, src)
	return err
}

// corpusParseFile is corpusParse returning the parsed file.
func corpusParseFile(edition, src string) (*ast.File, error) {
	fe, err := langparser.FrontEndFor(edition)
	if err != nil {
		return nil, err
	}
	prepared, err := fe.Prepare(src)
	if err != nil {
		return nil, err
	}
	return compiler.ParseFileSource(prepared)
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
			c: c, src: string(src), fixture: fixture, line: line,
			domain: fmt.Sprintf("%s_%d", base, i+1),
		})
	}
	entries, _ := fs.ReadDir(root, dir)
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
	if len(c.Calls) > 0 && c.Verdict != verdictEvaluate {
		return "calls answers the construct calls of an evaluate case; this case evaluates nothing"
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
		name := "case.memql"
		if corpusAutomationDecl.MatchString(r.src) {
			name = "automations.memql"
		}
		tree[r.domain+"/"+name] = &fstest.MapFile{Data: []byte(r.src)}
	}
	return tree
}

// corpusLoad runs every load case through the engine in one boot, falling
// back to one boot per case when a problem no domain claims makes the batch
// unattributable.
func corpusLoad(t *testing.T, runs []*corpusRun) {
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
	for _, r := range runs {
		r.loadDiags, r.loadRan = nil, false
		if rest := corpusLoadBatch(t, []*corpusRun{r}); len(rest) > 0 {
			r.loadDiags = append(r.loadDiags, rest...)
		}
	}
}

// corpusLoadBatch loads runs together and attributes every diagnostic to the
// run whose domain it names. It returns the diagnostics no run claims.
func corpusLoadBatch(t *testing.T, runs []*corpusRun) []string {
	t.Helper()
	tree := corpusTree(runs)
	byDomain := map[string]*corpusRun{}
	for _, r := range runs {
		byDomain[r.domain] = r
		r.loadRan = true
	}

	var unclaimed []string
	claim := func(file, msg string) {
		if r, ok := byDomain[corpusDomainOf(file, msg, byDomain)]; ok {
			r.loadDiags = append(r.loadDiags, msg)
			return
		}
		unclaimed = append(unclaimed, msg)
	}

	diags, _, err := memql.LintUnifiedTree(corpusQuiet, tree)
	if err != nil {
		unclaimed = append(unclaimed, err.Error())
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
	return unclaimed
}

// corpusDomainOf finds the run a diagnostic belongs to: by its file when it
// names one, else by the first domain the message mentions.
func corpusDomainOf(file, msg string, byDomain map[string]*corpusRun) string {
	if file != "" {
		f := strings.TrimPrefix(file, "unified:")
		if i := strings.IndexByte(f, '/'); i > 0 {
			return f[:i]
		}
	}
	best := ""
	for d := range byDomain {
		if strings.Contains(msg, d+"/") || strings.Contains(msg, ":"+d+":") {
			if len(d) > len(best) {
				best = d
			}
		}
	}
	return best
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

// ---- what a load proves -----------------------------------------------------------

// A load_ok verdict is only evidence if the engine read the case. Two ways it
// can pass having read nothing are ruled out here: a construct name another
// file already took (the engine's registries are flat and keep the first
// declaration, silently), and a construct no loader picked up at all -- a
// declaration form the loaders' slicers do not recognise raises no problem,
// because nothing ever parsed it.

// corpusDeclGroup names the registry a declaration lands in, "" for a
// declaration this check does not track. Concepts are not tracked: their ids
// carry the case's own overlay domain, so two cases never share one.
func corpusDeclGroup(def ast.Node) (group, name string) {
	switch d := def.(type) {
	case *ast.FunctionDef:
		if _, isAutomation := d.Body.(*ast.AutomationDef); isAutomation || d.Type == ast.FunctionTypeAutomation {
			return "automation", d.Name
		}
		return "function", d.Name
	case *ast.SpecDecl:
		return "spec", d.Name
	case *ast.ToolDecl:
		return "tool", d.Name
	case *ast.PromptDecl:
		return "prompt", d.Name
	case *ast.ShapeDecl:
		return "shape", d.Name
	}
	return "", ""
}

// corpusCheckConstructNames fails every construct name two files of the
// corpus declare. A directory's fixture counts once, however many of its cases
// load it, because every copy is the same declaration.
func corpusCheckConstructNames(t *testing.T, runs []*corpusRun) {
	t.Helper()
	owner := map[string]string{}
	record := func(file string, parsed *ast.File) {
		for _, def := range parsed.Definitions {
			group, name := corpusDeclGroup(def)
			if group == "" || name == "" {
				continue
			}
			key := group + " " + name
			if prev, taken := owner[key]; taken && prev != file {
				t.Errorf("%s %q is declared by both %s and %s: the engine's registries keep the first declaration and drop the other silently, so a case could pass having loaded nothing -- rename one", group, name, prev, file)
				continue
			}
			owner[key] = file
		}
	}
	loadsIn := map[string]int{}
	for _, r := range runs {
		if (r.c.Verdict == verdictLoadOK || r.c.Verdict == verdictRefuseLoad) && r.parseErr == nil {
			loadsIn[r.dir]++
		}
	}
	fixtures := map[string]bool{}
	for _, r := range runs {
		if r.fixture != "" && !fixtures[r.dir] {
			fixtures[r.dir] = true
			if f, err := corpusParseFile(r.line.Edition, r.fixture); err == nil && f != nil {
				record(r.dir+"/fixture.memql", f)
				corpusCheckSharedFixture(t, r.dir, f, loadsIn[r.dir])
			}
		}
		if r.file != nil {
			record(r.rel, r.file)
		}
	}
}

// corpusCheckSharedFixture fails a fixture that declares a query, mutation or
// logic when two or more load cases mount it. Each load case gets its own
// copy of the fixture in its own overlay domain, and a function declared in
// two domains has an ambiguous bare name -- so the function a mutation
// generates a tool for, or an automation step calls, no longer resolves, and
// the batch cannot load. A function one load case calls belongs in that case's
// file; a fixture shared by several holds what every case may share: concepts,
// specs and traits, and functions only the expression cases call.
func corpusCheckSharedFixture(t *testing.T, dir string, fixture *ast.File, loads int) {
	t.Helper()
	if loads < 2 {
		return
	}
	for _, def := range fixture.Definitions {
		if fn, ok := def.(*ast.FunctionDef); ok {
			switch fn.Type {
			case ast.FunctionTypeQuery, ast.FunctionTypeMutation, ast.FunctionTypeLogic:
				t.Errorf("%s/fixture.memql declares %s %q, and %d load cases mount the fixture: its bare name is ambiguous across their copies -- declare it in the one case file that calls it", dir, fn.Type, fn.Name, loads)
			}
		}
	}
}

// corpusCheckRegistered holds each load_ok case that loaded with no problem to
// having registered what it declares: its queries, mutations and logic, its
// specs and traits, its tools and its concepts. It boots the cases again,
// together, and looks each declaration up in the engine that booted. A
// declaration the engine does not hold is a problem on the case: the loaders
// never read it, so the load proved nothing about it.
func corpusCheckRegistered(t *testing.T, runs []*corpusRun) {
	t.Helper()
	var oks []*corpusRun
	for _, r := range runs {
		if r.c.Verdict == verdictLoadOK && r.loadRan && len(r.loadDiags) == 0 && r.file != nil {
			oks = append(oks, r)
		}
	}
	if len(oks) == 0 {
		return
	}
	eng, stop, err := corpusProbeEngine(corpusTree(oks))
	if err != nil {
		for _, r := range oks {
			r.loadDiags = append(r.loadDiags, "the cases that loaded did not boot again to be checked for what they registered: "+err.Error())
		}
		return
	}
	defer stop()
	for _, r := range oks {
		for _, def := range r.file.Definitions {
			if missing := corpusUnregistered(eng, r.domain, def); missing != "" {
				r.loadDiags = append(r.loadDiags, missing+": no loader read it, so loading the file proved nothing about it")
			}
		}
	}
}

// corpusUnregistered names a declaration the booted engine does not hold, or
// returns "". A @disabled declaration is skipped at load by design.
func corpusUnregistered(eng *memql.MemQLEngine, domain string, def ast.Node) string {
	switch d := def.(type) {
	case *ast.FunctionDef:
		if corpusDisabled(d.Attributes) {
			return ""
		}
		switch d.Type {
		case ast.FunctionTypeQuery, ast.FunctionTypeMutation, ast.FunctionTypeLogic:
			if !eng.Functions().Has(d.Name) {
				return fmt.Sprintf("the engine registered no %s %q", d.Type, d.Name)
			}
		}
	case *ast.SpecDecl:
		kind := "spec"
		if d.IsTrait {
			kind = "trait"
		}
		if !corpusDisabled(d.Attributes) && !eng.Specs().Has(d.Name) && !eng.Specs().IsDisabled(d.Name) {
			return fmt.Sprintf("the engine registered no %s %q", kind, d.Name)
		}
	case *ast.ToolDecl:
		if !d.Disabled && !eng.Tools().Has(d.Name) {
			return fmt.Sprintf("the engine registered no tool %q", d.Name)
		}
	case *ast.ConceptDecl:
		id := "v1:" + domain + ":" + d.Name
		if c, err := memoryNodes.DefaultRegistry().Get(id); err != nil || c == nil {
			return fmt.Sprintf("the engine registered no concept %q", id)
		}
	}
	return ""
}

func corpusDisabled(attrs []*ast.Attribute) bool {
	for _, a := range attrs {
		if a != nil && a.Name == ast.AttrDisabled {
			return true
		}
	}
	return false
}

// ---- the expression batch -------------------------------------------------------

// corpusProbe boots one engine with every expression directory's fixture
// mounted and runs each lower / evaluate case through the adapter. When the
// fixtures do not boot together it boots them one directory at a time, so a
// fixture that does not load fails its own directory's cases and no other.
func corpusProbe(t *testing.T, runs []*corpusRun) {
	t.Helper()
	if len(runs) == 0 {
		return
	}
	byDir := map[string][]*corpusRun{}
	var dirs []string
	for _, r := range runs {
		if _, seen := byDir[r.dir]; !seen {
			dirs = append(dirs, r.dir)
		}
		byDir[r.dir] = append(byDir[r.dir], r)
	}
	if corpusProbeBatch(runs) == nil || len(dirs) == 1 {
		return
	}
	for _, d := range dirs {
		for _, r := range byDir[d] {
			r.got, r.gotErr = nil, nil
		}
		_ = corpusProbeBatch(byDir[d])
	}
}

// corpusProbeBatch boots one engine over the fixtures of runs' directories and
// answers every run through the adapter. A boot that fails is every run's
// answer, and is returned.
func corpusProbeBatch(runs []*corpusRun) error {
	tree := fstest.MapFS{}
	fixtureDomain := map[string]string{}
	predicates := map[string]func(string) (string, ast.ExpressionNode, bool){}
	for _, r := range runs {
		if _, seen := fixtureDomain[r.dir]; seen {
			continue
		}
		d := corpusDomainName(strings.TrimPrefix(r.dir, r.edition+"/"))
		fixtureDomain[r.dir] = d
		predicates[r.dir] = adapterPredicates(r.line.Edition, r.fixture)
		tree[d+"/"+dslfs.ManifestFile] = &fstest.MapFile{Data: []byte(r.line.Render())}
		tree[d+"/fixture.memql"] = &fstest.MapFile{Data: []byte(r.fixture)}
	}
	eng, stop, err := corpusProbeEngine(tree)
	if err != nil {
		err = fmt.Errorf("the expression fixtures do not load: %w", err)
		for _, r := range runs {
			r.gotErr = err
		}
		return err
	}
	defer stop()
	for _, r := range runs {
		env := ExprEnv{
			Row: r.c.Row, Args: r.c.Args, Actor: r.c.Actor, Calls: r.c.Calls,
			Concept:    "v1:" + fixtureDomain[r.dir] + ":" + r.c.Concept,
			Engine:     eng,
			Predicates: predicates[r.dir],
		}
		expr := strings.TrimSpace(r.src)
		pos := tiers.Position(r.c.Position)
		if r.c.Verdict == verdictLower {
			r.got, r.gotErr = lower(pos, expr, env)
		} else {
			r.got, r.gotErr = evaluate(pos, expr, env)
		}
	}
	return nil
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
