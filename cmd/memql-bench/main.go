// Command memql-bench runs the proving corpus and publishes what it measured.
//
// It ADOPTS THE CAPABILITY-SCRIPT CONTRACT
// (docs/internal/design/capability-script-contract.md): `--flag=value` in, one
// JSON envelope on stdout, every human line on stderr, `--print-spec`, and the
// contract's exit codes. `scripts/lib/capability_contract_test.go` walks
// `scripts/**/*.sh` and will never see a Go binary, so what stands behind that
// claim is component/proving/capability -- whose test reads the printf format
// strings out of `scripts/lib/capability.sh` and fails when the two drift --
// plus the `proving` CI job, which parses this binary's envelope.
//
// Subcommands are selected by --do, not by a positional verb: the contract
// takes no positional arguments, and a verb in argv[1] is exactly the
// position-dependent parameter a machine caller gets wrong.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/proving"
	"github.com/znasllc-io/memql/component/proving/capability"
	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/scorecard"
)

// Paths, relative to the repository root. They are parameters with defaults
// rather than constants so a caller can point the binary at a fixture tree.
const (
	defaultCorpus    = "test/proving/scenarios"
	defaultCassettes = "test/proving/cassettes"
	defaultScorecard = "docs/public/overview/proving/scorecard"
	defaultPage      = "docs/public/overview/proving-scorecard.md"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(argv []string, stdout, stderr io.Writer) int {
	spec := capability.Spec{
		Id:      "bench.run",
		Summary: "Run the memQL proving corpus and publish what it measured",
		Params: []capability.Param{
			{Name: "do", Description: "run | gate | scorecard | record", Required: true},
			{Name: "tier", Description: "ci (replayed, no provider) or live (real providers)"},
			{Name: "corpus", Description: "directory of scenario JSON (default " + defaultCorpus + ")"},
			{Name: "cassettes", Description: "directory of recorded responses (default " + defaultCassettes + ")"},
			{Name: "scorecard-dir", Description: "directory of dated scorecards (default " + defaultScorecard + ")"},
			{Name: "page", Description: "generated page path (default " + defaultPage + ")"},
			{Name: "dsn", Description: "Postgres DSN; falls back to MEMQL_DATABASE_DSN"},
			{Name: "commit", Description: "short SHA to stamp on every figure; read from git when absent"},
			{Name: "date", Description: "YYYY-MM-DD to stamp; today when absent"},
			{Name: "runner", Description: "the machine class, so a wall-clock figure is not compared across hardware"},
			{Name: "write", Description: "write the scorecard and the page (default false: report only)"},
			{Name: "check", Description: "fail when the generated page is stale rather than rewriting it"},
			{Name: "ceiling-usd", Description: "live tier only: refuse rather than spend more than this"},
			{Name: "models", Description: "live tier only: comma-separated provider tiers to run"},
			{Name: "scenario", Description: "record only: the scenario to record, or `all`"},
			{Name: "synthetic", Description: "record only: write placeholder cassettes instead of calling a provider"},
		},
	}

	c, handled, err := capability.Parse(spec, argv, stdout, stderr)
	if handled {
		return capability.ExitOK
	}
	if err != nil {
		if pe, ok := err.(*capability.ParseError); ok {
			return c.Fail(pe.Code, "%s", pe.Msg)
		}
		return c.Fail(capability.ExitBadParam, "%v", err)
	}

	switch c.Param("do", "") {
	case "run":
		return doRun(c)
	case "gate":
		return doGate(c)
	case "scorecard":
		return doScorecard(c)
	case "record":
		return doRecord(c)
	default:
		return c.Fail(capability.ExitBadParam, "--do=%q is not one of run, gate, scorecard, record", c.Param("do", ""))
	}
}

// doRun runs the corpus and, with --write, publishes the result.
func doRun(c *capability.Capability) int {
	tier := figure.Tier(c.Param("tier", string(figure.TierCI)))
	if !tier.Valid() {
		return c.Fail(capability.ExitBadParam, "--tier=%q is not ci or live", tier)
	}
	if tier == figure.TierLive {
		// The live lane ships DISARMED (design P3). Refusing here rather than
		// silently running the CI path is what stops a caller believing it
		// measured against a provider.
		return c.Fail(capability.ExitPrerequisite,
			"the live tier is not armed in this build: it needs a provider credential and a per-run dollar ceiling, "+
				"and .github/workflows/proving-live.yml is workflow_dispatch with no schedule. "+
				"Arm it deliberately rather than by running this")
	}

	corpus, cassettes, code := loadInputs(c)
	if code != capability.ExitOK {
		return code
	}

	prov, code := provenance(c, tier)
	if code != capability.ExitOK {
		return code
	}

	eng, closeDB, code := openEngine(c)
	if code != capability.ExitOK {
		return code
	}
	defer closeDB()

	r := &proving.Runner{
		Engine:    eng,
		Cassettes: cassettes,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Prov:      prov,
	}

	c.Step("running %d scenarios on both arms", len(corpus.Scenarios))
	result, err := r.Run(context.Background(), corpus)
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "%v", err)
	}

	result.Scorecard.Tiers[figure.TierCI] = scorecard.TierState{
		LastRun: prov.Date, Armed: true,
		Note: "replayed on every pull request; it reaches no provider. Responses come from " +
			strings.Join(cassettes.ModelIds(), ", ") + ".",
	}
	result.Scorecard.Tiers[figure.TierLive] = scorecard.TierState{
		Armed: false,
		Note:  "dispatched by hand from proving-live.yml; it has not been run, so every live figure reads `tierNotRun`",
	}

	c.Set("scenarios", len(corpus.Scenarios))
	c.Set("corpusFingerprint", corpus.Fingerprint)
	c.Set("entries", len(result.Scorecard.Entries))
	c.Set("measured", countMeasured(result.Scorecard))
	c.Set("unmeasured", len(result.Scorecard.Entries)-countMeasured(result.Scorecard))
	c.Set("families", scorecard.FamilySummary(result.Scorecard))

	if blocking := result.Blocking(); len(blocking) > 0 {
		for _, b := range blocking {
			c.Warn("%s", b)
		}
		c.Set("blocking", blocking)
		return c.Fail(capability.ExitOpFailed, "%d blocking failure(s); the first is: %s", len(blocking), blocking[0])
	}

	if write, err := c.Bool("write", false); err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	} else if write {
		paths, werr := publish(c, result.Scorecard)
		if werr != nil {
			return c.Fail(capability.ExitOpFailed, "%v", werr)
		}
		c.Changed()
		c.Set("wrote", paths)

		// The graph rows are the OTHER publication, and they are written
		// after the committed artifact rather than instead of it. A refusal
		// here is reported and does NOT fail the run: the measurement is the
		// product of the run, and losing the published copy of it must not
		// turn a green suite red.
		runId, problems := r.WriteRows(context.Background(), result)
		c.Set("benchRunId", runId)
		if len(problems) > 0 {
			for _, p := range problems {
				c.Warn("%s", p)
			}
			c.Set("rowProblems", problems)
		}
	}

	for _, line := range scorecard.FamilySummary(result.Scorecard) {
		c.Info("%s", line)
	}
	return c.OK()
}

