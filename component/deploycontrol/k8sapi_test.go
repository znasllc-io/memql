package deploycontrol

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The read call sites, verbatim from repair.go and service.go. If one of those
// changes shape, this table is where the in-cluster substrate finds out --
// parseKubectlGet is strict on purpose, so an unrecognised shape is an error
// rather than a quietly different request.
func TestParseKubectlGetCoversEveryReadCallSite(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantNS   string
		wantRes  string
		wantName string
		wantPath string
	}{
		{
			name:     "argo application, by name (repair.go + service.go)",
			args:     []string{"-n", "argocd", "get", "app", "memql", "-o", "json"},
			wantNS:   "argocd",
			wantRes:  "app",
			wantName: "memql",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql",
		},
		{
			name:     "rollouts collection (service.go)",
			args:     []string{"-n", "memql", "get", "rollout", "-o", "json"},
			wantNS:   "memql",
			wantRes:  "rollout",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/memql/rollouts",
		},
		{
			name:     "analysisruns collection (service.go)",
			args:     []string{"-n", "memql", "get", "analysisrun", "-o", "json"},
			wantNS:   "memql",
			wantRes:  "analysisrun",
			wantPath: "apis/argoproj.io/v1alpha1/namespaces/memql/analysisruns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, verb, res, name, err := parseKubectlGet(tc.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if verb != "get" {
				t.Errorf("verb = %q, want get", verb)
			}
			if ns != tc.wantNS || res != tc.wantRes || name != tc.wantName {
				t.Errorf("got (ns=%q res=%q name=%q), want (ns=%q res=%q name=%q)",
					ns, res, name, tc.wantNS, tc.wantRes, tc.wantName)
			}
			plural, ok := kubectlPlural[res]
			if !ok {
				t.Fatalf("resource %q has no plural mapping, so the Role cannot grant it", res)
			}
			if got := argoPath(ns, plural, name); got != tc.wantPath {
				t.Errorf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// The strictness is the feature. A lenient parser would reinterpret a call it
// did not understand as a different, VALID one -- the wrong namespace, or a
// collection where a single object was meant -- and a deploy console reporting
// another namespace's state is worse than one that errors.
func TestParseKubectlGetRefusesWhatItCannotServeExactly(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no namespace", []string{"get", "app", "memql", "-o", "json"}, "names no namespace"},
		{"non-json output", []string{"-n", "argocd", "get", "app", "-o", "yaml"}, "only -o json"},
		{"flag with no value", []string{"-n"}, "ends after -n"},
		{"too few positionals", []string{"-n", "memql", "get"}, "cannot read"},
		{"too many positionals", []string{"-n", "memql", "get", "app", "memql", "extra"}, "cannot read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := parseKubectlGet(tc.args); err == nil {
				t.Fatalf("parsed %v without error; it must refuse rather than guess", tc.args)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A resource outside the closed map must be refused by NAME, because the Role
// grants three kinds and reaching a fourth has to be a compile-time decision
// with a matching rule -- not a string that happens to route.
func TestInClusterKubectlRefusesUngrantedResources(t *testing.T) {
	e := &inClusterExecutor{}
	for _, res := range []string{"secret", "pod", "configmap", "appproject"} {
		_, err := e.KubectlJSON(context.Background(), "-n", "memql", "get", res, "-o", "json")
		if err == nil {
			t.Errorf("%s was served; the substrate must refuse a resource its Role does not grant", res)
			continue
		}
		if !strings.Contains(err.Error(), "not one this node's Role grants") {
			t.Errorf("%s: error %q does not say the Role does not grant it", res, err)
		}
	}
}

// Both impossible verbs must name the missing prerequisite. The whole point of
// memql#4257 is that they answered "kickoff_failed", which sent operators
// looking at ArgoCD when the actual absence was a binary or a checkout.
func TestInClusterRefusalsNameTheMissingPrerequisite(t *testing.T) {
	e := &inClusterExecutor{}

	if _, err := e.RunRollback(context.Background(), "abcdef1234567890"); err == nil {
		t.Error("RunRollback succeeded with no checkout")
	} else {
		msg := err.Error()
		if !strings.HasPrefix(msg, ReasonNoOverlayCheckout+":") {
			t.Errorf("rollback refusal does not lead with %q: %s", ReasonNoOverlayCheckout, msg)
		}
		for _, want := range []string{"deploy checkout", "distroless", "memql#4275"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rollback refusal omits %q: %s", want, msg)
			}
		}
	}

	if _, err := e.RunRolloutAction(context.Background(), "bff", "promote"); err == nil {
		t.Error("RunRolloutAction succeeded with no kubectl plugin")
	} else {
		msg := err.Error()
		if !strings.HasPrefix(msg, ReasonNoRolloutPlugin+":") {
			t.Errorf("rollout refusal does not lead with %q: %s", ReasonNoRolloutPlugin, msg)
		}
		if !strings.Contains(msg, "plugin") {
			t.Errorf("rollout refusal does not say it is a plugin: %s", msg)
		}
	}
}

// The refusal distinguishes "no repo root configured" from "a repo root that
// holds nothing", because they ask the operator for different next steps.
func TestNoCheckoutRefusalDistinguishesUnsetFromEmpty(t *testing.T) {
	unset := (&inClusterExecutor{}).noCheckout("x").Error()
	if !strings.Contains(unset, "MEMQL_DEPLOY_REPO_ROOT is unset") {
		t.Errorf("unset case does not say so: %s", unset)
	}
	set := (&inClusterExecutor{repoRoot: "/app"}).noCheckout("x").Error()
	if !strings.Contains(set, "MEMQL_DEPLOY_REPO_ROOT is /app, which holds no repository") {
		t.Errorf("set case does not name the path: %s", set)
	}
}

// InClusterAvailable needs BOTH halves, and neither implies the other:
// KUBERNETES_SERVICE_* is injected into every pod including ones with
// automountServiceAccountToken false, while the token file exists only where a
// ServiceAccount is actually projected. Choosing the substrate on the address
// alone builds a client that 401s on its first call.
func TestInClusterAvailableNeedsBothHalves(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if InClusterAvailable() {
		t.Error("reported available with no API-server address")
	}
	// With an address but (on any developer machine or CI runner) no projected
	// token, it must still be false. Guard the assertion so it says something
	// on the one host where the path does exist.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if _, err := os.Stat(saTokenPath); err != nil && InClusterAvailable() {
		t.Error("reported available with an address but no ServiceAccount token")
	}
}

// TestArgoPathIsNamespacedAndVersioned pins the one string the Role's rules
// have to agree with. A path that dropped the namespace would read across the
// cluster, and the Role would refuse it -- as a 403 rather than as this test.
func TestArgoPathIsNamespacedAndVersioned(t *testing.T) {
	got := argoPath("argocd", "applications", "memql")
	const want = "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"
	if got != want {
		t.Fatalf("argoPath = %q, want %q", got, want)
	}
	if coll := argoPath("memql", "rollouts", ""); strings.HasSuffix(coll, "/") {
		t.Errorf("collection path has a trailing slash: %q", coll)
	}
}

// ---------------------------------------------------------------------------
// The test seam, the streaming GET and the status predicates (memql#5493)
// ---------------------------------------------------------------------------

// roundTripFunc is an http.RoundTripper that is a function: a fake transport for the
// tests that must watch a response body being read or closed, which a real server hides.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// trackedBody is a response body that remembers how much of it was read and whether it
// was closed.
type trackedBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

// holdOpenHandler is a followed pod log in miniature: one line at once, then the response
// stays open until release is closed (or the request's context ends), and a second line
// follows. The gap between the lines is what a per-call timeout would cut.
func holdOpenHandler(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "second\n")
		case <-r.Context().Done():
		}
	}
}

