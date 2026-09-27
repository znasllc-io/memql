// Command affected plans a run of .github/workflows/ci.yml (epic memql#5476):
// which Go packages a pull request reaches, how go-tests and db-tests are
// sharded, whether the node-tag passes run, and which modules
// module-boundaries checks. Every decision lives in scripts/ci/selection;
// this file only gathers the inputs and writes the outputs.
//
//	go run ./scripts/ci/affected plan --event=pull_request --base=<sha> \
//	    --classes="$CLASSES" --timings=scripts/ci/shard-timings.tsv \
//	    --gate-packages="$GATE_PACKAGES" --tag-trees="$TAG_TREES" \
//	    --github-output="$GITHUB_OUTPUT" --summary="$GITHUB_STEP_SUMMARY"
//	go run ./scripts/ci/affected timings --lane=go --table=scripts/ci/shard-timings.tsv LOG...
//	go run ./scripts/ci/affected graph [--out=FILE]
//
// Exit codes: 0 ok, 2 bad parameter, 5 refused -- a plan that cannot be
// trusted, which fails the plan job and with it ci-required. A change the
// planner merely cannot narrow is not a refusal: it plans a full run.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/znasllc-io/memql/scripts/ci/selection"
)

// modulePath is the first-party import path prefix. Pinned rather than read
// from `go list -m`, for the reason db-gated-packages.sh gives: under a
// workspace that prints one line per module.
const modulePath = "github.com/znasllc-io/memql"

const (
	exitOK       = 0
	exitBadParam = 2
	exitRefused  = 5
)

