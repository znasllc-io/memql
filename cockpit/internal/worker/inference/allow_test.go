package inference

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// operatorPolicy is a policy.yaml with a person's fingerprints on it:
// comments, blank lines, a two-space list under one key and a four-space
// list under another, an inline comment, and every section this cockpit
// reads. The whole point of the tests below is that all of it comes back
// unchanged -- a load-modify-save through yaml.v3 would not, and the file
// somebody hand-wrote is not this command's to reformat.
const operatorPolicy = `# ~/.memql/policy.yaml
# Edited by hand 2026-03-02. The deny list is deliberate -- ask before widening.

shell:
  allow:
    - rustc     # this machine builds the firmware
  deny:
    - npm

fs:
  workspace_root: ~/work
  deny:
  - ~/.gnupg
  - ~/Documents/Tax

http:
  max_body_bytes: 1048576
  block_private_net: true

apps:
  homes:
    api.memql.localhost:
      allow:
        - claude-code

backup:
  roots:
    - ~/Clients

models:
  # Default-deny: only what is listed here is offered to the cluster.
  allow:
    - llama3.1:8b

  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
      models:
        - id: qwen2.5-7b-instruct
          context_window: 32768
          structured_output: true
`

func writePolicy(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestAllowLeavesEveryOtherByteAlone is the requirement in one assertion:
// the file after is the file before with ONE line inserted. Comments,
// blank lines, the inline comment on the rustc entry and the two different
// list indentations all survive because nothing re-serialises them.
func TestAllowLeavesEveryOtherByteAlone(t *testing.T) {
	path := writePolicy(t, operatorPolicy, 0o600)

	if err := Allow(path, "nomic-embed-text"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	want := strings.Replace(operatorPolicy,
		"    - llama3.1:8b\n",
		"    - llama3.1:8b\n    - nomic-embed-text\n", 1)
	if got := read(t, path); got != want {
		t.Errorf("policy.yaml:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

// TestAllowMergesAndCollapsesDuplicates. Merge, never replace: the model
// already listed is the one this machine is serving right now, and a
// rewrite that dropped it would take it out of the fleet at the next
// reload.
func TestAllowMergesAndCollapsesDuplicates(t *testing.T) {
	path := writePolicy(t, operatorPolicy, 0o600)

	if err := Allow(path, "nomic-embed-text", "llama3.1:8b", "nomic-embed-text", "qwen2.5:7b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	want := strings.Replace(operatorPolicy,
		"    - llama3.1:8b\n",
		"    - llama3.1:8b\n    - nomic-embed-text\n    - qwen2.5:7b\n", 1)
	if got := read(t, path); got != want {
		t.Errorf("policy.yaml:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestAllowWritesNothingWhenThereIsNothingToAdd. Rerunning `setup
// --inference` is the ordinary case (design D2 asks for idempotence), and
// the strongest available proof that no write happened is that the file's
// mode is the one the operator left on it.
func TestAllowWritesNothingWhenThereIsNothingToAdd(t *testing.T) {
	path := writePolicy(t, operatorPolicy, 0o644)

	if err := Allow(path, "llama3.1:8b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if got := read(t, path); got != operatorPolicy {
		t.Error("the file was rewritten with nothing to add")
	}
	if got := mode(t, path); got != 0o644 {
		t.Errorf("mode = %v, want the operator's own 0644 untouched", got)
	}
}

// TestAllowTightensTheModeWhenItWrites, matching the care worker.yaml
// gets. policy.yaml decides what this machine will run and serve; it is
// not a file to leave world-writable behind us.
func TestAllowTightensTheModeWhenItWrites(t *testing.T) {
	path := writePolicy(t, operatorPolicy, 0o644)

	if err := Allow(path, "nomic-embed-text"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

// TestAllowCreatesAMissingFile. Every fresh machine has no policy.yaml,
// which is the state `setup --inference` runs into first.
func TestAllowCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "policy.yaml")

	if err := Allow(path, "llama3.1:8b", "nomic-embed-text"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
	assertPolicyServes(t, path, "llama3.1:8b", "nomic-embed-text")
}

// TestAllowOnAModelsBlockWithNoAllowKey. A machine that declared an
// OpenAI-compatible runtime and never allowed anything is exactly the
// state docs/local-models.md describes.
func TestAllowOnAModelsBlockWithNoAllowKey(t *testing.T) {
	before := `apps:
  homes:
    api.memql.localhost:
      allow:
        - codex

models:
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
`
	path := writePolicy(t, before, 0o600)

	if err := Allow(path, "llama3.1:8b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	want := strings.Replace(before, "models:\n", "models:\n  allow:\n    - llama3.1:8b\n", 1)
	if got := read(t, path); got != want {
		t.Errorf("policy.yaml:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	assertPolicyServes(t, path, "llama3.1:8b")
}

// TestAllowOnAPolicyWithNoModelsBlock appends, leaving the file it found
// entirely intact above.
func TestAllowOnAPolicyWithNoModelsBlock(t *testing.T) {
	before := `shell:
  allow:
    - rustc
`
	path := writePolicy(t, before, 0o600)

	if err := Allow(path, "llama3.1:8b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	got := read(t, path)
	if !strings.HasPrefix(got, before) {
		t.Errorf("the file it found must survive as a prefix, got:\n%s", got)
	}
	assertPolicyServes(t, path, "llama3.1:8b")
	// And a second call must find the block it just wrote rather than
	// append a duplicate `models:` key, which is not valid YAML at all.
	if err := Allow(path, "nomic-embed-text"); err != nil {
		t.Fatalf("second Allow: %v", err)
	}
	if n := strings.Count(read(t, path), "\nmodels:"); n > 1 {
		t.Errorf("wrote %d models keys; a duplicate key makes the file unparseable", n)
	}
	assertPolicyServes(t, path, "llama3.1:8b", "nomic-embed-text")
}

// TestAllowOnAnEmptyAllowKey -- `allow:` with nothing under it, which is
// what somebody leaves behind after deleting the last model.
func TestAllowOnAnEmptyAllowKey(t *testing.T) {
	before := `models:
  allow:
  runtimes: []
`
	path := writePolicy(t, before, 0o600)

	if err := Allow(path, "llama3.1:8b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	want := "models:\n  allow:\n    - llama3.1:8b\n  runtimes: []\n"
	if got := read(t, path); got != want {
		t.Errorf("policy.yaml:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	assertPolicyServes(t, path, "llama3.1:8b")
}

// TestAllowOnAFlowSequence keeps the style the operator chose. Rewriting
// `[a, b]` as a block list is a reformat of a line nobody asked us to
// touch, and it is the kind of diff that makes a person distrust the
// command that produced it.
func TestAllowOnAFlowSequence(t *testing.T) {
	for _, tc := range []struct{ before, want string }{
		{"models:\n  allow: [llama3.1:8b]\n", "models:\n  allow: [llama3.1:8b, nomic-embed-text]\n"},
		{"models:\n  allow: []\n", "models:\n  allow: [nomic-embed-text]\n"},
		{"models:\n  allow: [llama3.1:8b]  # two of them\n", "models:\n  allow: [llama3.1:8b, nomic-embed-text]  # two of them\n"},
	} {
		path := writePolicy(t, tc.before, 0o600)
		if err := Allow(path, "nomic-embed-text"); err != nil {
			t.Fatalf("%q: %v", tc.before, err)
		}
		if got := read(t, path); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// TestAllowRefusesAShapeItCannotEditAndLeavesTheFileAlone.
//
// Refusing is the whole point. This command could always fall back to
// re-serialising the document, and the price would be an operator's
// comments and layout deleted by a command they ran to add one model. A
// refusal that names the line and the ids costs them thirty seconds; a
// silent reformat costs them the file.
func TestAllowRefusesAShapeItCannotEditAndLeavesTheFileAlone(t *testing.T) {
	for _, before := range []string{
		"models: {allow: [llama3.1:8b]}\n",
		"models:\n  allow: llama3.1:8b\n",
		"- shell\n- fs\n",
	} {
		path := writePolicy(t, before, 0o600)
		err := Allow(path, "nomic-embed-text")
		if !errors.Is(err, ErrPolicyNotEditable) {
			t.Errorf("%q: err = %v, want ErrPolicyNotEditable", before, err)
		}
		if err != nil && !strings.Contains(err.Error(), "nomic-embed-text") {
			t.Errorf("%q: err = %q, want the ids to add named so the person can do it by hand", before, err)
		}
		if got := read(t, path); got != before {
			t.Errorf("%q: the file was changed by a call that refused", before)
		}
	}
}

// TestAllowOnAMalformedFile. A policy.yaml the worker cannot parse is
// already broken; overwriting it would delete whatever the operator was
// halfway through fixing.
func TestAllowOnAMalformedFile(t *testing.T) {
	before := "models:\n  allow: [llama3.1:8b, nomic\n"
	path := writePolicy(t, before, 0o600)

	if err := Allow(path, "nomic-embed-text"); err == nil {
		t.Fatal("want an error for a file that does not parse")
	}
	if got := read(t, path); got != before {
		t.Error("a file that does not parse must come back untouched")
	}
}

// TestAllowWithNoIds writes nothing. `setup --inference` calls this after
// a pull loop that may have pulled nothing.
func TestAllowWithNoIds(t *testing.T) {
	path := writePolicy(t, operatorPolicy, 0o644)
	if err := Allow(path); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if err := Allow(path, "", "   "); err != nil {
		t.Fatalf("Allow with blanks: %v", err)
	}
	if got := read(t, path); got != operatorPolicy {
		t.Error("the file was rewritten with nothing to add")
	}
}

// TestAllowOnAFileWithNoTrailingNewline. Files written by other tools do
// not always have one, and a naive append would produce `- b    models:`
// on one line.
func TestAllowOnAFileWithNoTrailingNewline(t *testing.T) {
	path := writePolicy(t, "shell:\n  allow:\n    - rustc", 0o600)
	if err := Allow(path, "llama3.1:8b"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	assertPolicyServes(t, path, "llama3.1:8b")
}

// assertPolicyServes reads the result back with the SAME loader the worker
// uses. A write this package considered valid but tools.LoadPolicy could
// not parse would be a model that never reaches the cluster, with every
// line of output here saying it worked.
func assertPolicyServes(t *testing.T, path string, want ...string) {
	t.Helper()
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("tools.LoadPolicy(%s): %v\n%s", path, err, read(t, path))
	}
	got := p.ModelsAllow()
	for _, id := range want {
		found := false
		for _, g := range got {
			if g == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("models.allow = %v, want %q in it\n%s", got, id, read(t, path))
		}
	}
}

// -----------------------------------------------------------------------------
// The editor addressed by a key path: apps.homes.<home>.allow
// -----------------------------------------------------------------------------

var homeAllow = []string{"apps", "homes", "api.memql.localhost", "allow"}

// TestMergeListCreatesTheMappingsItNeeds. Each missing mapping is created
// under the deepest one that exists, at the indentation that mapping's own
// children already use, and nothing already there moves.
func TestMergeListCreatesTheMappingsItNeeds(t *testing.T) {
	for name, tc := range map[string]struct{ before, want string }{
		"no apps block": {
			"shell:\n  allow: [rustc]\n",
			"shell:\n  allow: [rustc]\n\napps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code\n",
		},
		"apps with levels only": {
			"apps:\n  levels:\n    codex: {}\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code\n  levels:\n    codex: {}\n",
		},
		"homes with nothing under it": {
			"apps:\n  homes:\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code\n",
		},
		"another cluster already there": {
			"apps:\n  homes:\n    other:\n      allow: [codex]\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code\n    other:\n      allow: [codex]\n",
		},
		"four-space indentation stays four-space": {
			"apps:\n    levels: {}\n",
			"apps:\n    homes:\n      api.memql.localhost:\n        allow:\n          - claude-code\n    levels: {}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, err := MergeList(tc.before, homeAllow, []string{"claude-code"})
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if got != tc.want {
				t.Errorf("--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
			assertListAt(t, got, homeAllow, "claude-code")
		})
	}
}

// A key YAML could misread is quoted rather than assumed safe: a colon one
// character from ending a key is a file the worker then cannot parse.
func TestMergeListQuotesAKeyYAMLCouldMisread(t *testing.T) {
	path := []string{"apps", "homes", "localhost:8443", "allow"}
	got, _, err := MergeList("", path, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"localhost:8443":`) {
		t.Errorf("the key was not quoted:\n%s", got)
	}
	assertListAt(t, got, path, "codex")
}

func TestRemoveFromList(t *testing.T) {
	for name, tc := range map[string]struct{ before, want string }{
		"one of several, block": {
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code  # keep me out\n        - codex\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - codex\n",
		},
		"a list level with its key": {
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n      - claude-code\n      - codex\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow:\n      - codex\n",
		},
		"the last one, flow": {
			"apps:\n  homes:\n    api.memql.localhost:\n      allow: [claude-code]\n",
			"apps:\n  homes:\n    api.memql.localhost:\n      allow: []\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, err := RemoveFromList(tc.before, homeAllow, []string{"Claude-Code"})
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if got != tc.want {
				t.Errorf("--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// Nothing listed is nothing to remove -- not a refusal, and not a write.
func TestRemoveFromListOfWhatIsNotThere(t *testing.T) {
	for _, before := range []string{
		"",
		"shell:\n  allow: [rustc]\n",
		"apps:\n  homes:\n",
		"apps:\n  homes:\n    api.memql.localhost:\n      allow: [codex]\n",
	} {
		got, changed, err := RemoveFromList(before, homeAllow, []string{"claude-code"})
		if err != nil || changed || got != before {
			t.Errorf("%q: changed=%v err=%v", before, changed, err)
		}
	}
}

func TestRemoveFromListRefusesWhatItCannotBound(t *testing.T) {
	for _, before := range []string{
		// A flow list over two lines: which text is which entry is a guess.
		"apps:\n  homes:\n    api.memql.localhost:\n      allow: [claude-code,\n        codex]\n",
		// A flow mapping on the path, and a word where the list belongs.
		"apps:\n  homes:\n    api.memql.localhost: {allow: [claude-code]}\n",
		"apps:\n  homes:\n    api.memql.localhost:\n      allow: claude-code\n",
	} {
		_, _, err := RemoveFromList(before, homeAllow, []string{"claude-code"})
		if !errors.Is(err, ErrPolicyNotEditable) || !strings.Contains(err.Error(), "claude-code from apps.homes.api.memql.localhost.allow by hand") {
			t.Errorf("%q: err = %v, want a refusal naming the change", before, err)
		}
	}
}

func TestRemoveKey(t *testing.T) {
	path := []string{"apps", "allow"}
	for name, tc := range map[string]struct {
		before, want string
		values       []string
	}{
		"a block list": {
			"apps:\n  allow:\n    - claude-code\n    - codex\n  levels: {}\n", "apps:\n  levels: {}\n", []string{"claude-code", "codex"},
		},
		"a flow list": {"apps:\n  allow: [claude-code]\nmodels: {}\n", "apps:\nmodels: {}\n", []string{"claude-code"}},
		"a bare key":  {"apps:\n  allow:\n  levels: {}\n", "apps:\n  levels: {}\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, values, changed, err := RemoveKey(tc.before, path)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if got != tc.want || strings.Join(values, ",") != strings.Join(tc.values, ",") {
				t.Errorf("got %q values %v, want %q values %v", got, values, tc.want, tc.values)
			}
		})
	}
	if got, _, changed, err := RemoveKey("apps:\n  levels: {}\n", path); err != nil || changed || got != "apps:\n  levels: {}\n" {
		t.Errorf("a key that is not there: changed=%v err=%v", changed, err)
	}
	if _, _, _, err := RemoveKey("apps:\n  allow: |\n    claude-code\n", path); !errors.Is(err, ErrPolicyNotEditable) {
		t.Errorf("a block scalar cannot be bounded by lines; err = %v, want a refusal", err)
	}
}

// assertListAt reads the list back through a YAML parse, so a write this
// package thought valid but no parser could read fails here.
func assertListAt(t *testing.T, body string, path []string, want ...string) {
	t.Helper()
	root, err := parseRoot(body)
	if err != nil {
		t.Fatalf("the result does not parse: %v\n%s", err, body)
	}
	_, v, _, ok := walkTo(root, path)
	if !ok || strings.Join(sequenceValues(v), ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v\n%s", strings.Join(path, "."), sequenceValues(v), want, body)
	}
}
