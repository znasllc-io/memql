package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/errorpages"
)

// The error-pages subcommand serves component/errorpages; what is tested here
// is the part that is not the pages: that the binary routes to it, that it
// listens where it was told, that /healthz is answered on the probe listener and
// on no other, that it answers while it runs, and that it stops with the exit
// code a Deployment reads.

// A binary built without the routing case would treat `error-pages` as an
// ordinary start-up argument and boot a whole node, which is the failure to
// catch: the Deployment would restart it forever on a pod that never answers
// /healthz. Run under both build variants (`go test` and `go test -tags
// identity`), this proves each table has the case.
func TestErrorPagesSubcommandIsRouted(t *testing.T) {
	handled, code := dispatchSubcommand([]string{"error-pages", "--help"})
	if !handled {
		t.Fatal("`memql error-pages` is not routed; it would fall through to the server bootstrap")
	}
	if code != 0 {
		t.Fatalf("`error-pages --help` exited %d, want 0", code)
	}
}

func TestErrorPagesRejectsAnArgumentItDoesNotTake(t *testing.T) {
	handled, code := dispatchSubcommand([]string{"error-pages", "--port=9"})
	if !handled || code != 2 {
		t.Fatalf("`error-pages --port=9` handled=%v code=%d, want true/2", handled, code)
	}
}

func TestErrorPagesAddressesDefaultToTheDeploymentPorts(t *testing.T) {
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", "")
	t.Setenv("MEMQL_ERROR_PAGES_TLS_ADDR", "")
	if got := errorPagesTLSAddr(); got != ":8443" {
		t.Errorf("default TLS address = %q", got)
	}
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", "")
	if got := errorPagesAddr(); got != ":8080" {
		t.Errorf("default public address = %q, want :8080", got)
	}
	if got := errorPagesHealthAddr(); got != ":8081" {
		t.Errorf("default probe address = %q, want :8081", got)
	}
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", "  127.0.0.1:9191  ")
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", "  127.0.0.1:9192  ")
	if got := errorPagesAddr(); got != "127.0.0.1:9191" {
		t.Errorf("configured public address = %q, want 127.0.0.1:9191", got)
	}
	if got := errorPagesHealthAddr(); got != "127.0.0.1:9192" {
		t.Errorf("configured probe address = %q, want 127.0.0.1:9192", got)
	}
}

// busyAddr occupies a loopback port and returns its address.
func busyAddr(t *testing.T) string {
	t.Helper()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	t.Cleanup(func() { busy.Close() })
	return busy.Addr().String()
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// Nothing the subcommand does may need a database, a mesh or a secret, so it
// has to come up from an environment with none of them. MEMQL_DOMAIN is
// emptied so applySubcommandEnv cannot paint the identity values a domain
// derives into this process's environment, and the working directory is empty
// so no developer .env is layered over the test.
func hermeticErrorPagesEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("MEMQL_DOMAIN", "")
}

func TestErrorPagesListenFailureExitsOne(t *testing.T) {
	hermeticErrorPagesEnv(t)
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", busyAddr(t))
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", freeAddr(t))

	if code := runErrorPagesSubcommand(nil); code != 1 {
		t.Fatalf("listening on a public port already in use exited %d, want 1", code)
	}
}

// The probe listener is half of the contract: a pod whose probe address is taken
// has no readiness to report, so it must not run as if it had one.
func TestErrorPagesProbeListenFailureExitsOne(t *testing.T) {
	hermeticErrorPagesEnv(t)
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", freeAddr(t))
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", busyAddr(t))

	if code := runErrorPagesSubcommand(nil); code != 1 {
		t.Fatalf("listening on a probe port already in use exited %d, want 1", code)
	}
}

// One address for both is a configuration that cannot work, and the second
// bind says so.
func TestErrorPagesOneAddressForBothListenersExitsOne(t *testing.T) {
	hermeticErrorPagesEnv(t)
	addr := freeAddr(t)
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", addr)
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", addr)

	if code := runErrorPagesSubcommand(nil); code != 1 {
		t.Fatalf("one address for both listeners exited %d, want 1", code)
	}
}

func errorPagesGet(t *testing.T, method, url, host string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return resp, string(body)
}