// doGate compares a fresh run against the committed scorecard.
func doGate(c *capability.Capability) int {
	corpus, cassettes, code := loadInputs(c)
	if code != capability.ExitOK {
		return code
	}
	prov, code := provenance(c, figure.TierCI)
	if code != capability.ExitOK {
		return code
	}
	eng, closeDB, code := openEngine(c)
	if code != capability.ExitOK {
		return code
	}
	defer closeDB()

	r := &proving.Runner{
		Engine:    eng,
		Cassettes: cassettes,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Prov:      prov,
	}
	result, err := r.Run(context.Background(), corpus)
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "%v", err)
	}
	result.Scorecard.Tiers[figure.TierCI] = scorecard.TierState{LastRun: prov.Date, Armed: true, Note: "this run"}
	result.Scorecard.Tiers[figure.TierLive] = scorecard.TierState{Armed: false, Note: "dispatched by hand; not run"}

	dir := c.Param("scorecard-dir", defaultScorecard)
	before, name, have, err := scorecard.Newest(os.DirFS("."), dir)
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "reading %s: %v", dir, err)
	}

	g := scorecard.Gate(before, result.Scorecard, have)
	fmt.Fprint(os.Stderr, g.Render())

	c.Set("against", name)
	c.Set("blocking", len(g.Blocking))
	c.Set("lostMetrics", g.LostMetrics)
	c.Set("reported", len(g.Reported))
	c.Set("improvements", len(g.Improvements))
	c.Set("undecidable", len(g.Undecidable))

	suiteBlocking := result.Blocking()
	if len(suiteBlocking) > 0 {
		c.Set("suiteFailures", suiteBlocking)
		return c.Fail(capability.ExitOpFailed, "the suite itself failed: %s", suiteBlocking[0])
	}
	if !g.Passed() {
		return c.Fail(capability.ExitOpFailed, "%d blocking regression(s) and %d lost metric(s)", len(g.Blocking), len(g.LostMetrics))
	}
	return c.OK()
}

