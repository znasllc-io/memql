// A quiet grep must never read from a tar pipe in a workflow (memql#5713).
//
// publish-docs-bundle.yml verified its bundle with
//
//	tar -tzf "$tarball" | grep -qx './manifest.json'
//
// under `set -euo pipefail`. grep -q exits on the first match; tar's next write
// then takes EPIPE and tar exits non-zero; pipefail makes that the pipeline's
// status, and the `||` branch reported the manifest missing from a bundle that
// had one. It failed all 52 release runs from v0.20.22 through v0.23.5, and
// because the lane is not required nothing turned red anywhere a person looks:
// memql.io stayed on docs-0.20.21.tgz for three weeks.
//
// The failure depends on where the match sits in the listing, so it cannot be
// caught by running the step once on a small archive. It is caught here, by
// shape: every `run:` body in every workflow and composite action is split into
// pipelines, and a pipeline that runs tar in one stage and a quiet grep in a
// later one is refused. The fix is to write the listing to a file and grep the
// file, as the docs workflow now does.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// tarCommand matches tar in a command position: at the start of a stage, or
// after a character that starts a command (`$(`, `(`, `;`, `&`, `!`). "untar"
// or "tarball" do not match.
var tarCommand = regexp.MustCompile(`(?:^|[\s;&(!])(?:gtar|bsdtar|tar)(?:\s|$)`)

// grepCommand matches the grep family in a command position and captures the
// rest of the stage, whose leading options are read for a quiet flag.
var grepCommand = regexp.MustCompile(`(?:^|[\s;&(!])(?:grep|egrep|fgrep|zgrep)(\s.*)?$`)

func TestNoWorkflowQuietGrepsATarPipe(t *testing.T) {
	files := workflowShellFiles(t)
	if len(files) == 0 {
		t.Fatal("found no workflow files under .github; the guard would pass vacuously")
	}
	for _, path := range files {
		for _, run := range runBodies(t, path) {
			for _, hit := range quietGrepOnTarPipe(run) {
				t.Errorf("%s: a quiet grep reads from a tar pipe, which fails under pipefail "+
					"whenever grep exits before tar finishes writing (memql#5713):\n\t%s\n"+
					"write the listing to a file and grep the file instead", relToRepo(path), hit)
			}
		}
	}
}

// The guard is only worth its line if it fires on the shape that broke the
// release lane and stays quiet on the fix. Each case is a run body as a workflow
// would carry it.
func TestQuietGrepOnTarPipeDetector(t *testing.T) {
	cases := []struct {
		name string
		run  string
		want int
	}{
		{"the shape that failed every release since v0.20.22", `set -euo pipefail
tar -tzf "$tarball" | grep -qx './manifest.json' \
  || { echo "ERROR: $tarball has no manifest.json" >&2; exit 1; }`, 1},
		{"quiet flag in a cluster", `tar tzf x.tgz | grep -Eq '^a$'`, 1},
		{"long quiet flag", `tar -tzf x.tgz | grep --quiet a`, 1},
		{"silent spelling", `tar -tzf x.tgz | grep --silent a`, 1},
		{"a stage between tar and grep", `tar -tzf x.tgz | sort | grep -q a`, 1},
		{"inside a command substitution", `if [ -n "$(tar -tzf x.tgz | grep -q a && echo y)" ]; then :; fi`, 1},
		{"inside an if", `if tar -tzf x.tgz | grep -q a; then echo ok; fi`, 1},
		{"the fix: grep reads a captured listing", `tar -tzf "$tarball" > "$listing"
grep -qx './manifest.json' "$listing" || exit 1`, 0},
		{"a counting grep reads the whole pipe", `echo "$(tar -tzf x.tgz | grep -c '\.md$')"`, 0},
		{"an or-list is not a pipe", `tar -xzf x.tgz || grep -q a b.txt`, 0},
		{"a quiet grep on a non-tar pipe", `git status --porcelain | grep -q .`, 0},
		{"a comment naming the pattern", `# never write tar -tzf x | grep -q y`, 0},
		{"tar in a word", `echo tarball | grep -q ball`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quietGrepOnTarPipe(c.run)
			if len(got) != c.want {
				t.Errorf("found %d offending pipelines, want %d: %q", len(got), c.want, got)
			}
		})
	}
}

// quietGrepOnTarPipe returns every logical line of a shell body that pipes tar
// into a quiet grep. Continuation lines are joined first, so a pipeline split
// across lines with a trailing backslash is read as one.
func quietGrepOnTarPipe(body string) []string {
	var hits []string
	for _, line := range logicalLines(body) {
		stages := pipeStages(line)
		tarAt := -1
		for i, stage := range stages {
			if tarAt < 0 && tarCommand.MatchString(stage) {
				tarAt = i
				continue
			}
			if tarAt >= 0 && i > tarAt && isQuietGrep(stage) {
				hits = append(hits, strings.TrimSpace(line))
				break
			}
		}
	}
	return hits
}

// logicalLines joins backslash continuations and drops whole-line comments.
func logicalLines(body string) []string {
	var out []string
	var cur strings.Builder
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if cur.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			cur.WriteString(strings.TrimSuffix(line, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// pipeStages splits a line on single `|` (and `|&`), leaving `||` inside a
// stage: `a | b || c` is the pipeline a -> "b || c".
func pipeStages(line string) []string {
	var stages []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] != '|' {
			continue
		}
		if i+1 < len(line) && line[i+1] == '|' {
			i++ // `||`: an or-list, not a pipe
			continue
		}
		stages = append(stages, line[start:i])
		if i+1 < len(line) && line[i+1] == '&' {
			i++ // `|&`
		}
		start = i + 1
	}
	return append(stages, line[start:])
}

// isQuietGrep reports whether a stage runs grep with -q, a short-option cluster
// holding q (-qx, -Eq), --quiet or --silent among its leading options.
func isQuietGrep(stage string) bool {
	m := grepCommand.FindStringSubmatch(stage)
	if m == nil {
		return false
	}
	for _, tok := range strings.Fields(m[1]) {
		switch {
		case tok == "--":
			return false
		case tok == "--quiet" || tok == "--silent":
			return true
		case strings.HasPrefix(tok, "--"):
			continue
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			if strings.Contains(tok[1:], "q") {
				return true
			}
		default:
			return false // the first operand: options have ended
		}
	}
	return false
}

// workflowShellFiles lists every workflow and composite action definition.
func workflowShellFiles(t *testing.T) []string {
	t.Helper()
	root := repoRootFromHere(t)
	var files []string
	for _, pattern := range []string{
		filepath.Join(root, ".github", "workflows", "*.yml"),
		filepath.Join(root, ".github", "workflows", "*.yaml"),
		filepath.Join(root, ".github", "actions", "*", "action.yml"),
		filepath.Join(root, ".github", "actions", "*", "action.yaml"),
	} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	return files
}

// runBodies returns the value of every `run:` key in a workflow or action file,
// wherever it sits (job steps, composite steps). Parsed with yaml.v3 so YAML
// comments and block-scalar indentation never reach the scan.
func runBodies(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", relToRepo(path), err)
	}
	var runs []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value == "run" && v.Kind == yaml.ScalarNode {
					runs = append(runs, v.Value)
					continue
				}
				walk(v)
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return runs
}

func repoRootFromHere(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/release/ -> repo root is two directories up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func relToRepo(path string) string {
	if i := strings.Index(path, ".github"); i >= 0 {
		return path[i:]
	}
	return path
}
