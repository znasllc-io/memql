package procedure

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalize_AnAutomationSubrunIsASymbolLikeAnyAction(t *testing.T) {
	// D24's second corpus level: an automation invocation is a symbol in the
	// same vocabulary as an action, so one pipeline lifts repeated automation
	// sequences into higher automations.
	got := Canonicalize([]Step{
		{StepType: "exec", Input: map[string]any{"command": "ls"}, Consumed: true},
		{StepType: "automation", Call: Call{Construct: "automation", Name: "deployThing"},
			Input: map[string]any{"env": "prod"}},
	})
	if len(got) != 2 {
		t.Fatalf("got %d actions, want 2", len(got))
	}
	if got[0].Tool != "exec" {
		t.Errorf("an app action's tool is its stepType; got %q", got[0].Tool)
	}
	if got[1].Tool != "automation:deployThing" {
		t.Errorf("a subrun's tool is automation:<name>; got %q", got[1].Tool)
	}
}

func TestCanonicalize_ArgvIsParsedIntoATreeSoOneFilenameIsOneDifference(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "exec", Input: map[string]any{"command": `grep -n foo a.txt`}, Consumed: true}})
	b := Canonicalize([]Step{{StepType: "exec", Input: map[string]any{"command": `grep -n foo b.txt`}, Consumed: true}})

	argvA, ok := a[0].Args.At([]string{"command"})
	if !ok || argvA.Kind != KindArray {
		t.Fatalf("a command must canonicalize to an argv ARRAY; got %+v", argvA)
	}
	if len(argvA.Kids) != 4 {
		t.Fatalf("argv = %d elements, want 4 (grep, -n, foo, a.txt)", len(argvA.Kids))
	}
	argvB, _ := b[0].Args.At([]string{"command"})
	diff := 0
	for i := range argvA.Kids {
		if !argvA.Kids[i].Equal(argvB.Kids[i]) {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("two commands differing in one filename must differ in ONE leaf; got %d. "+
			"Unparsed, they differ in one opaque string and anti-unification learns nothing", diff)
	}
}

func TestCanonicalize_ArgvHonoursQuotes(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "exec", Input: map[string]any{"command": `echo "hello world" x`}, Consumed: true}})
	argv, _ := a[0].Args.At([]string{"command"})
	if len(argv.Kids) != 3 {
		t.Fatalf("argv = %d elements, want 3 -- a quoted run is ONE argument", len(argv.Kids))
	}
	if argv.Kids[1].Lit != "hello world" {
		t.Errorf("quoted argument = %q, want %q", argv.Kids[1].Lit, "hello world")
	}
}

func TestCanonicalize_AJSONStringArgumentBecomesATree(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "mcp", Input: map[string]any{"payload": `{"b":2,"a":1}`}, Consumed: true}})
	n, ok := a[0].Args.At([]string{"payload", "a"})
	if !ok || n.Lit != "1" {
		t.Fatalf("a JSON string argument must be parsed into a tree; At(payload.a) = %v, %v", n, ok)
	}
}

func TestCanonicalize_APathBecomesSegments(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "fs_write", Input: map[string]any{"path": "/srv/app/main.go"}, Consumed: true}})
	n, ok := a[0].Args.At([]string{"path"})
	if !ok || n.Kind != KindArray {
		t.Fatalf("a path must canonicalize to segments; got %+v", n)
	}
	if len(n.Kids) != 3 || n.Kids[2].Lit != "main.go" {
		t.Fatalf("path segments = %v, want [srv app main.go]", n.Kids)
	}
}

func TestCanonicalize_AnUnconsumedPureReadIsDropped(t *testing.T) {
	got := Canonicalize([]Step{
		{StepType: "fs_read", Input: map[string]any{"path": "/tmp/x"}, Consumed: false},
		{StepType: "exec", Input: map[string]any{"command": "ls"}, Consumed: false},
	})
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1 -- an unconsumed pure read is noise", len(got))
	}
	if got[0].Tool != "exec" {
		t.Errorf("the surviving action should be the exec; got %q", got[0].Tool)
	}
}

