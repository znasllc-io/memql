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
//   - refuse_parse: the edition's front end plus the parser every loader uses
//     refuse the file (corpusParse).
//   - load_ok / refuse_load: the file parses (compiler.ParseFileSource, the
//     loaders' own parse) and the engine's own boot-time validation -- MemQLEngine.Init over the embedded tree with the
//     case mounted as an overlay domain, via memql.LintUnifiedTree, plus the
//     automations loader for an automation and the capability catalog and
//     action loader for a capability or an action (corpusActionProblems) --
//     accepts it, or refuses it.
//
// ONE GRAMMAR. The loaders parse only the edition-2026 expression grammar, so
// they read the corpus in the edition it is written in, and refuse the retired
// spellings (a filter with no lambda header, a spec's `{ return }` body, a
// raw-text @filter) as the edition does.
//   - lower / evaluate: the expression lowers to SQL containing the expected
//     text, or evaluates against a row to the expected value, through the
//     adapter in engine_adapter_test.go -- or, for an evaluate case naming a
//     `call`, a logic the file declares runs to it (corpus_call_test.go).
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

	"github.com/znasllc-io/memql/component/actions"
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
	// Call, on an evaluate case, names a logic the case file declares: the
	// file loads like a load case, and the logic runs with Args, returning
	// Expect (corpus_call_test.go). Without it an evaluate case's file is a
	// bare expression.
	Call string `json:"call,omitempty"`
	// Note says why the case exists. Not checked.
	Note string `json:"note,omitempty"`
}

// corpusVersionManifest is an edition directory's manifest.json.
type corpusVersionManifest struct {
	Edition  string `json:"edition"`
	Language string `json:"language"`
	// Status was "draft" until the freeze epic (dsl-v1-freeze, memql#5390)
	// flipped it to "frozen". TestCorpusManifestMatchesTheEngine now requires
	// the edition this engine WRITES to be frozen; a later edition being
	// drafted lives in its own directory and may still say "draft".
	Status string `json:"status"`
	// FrozenAt is the date the edition froze and Note says what the freeze
	// means for anyone changing a case. Both are read by people rather than by
	// code -- but readCorpusManifest sets DisallowUnknownFields, so they are
	// declared here or the manifest does not parse at all.
	FrozenAt string `json:"frozenAt,omitempty"`
	Note     string `json:"note,omitempty"`
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
		switch {
		case r.c.Verdict == verdictRefuseParse:
			r.parseErr = corpusParse(r.line.Edition, r.src)
		case r.c.Verdict == verdictLoadOK || r.c.Verdict == verdictRefuseLoad || r.c.Call != "":
			// A logic an evaluate case calls loads like a load case first.
			r.file, r.parseErr = corpusParseFile(r.line.Edition, r.src)
		}
	}
	corpusCheckConstructNames(t, runs)
	var loads, probes, calls []*corpusRun
	for _, r := range runs {
		switch {
		case r.c.Call != "":
			// A logic to run loads like a load case first.
			if r.parseErr == nil {
				loads = append(loads, r)
			}
			calls = append(calls, r)
		case r.c.Verdict == verdictLoadOK || r.c.Verdict == verdictRefuseLoad:
			if r.parseErr == nil {
				loads = append(loads, r)
			}
		case r.c.Verdict == verdictLower || r.c.Verdict == verdictEvaluate:
			probes = append(probes, r)
		}
	}
	corpusLoad(t, loads)
	corpusCheckRegistered(t, loads)
	corpusProbe(t, probes)
	corpusCall(t, calls)

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
		if msg := corpusMatchRefusal(r.c, r.loadDiags); msg != "" {
			return msg
		}
		// The rule a parse refusal answers to, at load (D24): a refusal that
		// carries a rule id must have it named in code.
		if rule := corpusLoadRule(r.c.Message, r.loadDiags); rule != "" && r.c.Code == "" {
			return fmt.Sprintf("the refusal carries the rule id %q; name it in code -- the id is the part of the contract a reworded message keeps", rule)
		}
	case verdictLower:
		if r.gotErr != nil {
			return "did not lower: " + r.gotErr.Error()
		}
		sql, _ := r.got.(string)
		if !strings.Contains(sql, r.c.SQL) {
			return fmt.Sprintf("lowered to %q, which does not contain %q", sql, r.c.SQL)
		}
	case verdictEvaluate:
		if r.c.Call != "" {
			switch {
			case r.parseErr != nil:
				return "the parser refused the file: " + r.parseErr.Error()
			case !r.loadRan:
				return "the load batch did not run this case"
			case len(r.loadDiags) > 0:
				return "the engine refused the file at load:\n    " + strings.Join(r.loadDiags, "\n    ")
			}
		}
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