// errorPagesServer is serveErrorPages running on two loopback listeners, the
// way the subcommand runs it.
type errorPagesServer struct {
	public, probe string // base URLs
	stop          context.CancelFunc
	done          chan int
}

func startErrorPages(t *testing.T, pages http.Handler, drain, shutdownTimeout time.Duration) *errorPagesServer {
	t.Helper()
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &errorPagesServer{
		public: "http://" + public.Addr().String(),
		probe:  "http://" + probe.Addr().String(),
		stop:   cancel,
		done:   make(chan int, 1),
	}
	go func() { srv.done <- serveErrorPages(ctx, []net.Listener{public}, probe, pages, drain, shutdownTimeout) }()
	t.Cleanup(cancel)
	return srv
}

func (s *errorPagesServer) waitFor(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case code := <-s.done:
		return code
	case <-time.After(timeout):
		t.Fatal("the server did not stop")
		return -1
	}
}

func TestErrorPagesServesWhileRunningAndStopsCleanly(t *testing.T) {
	srv := startErrorPages(t, errorpages.Handler("memql.example", "MemQL"), 0, time.Second)

	// The platform's host gets the branded page, as a 503 that says to retry.
	resp, body := errorPagesGet(t, http.MethodGet, srv.public+"/anything/at/all", "os.memql.example", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("platform host answered %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "120" {
		t.Errorf("Retry-After = %q, want 120", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(body, "MemQL is being updated.") {
		t.Errorf("platform host did not get the MemQL page:\n%s", body)
	}

	// The apex is the owner's main website: a deployable, so the neutral page.
	resp, body = errorPagesGet(t, http.MethodGet, srv.public+"/", "memql.example", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(strings.ToLower(body), "memql") {
		t.Errorf("the apex answered %d with a page that names the platform:\n%s", resp.StatusCode, body)
	}

	// Anyone else's host gets the neutral one.
	resp, body = errorPagesGet(t, http.MethodGet, srv.public+"/", "www.customer.com", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(strings.ToLower(body), "memql") {
		t.Errorf("a customer host answered %d with a page that names the platform:\n%s", resp.StatusCode, body)
	}

	// The probe the Deployment uses is answered on the probe listener...
	resp, body = errorPagesGet(t, http.MethodGet, srv.probe+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("/healthz on the probe listener answered %d %q, want 200 ok", resp.StatusCode, body)
	}

	srv.stop()
	if code := srv.waitFor(t, 10*time.Second); code != 0 {
		t.Errorf("a clean shutdown exited %d, want 0", code)
	}
	for name, base := range map[string]string{"public": srv.public, "probe": srv.probe} {
		if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second); err == nil {
			conn.Close()
			t.Errorf("the %s listener still accepts connections after shutdown", name)
		}
	}
}

// TestErrorPagesHealthzIsAnsweredOnlyOnTheProbeListener is the false-green
// guard. This server is the default backend of every front door, and the Service
// lists only the public port, so `https://api.<domain>/healthz` can reach the
// public listener whenever the real node cannot answer. A 200 there would make
// `curl`, an uptime monitor and the post-roll /healthz gate read the middle of an
// outage as healthy. So /healthz is a 503 page on the public listener, whatever
// the host or the headers, and 200 ok only on the probe listener, which the
// kubelet reaches at the pod's own address.
func TestErrorPagesHealthzIsAnsweredOnlyOnTheProbeListener(t *testing.T) {
	srv := startErrorPages(t, errorpages.Handler("memql.example", "MemQL"), 0, time.Second)

	forwarded := http.Header{"X-Forwarded-For": {"203.0.113.9"}, "X-Real-IP": {"203.0.113.9"}, "X-Request-ID": {"abc"}}
	for _, host := range []string{"", "10.42.1.7:8080", "api.memql.example", "os.memql.example", "www.customer.com"} {
		for name, header := range map[string]http.Header{"direct": nil, "forwarded": forwarded} {
			resp, body := errorPagesGet(t, http.MethodGet, srv.public+"/healthz", host, header)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("GET /healthz on the public listener (host %q, %s) answered %d, want 503", host, name, resp.StatusCode)
			}
			if body == "ok" || resp.Header.Get("Retry-After") != "120" {
				t.Errorf("GET /healthz on the public listener (host %q, %s): body %q, Retry-After %q", host, name, body, resp.Header.Get("Retry-After"))
			}
		}
	}
	if resp, _ := errorPagesGet(t, http.MethodHead, srv.public+"/healthz", "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("HEAD /healthz on the public listener answered %d, want 503", resp.StatusCode)
	}

	// On the probe listener the same path is the probe, however it is asked.
	resp, body := errorPagesGet(t, http.MethodGet, srv.probe+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz on the probe listener answered %d %q, want 200 ok", resp.StatusCode, body)
	}
	resp, body = errorPagesGet(t, http.MethodGet, srv.probe+"/healthz", "10.42.1.7:8081", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz on the probe listener with the pod address as its host answered %d %q, want 200 ok", resp.StatusCode, body)
	}
	resp, body = errorPagesGet(t, http.MethodHead, srv.probe+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /healthz on the probe listener answered %d %q, want 200 and no body", resp.StatusCode, body)
	}

	// Only the exact path, and only for a read, is the probe; the rest of the
	// probe listener is the same 503 page as anywhere.
	if resp, _ := errorPagesGet(t, http.MethodPost, srv.probe+"/healthz", "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST /healthz on the probe listener answered %d, want 503", resp.StatusCode)
	}
	for _, path := range []string{"/", "/healthz/", "/healthz/extra", "/Healthz", "/readyz"} {
		if resp, _ := errorPagesGet(t, http.MethodGet, srv.probe+path, "", nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s on the probe listener answered %d, want 503", path, resp.StatusCode)
		}
	}
}

// probeRequest is a request that arrived on localAddr, as net/http records it.
func probeRequest(method, target, localAddr string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if localAddr != "" {
		addr, err := net.ResolveTCPAddr("tcp", localAddr)
		if err != nil {
			panic(err)
		}
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(addr)))
	}
	return req
}

