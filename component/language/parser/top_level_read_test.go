package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The top-level readers used to cut lines with strings.Split, read each
// line's head word with a regular expression, and walk each line's runes; a
// boot asked that of every line of every file, three times per file. They now
// read one scan (ReadTopLevel) that cuts lines with IndexByte, reads the head
// word byte by byte (lineHead) and counts brackets byte by byte. The replaced
// implementation is kept here as the ORACLE, and the tests hold the new one to
// it: same lines, same words, same depths, on the cases that could differ and
// on every line of the tree.

// oracleStatementHead is the regular expression lineHead replaced.
var oracleStatementHead = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)`)

// oracleTopLevelHeads is topLevelHeads as it was before ReadTopLevel.
func oracleTopLevelHeads(source string) []topLevelHead {
	var out []topLevelHead
	depth := 0
	for i, line := range strings.Split(BlankCommentsAndStrings(source), "\n") {
		if depth == 0 {
			if m := oracleStatementHead.FindStringSubmatch(line); m != nil {
				out = append(out, topLevelHead{line: i + 1, word: m[1], text: line})
			}
		}
		for _, c := range line {
			switch c {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return out
}

// headCases are the lines a byte-level head read could get wrong.
var headCases = []string{
	"", " ", "\t", " \t ", "query", "query x {", "  query x", "\tquery\tx", "_x", "__", "x1_y2",
	"1abc", "-x", "@trigger(x)", "}", "{", "(", "// comment", "/* block */ query", "=> logic x",
	"é", "équery", " é", "queryé", "query9é", "q x", " query", "\r", "query\r", "x\ry",
	"Z", "a", "_", "A_z9", "  \t\t  deep", "use a.b.{ c }", "func (Query) f(ctx any)",
}

// TestLineHeadReadsWhatTheRegexpRead: on every case, and on every line of
// the tree, lineHead finds a word exactly where the regular expression did,
// and the same word.
func TestLineHeadReadsWhatTheRegexpRead(t *testing.T) {
	check := func(where, line string) {
		t.Helper()
		word, ok := lineHead(line)
		m := oracleStatementHead.FindStringSubmatch(line)
		if ok != (m != nil) || (ok && word != m[1]) {
			t.Errorf("%s: lineHead(%q) = %q, %v; the regexp read %q", where, line, word, ok, m)
		}
	}
	for _, c := range headCases {
		check("case", c)
	}
	lines := 0
	for path, src := range treeSources(t) {
		for i, line := range strings.Split(src, "\n") {
			check(path+":"+strconv.Itoa(i+1), line)
			lines++
		}
	}
	if lines < 10000 {
		t.Fatalf("read %d lines of the tree; the comparison needs the tree to mean anything", lines)
	}
}

// TestReadTopLevelIsTheScanItReplaced: the one scan reads the same top-level
// statements the replaced scan did -- line, word and blanked text -- for every
// file of the tree and for sources built to stress the line cut and the
// bracket count.
func TestReadTopLevelIsTheScanItReplaced(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"only newline":     "\n",
		"trailing newline": "query a {\n}\n",
		"no newline":       "query a {\n}",
		"blank lines":      "\n\nquery a {\n}\n\n\nlogic b {\n}\n\n",
		"stray closers":    "}\n)\n]\nquery a {\n}\n",
		"brackets in text": "query a {\n  filter row => row.x == \"}\"\n}\nlogic b {\n}\n",
		"multibyte":        "// é }\nquery é {\n}\nquery a { \"é{\" }\nlogic b {\n}\n",
		"carriage returns": "query a {\r\n}\r\nlogic b {\r\n}\r\n",
		"late use":         "query a {\n}\nuse x.y.{ z }\n",
		// A line opening with a word inside an open ( or [ is not top level.
		"multi-line annotation": "@trigger(\n  schedule=\"0 * * * * *\"\n)\nautomation a {\n}\n",
		"multi-line list":       "@enum[\n  one,\n  two\n]\nconcept c {\n}\n",
	}
	for path, src := range treeSources(t) {
		cases[path] = src
	}
	for name, src := range cases {
		got, want := ReadTopLevel(src).heads, oracleTopLevelHeads(src)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ReadTopLevel read\n%v\nthe replaced scan read\n%v", name, got, want)
		}
	}
}

// TestTheReadersAnswerFromOneScan: each exported reader is its TopLevel
// method over one read, so a caller asking all three reads the file once and
// gets what three separate calls return.
func TestTheReadersAnswerFromOneScan(t *testing.T) {
	src := "use a.b.{ c }\nqurey q {\n}\nautomation x {\n}\nuse d.e.{ f }\nfunc (Query) old(ctx any) {\n}\n"
	read := ReadTopLevel(src)
	if got, want := read.Statements(), TopLevelStatements(src); !reflect.DeepEqual(got, want) || len(got) != 5 {
		t.Errorf("Statements() = %v, TopLevelStatements = %v", got, want)
	}
	if got, want := read.UnknownConstructKeywords(), FindUnknownConstructKeywords(src); !reflect.DeepEqual(got, want) || len(got) != 1 {
		t.Errorf("UnknownConstructKeywords() = %v, FindUnknownConstructKeywords = %v", got, want)
	}
	if got, want := read.MisplacedUseLines(), FindMisplacedUseLines(src); !reflect.DeepEqual(got, want) || len(got) != 1 || got[0].Line != 6 {
		t.Errorf("MisplacedUseLines() = %v, FindMisplacedUseLines = %v", got, want)
	}
}

// treeSources is every .memql file under dsl/, by path.
func treeSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := "../../../dsl"
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) < 100 {
		t.Fatalf("found %d .memql files under %s; the comparison needs the tree", len(out), root)
	}
	return out
}