// TestStreamReadsPastTheCallTimeout is the reason Stream exists. A followed pod log runs
// for the step's whole lifetime, and http.Client.Timeout bounds the BODY read as well as
// the round trip, so the per-call client would cut a long step's log off at the call timeout.
func TestStreamReadsPastTheCallTimeout(t *testing.T) {
	const callTimeout = 100 * time.Millisecond
	const path = "api/v1/namespaces/memql-pipelines/pods/step-0/log?container=step&follow=true"

	release := make(chan struct{})
	srv := httptest.NewServer(holdOpenHandler(release))
	defer srv.Close()
	api := NewClusterAPIWith(srv.URL, "tok", &http.Client{Timeout: callTimeout})

	// The control. Do runs on the per-call client, so the SAME response, held open past the
	// call timeout, is cut off by it. Without this the assertions below would pass against a
	// handler that was never slow enough to need Stream in the first place.
	if _, err := api.Do(context.Background(), http.MethodGet, path, "", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("control: Do on a response held open past the call timeout = %v, "+
			"want context.DeadlineExceeded -- the handler proves nothing about Stream", err)
	}

	rc, err := api.Stream(context.Background(), path)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	r := bufio.NewReader(rc)
	if line, err := r.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first line = %q, %v; want %q", line, err, "first\n")
	}

	// Hold the stream open for three call timeouts, and only THEN let the handler write.
	time.Sleep(3 * callTimeout)
	close(release)
	if line, err := r.ReadString('\n'); err != nil || line != "second\n" {
		t.Fatalf("second line, written after the call timeout = %q, %v; want %q "+
			"-- Stream is bound by the per-call timeout", line, err, "second\n")
	}
	if _, err := r.ReadString('\n'); !errors.Is(err, io.EOF) {
		t.Errorf("after the last line: %v, want io.EOF", err)
	}
}

