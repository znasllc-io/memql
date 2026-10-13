package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
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
	// errorPagesDefaultAddr is the public listener: the one the Service lists,
	// so the one every front door's default backend reaches.
	errorPagesDefaultAddr    = ":8080"
	errorPagesDefaultTLSAddr = ":8443"
	// errorPagesDefaultHealthAddr is the probe listener: the one the kubelet
	// reaches at the pod's own address and the Service does not list.
	errorPagesDefaultHealthAddr = ":8081"
	// errorPagesShutdownTimeout bounds how long in-flight responses get to
	// finish once the drain is over. A response is a few kilobytes.
	errorPagesShutdownTimeout = 10 * time.Second
)

func printErrorPagesUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: memql error-pages

Serves the 503 maintenance pages a front door falls back to when the host it
was asked for has nothing ready behind it: the configured installation name on
its own hosts, a neutral page for every other host (the bare domain included), and JSON
for an API client. Reads no database and no secret.

Environment:
  MEMQL_ERROR_PAGES_ADDR         public address to listen on (default %s)
  MEMQL_ERROR_PAGES_TLS_ADDR     HTTPS address to listen on (default :8443)
  MEMQL_IDENTITY_BRAND_NAME      optional installation name; blank uses the neutral page
  MEMQL_ERROR_PAGES_HEALTH_ADDR  probe address to listen on (default %s)
  MEMQL_DOMAIN                   the cluster's domain; decides which hosts are the platform's

Endpoints:
  public address, any path       503 with Retry-After: 120, /healthz included
  probe address, GET /healthz    200 ok, for the kubelet's readiness and liveness probes

The Service lists the public port only. The probe listener answers nothing a
visitor can reach, which is what keeps /healthz from reporting an outage as healthy.

Exit codes:
  0  stopped cleanly after SIGTERM or SIGINT
  1  could not listen, the server failed, or it did not stop in time
  2  bad usage