// The branch is on the port the connection arrived on, so it holds for a
// listener bound to every address, where the local address of a connection is
// whichever one the client dialled; and it fails closed, so a request with no
// local address, which only a test can produce, is never the probe.
func TestProbeListenerBranchIsOnTheLocalPortAndFailsClosed(t *testing.T) {
	pages := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	handler := withProbeListener(pages, &net.TCPAddr{IP: net.IPv4zero, Port: 8081})

	for _, tc := range []struct {
		name, method, local string
		want                int
	}{
		{"the probe port on the pod address", http.MethodGet, "10.42.1.5:8081", http.StatusOK},
		{"the probe port on loopback", http.MethodHead, "127.0.0.1:8081", http.StatusOK},
		{"the public port", http.MethodGet, "10.42.1.5:8080", http.StatusServiceUnavailable},
		{"some other port", http.MethodGet, "10.42.1.5:9000", http.StatusServiceUnavailable},
		{"no local address at all", http.MethodGet, "", http.StatusServiceUnavailable},
		{"a write on the probe port", http.MethodPost, "10.42.1.5:8081", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, probeRequest(tc.method, "/healthz", tc.local))
			if rec.Code != tc.want {
				t.Errorf("%s /healthz via %q answered %d, want %d", tc.method, tc.local, rec.Code, tc.want)
			}
		})
	}
}