// errBadParam marks an error as the caller's, so it exits 2 rather than 5.
var errBadParam = errors.New("bad parameter")

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitBadParam
	}
	var err error
	switch args[0] {
	case "plan":
		err = runPlan(ctx, args[1:], stdout, stderr)
	case "timings":
		err = runTimings(args[1:], stderr)
	case "graph":
		err = runGraph(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "affected: unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitBadParam
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errBadParam) || errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(stderr, "affected %s: %v\n", args[0], err)
		return exitBadParam
	default:
		fmt.Fprintf(stderr, "affected %s: REFUSED: %v\n", args[0], err)
		return exitRefused
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  affected plan --event=E [--base=SHA] --classes=TABLE --timings=FILE
                --gate-packages="PATTERNS" --tag-trees="DIRS"
                [--github-output=FILE] [--summary=FILE]
  affected timings --lane=go|db --table=FILE LOG...
  affected graph [--out=FILE]
`)
}

func badParam(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errBadParam, fmt.Sprintf(format, a...))
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	event := fs.String("event", "", "github.event_name")
	base := fs.String("base", "", "the pull request's base commit (pull_request only)")
	classes := fs.String("classes", "", "the class table: lane class trees timeout mode shards, one per line")
	timings := fs.String("timings", "", "the timing table file")
	gate := fs.String("gate-packages", "", "space-separated gate-input package patterns")
	tagTrees := fs.String("tag-trees", "", "space-separated directories the node-tag passes test")
	ghOut := fs.String("github-output", "", "append key=value outputs here (default: stdout)")
	summary := fs.String("summary", "", "append the markdown summary here")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errBadParam, err)
	}
	if fs.NArg() != 0 {
		return badParam("unexpected arguments %v", fs.Args())
	}
	if *event == "" || *classes == "" || *timings == "" || strings.TrimSpace(*gate) == "" || strings.TrimSpace(*tagTrees) == "" {
		return badParam("--event, --classes, --timings, --gate-packages and --tag-trees are all required")
	}
	parsedClasses, err := selection.ParseClasses(*classes)
	if err != nil {
		return fmt.Errorf("%w: %v", errBadParam, err)
	}
	table, err := readTable(*timings)
	if err != nil {
		return err
	}

	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	tags, err := selection.NodeTags(root)
	if err != nil {
		return err
	}
	start := time.Now()
	g, err := selection.LoadGraph(ctx, root, modulePath, tags, selection.GoList(root, modulePath+"/..."))
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "affected: graph of %d packages over the default build and tags %v in %s\n",
		g.Len(), tags, time.Since(start).Round(time.Millisecond))
	trees, err := dbGated(ctx, root, "--trees")
	if err != nil {
		return err
	}
	complement, err := dbGated(ctx, root, "--complement")
	if err != nil {
		return err
	}

	in := selection.PlanInput{
		Event:        *event,
		Graph:        g,
		DBTrees:      trees,
		Complement:   complement,
		Classes:      parsedClasses,
		Timings:      table,
		GatePatterns: strings.Fields(*gate),
		TagTrees:     strings.Fields(*tagTrees),
		CPUs:         runtime.NumCPU(),
	}
	if *event == "pull_request" {
		changed, err := changedFiles(ctx, root, *base)
		if err != nil {
			in.DiffError = err.Error()
		} else {
			in.Changed = changed
		}
	}
	plan, err := selection.MakePlan(in)
	if err != nil {
		return err
	}

	out, err := plan.GitHubOutput()
	if err != nil {
		return err
	}
	if err := appendTo(*ghOut, out, stdout); err != nil {
		return err
	}
	text := plan.Summary()
	fmt.Fprint(stderr, text)
	if *summary != "" {
		if err := appendTo(*summary, text, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

func runTimings(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("timings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lane := fs.String("lane", "", "go or db")
	tablePath := fs.String("table", "", "the timing table to rewrite")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errBadParam, err)
	}
	if *lane != "go" && *lane != "db" {
		return badParam("--lane must be go or db, got %q", *lane)
	}
	if *tablePath == "" || fs.NArg() == 0 {
		return badParam("--table and at least one log file are required")
	}
	table := selection.Timings{}
	if _, err := os.Stat(*tablePath); err == nil {
		if table, err = readTable(*tablePath); err != nil {
			return err
		}
	}
	samples := map[string][]float64{}
	for _, name := range fs.Args() {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		got, err := selection.ParseGoTestOutput(f, modulePath)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for dir, xs := range got {
			samples[dir] = append(samples[dir], xs...)
		}
	}
	if len(samples) == 0 {
		return fmt.Errorf("no passing `ok <package> <N>s` line in %d log file(s): these are not %s-lane test logs, and rewriting the table from them would erase it", fs.NArg(), *lane)
	}
	table.Merge(*lane, samples)
	var buf bytes.Buffer
	header := []string{
		"scripts/ci/shard-timings.tsv -- measured `go test` seconds per package: the",
		"weights scripts/ci/affected balances the go-tests and db-tests shards with",
		"(memql#5485). A package missing from it still runs, in the lightest shard of",
		"its class. Rewrite it with scripts/ci/refresh-shard-timings.sh, not by hand.",
		"last refreshed " + time.Now().UTC().Format("2006-01-02"),
	}
	if err := table.Write(&buf, header); err != nil {
		return err
	}
	if err := os.WriteFile(*tablePath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "affected: %d %s-lane package(s) measured from %d log file(s) into %s\n", len(samples), *lane, fs.NArg(), *tablePath)
	return nil
}

func runGraph(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outPath := fs.String("out", "", "write the graph here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errBadParam, err)
	}
	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	tags, err := selection.NodeTags(root)
	if err != nil {
		return err
	}
	g, err := selection.LoadGraph(ctx, root, modulePath, tags, selection.GoList(root, modulePath+"/..."))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := g.WriteJSON(&buf); err != nil {
		return err
	}
	if *outPath == "" {
		_, err = stdout.Write(buf.Bytes())
		return err
	}
	return os.WriteFile(*outPath, buf.Bytes(), 0o644)
}

func readTable(path string) (selection.Timings, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadParam, err)
	}
	defer f.Close()
	t, err := selection.ReadTimings(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func repoRoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("locating the repository root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		return "", fmt.Errorf("%s has no go.work: not the memql repository root", root)
	}
	return root, nil
}

// dbGated runs scripts/ci/db-gated-packages.sh, the canonical db-gated set,
// rather than restating it: every refusal that script makes (an unknown
// nested module, a truncated `go list`, a complement under its floor) then
// refuses the plan too.
func dbGated(ctx context.Context, root, mode string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts", "ci", "db-gated-packages.sh"), mode)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("db-gated-packages.sh %s: %w: %s", mode, err, strings.TrimSpace(stderr.String()))
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("db-gated-packages.sh %s printed nothing", mode)
	}
	return lines, nil
}

// changedFiles lists the files a pull request changes: the diff between its
// base commit and the merge commit Actions checks out, against their merge
// base (`base...HEAD`). Renames are split (--no-renames) so the OLD path of a
// moved file is reported too: its package lost a file, and that is a change.
//
// With `fetch-depth: 2` the merge commit's first parent IS the base, so the
// history this needs is already present; a base that is not is fetched once
// before giving up. Any failure is returned rather than papered over -- the
// caller turns it into a full run, never an empty change list.
func changedFiles(ctx context.Context, root, base string) ([]string, error) {
	if strings.TrimSpace(base) == "" {
		return nil, errors.New("no base commit was supplied")
	}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	if _, err := git("cat-file", "-e", base+"^{commit}"); err != nil {
		if _, ferr := git("fetch", "--no-tags", "--depth=1", "origin", base); ferr != nil {
			return nil, fmt.Errorf("base commit %s is not in the checkout and could not be fetched: %v", base, ferr)
		}
	}
	out, err := git("diff", "--name-only", "--no-renames", "-z", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if s := string(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// appendTo appends text to the named file, or writes it to fallback when no
// file is named.
func appendTo(path, text string, fallback io.Writer) error {
	if path == "" {
		_, err := io.WriteString(fallback, text)
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
