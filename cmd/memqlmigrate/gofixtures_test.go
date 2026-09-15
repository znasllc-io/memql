package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gofixtures_test.go -- the bodies rewrite over Go test literals (epic
// memql#5370, task memql#5373).

// A Go test file carrying every kind of literal the mode distinguishes. The
// helper logic is declared in one literal and called BARE from another, so the
// call's kind resolves only if the file's own declarations are indexed.
const goFixtureInput = "package demo\n\nimport \"testing\"\n\n" +
	"func TestDemo(t *testing.T) {\n" +
	"\thelper := `logic fixtureHelper {\n  args {\n    n any\n  }\n  body {\n    return args.n\n  }\n}\n`\n" +
	"\traw := `@trigger(event=\"probe.fired\")\nautomation fixtureRaw {\n  step decide {\n    fixtureHelper ( n: 1 )\n  }\n  step record {\n    logic fixtureHelper ( n: steps.decide.result )\n  }\n}\n`\n" +
	"\tquoted := \"@trigger(event=\\\"probe.fired\\\")\\nautomation fixtureQuoted {\\n  step decide {\\n    logic fixtureHelper ( n: 2 )\\n  }\\n}\\n\"\n" +
	"\tunknown := `@trigger(event=\"probe.fired\")\nautomation fixtureUnknown {\n  step decide {\n    nobodyDeclaresThis ( n: 1 )\n  }\n}\n`\n" +
	"\t// memqlmigrate:keep -- the retired form, on purpose\n" +
	"\tkept := `@trigger(event=\"probe.fired\")\nautomation fixtureKept {\n  step decide {\n    logic fixtureHelper ( n: 3 )\n  }\n}\n`\n" +
	"\tpiece := `  step alone {\n    logic fixtureHelper ( n: 4 )\n  }\n`\n" +
	"\t_, _, _, _, _, _ = helper, raw, quoted, unknown, kept, piece\n" +
	"}\n"

func writeGoFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "demo_test.go")
	if err := os.WriteFile(p, []byte(goFixtureInput), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

// fixtureLine is the line of goFixtureInput a literal starts on: the one
// holding the variable it is assigned to.
func fixtureLine(t *testing.T, variable string) string {
	t.Helper()
	for i, l := range strings.Split(goFixtureInput, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), variable+" := ") {
			return strconv.Itoa(i + 1)
		}
	}
	t.Fatalf("no literal assigned to %s", variable)
	return ""
}

func TestGoFixturesReportsEachLiteralAndWritesNothingDry(t *testing.T) {
	dir, p := writeGoFixture(t)
	var out bytes.Buffer
	if err := run([]string{"--rewrite=bodies", "--go-fixtures", "-v", dir}, &out, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	report := out.String()
	for _, want := range []string{
		"CHANGE   " + p + ":" + fixtureLine(t, "helper"),  // the logic's body block
		"CHANGE   " + p + ":" + fixtureLine(t, "raw"),     // the raw automation
		"CHANGE   " + p + ":" + fixtureLine(t, "quoted"),  // the quoted automation
		"REFUSED  " + p + ":" + fixtureLine(t, "unknown"), // a callee nothing declares
		"KEPT     " + p + ":" + fixtureLine(t, "kept"),    // the marker
		"FRAGMENT " + p + ":" + fixtureLine(t, "piece"),   // a step with no automation around it
		"3 literal(s) in 1 file(s) would change; 1 refused; 1 fragment(s) not rewritten; 1 kept",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report has no %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "nobodyDeclaresThis names no query, mutation, logic or builtin") {
		t.Errorf("the refusal does not say why:\n%s", report)
	}
	if got, _ := os.ReadFile(p); string(got) != goFixtureInput {
		t.Fatal("a dry run wrote the file")
	}
}

func TestGoFixturesWritesStatementsAndIsIdempotent(t *testing.T) {
	dir, p := writeGoFixture(t)
	var out bytes.Buffer
	if err := run([]string{"--rewrite=bodies", "--go-fixtures", "-w", dir}, &out, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), p, got, 0); err != nil {
		t.Fatalf("the rewritten file is not Go: %v\n%s", err, got)
	}
	lits, _, err := goFixtureLiterals(p, got)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]goFixtureLiteral{}
	for _, l := range lits {
		for _, name := range []string{"fixtureHelper {", "fixtureRaw", "fixtureQuoted", "fixtureUnknown", "fixtureKept", "step alone"} {
			if strings.Contains(l.text, name) {
				byName[name] = l
			}
		}
	}
	helper, raw, quoted := byName["fixtureHelper {"], byName["fixtureRaw"], byName["fixtureQuoted"]
	if strings.Contains(helper.text, "body {") || !strings.Contains(helper.text, "return args.n") {
		t.Errorf("the helper's body did not become statements:\n%s", helper.text)
	}
	if !raw.raw || strings.Contains(raw.text, "step ") || !strings.Contains(raw.text, "decide := logic fixtureHelper(n: 1)") ||
		!strings.Contains(raw.text, "logic fixtureHelper(n: decide)") {
		t.Errorf("the raw automation is not its statements, or is no longer raw:\n%s", raw.text)
	}
	if quoted.raw || strings.Contains(quoted.text, "step ") || !strings.Contains(quoted.text, "logic fixtureHelper(n: 2)") {
		t.Errorf("the quoted automation is not its statements, re-quoted:\n%s", quoted.text)
	}
	for _, left := range []string{"fixtureUnknown", "fixtureKept", "step alone"} {
		if !strings.Contains(byName[left].text, "step ") {
			t.Errorf("%s was rewritten; it should stand as it was:\n%s", left, byName[left].text)
		}
	}

	out.Reset()
	if err := run([]string{"--rewrite=bodies", "--go-fixtures", "-w", dir}, &out, &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again, _ := os.ReadFile(p); !bytes.Equal(again, got) {
		t.Fatalf("a second run changed the file:\n%s", again)
	}
	if !strings.Contains(out.String(), "0 literal(s) in 0 file(s) changed") {
		t.Errorf("the second run's report: %s", out.String())
	}
}

func TestGoFixturesRunsTheBodiesRewriteAlone(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--rewrite=expressions", "--go-fixtures", t.TempDir()}, &out, &out); err != errUsage {
		t.Fatalf("--go-fixtures with another rewrite: %v, want a usage error", err)
	}
}
