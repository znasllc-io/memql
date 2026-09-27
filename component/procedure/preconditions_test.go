package procedure

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// fingerprint decodes one session fingerprint in the cockpit's wire shape
// (memql-cockpit internal/worker/harness/record.go, type Fingerprint) the way
// the engine reads a step row: through encoding/json, so numbers arrive as
// float64 and lists as []any. Tests build the JSON rather than a Go map so the
// fixture IS the shape a recording carries.
func fingerprint(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return m
}

// macStart is a start on a Mac with node and git on PATH and an empty
// workspace. The parameters are what the tests vary.
func macStart(t *testing.T, node, git string, cwdEntries int) map[string]any {
	t.Helper()
	return fingerprint(t, `{
		"type": "memql.app_session.fingerprint", "v": 1, "seq": 0,
		"takenAt": "2026-09-20T10:00:00Z",
		"app": {"id": "claude-code", "version": "2.1.0", "harness": "claude-headless"},
		"platform": {"os": "darwin", "arch": "arm64"},
		"tools": [
			{"name": "git", "version": "git version `+git+`"},
			{"name": "node", "version": "v`+node+`"}
		],
		"cwd": "/Users/x/work", "cwdDigest": "sha256:aaaa", "cwdEntries": `+itoa(cwdEntries)+`,
		"variables": [
			{"name": "PATH", "set": true, "digest": "sha256:path1"},
			{"name": "LC_ALL", "set": false}
		],
		"inputs": []
	}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// TestAPredicateIsLearnedOnlyWhenEveryStartAgreed is D16's definition of a
// precondition: an initiation set is what was true at EVERY successful start.
// A predicate the starts disagreed on was evidently not needed for success.
func TestAPredicateIsLearnedOnlyWhenEveryStartAgreed(t *testing.T) {
	got := LearnPreconditions([]map[string]any{
		macStart(t, "22.1.0", "2.43.0", 0),
		macStart(t, "22.1.0", "2.44.0", 0),
	}, []string{"git", "node"})
	if got.Tools["node"] != "22.1.0" {
		t.Errorf("node agreed on every start and must be kept; Tools = %v", got.Tools)
	}
	if _, kept := got.Tools["git"]; kept {
		t.Errorf("git was 2.43 on one start and 2.44 on the other; it is not a precondition: %v", got.Tools)
	}
	if want := map[string]string{"os": "darwin", "arch": "arm64"}; !reflect.DeepEqual(got.Platform, want) {
		t.Errorf("Platform = %v, want %v", got.Platform, want)
	}
	if want := map[string]string{"PATH": "sha256:path1", "LC_ALL": VariableUnset}; !reflect.DeepEqual(got.Variables, want) {
		t.Errorf("Variables = %v, want %v -- an unset variable is a fact too", got.Variables, want)
	}
}

// TestOnlyToolsTheProcedureInvokesArePreconditions: the fingerprint probes
// every common toolchain, and a precondition on one the procedure never runs
// would refuse replays for a reason that cannot matter.
func TestOnlyToolsTheProcedureInvokesArePreconditions(t *testing.T) {
	got := LearnPreconditions([]map[string]any{
		macStart(t, "22.1.0", "2.43.0", 0),
		macStart(t, "22.1.0", "2.43.0", 0),
	}, []string{"node"})
	if _, kept := got.Tools["git"]; kept {
		t.Fatalf("git agreed but the procedure never invokes it; Tools = %v", got.Tools)
	}
	if got.Tools["node"] != "22.1.0" {
		t.Fatalf("node is invoked and agreed; Tools = %v", got.Tools)
	}
}

// TestEmptyWorkspaceIsLearnedFromCwdEntries: the predicate is only ever
// learned TRUE. A workspace that was empty at some starts and not at others
// says nothing, and "must not be empty" is a claim no recording made.
func TestEmptyWorkspaceIsLearnedFromCwdEntries(t *testing.T) {
	both := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), macStart(t, "22.1.0", "2.43.0", 0)}, nil)
	if both.EmptyWorkspace == nil || !*both.EmptyWorkspace {
		t.Fatalf("every start had cwdEntries 0; EmptyWorkspace = %v, want true", both.EmptyWorkspace)
	}
	one := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), macStart(t, "22.1.0", "2.43.0", 3)}, nil)
	if one.EmptyWorkspace != nil {
		t.Fatalf("one start had 3 entries; EmptyWorkspace must be ABSENT, never false; got %v", *one.EmptyWorkspace)
	}
	unmeasured := macStart(t, "22.1.0", "2.43.0", 0)
	delete(unmeasured, "cwdEntries")
	if got := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), unmeasured}, nil); got.EmptyWorkspace != nil {
		t.Fatal("a start whose workspace could not be listed did not agree that it was empty")
	}
}

// TestAStartWithNoFingerprintIsNotEvidence: a recording whose fingerprint
// never arrived measured nothing. Letting it veto every predicate would make
// one lost chunk erase the whole initiation set -- the permissive direction.
func TestAStartWithNoFingerprintIsNotEvidence(t *testing.T) {
	got := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), nil, {}}, []string{"node"})
	if got.Tools["node"] != "22.1.0" || got.Platform["os"] != "darwin" {
		t.Fatalf("the measured start's predicates must stand; got %+v", got)
	}
	if empty := LearnPreconditions(nil, []string{"node"}); !reflect.DeepEqual(empty, Preconditions{}) {
		t.Fatalf("no fingerprints learn nothing; got %+v", empty)
	}
}

// TestPlatformAndVariablesAreNotComparedOnTheWorkbench is the plan's first
// review focus. A procedure recorded on a Mac replays on the Linux workbench
// only because its footprint is portable (D4), and a portable footprint is
// platform-independent by definition; the person's environment variables do
// not exist there either. Tool versions still are compared.
func TestPlatformAndVariablesAreNotComparedOnTheWorkbench(t *testing.T) {
	learned := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), macStart(t, "22.1.0", "2.43.0", 0)}, []string{"node"})
	yes := true
	observed := Preconditions{
		Platform:       map[string]string{"os": "linux", "arch": "amd64"},
		Tools:          map[string]string{"node": "22.1.0"},
		EmptyWorkspace: &yes,
	}
	got := CheckPreconditions(learned, observed, TargetWorkbench)
	if !got.Held || len(got.Mismatches) != 0 || len(got.Unmeasured) != 0 {
		t.Fatalf("platform and variables must not be compared on the workbench; got %+v", got)
	}
	observed.Tools["node"] = "20.3.0"
	if CheckPreconditions(learned, observed, TargetWorkbench).Held {
		t.Fatal("a tool version the procedure uses IS compared on the workbench")
	}
}

// TestPlatformIsComparedOnTheMachine: on the person's own machine the platform
// is part of the world the recording started in.
func TestPlatformIsComparedOnTheMachine(t *testing.T) {
	learned := Preconditions{Platform: map[string]string{"os": "darwin", "arch": "arm64"}}
	observed := Preconditions{Platform: map[string]string{"os": "linux", "arch": "arm64"}}
	got := CheckPreconditions(learned, observed, TargetMachine)
	if got.Held {
		t.Fatal("a machine target running a different OS must not hold")
	}
	if len(got.Mismatches) != 1 || !strings.HasPrefix(got.Mismatches[0], "platform.os") {
		t.Fatalf("Mismatches = %v, want exactly one naming platform.os", got.Mismatches)
	}
	if want := "platform.os: recorded darwin, found linux"; got.Mismatches[0] != want {
		t.Fatalf("mismatch reads %q, want %q", got.Mismatches[0], want)
	}
}

// TestAnUnknownTargetComparesThePlatform: only the workbench relaxes the
// platform. A target nobody named is not a reason to skip a predicate.
func TestAnUnknownTargetComparesThePlatform(t *testing.T) {
	learned := Preconditions{Platform: map[string]string{"os": "darwin", "arch": "arm64"}}
	observed := Preconditions{Platform: map[string]string{"os": "linux"}}
	got := CheckPreconditions(learned, observed, "")
	if got.Held || len(got.Mismatches) != 1 || len(got.Unmeasured) != 1 {
		t.Fatalf("an unnamed target must compare the platform: one mismatch (os) and one unmeasured (arch); got %+v", got)
	}
}

// TestVariablesAreLearnedButNeverCompared is the coordinator's decision for
// epic memql#5408. The fingerprint's variables describe the APP'S environment
// -- the session the cockpit launched -- and a replay never runs there, on the
// person's machine any more than on the workbench. So they stay in the learned
// set (they are evidence) and no target compares them: a differing digest and
// an unmeasured variable both leave the check holding. The per-step comparison
// is what catches a variable that genuinely mattered.
//
// The control is the platform in the same call: it IS compared on the machine,
// so a check that held here because it compared nothing at all fails.
func TestVariablesAreLearnedButNeverCompared(t *testing.T) {
	learned := LearnPreconditions([]map[string]any{macStart(t, "22.1.0", "2.43.0", 0), macStart(t, "22.1.0", "2.43.0", 0)}, []string{"node"})
	if len(learned.Variables) == 0 {
		t.Fatal("the fixture learned no variables, so this test would pass having compared nothing")
	}
	yes := true
	observed := Preconditions{
		Platform:       map[string]string{"os": "darwin", "arch": "arm64"},
		Tools:          map[string]string{"node": "22.1.0"},
		Variables:      map[string]string{"PATH": "sha256:somewhere-else"}, // LC_ALL unmeasured
		EmptyWorkspace: &yes,
	}
	for _, target := range []string{TargetMachine, TargetWorkbench, ""} {
		got := CheckPreconditions(learned, observed, target)
		if !got.Held {
			t.Fatalf("target %q: variables must not be compared; got %+v", target, got)
		}
	}
	observed.Platform["os"] = "linux"
	if CheckPreconditions(learned, observed, TargetMachine).Held {
		t.Fatal("the control failed: the platform IS compared on the machine, so this check compares nothing")
	}
}

func TestAToolVersionMismatchIsReported(t *testing.T) {
	learned := Preconditions{Tools: map[string]string{"node": "22.1.0", "git": "2.43.0"}}
	observed := Preconditions{Tools: map[string]string{"node": "20.3.0", "git": "2.43.0"}}
	got := CheckPreconditions(learned, observed, TargetWorkbench)
	if got.Held {
		t.Fatal("a different node version must not hold")
	}
	if want := []string{"tools.node: recorded 22.1.0, found 20.3.0"}; !reflect.DeepEqual(got.Mismatches, want) {
		t.Fatalf("Mismatches = %q, want %q", got.Mismatches, want)
	}
}

// TestAToolVersionIsComparedByItsVersionAlone: a tool reports its version as a
// line of its own choosing, and `go version` names the platform in it. Compared
// as lines, a Go procedure recorded on a Mac could never replay on the Linux
// workbench -- the platform the workbench deliberately does not compare would
// come back in through the tool line.
func TestAToolVersionIsComparedByItsVersionAlone(t *testing.T) {
	learned := LearnPreconditions([]map[string]any{fingerprint(t, `{
		"platform": {"os": "darwin", "arch": "arm64"},
		"tools": [{"name": "go", "version": "go version go1.22.1 darwin/arm64"},
		          {"name": "docker", "version": "Docker version 24.0.7, build afdd53b"}],
		"variables": []
	}`)}, []string{"go", "docker"})
	if want := map[string]string{"go": "1.22.1", "docker": "24.0.7"}; !reflect.DeepEqual(learned.Tools, want) {
		t.Fatalf("Tools = %v, want the version numbers %v", learned.Tools, want)
	}
	observed := Preconditions{Tools: map[string]string{
		"go":     "go version go1.22.1 linux/amd64", // a prober may report the raw line
		"docker": "Docker version 24.0.7, build 311b9ff",
	}}
	if got := CheckPreconditions(learned, observed, TargetWorkbench); !got.Held {
		t.Fatalf("the same versions on another platform must hold; got %+v", got)
	}
}

// TestALearnedPredicateTheObservationDoesNotCarryIsUnmeasuredAndDoesNotHold is
// the step concept's rule: an ABSENT measurement is never a match. A tool the
// target could not report may be missing, and a replay that assumed otherwise
// would find out halfway through.
func TestALearnedPredicateTheObservationDoesNotCarryIsUnmeasuredAndDoesNotHold(t *testing.T) {
	yes := true
	learned := Preconditions{Tools: map[string]string{"node": "22.1.0"}, EmptyWorkspace: &yes}
	got := CheckPreconditions(learned, Preconditions{}, TargetWorkbench)
	if got.Held {
		t.Fatal("an unmeasured predicate must not hold")
	}
	if want := []string{"tools.node", "emptyWorkspace"}; !reflect.DeepEqual(got.Unmeasured, want) {
		t.Fatalf("Unmeasured = %v, want %v", got.Unmeasured, want)
	}
	if len(got.Mismatches) != 0 {
		t.Fatalf("an absent measurement is unmeasured, not a mismatch; got %v", got.Mismatches)
	}
	no := false
	got = CheckPreconditions(learned, Preconditions{Tools: map[string]string{"node": "22.1.0"}, EmptyWorkspace: &no}, TargetWorkbench)
	if want := []string{"emptyWorkspace: recorded true, found false"}; !reflect.DeepEqual(got.Mismatches, want) {
		t.Fatalf("Mismatches = %v, want %v", got.Mismatches, want)
	}
}

// execTemplate builds a template of exec steps from command lines.
func execTemplate(commands ...string) Template {
	var steps []TemplateStep
	for _, c := range commands {
		steps = append(steps, TemplateStep{Tool: "exec", Args: execStep(c).Args})
	}
	return Template{Steps: steps}
}

// TestUsedToolsReadsArgvZeroOfExecSteps: the tools a procedure invokes are
// the command words of its exec steps, by basename, because the fingerprint
// names tools the way PATH resolves them.
func TestUsedToolsReadsArgvZeroOfExecSteps(t *testing.T) {
	tmpl := execTemplate("node build.js", "/usr/local/bin/git status", "node test.js")
	tmpl.Steps = append(tmpl.Steps, TemplateStep{Tool: "fs_write", Args: Obj(map[string]*Node{
		"path": Arr(Lit("tmp"), Lit("out")), "command": Arr(Lit("python3")),
	})})
	if got, want := UsedTools(tmpl), []string{"git", "node"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UsedTools = %v, want %v -- only exec steps invoke tools", got, want)
	}
}

// TestUsedToolsSkipsACommandWordThatIsAHole: a command word that varied across
// the recordings names no one tool, and guessing one would be a precondition
// the procedure does not have.
func TestUsedToolsSkipsACommandWordThatIsAHole(t *testing.T) {
	argv := Arr(HoleNode("s0.command.0", "string"), Lit("--version"))
	argv.Form = FormArgv
	tmpl := Template{Steps: []TemplateStep{{Tool: "exec", Args: Obj(map[string]*Node{"command": argv})}}}
	if got := UsedTools(tmpl); len(got) != 0 {
		t.Fatalf("UsedTools = %v, want none", got)
	}
}

// TestUsedToolsFindsEveryCommandOfACommandLine: `cd app && npm test` invokes
// npm, and so does `bash -lc 'npm test'` -- which is how Codex runs EVERY
// command. Reading only the first word would leave a Codex procedure with no
// tool preconditions at all.
func TestUsedToolsFindsEveryCommandOfACommandLine(t *testing.T) {
	codexVector := Template{Steps: []TemplateStep{{Tool: "exec", Args: Canonicalize([]Step{{
		StepType: "exec", Consumed: true,
		Input: map[string]any{"command": []any{"/bin/bash", "-lc", "cd app && npm ci; FOO=1 node x.js | grep ok"}},
	}})[0].Args}}}
	for _, c := range []struct {
		name string
		tmpl Template
		want []string
	}{
		{"operators", execTemplate("cd app && npm test || make build; git status | wc -l"), []string{"cd", "git", "make", "npm", "wc"}},
		{"a trailing semicolon", execTemplate("cd app; npm test"), []string{"cd", "npm"}},
		{"an assignment before the command", execTemplate("NODE_ENV=test node x.js"), []string{"node"}},
		{"a shell wrapper as a string", execTemplate(`/bin/zsh -lc "go test ./..."`), []string{"go", "zsh"}},
		{"a shell wrapper as a vector", codexVector, []string{"bash", "cd", "grep", "node", "npm"}},
	} {
		if got := UsedTools(c.tmpl); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: UsedTools = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestUsedToolsReadsOperatorsAttachedToWords (E2): a shell reads `&&`, `;`
// and `|` wherever they are unquoted, attached to a word or not, so
// `cd app&&npm test` runs npm -- and it reads them nowhere they ARE quoted, so
// `grep "a|b" f` runs grep and nothing called b. The spelling of each
// argument is what says which; its value does not. A redirection that holds
// `&` or `|` (`2>&1`, `>|`) is not an operator, a newline ends a command as a
// `;` does, a here-document's body is not a command at all, and a subshell's
// first word is a command word.
func TestUsedToolsReadsOperatorsAttachedToWords(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{"cd app&&npm test", []string{"cd", "npm"}},
		{`grep "a|b" f`, []string{"grep"}},
		{`grep 'x;y' f; wc -l f`, []string{"grep", "wc"}},
		{"cd app;npm test|grep ok", []string{"cd", "grep", "npm"}},
		{"npm test 2>&1 | tee log", []string{"npm", "tee"}},
		{"make build >| out.log && git status", []string{"git", "make"}},
		{"cd app\nnpm test", []string{"cd", "npm"}},
		{"cat <<'EOF' > notes.txt\nhello world\n  rm -rf x\nEOF\ngit add notes.txt", []string{"cat", "git"}},
		{"(cd app && npm ci)", []string{"cd", "npm"}},
		{`bash -c "cd x;make"`, []string{"bash", "cd", "make"}},
		{`echo "$(date)" && ls`, []string{"echo", "ls"}},
	} {
		if got := UsedTools(execTemplate(c.line)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: UsedTools = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestUsedToolsReadsAVectorsScript: a shell's script inside an argument
// vector is a command line of its own, read as one -- behind a wrapper as
// well -- and a vector stored before scripts were read (its script one
// literal) is read as it always was.
func TestUsedToolsReadsAVectorsScript(t *testing.T) {
	vector := func(vec ...any) Template {
		return Template{Steps: []TemplateStep{{Tool: "exec", Args: Canonicalize([]Step{{
			StepType: "exec", Consumed: true, Input: map[string]any{"command": vec},
		}})[0].Args}}}
	}
	if got, want := UsedTools(vector("sudo", "-u", "bob", "bash", "-c", "cd app&&npm test")), []string{"bash", "cd", "npm", "sudo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind sudo: UsedTools = %v, want %v", got, want)
	}
	stored := Template{Steps: []TemplateStep{{Tool: "exec", Args: Obj(map[string]*Node{
		"command": Arr(Lit("bash"), Lit("-lc"), Lit("make build && git status")),
	})}}}
	if got, want := UsedTools(stored), []string{"bash", "git", "make"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a stored literal script: UsedTools = %v, want %v", got, want)
	}
}
