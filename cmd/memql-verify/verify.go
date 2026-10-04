package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// verify.go -- the command's configuration, its retry loop and its report.
//
// The loop is the whole behavior: probe everything; if everything passes, say
// so and stop; otherwise wait the interval and probe everything again, until
// the wait is spent. A rollout takes minutes to land and the command is run at
// the end of a release, so "not yet" is the ordinary answer for a while and the
// verdict is the LAST round's. What it prints when it gives up is what the
// pipeline's run page shows inline, so the findings and the summary are the last
// lines of its output and nothing is written after them.

// The exit codes. Zero is reserved for "verified": a command that gates a
// release must never exit 0 having verified nothing, which is why a request for
// help exits 2 as well.
const (
	exitVerified   = 0
	exitUnverified = 1
	exitUsage      = 2
)

// config is everything a run needs. main fills it from the flags; a test fills
// it directly.
type config struct {
	domain         string
	version        string
	docsURL        string
	wait           time.Duration
	interval       time.Duration
	samples        int
	evidencePath   string
	requestTimeout time.Duration

	// The origins of the three hosts probed. Each defaults to
	// https://<role>.<domain>; the overrides are for tests, and for a front door
	// that is not laid out that way.
	apiURL      string
	identityURL string
	osURL       string

	// Seams for the tests: the clock and the sleep between attempts. A nil one
	// is the real thing.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// resolve checks the configuration and fills in what it derives: the three
// origins from the domain, and what --version asks for. Every refusal is a
// usage error, found before anything is probed.
func (c config) resolve() (config, expectation, error) {
	want, err := parseExpectation(c.version)
	if err != nil {
		return c, want, err
	}
	c.version = want.raw

	switch {
	case c.samples < 1:
		return c, want, errors.New("--samples must be at least 1")
	case c.wait < 0:
		return c, want, errors.New("--wait must not be negative")
	case c.interval <= 0:
		return c, want, errors.New("--interval must be positive")
	case c.requestTimeout <= 0:
		return c, want, errors.New("--request-timeout must be positive")
	case c.evidencePath == "":
		return c, want, errors.New("--evidence must name a file")
	case c.domain != "" && strings.ContainsAny(c.domain, "/ \t\r\n"):
		return c, want, fmt.Errorf("--domain %q must be a bare host name such as example.com, not a URL", c.domain)
	}

	for _, origin := range []struct {
		flag, role string
		target     *string
	}{
		{"--api-url", "api", &c.apiURL},
		{"--identity-url", "identity", &c.identityURL},
		{"--os-url", "os", &c.osURL},
	} {
		switch {
		case *origin.target != "":
			if err := checkURL(origin.flag, *origin.target); err != nil {
				return c, want, err
			}
			// Every probe appends its own path, so an origin has no trailing slash.
			*origin.target = strings.TrimRight(*origin.target, "/")
		case c.domain != "":
			*origin.target = "https://" + origin.role + "." + c.domain
		default:
			return c, want, errors.New("--domain is required unless --api-url, --identity-url and --os-url are all given")
		}
	}
	// The docs URL is used exactly as given: its trailing slash is part of the
	// page's address, and the docs host redirects the address without it.
	if c.docsURL != "" {
		if err := checkURL("--docs", c.docsURL); err != nil {
			return c, want, err
		}
	}
	return c, want, nil
}

// checkURL accepts an http or https URL that names a host.
func checkURL(flag, v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q is not an http or https URL", flag, v)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// finding is one thing a probe found wrong. Expected and Observed are set when
// there is a thing to compare, and Detail is the sentence an operator reads.
type finding struct {
	Code     string `json:"code"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	Detail   string `json:"detail"`
}

// sample is one /healthz answer: which node gave it and what it said.
type sample struct {
	NodeID  string `json:"nodeId"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Status  string `json:"status"`
}

// checkResult is one probe's answer. A check that was not made says so
// (Skipped, and why) and is NOT Passed: a verdict that does not report what it
// did not look at is a statement about the checker, not about the cluster.
type checkResult struct {
	Check    string    `json:"check"`
	URL      string    `json:"url"`
	Passed   bool      `json:"passed"`
	Skipped  bool      `json:"skipped,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Findings []finding `json:"findings"`
	Samples  []sample  `json:"samples,omitempty"`
}

// newCheck starts a result with an empty findings list, so a check that found
// nothing is an empty array in the evidence and not a null.
func newCheck(name, target string) checkResult {
	return checkResult{Check: name, URL: target, Findings: []finding{}}
}

// add records a finding, once: three samples that reach the same bad node say
// the same thing three times, and the evidence says it once.
func (c *checkResult) add(f finding) {
	for _, have := range c.Findings {
		if have == f {
			return
		}
	}
	c.Findings = append(c.Findings, f)
}

func (c *checkResult) skip(reason string) checkResult {
	c.Skipped, c.Reason = true, reason
	return *c
}

func (c *checkResult) finish() checkResult {
	c.Passed = !c.Skipped && len(c.Findings) == 0
	return *c
}

// evidence is the file the run leaves behind, and the artifact the pipeline
// keeps: what was asked, what was found, and when.
type evidence struct {
	Version    string        `json:"version"`
	Verified   bool          `json:"verified"`
	Attempts   int           `json:"attempts"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Checks     []checkResult `json:"checks"`
}

func (e *evidence) write(path string) error {
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode the evidence: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("cannot write the evidence file: %w", err)
	}
	return nil
}

// counts is how many checks ran and how many of them found something. A
// skipped check is neither.
func (e *evidence) counts() (failing, total int) {
	for _, c := range e.Checks {
		if c.Skipped {
			continue
		}
		total++
		if len(c.Findings) > 0 {
			failing++
		}
	}
	return failing, total
}

func (e *evidence) duration() time.Duration {
	return e.FinishedAt.Sub(e.StartedAt).Round(time.Second)
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// run probes the cluster until it verifies or the wait is spent, and returns
// the exit code: 0 verified, 1 not verified, 2 a configuration it refuses.
func run(ctx context.Context, cfg config, stdout, stderr io.Writer) int {
	cfg, want, err := cfg.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "memql-verify: ERROR: %v\n", err)
		return exitUsage
	}
	if cfg.now == nil {
		cfg.now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.sleep == nil {
		cfg.sleep = sleepContext
	}
	probe := newProber(cfg, want)

	started := cfg.now()
	ev := &evidence{Version: cfg.version, StartedAt: started, FinishedAt: started, Checks: []checkResult{}}
	// A path that cannot be written is found now. Finding it after a wait of up
	// to hours would spend the whole wait on a verdict with nowhere to go.
	if err := ev.write(cfg.evidencePath); err != nil {
		fmt.Fprintf(stderr, "memql-verify: ERROR: %v\n", err)
		return exitUsage
	}

	deadline := started.Add(cfg.wait)
	interrupted := false
	for {
		round := probe.probeAll(ctx, cfg)
		if ctx.Err() != nil {
			// The round was cut short, so what it found says nothing about the
			// cluster. The evidence keeps the last round that finished.
			interrupted = true
			break
		}
		now := cfg.now()
		ev.Attempts++
		ev.Checks, ev.FinishedAt = round, now
		failing, total := ev.counts()
		ev.Verified = failing == 0
		if ev.Attempts == 1 {
			logSkipped(stderr, round)
		}
		// Written after every attempt, so a run that is killed leaves the last
		// round behind and not the empty file it began with.
		if err := ev.write(cfg.evidencePath); err != nil {
			fmt.Fprintf(stderr, "memql-verify: WARNING: %v\n", err)
		}
		if ev.Verified || !now.Before(deadline) {
			break
		}
		// The last attempt is AT the deadline, not past it.
		pause := min(cfg.interval, deadline.Sub(now))
		fmt.Fprintf(stderr, "INFO: attempt %d: %d of %d checks failing (%s); trying again in %s\n",
			ev.Attempts, failing, total, failureSummary(round), pause)
		if err := cfg.sleep(ctx, pause); err != nil {
			interrupted = true
			break
		}
	}
	ev.FinishedAt = cfg.now()

	return conclude(ev, interrupted, cfg.evidencePath, stdout)
}

// conclude prints the verdict and returns the exit code. The findings come
// first and the summary last, and an exit of 1 always ends on a line that
// begins with the summary code.
//
// The evidence is written between the two, and failing to write it is itself
// the verdict: a rollout whose evidence could not be kept is not one the
// pipeline can show to anybody, so it is not called verified.
func conclude(ev *evidence, interrupted bool, evidencePath string, stdout io.Writer) int {
	for _, c := range ev.Checks {
		for _, f := range c.Findings {
			fmt.Fprintf(stdout, "%s %s: %s\n", f.Code, c.Check, f.Detail)
		}
	}
	if err := ev.write(evidencePath); err != nil {
		fmt.Fprintf(stdout, "%s: %v\n", codeUnverified, err)
		return exitUnverified
	}
	failing, total := ev.counts()
	if ev.Verified {
		fmt.Fprintf(stdout, "verified: %d of %d checks passing after %d attempts over %s\n", total, total, ev.Attempts, ev.duration())
		return exitVerified
	}
	suffix := ""
	if interrupted {
		suffix = " (interrupted)"
	}
	fmt.Fprintf(stdout, "%s: %d of %d checks failing after %d attempts over %s%s\n", codeUnverified, failing, total, ev.Attempts, ev.duration(), suffix)
	return exitUnverified
}

// probeAll asks everything, in the order the checks are reported.
func (p *prober) probeAll(ctx context.Context, cfg config) []checkResult {
	return []checkResult{
		p.probeHealth(ctx, checkAPI, cfg.apiURL),
		p.probeHealth(ctx, checkIdentity, cfg.identityURL),
		p.probeHealth(ctx, checkOSHealth, cfg.osURL),
		p.probeOS(ctx, cfg.osURL),
		p.probeDocs(ctx, cfg.docsURL),
	}
}

// logSkipped says which checks were not made, once, at the start: what a run
// did not look at is part of what it reports.
func logSkipped(w io.Writer, round []checkResult) {
	for _, c := range round {
		if c.Skipped {
			fmt.Fprintf(w, "INFO: %s skipped: %s\n", c.Check, c.Reason)
		}
	}
}

// failureSummary is one line of what is failing, for the attempts in between:
// each failing check and the codes it found.
func failureSummary(round []checkResult) string {
	var parts []string
	for _, c := range round {
		if len(c.Findings) == 0 {
			continue
		}
		codes := make([]string, 0, len(c.Findings))
		for _, f := range c.Findings {
			codes = append(codes, f.Code)
		}
		parts = append(parts, c.Check+": "+strings.Join(codes, ", "))
	}
	return strings.Join(parts, "; ")
}

// sleepContext waits d, or until the run is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