// doScorecard regenerates the page from the newest committed scorecard.
func doScorecard(c *capability.Capability) int {
	dir := c.Param("scorecard-dir", defaultScorecard)
	page := c.Param("page", defaultPage)

	s, name, have, err := scorecard.Newest(os.DirFS("."), dir)
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "reading %s: %v", dir, err)
	}
	if !have {
		return c.Fail(capability.ExitPrerequisite, "%s holds no scorecard yet; run `--do=run --write` first", dir)
	}
	want := scorecard.RenderPage(s)

	check, err := c.Bool("check", false)
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	got, readErr := os.ReadFile(page)
	if check {
		if readErr != nil {
			return c.Fail(capability.ExitOpFailed, "%s does not exist; regenerate it with `--do=scorecard`", page)
		}
		if string(got) != want {
			return c.Fail(capability.ExitOpFailed,
				"%s is stale against %s/%s. The JSON is the source and the page is derived: regenerate with `--do=scorecard`", page, dir, name)
		}
		c.Set("page", page)
		c.Set("source", name)
		return c.OK()
	}

	if readErr == nil && string(got) == want {
		c.Set("page", page)
		c.Set("source", name)
		return c.OK()
	}
	if err := os.WriteFile(page, []byte(want), 0o644); err != nil {
		return c.Fail(capability.ExitOpFailed, "writing %s: %v", page, err)
	}
	c.Changed()
	c.Set("page", page)
	c.Set("source", name)
	return c.OK()
}

// doRecord captures cassettes.
func doRecord(c *capability.Capability) int {
	synthetic, err := c.Bool("synthetic", false)
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	if !synthetic {
		// Recording against a real model needs a provider, which this build
		// does not wire. Refusing with a prerequisite code and naming what is
		// missing is honest; quietly writing placeholders and calling them a
		// recording is the failure this whole suite exists to avoid.
		return c.Fail(capability.ExitPrerequisite,
			"recording against a real model needs a provider credential and the live lane, which is not armed in this build. "+
				"Pass --synthetic to write placeholder cassettes: they make a run deterministic and measure the STRUCTURE "+
				"(exchanges needed, steps re-executed, effects duplicated), and every model-dependent figure stays `notMeasurableOnReplay`")
	}

	corpusDir := c.Param("corpus", defaultCorpus)
	cassetteDir := c.Param("cassettes", defaultCassettes)
	corpus, err := scenario.Load(os.DirFS("."), corpusDir)
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "%v", err)
	}
	only := c.Param("scenario", "all")
	date := c.Param("date", time.Now().UTC().Format("2006-01-02"))

	var wrote []string
	for _, s := range corpus.Scenarios {
		if only != "all" && s.Id != only {
			continue
		}
		if !proving.NeedsCassette(s) {
			continue
		}
		for _, arm := range []figure.Arm{figure.ArmPlatform, figure.ArmBaseline} {
			path, werr := proving.WriteCassette(cassetteDir, proving.SyntheticCassette(s, arm, date))
			if werr != nil {
				return c.Fail(capability.ExitOpFailed, "%v", werr)
			}
			wrote = append(wrote, path)
		}
	}
	if len(wrote) == 0 && only != "all" {
		return c.Fail(capability.ExitBadParam, "no scenario named %q needs a cassette", only)
	}
	c.Changed()
	c.Set("wrote", wrote)
	c.Set("synthetic", true)
	return c.OK()
}