// TestStreamIsBoundedByTheContext: with no timeout of its own, the caller's context is the
// ONLY thing that ends a followed log, so cancelling it must end the read.
func TestStreamIsBoundedByTheContext(t *testing.T) {
	srv := httptest.NewServer(holdOpenHandler(make(chan struct{})))
	defer srv.Close()
	api := NewClusterAPIWith(srv.URL, "tok", srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := api.Stream(ctx, "api/v1/namespaces/memql-pipelines/pods/step-0/log?follow=true")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	r := bufio.NewReader(rc)
	if line, err := r.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first line = %q, %v; want %q", line, err, "first\n")
	}

	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.ReadString('\n')
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("read after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the stream: Stream is not bound by the caller's context")
	}
}

// TestStreamSharesTheCallClientsTransport: the streaming client must reuse the per-call
// client's Transport, because that Transport carries the cluster CA pool. A streaming
// client with a transport of its own cannot verify the API server, so every followed log
// would fail in a real cluster while every call kept working. srv.Client() trusts only this
// server's certificate: it stands in for the pool NewClusterAPI builds from ca.crt.
func TestStreamSharesTheCallClientsTransport(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "a line\n")
	}))
	defer srv.Close()
	api := NewClusterAPIWith(srv.URL, "tok", srv.Client())
	const path = "api/v1/namespaces/memql-pipelines/pods/step-0/log"

	if _, err := api.Do(context.Background(), http.MethodGet, path, "", nil); err != nil {
		t.Fatalf("control: Do could not verify the server its own client trusts: %v", err)
	}
	rc, err := api.Stream(context.Background(), path)
	if err != nil {
		t.Fatalf("Stream could not verify a server the per-call client verifies: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if got, err := io.ReadAll(rc); err != nil || string(got) != "a line\n" {
		t.Errorf("body = %q, %v; want %q", got, err, "a line\n")
	}
}

// TestStreamReturnsStatusErrorOnNon2xx: a refusal is a *StatusError carrying the code and
// the API server's own sentence, exactly as Do reports it, and no reader comes back with it.
func TestStreamReturnsStatusErrorOnNon2xx(t *testing.T) {
	const message = `{"kind":"Status","status":"Failure","message":"container \"step\" in pod ` +
		`\"step-0\" is waiting to start: ContainerCreating","reason":"BadRequest","code":400}`
	const path = "/api/v1/namespaces/memql-pipelines/pods/step-0/log?container=step&follow=true"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, message+"\n")
	}))
	defer srv.Close()
	api := NewClusterAPIWith(srv.URL, "tok", srv.Client())

	rc, err := api.Stream(context.Background(), path)
	if rc != nil {
		_ = rc.Close()
		t.Fatal("Stream returned a reader alongside an error")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Stream error = %T (%v), want *StatusError", err, err)
	}
	if se.Code != http.StatusBadRequest || se.Method != http.MethodGet || se.Path != path ||
		se.Status != "400 Bad Request" || se.Body != message {
		t.Errorf("StatusError = %+v; want code 400, GET, the path as passed, %q and the API server's body", se, "400 Bad Request")
	}

	// "The same as Do": one answer, one error, whichever way it was asked for.
	_, doErr := api.Do(context.Background(), http.MethodGet, path, "", nil)
	if doErr == nil || doErr.Error() != err.Error() {
		t.Errorf("Stream's error %q is not Do's error %q for the same answer", err, doErr)
	}
}

