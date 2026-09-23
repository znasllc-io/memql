package procedure

import "testing"

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