// --- shared plumbing -------------------------------------------------------

func loadInputs(c *capability.Capability) (scenario.Corpus, cassette.Set, int) {
	corpusDir := c.Param("corpus", defaultCorpus)
	corpus, err := scenario.Load(os.DirFS("."), corpusDir)
	if err != nil {
		return scenario.Corpus{}, cassette.Set{}, c.Fail(capability.ExitOpFailed, "%v", err)
	}
	cassetteDir := c.Param("cassettes", defaultCassettes)
	set, err := cassette.Load(os.DirFS("."), cassetteDir)
	if err != nil {
		return scenario.Corpus{}, cassette.Set{}, c.Fail(capability.ExitOpFailed, "%v", err)
	}
	c.Info("loaded %d scenarios and %d cassettes", len(corpus.Scenarios), set.Len())
	return corpus, set, capability.ExitOK
}

func provenance(c *capability.Capability, tier figure.Tier) (figure.Provenance, int) {
	p := figure.Provenance{
		Tier:   tier,
		Commit: c.Param("commit", gitCommit()),
		Date:   c.Param("date", time.Now().UTC().Format("2006-01-02")),
		Runner: c.Param("runner", "unspecified"),
	}
	if p.Commit == "" {
		return p, c.Fail(capability.ExitPrerequisite,
			"no commit: pass --commit, or run inside a git checkout. Every figure carries the commit it was measured at")
	}
	return p, capability.ExitOK
}

// gitCommit reads the short SHA. An empty answer is a REFUSAL upstream rather
// than a blank stamp: a figure with no commit is one nobody can reproduce.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func openEngine(c *capability.Capability) (*memqlengine.MemQLEngine, func(), int) {
	dsn := c.Param("dsn", os.Getenv("MEMQL_DATABASE_DSN"))
	if dsn == "" {
		return nil, nil, c.Fail(capability.ExitPrerequisite,
			"no database: pass --dsn or set MEMQL_DATABASE_DSN. The proving suite runs against a real Postgres with "+
				"TimescaleDB on purpose -- a speed claim that excluded the database would be measuring a different product")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, c.Fail(capability.ExitPrerequisite, "the database at the configured DSN is unreachable: %v", err)
	}
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		_ = db.Close()
		return nil, nil, c.Fail(capability.ExitOpFailed, "loading concepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		_ = db.Close()
		return nil, nil, c.Fail(capability.ExitOpFailed, "opening the engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		_ = db.Close()
		return nil, nil, c.Fail(capability.ExitOpFailed, "initialising the engine: %v", err)
	}
	return eng, func() { _ = db.Close() }, capability.ExitOK
}

func publish(c *capability.Capability, s scorecard.Scorecard) ([]string, error) {
	dir := c.Param("scorecard-dir", defaultScorecard)
	page := c.Param("page", defaultPage)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	b, err := s.Marshal()
	if err != nil {
		return nil, err
	}
	jsonPath := filepath.Join(dir, s.Date+".json")
	if err := os.WriteFile(jsonPath, b, 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", jsonPath, err)
	}
	if err := os.WriteFile(page, []byte(scorecard.RenderPage(s)), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", page, err)
	}
	return []string{jsonPath, page}, nil
}

func countMeasured(s scorecard.Scorecard) int {
	n := 0
	for _, e := range s.Entries {
		if e.Figure.IsMeasured() {
			n++
		}
	}
	return n
}

// parseCeiling is the live tier's per-run dollar bound. It is parsed here, and
// refused rather than defaulted, so an unparseable value can never read as
// "no ceiling".
func parseCeiling(raw string) (float64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("the live tier needs an explicit --ceiling-usd; there is no default, because a defaulted ceiling is one nobody chose")
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("--ceiling-usd=%q is not a positive number of dollars", raw)
	}
	return v, nil
}