// TestCanonicalize_AnUnconsumedExecIsNotDropped is the NEGATIVE CONTROL for
// the rule above. Dropping on Consumed alone would delete every command whose
// output nobody read -- which is most commands -- and the corpus would lose
// exactly the side effects it exists to learn.
func TestCanonicalize_AnUnconsumedExecIsNotDropped(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "exec", Input: map[string]any{"command": "rm -rf build"}, Consumed: false}})
	if len(got) != 1 {
		t.Fatalf("an exec is never noise: its effect is not knowable from whether its output was read")
	}
}

func TestCanonicalize_APureReadThatWasConsumedIsKept(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "fs_read", Input: map[string]any{"path": "/tmp/x"}, Consumed: true}})
	if len(got) != 1 {
		t.Fatal("a read whose result a later step used is evidence, not noise")
	}
}

func TestCanonicalize_CarriesDigestsAndOrder(t *testing.T) {
	got := Canonicalize([]Step{
		{StepType: "exec", Seq: 7, Key: "call7", ResultDigest: "rd", EffectDigest: "ed",
			Input: map[string]any{"command": "ls"}, Consumed: true},
	})
	a := got[0]
	if a.Seq != 7 || a.Key != "call7" || a.ResultDigest != "rd" || a.EffectDigest != "ed" {
		t.Fatalf("canonicalize must carry seq, key and both digests through; got %+v", a)
	}
}

// TestCanonicalizeMarksTheFormItParsed: every string canonicalization parses
// into a tree says which reading it took, because that reading is the only
// thing that can turn the tree back into the string a replay has to send.
func TestCanonicalizeMarksTheFormItParsed(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{
		"command": "git status",
		"payload": `{"a":1}`,
		"path":    "/tmp/a",
		"target":  "./a/b",
		"message": "hello world",
	}}})
	args := got[0].Args
	for key, want := range map[string]string{
		"command": FormArgv,
		"payload": FormJSON,
		"path":    FormRootedPath,
		"target":  FormPath,
		"message": "",
	} {
		n, ok := args.At([]string{key})
		if !ok {
			t.Fatalf("%s: missing from the canonical tree", key)
		}
		if n.Form != want {
			t.Errorf("%s: Form = %q, want %q", key, n.Form, want)
		}
	}
	if args.Form != "" {
		t.Errorf("the argument object itself was never a string; Form = %q, want none", args.Form)
	}
}

// TestCanonicalizeKeepsAnArgumentVectorsElementsWhole: an array under a
// command key (Codex's exec events record argv this way) is ALREADY split
// into arguments. Splitting each element again as a command line would read
// the apostrophe in a commit message as an opening quote and lose it, and the
// replay would send a message nobody wrote.
func TestCanonicalizeKeepsAnArgumentVectorsElementsWhole(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{
		"command": []any{"git", "commit", "-m", "don't break it"},
	}}})
	argv, _ := got[0].Args.At([]string{"command"})
	if argv.Kind != KindArray || len(argv.Kids) != 4 {
		t.Fatalf("an argument vector must stay one element per argument; got %+v", argv)
	}
	if argv.Kids[3].Kind != KindLit || argv.Kids[3].Lit != "don't break it" {
		t.Fatalf("an argument must be kept verbatim; got %+v", argv.Kids[3])
	}
	// The same command recorded as a string generalizes with it: both
	// readings are four arguments with the message as one literal.
	str := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{
		"command": `git commit -m "don't break it"`,
	}}})
	line, _ := str[0].Args.At([]string{"command"})
	if !line.Equal(argv) {
		t.Fatalf("the string and the vector recording of one command must be Equal:\n %+v\n %+v", line, argv)
	}
}