// corpusLoadRuleRe is a rule id as a load diagnostic prints it: last, in
// brackets -- the convention the annotation registry's refusals and the
// lowering's (LowerError) share, so a caller that prefixes context leaves the
// code findable.
var corpusLoadRuleRe = regexp.MustCompile(`\[([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\]\s*$`)

// corpusLoadRule is the rule id the load diagnostic carrying message ends
// with, or "" when it carries none.
func corpusLoadRule(message string, diags []string) string {
	for _, d := range diags {
		if !strings.Contains(d, message) {
			continue
		}
		if m := corpusLoadRuleRe.FindStringSubmatch(d); m != nil {
			return m[1]
		}
	}
	return ""
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
	editions, err := corpusEditions(root)
	if err != nil {
		t.Fatalf("read test/conformance: %v", err)
	}
	var runs []*corpusRun
	domains := map[string]string{}
	for _, edition := range editions {
		vm := readCorpusManifest(t, root, edition)
		line := dslfs.Manifest{Language: vm.Language, Edition: vm.Edition}
		dirs, err := corpusExpectDirsIn(root, edition)
		if err != nil {
			t.Fatalf("walk %s: %v", edition, err)
		}
		for _, dir := range dirs {
			runs = append(runs, readCorpusDir(t, root, edition, dir, line, domains)...)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].rel < runs[j].rel })
	return runs
}