// The branch is by port, so two listeners on one port would make /healthz a 200
// on the public side: the one answer this arrangement exists to prevent. The
// server refuses to start that way.
func TestErrorPagesRefusesTwoListenersOnOnePort(t *testing.T) {
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer public.Close()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer probe.Close()

	// A listener that reports the public listener's port as its own. The context
	// ends the run if the refusal were missing, so that the failure is an
	// assertion and not a hang.
	sharing := &portOverride{Listener: probe, addr: public.Addr()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	code := serveErrorPages(ctx, []net.Listener{public}, sharing, errorpages.Handler("memql.example", "MemQL"), 0, time.Second)
	if code != 1 {
		t.Fatalf("two listeners on one port exited %d, want 1", code)
	}
	if ctx.Err() != nil {
		t.Fatal("two listeners on one port were served until the context ended instead of being refused at once")
	}
}

// portOverride is a listener that reports a different address than it holds.
type portOverride struct {
	net.Listener
	addr net.Addr
}

func (p *portOverride) Addr() net.Addr { return p.addr }

// The drain is what keeps the last requests the ingress sends to a terminating
// pod from being refused: the server must still answer during it, on both
// listeners, and must not stop before it has elapsed.
func TestErrorPagesKeepsServingDuringTheDrain(t *testing.T) {
	const drain = time.Second
	srv := startErrorPages(t, errorpages.Handler("memql.example", "MemQL"), drain, time.Second)

	if resp, _ := errorPagesGet(t, http.MethodGet, srv.public+"/", "os.memql.example", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before the signal: %d, want 503", resp.StatusCode)
	}
	signalled := time.Now()
	srv.stop()
	time.Sleep(drain / 10)
	if resp, body := errorPagesGet(t, http.MethodGet, srv.public+"/", "os.memql.example", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("during the drain: %d %q, want 503 still being served", resp.StatusCode, body)
	}
	// The probe keeps answering while the pod drains: a pod that is leaving is
	// alive until it has left, and flipping the probe is the Service's business.
	if resp, body := errorPagesGet(t, http.MethodGet, srv.probe+"/healthz", "", nil); resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("the probe during the drain: %d %q, want 200 ok", resp.StatusCode, body)
	}
	if code := srv.waitFor(t, 10*time.Second); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	if elapsed := time.Since(signalled); elapsed < drain {
		t.Errorf("stopped %s after the signal, before the %s drain had elapsed", elapsed, drain)
	}
}

// A request that will not finish is the one thing that can keep a stop from
// being clean. The exit code has to say so, because the contract is that 0
// means every in-flight response was written. The handler holds the request open
// on purpose, so it is in flight for certain when the stop begins.
func TestErrorPagesExitsOneWhenARequestWillNotFinish(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	stuck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})
	srv := startErrorPages(t, stuck, 0, 50*time.Millisecond)

	go func() {
		// Its outcome is not the point; the connection being open is.
		if resp, err := http.Get(srv.public + "/"); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	srv.stop()
	if code := srv.waitFor(t, 10*time.Second); code != 1 {
		t.Errorf("a stop that left a request unfinished exited %d, want 1", code)
	}
}

func TestErrorPagesTLSAndHTTPKeepPublicHealthUnavailable(t *testing.T) {
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secure, err := listenErrorPagesTLS("127.0.0.1:0")
	if err != nil {
		public.Close()
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		public.Close()
		secure.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- serveErrorPages(ctx, []net.Listener{public, secure}, probe, errorpages.Handler("example.test", "Acme"), 0, time.Second)
	}()
	// Match identity's existing upstream proxy: encryption without CA verification.
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // internal fallback certificate
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, tc := range []struct {
		url    string
		status int
	}{
		{"http://" + public.Addr().String() + "/healthz", 503},
		{"https://" + secure.Addr().String() + "/healthz", 503},
		{"https://" + secure.Addr().String() + "/login", 503},
		{"http://" + probe.Addr().String() + "/healthz", 200},
	} {
		req, _ := http.NewRequest(http.MethodGet, tc.url, nil)
		req.Host = "identity.example.test"
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s: status=%d want %d", tc.url, response.StatusCode, tc.status)
		}
		if tc.status == 503 && (!strings.Contains(string(body), "Acme is temporarily unavailable.") || strings.Contains(string(body), "MemQL")) {
			t.Errorf("wrong outage page: %s", body)
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("shutdown=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listeners did not stop")
	}
	for _, ln := range []net.Listener{public, secure, probe} {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("listener %s survived shutdown", ln.Addr())
		}
	}
}

func TestErrorPagesTLSListenFailureClosesOtherListeners(t *testing.T) {
	hermeticErrorPagesEnv(t)
	public, probe := freeAddr(t), freeAddr(t)
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", public)
	t.Setenv("MEMQL_ERROR_PAGES_HEALTH_ADDR", probe)
	t.Setenv("MEMQL_ERROR_PAGES_TLS_ADDR", busyAddr(t))
	if code := runErrorPagesSubcommand(nil); code != 1 {
		t.Fatalf("TLS bind failure exited %d", code)
	}
	for _, address := range []string{public, probe} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Errorf("failed start leaked listener %s: %v", address, err)
		} else {
			listener.Close()
		}
	}
}