// TestCanonicalizeKeepsEveryEmptySegmentButTheRoot: a rooted path's leading
// slash is recorded as its Form rather than as a segment, so two recordings
// differing only in a root still differ in no segment. Every OTHER empty
// segment is information -- a protocol-relative URL, rsync's trailing slash,
// a source file whose first line is a // comment -- and a segment dropped here
// is a byte no materialization can put back.
func TestCanonicalizeKeepsEveryEmptySegmentButTheRoot(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "fs_write", Consumed: true, Input: map[string]any{
		"path":   "/srv/app/main.go",
		"target": "//cdn.example.com/x",
		"file":   "out/",
	}}})
	args := got[0].Args
	root, _ := args.At([]string{"path"})
	if len(root.Kids) != 3 {
		t.Fatalf("a rooted path's leading slash is its Form, not a segment; got %d segments", len(root.Kids))
	}
	cdn, _ := args.At([]string{"target"})
	if cdn.Form != FormRootedPath || len(cdn.Kids) != 3 || cdn.Kids[0].Lit != "" {
		t.Fatalf("//cdn.example.com/x must keep its second slash as an empty segment; got %+v", cdn)
	}
	dir, _ := args.At([]string{"file"})
	if len(dir.Kids) != 2 || dir.Kids[1].Lit != "" {
		t.Fatalf("a trailing slash is a trailing empty segment; got %+v", dir)
	}
}

// TestCanonicalizeRecordsANullAsNull: JSON null and the empty string compare
// EQUAL (Equal ignores LitType, so the two still generalize together), but a
// replay that wrote "" where the recording sent null would change what the
// call means.
func TestCanonicalizeRecordsANullAsNull(t *testing.T) {
	got := Canonicalize([]Step{{StepType: "mcp", Consumed: true, Input: map[string]any{"cursor": nil}}})
	n, _ := got[0].Args.At([]string{"cursor"})
	if n.Kind != KindLit || n.LitType != "null" {
		t.Fatalf("a null must be recorded as a null literal; got %+v", n)
	}
	if !n.Equal(Lit("")) {
		t.Fatal("a null literal must still Equal the empty string: canonicalization folds scalars to one spelling")
	}
}

// lits is a token list's values, for comparing what splitArgv read.
func lits(nodes []*Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Lit
	}
	return out
}

