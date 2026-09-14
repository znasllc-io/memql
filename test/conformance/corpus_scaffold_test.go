package conformance

// corpus_scaffold_test.go -- a skeleton writer for the corpus's registry
// cells (memql#5361). It is NOT a gate: it runs only when asked,
//
//	go test ./test/conformance -run TestScaffoldCorpusCells -scaffold
//
// and for every annotation placement with no cell directory yet it writes one:
// a fixture the construct leans on, a case that carries the placement in the
// form the registry accepts, a case that carries it in a form the registry
// refuses, and an expect.json pinning the refusal to the code and the opening
// words of the message annotations.Check gives for that form.
//
// It NEVER touches a cell that exists -- not one file in it -- so a cell a
// person has corrected cannot be overwritten, and a new placement gets a
// starting point rather than a blank page. What it writes is a skeleton: run
// the runner (TestCorpusVerdicts), read every case it wrote as an author would,
// and correct what the engine or a reader would object to. A skeleton that
// loads is not yet a case: it has to load for the right reason.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

var scaffoldCorpusCells = flag.Bool("scaffold", false,
	"write a skeleton cell for every annotation placement that has no cell under test/conformance/<edition>/cells/ (never overwrites)")

// TestScaffoldCorpusCells writes the missing cells. Skipped unless -scaffold.
func TestScaffoldCorpusCells(t *testing.T) {
	if !*scaffoldCorpusCells {
		t.Skip("writes corpus cell skeletons; run with -scaffold")
	}
	wrote := 0
	for _, p := range annotations.Placements() {
		dir := filepath.Join(langparser.Edition, filepath.FromSlash(corpusCellDir(p)))
		if _, err := os.Stat(dir); err == nil {
			continue // a cell that exists is a person's; never touch it
		}
		files, err := scaffoldCell(p)
		if err != nil {
			t.Errorf("%s: %v", corpusCellDir(p), err)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			target := filepath.Join(dir, name)
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				t.Fatalf("%s: %v", target, err) // O_EXCL: never overwrite
			}
			if _, err := f.WriteString(content); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		wrote++
		t.Logf("wrote %s", dir)
	}
	t.Logf("%d cell(s) written; read every one before committing it", wrote)
}

// scaffoldCell renders one placement's cell as file name -> content.
func scaffoldCell(p annotations.Placement) (map[string]string, error) {
	if msg := scaffoldUnmapped(p); msg != "" {
		return nil, errors.New(msg)
	}
	mistake := scaffoldMistakeFor(p)
	ref := annotations.Check(p.Receiver, mistake.use)
	if ref == nil {
		return nil, fmt.Errorf("the registry accepts %s on %s, so it is no mistake", mistake.text, p.Receiver)
	}

	good := scaffoldAnnotation(p)
	fixture, goodSrc, sidecars := scaffoldSkeleton(p, good)
	_, badSrc, _ := scaffoldSkeleton(p, mistake.text)

	goodFile := scaffoldAcceptedFile(p)
	verdict := verdictRefuseParse
	if scaffoldCheckedAtLoad(p.Receiver) {
		verdict = verdictRefuseLoad
	}
	ef := corpusExpectFile{Cases: []corpusCase{
		{File: goodFile, Verdict: verdictLoadOK},
		{File: mistake.file, Verdict: verdict, Code: ref.Code, Message: scaffoldMessagePrefix(ref.Message)},
	}}
	expect, err := json.MarshalIndent(ef, "", "  ")
	if err != nil {
		return nil, err
	}

	files := map[string]string{
		goodFile:      goodSrc,
		mistake.file:  badSrc,
		"expect.json": string(expect) + "\n",
	}
	if fixture != "" {
		files["fixture.memql"] = fixture
	}
	for name, content := range sidecars {
		files[name] = content
	}
	return files, nil
}

// scaffoldCheckedAtLoad reports whether the receiver's annotations are checked
// by the concept translator at load rather than by the parser.
func scaffoldCheckedAtLoad(r annotations.Receiver) bool {
	return r == annotations.Concept || r == annotations.ConceptBody || r == annotations.ConceptField
}

// scaffoldMessagePrefix is the part of a refusal worth pinning: the words that
// name the receiver, the annotation and the rule, before the example and the
// echo of what was written.
func scaffoldMessagePrefix(msg string) string {
	for _, cut := range []string{", as in ", " -- "} {
		if i := strings.Index(msg, cut); i > 0 {
			return msg[:i]
		}
	}
	return msg
}

// ---- the mistake ------------------------------------------------------------

type scaffoldMistake struct {
	text string // the annotation as the mistaken author wrote it
	use  annotations.Use
	file string
}

var scaffoldWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// scaffoldMistakeFor picks, for a placement, a form the registry refuses and
// that an author plausibly writes: a flag given `true`, a number quoted, a
// word left unquoted, a keyword annotation given a positional value.
func scaffoldMistakeFor(p annotations.Placement) scaffoldMistake {
	args := scaffoldExampleArgs(scaffoldAnnotation(p))
	switch {
	case p.Forms == annotations.FormFlag:
		return scaffoldMistake{"@" + p.Name + "(true)", annotations.Use{Name: p.Name, Form: annotations.FormBool}, "with-true.memql"}

	case p.Forms&annotations.FormKeywords != 0 && p.Forms&(annotations.FormString|annotations.FormNumber) == 0:
		// A keyword annotation given its first value positionally.
		if v := scaffoldFirstKeywordValue(args); v != "" {
			if strings.HasPrefix(v, `"`) {
				return scaffoldMistake{"@" + p.Name + "(" + v + ")", annotations.Use{Name: p.Name, Form: annotations.FormString}, "with-a-positional-value.memql"}
			}
			return scaffoldMistake{"@" + p.Name + "(" + v + ")", annotations.Use{Name: p.Name, Form: annotations.FormNumber}, "with-a-positional-value.memql"}
		}

	case p.Forms&annotations.FormNumber != 0 && p.Forms&annotations.FormString == 0:
		// A number, quoted.
		n := strings.Split(args, ",")[0]
		if strings.Contains(n, "=") {
			n = strings.TrimSpace(n[strings.Index(n, "=")+1:])
		}
		n = strings.Trim(strings.TrimSpace(n), `"`)
		return scaffoldMistake{"@" + p.Name + `("` + n + `")`, annotations.Use{Name: p.Name, Form: annotations.FormString}, "with-a-quoted-number.memql"}

	case p.Forms&annotations.FormKeywords != 0 && p.Forms&annotations.FormString != 0:
		// @schedule: a keyword form and a string; a bare number is neither.
		return scaffoldMistake{"@" + p.Name + "(3600)", annotations.Use{Name: p.Name, Form: annotations.FormNumber}, "with-a-number.memql"}

	case p.Forms&annotations.FormExpression != 0:
		return scaffoldMistake{"@" + p.Name + "()", annotations.Use{Name: p.Name, Form: annotations.FormEmpty}, "with-empty-parentheses.memql"}
	}

	// A string placement: a number, unquoted, when the value is one; the
	// value, unquoted, when it is a word; otherwise the annotation with its
	// value left off.
	if n := strings.Trim(strings.TrimSpace(args), `"`); n != "" && strings.Trim(n, "0123456789") == "" {
		return scaffoldMistake{"@" + p.Name + "(" + n + ")", annotations.Use{Name: p.Name, Form: annotations.FormNumber}, "with-a-number.memql"}
	}
	var words []string
	for _, a := range strings.Split(args, ",") {
		a = strings.TrimSpace(a)
		if len(a) < 2 || !strings.HasPrefix(a, `"`) || !strings.HasSuffix(a, `"`) {
			words = nil
			break
		}
		w := strings.Trim(a, `"`)
		if !scaffoldWord.MatchString(w) {
			words = nil
			break
		}
		words = append(words, w)
	}
	if len(words) > 0 {
		keys := make([]annotations.WrittenKey, 0, len(words))
		for _, w := range words {
			keys = append(keys, annotations.WrittenKey{Name: w, Bare: true})
		}
		file := "with-an-unquoted-value.memql"
		if len(words) > 1 {
			file = "with-unquoted-values.memql"
		}
		return scaffoldMistake{"@" + p.Name + "(" + strings.Join(words, ", ") + ")", annotations.Use{Name: p.Name, Form: annotations.FormKeywords, Keys: keys}, file}
	}
	return scaffoldMistake{"@" + p.Name, annotations.Use{Name: p.Name, Form: annotations.FormFlag}, "with-no-value.memql"}
}

// scaffoldExampleArgs returns what is inside the example's parentheses.
func scaffoldExampleArgs(example string) string {
	open := strings.IndexByte(example, '(')
	if open < 0 || !strings.HasSuffix(example, ")") {
		return ""
	}
	return example[open+1 : len(example)-1]
}

// scaffoldFirstKeywordValue returns the value of the first key=value pair.
func scaffoldFirstKeywordValue(args string) string {
	for _, a := range strings.Split(args, ",") {
		if i := strings.IndexByte(a, '='); i > 0 {
			return strings.TrimSpace(a[i+1:])
		}
	}
	return ""
}

// scaffoldAcceptedFile names the accepted case after the form it shows.
func scaffoldAcceptedFile(p annotations.Placement) string {
	args := scaffoldExampleArgs(scaffoldAnnotation(p))
	switch {
	case !strings.Contains(scaffoldAnnotation(p), "("):
		return "written-bare.memql"
	case p.Forms&annotations.FormExpression != 0:
		return "an-expression.memql"
	case strings.Contains(args, "="):
		return "keyword-arguments.memql"
	case strings.HasPrefix(strings.TrimSpace(args), `"`) && strings.Count(args, `"`) > 2:
		return "a-list-of-strings.memql"
	case strings.HasPrefix(strings.TrimSpace(args), `"`):
		return "a-string.memql"
	}
	return "a-number.memql"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// TestScaffoldRefusesAPlacementItHasNoSkeletonFor: a capability, rule or seed
// placement the per-name tables do not name is an error naming the placement
// and the table to extend -- not a skeleton with an empty verb, precedence or
// concept in it. Runs without -scaffold and writes nothing.
func TestScaffoldRefusesAPlacementItHasNoSkeletonFor(t *testing.T) {
	for _, r := range []annotations.Receiver{annotations.Capability, annotations.Rule, annotations.Seed} {
		p := annotations.Placement{Receiver: r, Name: "zzUnmapped", Forms: annotations.FormFlag, Example: "@zzUnmapped"}
		files, err := scaffoldCell(p)
		if err == nil {
			t.Errorf("%s: scaffoldCell wrote %d file(s) for a placement it has no skeleton for", r, len(files))
			continue
		}
		if !strings.Contains(err.Error(), "@zzUnmapped on "+r.Phrase()) {
			t.Errorf("%s: the error does not name the placement: %v", r, err)
		}
	}
	for _, p := range annotations.Placements() {
		if msg := scaffoldUnmapped(p); msg != "" {
			t.Errorf("a registry placement has no skeleton: %s", msg)
		}
	}
}
