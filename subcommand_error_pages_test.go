package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/errorpages"
)

// The error-pages subcommand serves component/errorpages; what is tested here
// is the part that is not the pages: that the binary routes to it, that it
// listens where it was told, that it answers while it runs, and that it stops
// with the exit code a Deployment reads.

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

func TestErrorPagesAddressDefaultsToTheDeploymentPort(t *testing.T) {
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", "")
	if got := errorPagesAddr(); got != ":8080" {
		t.Errorf("default address = %q, want :8080", got)
	}
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", "  127.0.0.1:9191  ")
	if got := errorPagesAddr(); got != "127.0.0.1:9191" {
		t.Errorf("configured address = %q, want 127.0.0.1:9191", got)
	}
}

// Nothing the subcommand does may need a database, a mesh or a secret, so it
// has to come up from an environment with none of them. MEMQL_DOMAIN is
// emptied so applySubcommandEnv cannot paint the identity values a domain
// derives into this process's environment, and the working directory is empty
// so no developer .env is layered over the test.
func TestErrorPagesListenFailureExitsOne(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MEMQL_DOMAIN", "")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer busy.Close()
	t.Setenv("MEMQL_ERROR_PAGES_ADDR", busy.Addr().String())

	if code := runErrorPagesSubcommand(nil); code != 1 {
		t.Fatalf("listening on a port already in use exited %d, want 1", code)
	}
}

func errorPagesGet(t *testing.T, url, host string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return resp, string(body)
}

func TestErrorPagesServesWhileRunningAndStopsCleanly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- serveErrorPages(ctx, ln, errorpages.Handler("memql.example"), 0, time.Second) }()
	base := "http://" + ln.Addr().String()

	// The platform's host gets the branded page, as a 503 that says to retry.
	resp, body := errorPagesGet(t, base+"/anything/at/all", "os.memql.example", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("platform host answered %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "120" {
		t.Errorf("Retry-After = %q, want 120", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(body, "MemQL is being updated.") {
		t.Errorf("platform host did not get the MemQL page:\n%s", body)
	}

	// Anyone else's host gets the neutral one.
	resp, body = errorPagesGet(t, base+"/", "www.customer.com", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(strings.ToLower(body), "memql") {
		t.Errorf("a customer host answered %d with a page that names the platform:\n%s", resp.StatusCode, body)
	}

	// The probe the Deployment uses.
	resp, body = errorPagesGet(t, base+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("/healthz answered %d %q, want 200 ok", resp.StatusCode, body)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("a clean shutdown exited %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop after its context was cancelled")
	}
	if conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Error("the listener still accepts connections after shutdown")
	}
}

// The drain is what keeps the last requests the ingress sends to a terminating
// pod from being refused: the server must still answer during it, and must not
// stop before it has elapsed.
func TestErrorPagesKeepsServingDuringTheDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	const drain = time.Second
	go func() { done <- serveErrorPages(ctx, ln, errorpages.Handler("memql.example"), drain, time.Second) }()
	base := "http://" + ln.Addr().String()

	if resp, _ := errorPagesGet(t, base+"/", "os.memql.example", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before the signal: %d, want 503", resp.StatusCode)
	}
	signalled := time.Now()
	cancel()
	time.Sleep(drain / 10)
	if resp, body := errorPagesGet(t, base+"/", "os.memql.example", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("during the drain: %d %q, want 503 still being served", resp.StatusCode, body)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
		if elapsed := time.Since(signalled); elapsed < drain {
			t.Errorf("stopped %s after the signal, before the %s drain had elapsed", elapsed, drain)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
}

// A request that will not finish is the one thing that can keep a stop from
// being clean. The exit code has to say so, because the contract is that 0
// means every in-flight response was written. The handler holds the request
// open on purpose, so it is in flight for certain when the stop begins.
func TestErrorPagesExitsOneWhenARequestWillNotFinish(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	stuck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serveErrorPages(ctx, ln, stuck, 0, 50*time.Millisecond) }()

	go func() {
		// Its outcome is not the point; the connection being open is.
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	cancel()
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("a stop that left a request unfinished exited %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not give up on the unfinished request")
	}
}
