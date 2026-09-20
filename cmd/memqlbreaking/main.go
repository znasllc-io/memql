// Command memqlbreaking classifies the authoring-surface changes between two
// MemQL trees as parse, meaning or wire breaks, and refuses a deletion that
// carries no reserved entry (memql#5389, D21).
//
//	memqlbreaking -capture -out surface.json     write this tree's surface
//	memqlbreaking -baseline surface.json         diff this tree against it
//	memqlbreaking -baseline surface.json -json   the same, machine-readable
//
// With neither flag it diffs against the committed baseline,
// component/language/surface/2026.json, which is what the `memqlbreaking` make
// target runs.
//
// # Why this exists
//
// A breaking change to the DSL does not reach the person it breaks. It reaches
// a bundle author, in their own tree, weeks later, as a load failure with no
// way to tell a deliberate retirement from an accident -- and by then the
// commit that caused it is one of several hundred. What this command produces
// is the same fact at the commit that causes it: named, classified, and next to
// the ledger entry that says whether it was meant.
//
// # What it reports, and what it deliberately does not
//
// It reports BREAKS. An addition -- a new annotation, a new construct, a new
// function, a new shape key, a widened argument form -- produces nothing. A
// command that reports every change is a command whose output nobody reads, and
// then the one signal that matters drowns in the rest. The single exception is
// a RESERVED name coming back, which is a resurrection rather than an addition;
// reserved.go says why that one costs something.
//
// Exit codes: 0 no breaks; 1 breaks found; 2 bad usage.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("memqlbreaking", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		capture  = fs.Bool("capture", false, "write this tree's authoring surface instead of diffing")
		out      = fs.String("out", "", "with -capture: where to write the surface (default: the committed baseline)")
		baseline = fs.String("baseline", "", "the surface to diff this tree against (default: the committed baseline)")
		reserved = fs.String("reserved", "", "the reservation ledger (default: "+ReservedPath+")")
		asJSON   = fs.Bool("json", false, "emit the findings as JSON")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: memqlbreaking [-capture -out <file>] [-baseline <file>] [-reserved <file>] [-json]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Classifies the authoring-surface changes between a captured baseline and this tree")
		fmt.Fprintln(stderr, "as parse, meaning or wire breaks, and refuses a deletion with no reserved entry.")
		fmt.Fprintln(stderr, "")
		fs.PrintDefaults()
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Exit codes:")
		fmt.Fprintln(stderr, "  0  no breaks")
		fmt.Fprintln(stderr, "  1  breaks found")
		fmt.Fprintln(stderr, "  2  invalid usage / filesystem error")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "ERROR: unexpected argument %q; every input is a flag\n", fs.Arg(0))
		return 2
	}

	current := Capture()
	shapes, shapesErr := CaptureShapes(memqldsl.Tree())
	if shapesErr == nil {
		current.Shapes = shapes
	}

	if *capture {
		// A CAPTURE must be whole. Writing a baseline whose shape half is
		// missing records 232 shapes as never having existed, and the next run
		// reports them all as deleted -- a wire catastrophe that is really a
		// tree that did not load.
		if shapesErr != nil {
			fmt.Fprintf(stderr, "ERROR: %v\n", firstLines(shapesErr, 3))
			fmt.Fprintln(stderr, "ERROR: refusing to capture a baseline without its shape half; fix the tree first "+
				"(go run ./cmd/memqllint dsl/ says the same thing at greater length)")
			return 2
		}
		path := *out
		if path == "" {
			path = defaultBaselinePath()
		}
		if err := WriteSurface(path, current); err != nil {
			fmt.Fprintf(stderr, "ERROR: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "SUCCESS: wrote the authoring surface of edition %s (grammar %s) to %s\n",
			current.Edition, current.GrammarVersion, path)
		fmt.Fprintf(stdout, "         %s\n", counts(current))
		return 0
	}

	basePath := *baseline
	if basePath == "" {
		basePath = defaultBaselinePath()
	}
	base, err := LoadSurface(basePath)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return 2
	}
	resPath := *reserved
	if resPath == "" {
		resPath = defaultReservedPath()
	}
	res, err := LoadReservations(resPath)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return 2
	}

	// An UNMEASURED half is reported as unmeasured, never as zero and never as
	// deleted (memql#5389).
	//
	// This is the case the command exists for, so getting it wrong would be
	// costly: removing an annotation the tree itself writes makes the tree stop
	// loading, and the first cut of this command answered that by exiting 2 with
	// a wall of load diagnostics -- refusing to classify at the exact commit it
	// was built to classify. The registry half needs no tree, and the parse
	// break is in it, named.
	//
	// So the shape half is compared against ITSELF when it could not be read,
	// which produces no wire findings, and the run says so on a line of its own.
	// It then cannot answer "no breaks" -- that claim would be made over a third
	// of a surface this run never looked at -- so it exits 1 either way.
	if shapesErr != nil {
		current.Shapes = base.Shapes
	}

	findings := Diff(base, current, res)

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Baseline     string    `json:"baseline"`
			Was          Surface   `json:"-"`
			Edition      string    `json:"edition"`
			Grammar      string    `json:"grammarVersion"`
			Findings     []Finding `json:"findings"`
			ShapesUnread string    `json:"shapesUnread,omitempty"`
		}{Baseline: basePath, Edition: current.Edition, Grammar: current.GrammarVersion, Findings: findings,
			ShapesUnread: shapesUnread(shapesErr)}); err != nil {
			fmt.Fprintf(stderr, "ERROR: encoding findings: %v\n", err)
			return 2
		}
		if len(findings) > 0 || shapesErr != nil {
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "INFO: baseline %s -- edition %s, grammar %s\n", basePath, base.Edition, base.GrammarVersion)
	fmt.Fprintf(stdout, "INFO: this tree -- edition %s, grammar %s\n", current.Edition, current.GrammarVersion)
	fmt.Fprintf(stdout, "INFO: %s\n", counts(current))
	if shapesErr != nil {
		fmt.Fprintf(stdout, "WARNING: the shape (wire) half of the surface was NOT read: %s\n",
			firstLines(shapesErr, 3))
		fmt.Fprintln(stdout, "WARNING: shapes are therefore not compared -- an unread half is reported as unread, "+
			"never as zero and never as deleted.")
	}
	if len(findings) == 0 {
		if shapesErr != nil {
			fmt.Fprintf(stdout, "ERROR: no breaks in the registry half, and the shape half was not read, so this run "+
				"cannot say there are no breaks against %s\n", basePath)
			return 1
		}
		fmt.Fprintf(stdout, "SUCCESS: no breaks against %s\n", basePath)
		return 0
	}
	byCat := map[Category]int{}
	for _, f := range findings {
		byCat[f.Category]++
	}
	fmt.Fprintf(stdout, "ERROR: %d break(s) against %s -- %d parse, %d meaning, %d wire\n\n",
		len(findings), basePath, byCat[CategoryParse], byCat[CategoryMeaning], byCat[CategoryWire])
	for _, f := range findings {
		fmt.Fprintf(stdout, "  %s\n", f)
	}
	fmt.Fprintf(stdout, "\nINFO: a deliberate break is landed by reserving what was removed in %s and then\n"+
		"      re-capturing the baseline (the memqlbreaking-capture make target) in the SAME change, so\n"+
		"      the reservation and the surface move together.\n", ReservedPath)
	return 1
}

// firstLines trims a multi-line load failure to its first n lines plus a count.
//
// The tree's loader reports every refusal, and a removed annotation the tree
// itself writes produces one per file -- twenty-one of them, here, for one
// deletion. Printing all of them buries the ONE finding this command exists to
// produce under its own consequences, which is the drowning this command's
// whole design is arranged against.
func firstLines(err error, n int) string {
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n         ")
	}
	return strings.Join(lines[:n], "\n         ") +
		fmt.Sprintf("\n         ... and %d more (go run ./cmd/memqllint dsl/ prints them all)", len(lines)-n)
}

// shapesUnread renders the JSON form's one-line reason, or "" when the shape
// half was read. Absent and empty are different answers, so the field is
// omitted rather than blank when there is nothing to say.
func shapesUnread(err error) string {
	if err == nil {
		return ""
	}
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

// counts is the one line that says how much surface was read. A report over a
// surface that captured almost nothing is indistinguishable from a clean one
// without it -- the empty-set fail-open this repo names by that phrase.
func counts(s Surface) string {
	return fmt.Sprintf("surface: %d constructs, %d annotations, %d functions, %d shapes",
		len(s.Constructs), len(s.Annotations), len(s.Functions), len(s.Shapes))
}