`, errorPagesDefaultAddr, errorPagesDefaultHealthAddr)
}

// error-pages serves the maintenance pages every front-door Ingress names as
// its default backend (component/errorpages): an installation page for its
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
// IT NEEDS ONLY A DOMAIN AND AN OPTIONAL DISPLAY NAME. No database, no mesh, no secret, no object
// storage: the one thing that must still answer when all of those are gone
// cannot depend on any of them. It is also why this does not go through the
// server bootstrap, which would start the dependency chain it exists to
// outlive.
//
// THREE LISTENERS, ONE SERVER. HTTP and HTTPS are public; The public listener (MEMQL_ERROR_PAGES_ADDR) is
// listed by the HTTP Service; the TLS listener has its own Service. Both and it answers every path with the 503 page, /healthz
// included. The probe listener (MEMQL_ERROR_PAGES_HEALTH_ADDR) is the one the
// Service does not list, reached only by the kubelet at the pod's own address,
// and it answers GET /healthz with 200 ok. That split is the point. This server
// is the default backend of every front door, so a request for
// `https://api.<domain>/healthz` that finds the real node down lands here, and
// if /healthz were answered 200 on the same port, the status-code loop in
// docs/public/operate/instance-scale-and-upgrade.md, the post-roll /healthz gate
// and any uptime monitor would all read the middle of an outage as healthy. It
// is one http.Server serving all listeners, with the branch on the port a
// connection arrived on (http.LocalAddrContextKey), so the probe is answered by
// the same server, handler and accept machinery that answers the pages and still
// proves this process is alive; a second server would prove only itself.
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

	// Built before the listeners open: a page that cannot render fails the
	// start, not the first visitor.
	handler := errorpages.Handler(domain, os.Getenv("MEMQL_IDENTITY_BRAND_NAME"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	public, err := net.Listen("tcp", errorPagesAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error-pages: %v\n", err)
		return 1
	}
	probe, err := net.Listen("tcp", errorPagesHealthAddr())
	if err != nil {
		public.Close()
		fmt.Fprintf(os.Stderr, "error-pages: probe listener: %v\n", err)
		return 1
	}
	secure, err := listenErrorPagesTLS(errorPagesTLSAddr())
	if err != nil {
		public.Close()
		probe.Close()
		fmt.Fprintf(os.Stderr, "error-pages: TLS listener: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "error-pages: serving on %s and TLS %s, probe on %s\n", public.Addr(), secure.Addr(), probe.Addr())

	done := make(chan int, 1)
	go func() {
		done <- serveErrorPages(ctx, []net.Listener{public, secure}, probe, handler, app.DefaultShutdownDrainDelay, errorPagesShutdownTimeout)
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

// errorPagesAddr is where the public listener binds: MEMQL_ERROR_PAGES_ADDR,
// else :8080.
func errorPagesAddr() string {
	if addr := strings.TrimSpace(os.Getenv("MEMQL_ERROR_PAGES_ADDR")); addr != "" {
		return addr
	}
	return errorPagesDefaultAddr
}

// errorPagesHealthAddr is where the probe listener binds:
// MEMQL_ERROR_PAGES_HEALTH_ADDR, else :8081.
func errorPagesHealthAddr() string {
	if addr := strings.TrimSpace(os.Getenv("MEMQL_ERROR_PAGES_HEALTH_ADDR")); addr != "" {
		return addr
	}
	return errorPagesDefaultHealthAddr
}

// withProbeListener answers GET and HEAD /healthz with 200 "ok" when the
// connection arrived on the probe listener, and gives every other request, the
// same path on the public listener included, to pages.
//
// The branch is on the local PORT of the connection (http.LocalAddrContextKey),
// not its address: a listener bound to every address sees each connection's
// local address as whichever one the client dialled, and only the port is the
// listener's. It fails closed: a request with no local address, which net/http
// never produces and only a test can, is never the probe.
func withProbeListener(pages http.Handler, probe net.Addr) http.Handler {
	probePort := portOf(probe)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && arrivedOnPort(r, probePort) {
			hdr := w.Header()
			hdr.Set("Content-Type", "text/plain; charset=utf-8")
			hdr.Set("Content-Length", "2")
			hdr.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("ok"))
			}
			return
		}
		pages.ServeHTTP(w, r)
	})
}

// portOf is the port of a listener or connection address, empty when it has none.
func portOf(addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return port
}

func arrivedOnPort(r *http.Request, port string) bool {
	if port == "" {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && portOf(local) == port
}

// serveErrorPages serves pages on public, and answers the probe on probe, until
// ctx is done, then stops. It returns the exit code and owns every listener.
//
// A stop is a drain, then a shutdown. The drain keeps serving, on both
// listeners, for the same endpoint-removal window the engine's nodes keep
// (app.DefaultShutdownDrainDelay): a pod that is being deleted leaves the
// Service's endpoints, and ingress-nginx learns that a moment later, so a
// listener closed at the signal would refuse the requests still being sent to
// it. Those would get ingress-nginx's own 502, on the one page that exists to be
// better than that. The probe keeps answering through it: a pod that is leaving
// is alive until it has left.
//
// Public listeners must use different ports from the probe, because the branch between
// them is by port. Two on one would make /healthz a 200 on the public side,
// which is the answer this arrangement exists to prevent, so it is refused.
//
// Logging is startup and shutdown only. A line per request is exactly the
// volume this release is removing.
func serveErrorPages(ctx context.Context, public []net.Listener, probe net.Listener, pages http.Handler, drain, shutdownTimeout time.Duration) int {
	for _, listener := range public {
		if portOf(listener.Addr()) == portOf(probe.Addr()) {
			fmt.Fprintln(os.Stderr, "error-pages: public and probe listeners must use different ports")
			for _, ln := range public {
				ln.Close()
			}
			probe.Close()
			return 1
		}
	}

	srv := &http.Server{
		Handler: withProbeListener(pages, probe.Addr()),
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

	failed := make(chan error, len(public)+1)
	for _, listener := range public {
		go func() { failed <- srv.Serve(listener) }()
	}
	go func() { failed <- srv.Serve(probe) }()

	select {
	case err := <-failed:
		// Serve returns only on failure here: Shutdown has not been called.
		_ = srv.Close()
		fmt.Fprintf(os.Stderr, "error-pages: server stopped: %v\n", err)
		return 1
	case <-ctx.Done():
	}

	fmt.Fprintf(os.Stderr, "error-pages: stopping; serving for %s more so the endpoint removal can reach the ingress\n", drain)
	select {
	case <-time.After(drain):
	case err := <-failed:
		_ = srv.Close()
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

// errorPagesTLSAddr is the HTTPS fallback's address. Its separate Service puts
// this port first: ingress-nginx retains the identity location's HTTPS protocol.
func errorPagesTLSAddr() string {
	if addr := strings.TrimSpace(os.Getenv("MEMQL_ERROR_PAGES_TLS_ADDR")); addr != "" {
		return addr
	}
	return errorPagesDefaultTLSAddr
}

// The existing identity ingress disables upstream certificate verification.
// This ephemeral certificate encrypts that internal hop without depending on
// secrets, disk, the mesh, or the database that may be unavailable.
func listenErrorPagesTLS(addr string) (net.Listener, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: serial, DNSNames: []string{"error-pages"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, pub, key)
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
}
