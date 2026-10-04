// Command memql-verify says whether a deployed cluster is serving the release
// it claims, by asking the way any client would: through its public front door.
// It is the command behind a pipeline's verify-rollout step (epic memql#5480),
// and it replaces the checks a deployment observer used to make from inside
// the cluster -- checks a cluster can pass while its front door serves
// something else.
//
// What it probes, each from outside:
//
//   - /healthz of the api, identity and os hosts, sampled --samples times each
//     (one request reaches one replica, so a rollout that is half done answers
//     correctly to half of them): 200, status ok, and the release or commit
//     asked for;
//   - the OS shell and every script and stylesheet it names, each held to the
//     sha256 the edge image's own manifest (/memql-bundle.json) lists for it,
//     and to being a real file and not the shell served in its place;
//   - the docs site's version meta, for a release.
//
// A rollout takes minutes to land, so a failing round is retried every
// --interval until --wait is spent, and the verdict is the last round's. When
// it gives up it prints each finding as `<code> <check>: <detail>`, then
// `rollout_unverified: <n> of <m> checks failing ...`, as the last lines of
// output; the evidence JSON (--evidence) is written either way.
//
// Exit codes: 0 verified, 1 not verified, 2 bad flags. Zero is only ever
// "verified". Usage and a request for help are 2.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// A step that is cancelled signals the process, and a run that is cut
	// short still reports what it had found.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// execute is main without the process: arguments in, an exit code out, so the
// exit-2 contract is a thing a test can hold.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return exitUsage
	}
	return run(ctx, cfg, stdout, stderr)
}

// parseFlags reads the command line into a config. It says what is wrong with
// a flag on stderr itself and returns the error; whether the VALUES make sense
// together is config.resolve's to say, so there is one place that does.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("memql-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "memql-verify probes a deployed cluster through its public front door and says\n"+
			"whether it is serving the release it claims.\n\n"+
			"Usage: memql-verify --domain=<domain> --version=<release or commit> [flags]\n\n"+
			"Exit codes: 0 verified, 1 not verified (findings on stdout, evidence in --evidence), 2 bad flags.\n\n")
		fs.PrintDefaults()
	}

	fs.StringVar(&cfg.domain, "domain", "", "the cluster's front-door domain; probes go to https://api.<domain>, https://identity.<domain> and https://os.<domain>")
	fs.StringVar(&cfg.version, "version", "", "the release (v1.2.3) or the commit (at least 7 hex characters) the cluster must be serving")
	fs.StringVar(&cfg.docsURL, "docs", "", "URL of the docs page whose memql-docs-version meta must carry the release (a release is checked, a commit is not)")
	fs.DurationVar(&cfg.wait, "wait", 15*time.Minute, "how long to keep trying before the rollout is called unverified")
	fs.DurationVar(&cfg.interval, "interval", 30*time.Second, "the pause between attempts")
	fs.IntVar(&cfg.samples, "samples", 3, "GETs of each /healthz per attempt, so replicas the first one missed are met")
	fs.StringVar(&cfg.evidencePath, "evidence", "verify-rollout.json", "where to write the evidence JSON, on success and on failure")
	fs.DurationVar(&cfg.requestTimeout, "request-timeout", 20*time.Second, "the bound on one request, its body included")
	fs.StringVar(&cfg.apiURL, "api-url", "", "test-only: the api origin, instead of https://api.<domain>")
	fs.StringVar(&cfg.identityURL, "identity-url", "", "test-only: the identity origin, instead of https://identity.<domain>")
	fs.StringVar(&cfg.osURL, "os-url", "", "test-only: the os origin, instead of https://os.<domain>")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintf(stderr, "memql-verify: ERROR: %v\n", err)
		fs.Usage()
		return config{}, err
	}
	return cfg, nil
}