// TestStreamErrorBodyIsCappedAndASuccessBodyIsTheCallers: a refusal's body is read up to
// 64 KiB and closed (an unread, unclosed body pins the connection); a success body is
// handed back open and unread, to be read and closed by the caller.
func TestStreamErrorBodyIsCappedAndASuccessBodyIsTheCallers(t *testing.T) {
	apiWith := func(status int, body io.ReadCloser) *ClusterAPI {
		return NewClusterAPIWith("https://k8s.invalid", "tok", &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
					Header:     http.Header{},
					Body:       body,
				}, nil
			}),
		})
	}

	t.Run("an error body is capped at 64 KiB and closed", func(t *testing.T) {
		body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", 200<<10))}
		_, err := apiWith(http.StatusInternalServerError, body).Stream(context.Background(), "api/v1/x")
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("Stream error = %T (%v), want *StatusError", err, err)
		}
		if len(se.Body) != 64<<10 {
			t.Errorf("StatusError.Body is %d bytes, want %d", len(se.Body), 64<<10)
		}
		if body.read != 64<<10 {
			t.Errorf("Stream read %d bytes off the wire, want exactly the %d cap", body.read, 64<<10)
		}
		if !body.closed {
			t.Error("the error body was never closed")
		}
	})

	t.Run("a success body is left open and unread", func(t *testing.T) {
		body := &trackedBody{Reader: strings.NewReader("a line\n")}
		rc, err := apiWith(http.StatusOK, body).Stream(context.Background(), "api/v1/x")
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if body.closed || body.read != 0 {
			t.Fatalf("Stream touched a success body before returning it (closed=%v, read=%d)", body.closed, body.read)
		}
		if got, err := io.ReadAll(rc); err != nil || string(got) != "a line\n" {
			t.Errorf("body = %q, %v; want %q", got, err, "a line\n")
		}
		if err := rc.Close(); err != nil || !body.closed {
			t.Errorf("Close = %v, closed=%v; the caller's Close must reach the response body", err, body.closed)
		}
	})
}

