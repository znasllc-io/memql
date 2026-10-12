package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/errorpages"
)

const (
	// errorPagesDefaultAddr is where the Deployment's probes and Service look.
	errorPagesDefaultAddr = ":8080"
	// errorPagesShutdownTimeout bounds how long in-flight responses get to
	// finish once the drain is over. A response is a few kilobytes.
	errorPagesShutdownTimeout = 10 * time.Second
)

func printErrorPagesUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: memql error-pages

Serves the 503 maintenance pages a front door falls back to when the host it
was asked for has nothing ready behind it: a MemQL page for the platform's own
hosts, a neutral page for every other host, and JSON for an API client. Reads
no database and no secret.

Environment:
  MEMQL_ERROR_PAGES_ADDR  address to listen on (default %s)
  MEMQL_DOMAIN            the cluster's domain; decides which hosts are the platform's

Endpoints:
  GET /healthz            200 ok, for the kubelet's readiness and liveness probes
  anything else           503 with Retry-After: 120

Exit codes:
  0  stopped cleanly after SIGTERM or SIGINT
  1  could not listen, the server failed, or it did not stop in time
  2  bad usage
`, errorPagesDefaultAddr)
}

// error-pages serves the maintenance pages every front-door Ingress names as
// its default backend (component/errorpages): a MemQL page for the platform's
// own hosts, a neutral one for everyone else's, JSON for an API client, all of
// them a 503 with Retry-After. It is what a visitor sees when the host they
// asked for has nothing ready behind it, in place of ingress-nginx's bare
// "503 Service Temporarily Unavailable" every host answered on 2026-10-11, when
// the whole mesh was down.
//
// It lives on the engine binary rather than in an image of its own for the
// reason dsl-fetch does: every deployed image already carries /app/memql, so
// the Deployment names an image the cluster has and the pages ship with the
// release they belong to.
//
// IT NEEDS NOTHING BUT A DOMAIN. No database, no mesh, no secret, no object
// storage: the one thing that must still answer when all of those are gone
// cannot depend on any of them. It is also why this does not go through the
// server bootstrap, which would start the dependency chain it exists to
// outlive.
//
// EXIT CODES ARE THE CONTRACT, because a Deployment restarts on them:
//
//	0  stopped cleanly after SIGTERM or SIGINT
//	1  could not listen, or the server failed, or did not stop in time
//	2  an argument this subcommand does not take
//
// A MISSING DOMAIN IS A WARNING, NOT A REFUSAL, and the asymmetry with
// dsl-fetch is deliberate. A fetcher that cannot say where to look has failed
// to look at all, so it refuses. This server without a domain still answers
// every request; it can only treat every host as somebody's site, which is the
// direction to be wrong in (a platform page missing its brand, not a customer's
// page wearing ours). Exiting instead would take the default backend away and
// give the visitor ingress-nginx's bare 503 again, the thing this replaces.
func runErrorPagesSubcommand(args []string) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			printErrorPagesUsage(os.Stderr)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "error-pages: unexpected argument %q\n", a)
			printErrorPagesUsage(os.Stderr)
			return 2
		}
	}

	// MEMQL_DOMAIN reaches this pod the way it reaches every node, from the
	// memql-domain ConfigMap, and a developer's repo-root .env may carry it.
	// Subcommands run before server bootstrap, so apply the layers it applies.
	if err := applySubcommandEnv("error-pages"); err != nil {
		return 1
	}

	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	switch {
	case domain == "":
		fmt.Fprintln(os.Stderr, "error-pages: MEMQL_DOMAIN is not set; every host will get the neutral page")
	case len(envregistry.DomainDerivations(domain)) == 0:
		fmt.Fprintf(os.Stderr, "error-pages: MEMQL_DOMAIN %q is not a domain the cluster derives its hosts from; the platform's own hosts may get the neutral page\n", domain)
	}

	// Built before the listener opens: a page that cannot render fails the
	// start, not the first visitor.
	handler := errorpages.Handler(domain)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", errorPagesAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error-pages: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "error-pages: serving on %s\n", ln.Addr())

	done := make(chan int, 1)
	go func() {
		done <- serveErrorPages(ctx, ln, handler, app.DefaultShutdownDrainDelay, errorPagesShutdownTimeout)
	}()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		// The first signal starts the shutdown. Handing the signals back to the
		// runtime means a second one, from an operator who will not wait, ends
		// the process at once.
		stop()
		return <-done
	}
}

// errorPagesAddr is where to listen: MEMQL_ERROR_PAGES_ADDR, else :8080.
func errorPagesAddr() string {
	if addr := strings.TrimSpace(os.Getenv("MEMQL_ERROR_PAGES_ADDR")); addr != "" {
		return addr
	}
	return errorPagesDefaultAddr
}

// serveErrorPages serves handler on ln until ctx is done, then stops. It
// returns the exit code.
//
// A stop is a drain, then a shutdown. The drain keeps serving for the same
// endpoint-removal window the engine's nodes keep
// (app.DefaultShutdownDrainDelay): a pod that is being deleted leaves the
// Service's endpoints, and ingress-nginx learns that a moment later, so a
// listener closed at the signal would refuse the requests still being sent to
// it. Those would get ingress-nginx's own 502, on the one page that exists to
// be better than that.
//
// Logging is startup and shutdown only. A line per request is exactly the
// volume this release is removing.
func serveErrorPages(ctx context.Context, ln net.Listener, handler http.Handler, drain, shutdownTimeout time.Duration) int {
	srv := &http.Server{
		Handler: handler,
		// The response is a few kilobytes, so the read and write limits are far
		// from a constraint on an honest client; they exist so a stalled one
		// cannot hold a connection. The header size is left at the default on
		// purpose: a customer's site can legitimately send a very large cookie,
		// and a 431 from here would be the one request the page failed to answer.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		// Longer than ingress-nginx's 60 s upstream keepalive, so the ingress is
		// always the side that closes an idle connection. Shorter, a request
		// sent down a connection this server has just closed is a 502.
		IdleTimeout: 2 * time.Minute,
	}

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	select {
	case err := <-failed:
		// Serve returns only on failure here: Shutdown has not been called.
		fmt.Fprintf(os.Stderr, "error-pages: server stopped: %v\n", err)
		return 1
	case <-ctx.Done():
	}

	fmt.Fprintf(os.Stderr, "error-pages: stopping; serving for %s more so the endpoint removal can reach the ingress\n", drain)
	select {
	case <-time.After(drain):
	case err := <-failed:
		fmt.Fprintf(os.Stderr, "error-pages: server stopped while draining: %v\n", err)
		return 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		fmt.Fprintf(os.Stderr, "error-pages: did not stop cleanly: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "error-pages: stopped")
	return 0
}