// corpusEditions lists the edition directories under test/conformance, in
// name order. It is shared with the refusal-wording pins
// (refusal_wording_test.go): two readers of the corpus that disagreed about
// WHICH directories are the corpus would each be correct about a different
// set, and the narrower one would pass by matching nothing.
func corpusEditions(root fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && corpusEditionDir.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// corpusExpectDirsIn lists every directory under one edition that holds an
// expect.json, in path order. Shared with the refusal-wording pins.
func corpusExpectDirsIn(root fs.FS, edition string) ([]string, error) {
	var out []string
	err := fs.WalkDir(root, edition, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || path.Base(p) != "expect.json" {
			return nil
		}
		out = append(out, path.Dir(p))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// readCorpusExpect decodes one directory's expect.json. It is the ONE decoder
// for that file format -- readCorpusDir builds the runner's cases from it and
// the refusal-wording pins read the same file through it. A second decoder
// would be a second answer about what the corpus says, and the two would
// disagree first about a field one of them had not heard of, silently, since
// only this one refuses unknown fields.
func readCorpusExpect(root fs.FS, dir string) (corpusExpectFile, error) {
	data, err := fs.ReadFile(root, dir+"/expect.json")
	if err != nil {
		return corpusExpectFile{}, fmt.Errorf("read %s/expect.json: %w", dir, err)
	}
	var ef corpusExpectFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ef); err != nil {
		return corpusExpectFile{}, fmt.Errorf("%s/expect.json: %w", dir, err)
	}
	return ef, nil
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
	ef, err := readCorpusExpect(root, dir)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(ef.Cases) == 0 {
		t.Fatalf("%s/expect.json lists no cases", dir)
	}
	fixture := ""
	if b, err := fs.ReadFile(root, dir+"/fixture.memql"); err == nil {
		fixture = string(b)
	}
	sidecars, memqlFiles, err := corpusDirFiles(root, dir)
	if err != nil {
		t.Fatalf("%v", err)
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
	for _, rel := range memqlFiles {
		if !named[rel] {
			t.Fatalf("%s/%s is in the corpus but no case in expect.json names it", dir, rel)
		}
	}
	return runs
}

// corpusDirFiles reads one case directory's files: the SIDECARS (everything
// that is neither expect.json nor a .memql, keyed by its path relative to the
// directory) and the relative paths of every .memql it holds, for the stray
// check above.
//
// It descends into subdirectories, because `@templateFile("prompts/x.tmpl")`
// is the layout every one of the tree's 25 shipped prompts uses and the
// MEMQL_DSL_PATH layout publishes. A reader that could not carry a nested
// sidecar would force a corpus-backed docs example to teach a flat path
// instead -- the gate reshaping the documentation to fit the gate (memql#5388).
//
// It does NOT descend into a nested CASE directory -- one with an expect.json
// of its own, which corpusExpectDirsIn returns separately and readCorpusDir is
// called for in its own right. Those files are that case's, not this one's;
// walking into them would mount a sibling case's fixture beside this one and
// swallow its expect.json as a sidecar.
//
// The stray property is kept and WIDENED rather than weakened: a .memql at any
// depth must be named by a case, and since a case's `file` may not contain a
// slash (corpusValidateFile), a nested .memql is always a stray and always
// fatal. A sidecar is mounted without having to be referenced, at any depth,
// exactly as a top-level one always has been -- cells/prompt/templateFile
// leans on that, holding a .tmpl one case names and another case must not find.
func corpusDirFiles(root fs.FS, dir string) (map[string][]byte, []string, error) {
	var sidecars map[string][]byte
	var memqlFiles []string
	err := fs.WalkDir(root, dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if d.IsDir() {
			if rel == "" {
				return nil
			}
			if _, err := fs.Stat(root, p+"/expect.json"); err == nil {
				return fs.SkipDir // a case directory of its own
			}
			return nil
		}
		switch {
		case rel == "expect.json":
			return nil
		case strings.HasSuffix(rel, ".memql"):
			memqlFiles = append(memqlFiles, rel)
			return nil
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if sidecars == nil {
			sidecars = map[string][]byte{}
		}
		sidecars[rel] = b
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(memqlFiles)
	return sidecars, memqlFiles, nil
}

func corpusValidateCase(dir string, c corpusCase) string {
	if c.Call != "" && c.Verdict != verdictEvaluate {
		return "call names a logic to run, which only an evaluate case does"
	}
	if len(c.Calls) > 0 && c.Verdict != verdictEvaluate {
		return "calls answers the construct calls of an evaluate case; this case evaluates nothing"
	}
	switch c.Verdict {
	case verdictLoadOK:
	case verdictRefuseParse, verdictRefuseLoad:
		if c.Message == "" {
			return "a refusal names the text its diagnostic must contain (message); the wording is part of the contract"
		}
	case verdictEvaluate:
		if c.Call == "" {
			return corpusValidateExpression(dir, c)
		}
		if len(c.Expect) == 0 {
			return "an evaluate case names the value it must produce (expect)"
		}
	case verdictLower:
		return corpusValidateExpression(dir, c)
	default:
		return fmt.Sprintf("verdict %q is not one of load_ok, refuse_parse, refuse_load, lower, evaluate", c.Verdict)
	}
	return corpusValidateFile(c)
}

// corpusValidateExpression checks a lower or evaluate case whose file is a
// bare expression.
func corpusValidateExpression(dir string, c corpusCase) string {
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
	return corpusValidateFile(c)
}

// corpusValidateFile checks that a case names a case file of its directory.
func corpusValidateFile(c corpusCase) string {
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
// `query ticket openTickets {`, `capability fs.list {`, `seed skill a-b {`,
// and the brace-less edition-2026 spec or trait, `spec ticket isOpen = row =>`
// and `trait isOpen = row =>`.
var corpusConstructHeader = regexp.MustCompile(`(?m)^[ \t]*(?:query|mutation|logic|automation|action|capability|spec|trait|tool|builtin|prompt|provider|shape|policy|rule|seed|concept)[ \t]+(?:[A-Za-z_][\w]*[ \t]+)?([A-Za-z_][\w.-]*)[ \t]*(?:\{|=)`)

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
	eng, initErr := automations.NewOfflineEngine(corpusQuiet, memoryNodes.DefaultRegistry())
	if initErr != nil {
		return []string{"loading functions for loop analysis: " + initErr.Error()}
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: corpusQuiet, Registry: memoryNodes.DefaultRegistry(), Functions: eng.Functions()})
	_, err := loader.LoadAll()
	if err == nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, strings.TrimPrefix(l, "- "))
		} else if l != "" && len(lines) > 0 {
			// Loop refusals include the cycle, origins and remedy on separate
			// lines. Keep their trailing rule code with the same diagnostic.
			lines[len(lines)-1] += "\n" + l
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
// file already took (two domains declaring one name make every bare lookup of
// it ambiguous, and the load says nothing about which declaration a call
// reaches), and a construct no loader picked up at all -- a declaration form
// the loaders' slicers do not recognise raises no problem, because nothing
// ever parsed it.

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
// corpus declare where both can be in one boot. A directory's fixture counts
// once, however many of its cases load it, because every copy is the same
// declaration. Two CASE files of one directory may declare one name -- the
// accepted form of a query beside a refused form of it -- because no two cases
// of a directory share a boot (corpusRounds); a case may not repeat a name its
// own fixture declares, since the two load in one domain.
func corpusCheckConstructNames(t *testing.T, runs []*corpusRun) {
	t.Helper()
	type decl struct{ file, dir string }
	owner := map[string]decl{}
	record := func(file, dir string, fixture bool, parsed *ast.File) {
		for _, def := range parsed.Definitions {
			group, name := corpusDeclGroup(def)
			if group == "" || name == "" {
				continue
			}
			key := group + " " + name
			if prev, taken := owner[key]; taken && prev.file != file {
				sameDirCases := prev.dir == dir && !fixture && path.Base(prev.file) != "fixture.memql"
				if !sameDirCases {
					t.Errorf("%s %q is declared by both %s and %s: two domains declaring one name make every bare lookup of it ambiguous, so a case could pass without the engine reading the declaration it names -- rename one", group, name, prev.file, file)
				}
				continue
			}
			owner[key] = decl{file: file, dir: dir}
		}
	}
	fixtures := map[string]bool{}
	for _, r := range runs {
		if r.fixture != "" && !fixtures[r.dir] {
			fixtures[r.dir] = true
			if f, err := corpusParseFile(r.line.Edition, r.fixture); err == nil && f != nil {
				record(r.dir+"/fixture.memql", r.dir, true, f)
			}
		}
		if r.file != nil {
			record(r.rel, r.dir, false, r.file)
		}
	}
}

// corpusCheckRegistered holds each load_ok case that loaded with no problem to
// having registered what it declares: its queries, mutations and logic, its
// specs and traits, its tools and its concepts. It boots the cases again --
// in the load's rounds, so no two cases of one directory share a boot
// (corpusRounds) -- and looks each declaration up in the engine that booted.
// A declaration the engine does not hold is a problem on the case: the
// loaders never read it, so the load proved nothing about it.
func corpusCheckRegistered(t *testing.T, runs []*corpusRun) {
	t.Helper()
	var oks []*corpusRun
	for _, r := range runs {
		if r.c.Verdict == verdictLoadOK && r.loadRan && len(r.loadDiags) == 0 && r.file != nil {
			oks = append(oks, r)
		}
	}
	for _, round := range corpusRounds(oks) {
		corpusCheckRegisteredRound(round)
	}
}

// corpusCheckRegisteredRound boots one round of loaded cases and checks each
// declaration of each against the engine.
func corpusCheckRegisteredRound(oks []*corpusRun) {
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
		id := "v1:" + corpusConceptNamespace(domain, d.Attributes) + ":" + d.Name
		if c, err := memoryNodes.DefaultRegistry().Get(id); err != nil || c == nil {
			return fmt.Sprintf("the engine registered no concept %q", id)
		}
	}
	return ""
}

// corpusConceptNamespace is the namespace a concept's canonical id carries:
// its @namespace when it names one (a domain's namespace.pin admits it), else
// the domain it loads in.
func corpusConceptNamespace(domain string, attrs []*ast.Attribute) string {
	for _, a := range attrs {
		if a == nil || a.Name != "namespace" {
			continue
		}
		if ns, ok := a.Value.(string); ok && strings.TrimSpace(ns) != "" {
			return strings.TrimSpace(ns)
		}
	}
	return domain
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
	w := &corpusRun{src: "trait isOpenTicket = row => row.status == \"open\"\n", domain: "d4"}
	byName := corpusDeclaredNames([]*corpusRun{x, y, z, w})
	if r, ok := corpusRunNamed(`policy "anyLocalModel": policy entry "fleet:*" is retired`, byName); !ok || r != x {
		t.Errorf("a refusal quoting one case's construct is that case's")
	}
	if r, ok := corpusRunNamed(`trait "isOpenTicket" (lower): does not lower`, byName); !ok || r != w {
		t.Errorf("a brace-less edition-2026 trait is a construct its case declares")
	}
	if _, ok := corpusRunNamed(`2 prompt(s): prompt "summariseA": ...; prompt "summariseB": ...`, byName); ok {
		t.Errorf("an aggregated refusal quoting two cases' constructs was claimed by one of them")
	}
}

// TestCorpusDirFilesCarriesNestedSidecars pins what the case-directory reader
// does with a subdirectory: a nested sidecar is carried under its relative path
// (so `@templateFile("prompts/x.tmpl")` -- the layout every shipped prompt uses
// -- is expressible as a case), a nested .memql is reported as a file the
// directory holds (and therefore a stray, since a case's `file` may not contain
// a slash), and a nested CASE directory is not descended into at all.
func TestCorpusDirFilesCarriesNestedSidecars(t *testing.T) {
	root := fstest.MapFS{
		"d/expect.json":                 {Data: []byte(`{"cases":[]}`)},
		"d/case.memql":                  {Data: []byte("concept a { }")},
		"d/fixture.memql":               {Data: []byte("concept b { }")},
		"d/namespace.pin":               {Data: []byte("probe")},
		"d/prompts/summarise.tmpl":      {Data: []byte("hello {{ .x }}")},
		"d/prompts/deep/deeper.tmpl":    {Data: []byte("deep")},
		"d/stray/orphan.memql":          {Data: []byte("concept c { }")},
		"d/nested/expect.json":          {Data: []byte(`{"cases":[]}`)},
		"d/nested/case.memql":           {Data: []byte("concept d { }")},
		"d/nested/prompts/its-own.tmpl": {Data: []byte("not the parent's")},
	}
	sidecars, memqlFiles, err := corpusDirFiles(root, "d")
	if err != nil {
		t.Fatalf("corpusDirFiles: %v", err)
	}

	wantSidecars := []string{"namespace.pin", "prompts/deep/deeper.tmpl", "prompts/summarise.tmpl"}
	var gotSidecars []string
	for k := range sidecars {
		gotSidecars = append(gotSidecars, k)
	}
	sort.Strings(gotSidecars)
	if strings.Join(gotSidecars, ",") != strings.Join(wantSidecars, ",") {
		t.Errorf("sidecars = %v, want %v -- a nested sidecar is carried under its relative path, and a nested "+
			"CASE directory's files belong to that case", gotSidecars, wantSidecars)
	}
	if got := string(sidecars["prompts/summarise.tmpl"]); got != "hello {{ .x }}" {
		t.Errorf("nested sidecar content = %q", got)
	}

	// The stray check in readCorpusDir consumes this list and fatals on a name
	// no case declares. "stray/orphan.memql" can never be declared -- a case's
	// `file` may not contain a slash -- so a nested .memql is always fatal, and
	// "nested/case.memql" must not appear at all or it would be fatal for the
	// wrong directory.
	want := []string{"case.memql", "fixture.memql", "stray/orphan.memql"}
	if strings.Join(memqlFiles, ",") != strings.Join(want, ",") {
		t.Errorf("memqlFiles = %v, want %v", memqlFiles, want)
	}
	if corpusValidateFile(corpusCase{File: "stray/orphan.memql", Verdict: verdictLoadOK}) == "" {
		t.Error("a case naming a nested .memql must be refused, or a nested stray could be named into legitimacy")
	}
}