// TestSplitArgvRemovesQuotesTheWayAPOSIXShellDoes (A1): every expectation
// below is what dash and bash hand the program. Inside double quotes a
// backslash escapes only $, `, ", \ and a newline -- before anything else it
// is a literal backslash, so `grep -E "\d+"` searches for \d+ and not for d+.
// A backslash-newline outside single quotes is a line continuation and
// vanishes; inside single quotes nothing is special.
func TestSplitArgvRemovesQuotesTheWayAPOSIXShellDoes(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{`grep -E "\d+"`, []string{"grep", "-E", `\d+`}},
		{`printf "a\tb"`, []string{"printf", `a\tb`}},
		{`echo "a\"b" "a\\b" "\$HOME" "\` + "`" + `"`, []string{"echo", `a"b`, `a\b`, `$HOME`, "`"}},
		{`grep -n "foo\|bar" f`, []string{"grep", "-n", `foo\|bar`, "f"}},
		{"echo \"a\\\nb\"", []string{"echo", "ab"}},
		{"echo a\\\nb", []string{"echo", "ab"}},
		{"echo a \\\n b", []string{"echo", "a", "b"}},
		{`echo 'a\b' 'x"y'`, []string{"echo", `a\b`, `x"y`}},
		{"echo 'a\\\nb'", []string{"echo", "a\\\nb"}},
		{`echo a\ b \'x\' \\`, []string{"echo", "a b", "'x'", `\`}},
		{`echo "" ''`, []string{"echo", "", ""}},
		{`echo a\`, []string{"echo", `a\`}},
		{`echo "$(date +%F)"`, []string{"echo", "$(date +%F)"}},
	} {
		if got := lits(splitArgv(c.line)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitArgv(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// recordedCommands are command lines an app writes and a replay has to send
// back EXACTLY: Claude Code's heredoc commit message, expansions inside double
// quotes, backslashes a shell keeps, a heredoc, and the whitespace between the
// words. Each was measured wrong before Raw and Seps: re-quoted from the tree,
// every expansion became a literal, every kept backslash vanished and every
// newline became a space.
var recordedCommands = []string{
	"git commit -m \"$(cat <<'EOF'\nAdd the replay runner\n\nIt serves a goal with no model.\nEOF\n)\"",
	`mkdir -p "$HOME/My Docs"`,
	`echo "Build $(date +%F)" > build.txt`,
	`grep -E "\d+" log.txt`,
	`printf "a\tb"`,
	`grep -n "foo\|bar" f`,
	"cat <<'EOF' > notes.txt\nline one\n  indented line\nEOF",
	"ls   -la\t/tmp",
	"docker run \\\n  --rm alpine echo hi",
	"  echo hi  ",
	`echo 'it'\''s' "" ''`,
	`cd app&&npm test`,
	"",
}

// TestSplitArgvRecordsEachTokensSpellingAndTheTextBetweenThem (A2): a token
// keeps its exact source spelling (Raw) and the argv keeps the exact text
// around its tokens (Seps, one more than the tokens), so the pieces put back
// together ARE the recorded command, byte for byte.
func TestSplitArgvRecordsEachTokensSpellingAndTheTextBetweenThem(t *testing.T) {
	for _, line := range recordedCommands {
		toks, seps := scanArgv(line)
		if len(seps) != len(toks)+1 {
			t.Fatalf("%q: %d tokens and %d separators, want one more separator than tokens", line, len(toks), len(seps))
		}
		var b strings.Builder
		b.WriteString(seps[0])
		for i, tok := range toks {
			if tok.Raw == "" {
				t.Fatalf("%q: token %d (%q) has no spelling", line, i, tok.Lit)
			}
			b.WriteString(tok.Raw)
			b.WriteString(seps[i+1])
		}
		if b.String() != line {
			t.Errorf("the pieces of %q put back together are %q", line, b.String())
		}
	}
	toks, seps := scanArgv(`mkdir  -p "$HOME/My Docs"` + "\n")
	if got := []string{toks[0].Raw, toks[1].Raw, toks[2].Raw}; !reflect.DeepEqual(got, []string{"mkdir", "-p", `"$HOME/My Docs"`}) {
		t.Errorf("spellings = %q", got)
	}
	if want := []string{"", "  ", " ", "\n"}; !reflect.DeepEqual(seps, want) {
		t.Errorf("separators = %q, want %q", seps, want)
	}
	if toks[2].Lit != "$HOME/My Docs" {
		t.Errorf("the value is still what the quotes enclose; got %q", toks[2].Lit)
	}
}

// TestCanonicalizeKeepsTheSpellingOnTheArgv: the reading canonicalization
// records is the one Materialize writes back from, so the argv node carries
// the separators and every token its spelling.
func TestCanonicalizeKeepsTheSpellingOnTheArgv(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": `echo  "a b"`}}})[0]
	argv, _ := a.Args.At([]string{"command"})
	if want := []string{"", "  ", ""}; !reflect.DeepEqual(argv.Seps, want) {
		t.Fatalf("Seps = %q, want %q", argv.Seps, want)
	}
	if argv.Kids[1].Raw != `"a b"` || argv.Kids[1].Lit != "a b" {
		t.Fatalf("token 1 = %+v, want the value a b spelled \"a b\"", *argv.Kids[1])
	}
}

// TestFormatNumberNeverNarrowsAFloatOutsideInt64 (E3): converting a float
// outside int64's range to int64 is implementation-defined in Go. On amd64 it
// yields the integer indefinite value, which never compares equal to the
// float, so the old spelling happened to come out right there; on arm64 it
// SATURATES (FCVTZS), and 2^63 came back as 9223372036854775807 -- a
// different number, and a different literal on a developer's Mac than on a
// Linux replica, so one recording spelled two ways. The range is checked
// before any conversion.
func TestFormatNumberNeverNarrowsAFloatOutsideInt64(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{math.Ldexp(1, 63), "9.223372036854776e+18"},
		{-math.Ldexp(1, 63), "-9223372036854775808"},
		{1e19, "1e+19"},
		{-1e19, "-1e+19"},
		{math.Ldexp(1, 63) - 1024, "9223372036854774784"},
		{3, "3"},
		{0.25, "0.25"},
		{math.Inf(1), "+Inf"},
	} {
		if got := formatNumber(c.f); got != c.want {
			t.Errorf("formatNumber(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

// vectorCommand canonicalizes one exec step whose command is an argument
// vector and answers its command node.
func vectorCommand(t *testing.T, vec ...any) *Node {
	t.Helper()
	a := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": vec}}})[0]
	n, ok := a.Args.At([]string{"command"})
	if !ok || n.Kind != KindArray {
		t.Fatalf("%q canonicalized to %+v, want a vector", vec, n)
	}
	return n
}

// TestAShellsScriptInAVectorIsReadAsACommandLine: Codex records every command
// as ["bash", "-lc", "<script>"], and the script is a command line in its own
// right. Kept as one literal, any value that varied between two recordings
// made the WHOLE script a parameter -- code a goal would choose -- so no
// parameterised Codex procedure could ever be promoted. Read as a command line
// (with its spelling, exactly as a `command` string is), the value is one
// word inside it. The shell is found where it runs the script: first in the
// vector, or behind a wrapper that runs what follows it; the script is its
// first operand after a -c among its options, an option's own argument
// stepped over.
func TestAShellsScriptInAVectorIsReadAsACommandLine(t *testing.T) {
	for _, c := range []struct {
		vec    []any
		script int
	}{
		{[]any{"bash", "-lc", "cp report.txt out/a.txt"}, 2},
		{[]any{"/bin/sh", "-c", "make build"}, 2},
		{[]any{"zsh", "-ec", "make build"}, 2},
		{[]any{"bash", "-c", "-e", "make build"}, 3},
		{[]any{"bash", "-o", "pipefail", "-c", "make build"}, 4},
		{[]any{"bash", "-c", "make build", "arg0", "arg1"}, 2},
		{[]any{"sudo", "-u", "bob", "bash", "-c", "make build"}, 5},
		{[]any{"env", "X=1", "bash", "-lc", "make build"}, 4},
	} {
		n := vectorCommand(t, c.vec...)
		for i, k := range n.Kids {
			if i == c.script {
				if k.Kind != KindArray || k.Form != FormArgv || len(k.Seps) != len(k.Kids)+1 {
					t.Errorf("%q: element %d = %+v, want the script read as a command line", c.vec, i, *k)
				}
				continue
			}
			if k.Kind != KindLit || k.Lit != c.vec[i] {
				t.Errorf("%q: element %d = %+v, want the argument kept whole", c.vec, i, *k)
			}
		}
	}
	// Not a script a POSIX shell runs: a script FILE, an interpreter, fish
	// (whose quoting is not POSIX), a shell word some other program is merely
	// handed, and no -c at all. Every element stays whole.
	for _, vec := range [][]any{
		{"bash", "script.sh", "-c", "make build"},
		{"python3", "-c", "print(1)"},
		{"fish", "-c", "echo hi"},
		{"echo", "bash", "-c", "make build"},
		{"bash", "-l", "make build"},
		{"git", "commit", "-m", "don't break it"},
	} {
		for i, k := range vectorCommand(t, vec...).Kids {
			if k.Kind != KindLit || k.Lit != vec[i] {
				t.Errorf("%q: element %d = %+v, want every element kept whole", vec, i, *k)
			}
		}
	}
}