// TestListMetadataAsksForMetadataAlone: a list made through ListMetadata asks
// for a PartialObjectMetadataList and for nothing else, so a list of Secrets
// never carries their values -- and a server that cannot answer the form
// refuses rather than sending the objects whole. Everything else is Do's: the
// bearer, the body, a non-2xx as a *StatusError.
func TestListMetadataAsksForMetadataAlone(t *testing.T) {
	var accept, auth, path string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept, auth, path = r.Header.Get("Accept"), r.Header.Get("Authorization"), r.URL.RequestURI()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"kind":"PartialObjectMetadataList","items":[]}`)
	}))
	defer srv.Close()
	api := NewClusterAPIWith(srv.URL, "tok", srv.Client())

	out, err := api.ListMetadata(context.Background(), "api/v1/namespaces/ns/secrets?labelSelector=a%3Db&limit=2")
	if err != nil || string(out) != `{"kind":"PartialObjectMetadataList","items":[]}` {
		t.Fatalf("ListMetadata = %q, %v; want the body", out, err)
	}
	if accept != "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1" {
		t.Errorf("Accept = %q, want the PartialObjectMetadataList form alone", accept)
	}
	if auth != "Bearer tok" || path != "/api/v1/namespaces/ns/secrets?labelSelector=a%3Db&limit=2" {
		t.Errorf("Authorization %q, path %q; want the bearer and the path as given", auth, path)
	}

	status = http.StatusNotAcceptable
	var se *StatusError
	if _, err := api.ListMetadata(context.Background(), "api/v1/x"); !errors.As(err, &se) || se.Code != http.StatusNotAcceptable {
		t.Errorf("a refusal = %v, want a *StatusError carrying 406", err)
	}
	if _, err := api.Do(context.Background(), http.MethodGet, "api/v1/x", "", nil); err == nil || accept != "application/json" {
		t.Errorf("Do after ListMetadata sent Accept %q, want plain JSON: the form is ListMetadata's alone", accept)
	}
}

// TestIsConflictAndIsForbiddenQuota pins what each predicate answers for what the API
// server actually says. They read the CODE and, for a quota, the sentence too: a 403 is
// also what a missing Role answers, and the runner waits on the one and fails on the other.
func TestIsConflictAndIsForbiddenQuota(t *testing.T) {
	const quotaBody = `{"kind":"Status","status":"Failure","message":"jobs.batch \"step-7\" is forbidden: ` +
		`exceeded quota: memql-pipelines-ceiling, requested: count/jobs.batch=1, used: count/jobs.batch=8, ` +
		`limited: count/jobs.batch=8","reason":"Forbidden","code":403}`
	const rbacBody = `{"kind":"Status","status":"Failure","message":"jobs.batch is forbidden: User ` +
		`\"system:serviceaccount:memql:memql-engine\" cannot create resource \"jobs\" in API group \"batch\" ` +
		`in the namespace \"memql-pipelines\"","reason":"Forbidden","code":403}`

	cases := []struct {
		name         string
		code         int
		body         string
		wantConflict bool
		wantQuota    bool
	}{
		{"409 AlreadyExists on create", 409, `{"reason":"AlreadyExists","code":409}`, true, false},
		{"409 resourceVersion mismatch on update", 409, `{"reason":"Conflict","message":"the object has been modified","code":409}`, true, false},
		{"403 exceeded quota", 403, quotaBody, false, true},
		{"403 exceeded quota, another case", 403, strings.Replace(quotaBody, "exceeded quota", "Exceeded Quota", 1), false, true},
		{"403 without it (an RBAC refusal)", 403, rbacBody, false, false},
		{"409 whose body names a quota", 409, quotaBody, true, false},
		{"500 whose body names a quota", 500, quotaBody, false, false},
		{"404", 404, `{"reason":"NotFound","code":404}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			api := NewClusterAPIWith(srv.URL, "tok", srv.Client())

			_, doErr := api.Do(context.Background(), http.MethodPost, "apis/batch/v1/namespaces/memql-pipelines/jobs", "application/json", []byte("{}"))
			rc, streamErr := api.Stream(context.Background(), "api/v1/namespaces/memql-pipelines/pods/p/log")
			if rc != nil {
				_ = rc.Close()
			}
			for name, err := range map[string]error{
				"Do":              doErr,
				"Stream":          streamErr,
				"Do, wrapped":     fmt.Errorf("creating job: %w", doErr),
				"Stream, wrapped": fmt.Errorf("following log: %w", streamErr),
			} {
				if got := IsConflict(err); got != tc.wantConflict {
					t.Errorf("%s: IsConflict = %v, want %v (err: %v)", name, got, tc.wantConflict, err)
				}
				if got := IsForbiddenQuota(err); got != tc.wantQuota {
					t.Errorf("%s: IsForbiddenQuota = %v, want %v (err: %v)", name, got, tc.wantQuota, err)
				}
			}
		})
	}

	t.Run("an error that is not a StatusError is neither", func(t *testing.T) {
		for _, err := range []error{
			nil,
			errors.New("409 Conflict"),
			errors.New("403 Forbidden: exceeded quota: memql-pipelines-ceiling"),
			fmt.Errorf("dial tcp: %w", errors.New("connection refused")),
		} {
			if IsConflict(err) || IsForbiddenQuota(err) {
				t.Errorf("%v classified as a conflict or a quota refusal; only a *StatusError carries a code", err)
			}
		}
	})
}

// TestNewClusterAPIWithSendsTheBearer: the seam sends the token it was given, and builds
// its requests exactly as the in-cluster constructor does, on Do and on Stream alike.
func TestNewClusterAPIWithSendsTheBearer(t *testing.T) {
	type request struct{ method, path, query, authorization, accept string }
	var (
		mu   sync.Mutex
		seen []request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Accept")})
		mu.Unlock()
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	last := func() request {
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}

	const logPath = "api/v1/namespaces/memql-pipelines/pods/step-0/log"
	const logQuery = "container=step&follow=true&sinceTime=2026-10-03T22%3A38%3A00Z&timestamps=true"
	cases := []struct {
		name   string
		client *http.Client
		call   func(api *ClusterAPI) error
		want   request
	}{
		{
			name:   "Do",
			client: srv.Client(),
			call: func(api *ClusterAPI) error {
				_, err := api.Do(context.Background(), http.MethodGet, "apis/batch/v1/namespaces/memql-pipelines/jobs/j", "", nil)
				return err
			},
			want: request{http.MethodGet, "/apis/batch/v1/namespaces/memql-pipelines/jobs/j", "", "Bearer tok-123", "application/json"},
		},
		{
			name:   "Do with a leading slash",
			client: srv.Client(),
			call: func(api *ClusterAPI) error {
				_, err := api.Do(context.Background(), http.MethodGet, "/apis/batch/v1/namespaces/memql-pipelines/jobs/j", "", nil)
				return err
			},
			want: request{http.MethodGet, "/apis/batch/v1/namespaces/memql-pipelines/jobs/j", "", "Bearer tok-123", "application/json"},
		},
		{
			name:   "Stream, query string intact",
			client: srv.Client(),
			call: func(api *ClusterAPI) error {
				rc, err := api.Stream(context.Background(), logPath+"?"+logQuery)
				if err != nil {
					return err
				}
				_, _ = io.Copy(io.Discard, rc)
				return rc.Close()
			},
			want: request{http.MethodGet, "/" + logPath, logQuery, "Bearer tok-123", "application/json"},
		},
		{
			name:   "a nil client is the stock transport",
			client: nil,
			call: func(api *ClusterAPI) error {
				_, err := api.Do(context.Background(), http.MethodGet, "api/v1/namespaces", "", nil)
				return err
			},
			want: request{http.MethodGet, "/api/v1/namespaces", "", "Bearer tok-123", "application/json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(NewClusterAPIWith(srv.URL, "tok-123", tc.client)); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := last(); got != tc.want {
				t.Errorf("the server saw %+v, want %+v", got, tc.want)
			}
		})
	}
}

// pemOf is a DER certificate as the PEM a ca.crt file holds.
func pemOf(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// unrelatedCA is a self-signed CA that signed nothing: a trust root no certificate in these
// tests chains to. A second httptest TLS server would NOT do, because every httptest server
// presents the same built-in certificate.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "an unrelated test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return der
}

// TestNewInClusterAPIAssemblesTheClientFromTheProjectedFiles characterises what
// NewClusterAPI builds, through the seam that takes the projected paths as parameters (the
// real ones exist only inside a pod, so nothing else can run the production constructor).
// The cluster CA read from the file is the ONLY trust root, the token is the file's and is
// re-read on every request, and the per-call client is bounded while the streaming client
// shares its Transport and is not.
func TestNewInClusterAPIAssemblesTheClientFromTheProjectedFiles(t *testing.T) {
	var (
		mu     sync.Mutex
		bearer string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearer = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile, caFile := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", srv.Listener.Addr(), err)
	}
	seenBearer := func() string {
		mu.Lock()
		defer mu.Unlock()
		return bearer
	}

	write(caFile, pemOf(srv.Certificate().Raw))
	write(tokenFile, "token-1\n")
	api, err := newInClusterAPI(host, port, tokenFile, caFile)
	if err != nil {
		t.Fatalf("newInClusterAPI: %v", err)
	}

	// What it assembled.
	if want := "https://" + srv.Listener.Addr().String(); api.base != want {
		t.Errorf("base = %q, want %q", api.base, want)
	}
	if api.client.Timeout != k8sRequestTimeout {
		t.Errorf("per-call Timeout = %v, want %v", api.client.Timeout, k8sRequestTimeout)
	}
	if api.stream.Timeout != 0 {
		t.Errorf("streaming Timeout = %v, want none", api.stream.Timeout)
	}
	if api.stream.Transport != api.client.Transport {
		t.Error("the streaming client does not share the per-call client's Transport")
	}
	tr, ok := api.client.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("transport = %#v, want an *http.Transport pinned to a CA pool with TLS 1.2 or later", api.client.Transport)
	}

	// The CA from the file verifies the server, over a call and over a stream.
	if _, err := api.Do(context.Background(), http.MethodGet, "api/v1/namespaces", "", nil); err != nil {
		t.Fatalf("Do against the server the CA file names: %v", err)
	}
	rc, err := api.Stream(context.Background(), "api/v1/namespaces")
	if err != nil {
		t.Fatalf("Stream against the server the CA file names: %v", err)
	}
	_ = rc.Close()

	// The token is the file's, and a rotation in place is picked up by the NEXT request.
	if got := seenBearer(); got != "Bearer token-1" {
		t.Errorf("bearer = %q, want %q", got, "Bearer token-1")
	}
	write(tokenFile, "token-2")
	if _, err := api.Do(context.Background(), http.MethodGet, "api/v1/namespaces", "", nil); err != nil {
		t.Fatalf("Do after rotation: %v", err)
	}
	if got := seenBearer(); got != "Bearer token-2" {
		t.Errorf("bearer after the file was rotated = %q, want %q", got, "Bearer token-2")
	}

	// The control: the CA file is the ONLY trust root. A file naming a different authority
	// must refuse this very server, so the successes above cannot be a system-pool accident.
	write(caFile, pemOf(unrelatedCA(t)))
	wrongCA, err := newInClusterAPI(host, port, tokenFile, caFile)
	if err != nil {
		t.Fatalf("newInClusterAPI with another CA: %v", err)
	}
	if _, err := wrongCA.Do(context.Background(), http.MethodGet, "api/v1/namespaces", "", nil); err == nil {
		t.Error("a client whose CA file names another authority verified this server")
	}

	// A missing token, a missing CA and a CA with no certificate in it are each refused by
	// name, never papered over with the system pool.
	write(caFile, "not a certificate")
	for name, args := range map[string][4]string{
		"missing token":  {host, port, filepath.Join(dir, "absent"), caFile},
		"missing CA":     {host, port, tokenFile, filepath.Join(dir, "absent")},
		"CA without any": {host, port, tokenFile, caFile},
	} {
		if _, err := newInClusterAPI(args[0], args[1], args[2], args[3]); err == nil || !strings.HasPrefix(err.Error(), ReasonNoClusterAPI+":") {
			t.Errorf("%s: error = %v, want one leading with %q", name, err, ReasonNoClusterAPI)
		}
	}
}

// TestTokenNowRereadsTheProjectedFileAndFallsBack: NewClusterAPI's token is a projected
// file the kubelet rotates in place, so it is re-read on every request, with the token read
// at construction as the fallback. NewClusterAPIWith has no such file: the token it was
// given is the whole credential, so a test run inside a pod never sends the pod's own.
func TestTokenNowRereadsTheProjectedFileAndFallsBack(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
	}
	api := &ClusterAPI{token: "at-construction", tokenPath: file} // what NewClusterAPI builds, with another path

	if got := api.tokenNow(); got != "at-construction" {
		t.Errorf("no file yet: tokenNow = %q, want the construction-time token", got)
	}
	write("projected-1\n")
	if got := api.tokenNow(); got != "projected-1" {
		t.Errorf("file present: tokenNow = %q, want %q (the file wins, trimmed)", got, "projected-1")
	}
	write("projected-2")
	if got := api.tokenNow(); got != "projected-2" {
		t.Errorf("file rotated in place: tokenNow = %q, want %q", got, "projected-2")
	}
	write("  \n")
	if got := api.tokenNow(); got != "at-construction" {
		t.Errorf("file blank: tokenNow = %q, want the construction-time token", got)
	}

	explicit := NewClusterAPIWith("https://k8s.invalid", "explicit", nil)
	if explicit.tokenPath != "" {
		t.Errorf("NewClusterAPIWith reads the token file %q; an explicit token must be the whole credential", explicit.tokenPath)
	}
	if got := explicit.tokenNow(); got != "explicit" {
		t.Errorf("explicit client: tokenNow = %q, want %q", got, "explicit")
	}
}
