//go:build clustere2e

package clustere2e

// calls_live_test.go -- two people hold a call on a deployable, through the cluster's
// real front door and its real media server (the calls capability, end to end).
//
// WHAT IT PROVES. Everything between a key and two people hearing each other, on the
// two-replica local cluster and the media server beside it, with no fake in the path:
//
//  1. an operator mints a test key for a deployable (callsTestKeyCreate) and turns its
//     calls on (callsSiteEnable), both as a person over the stream, as Deployables does;
//  2. the edge serves the deployable's calls routes only once the switch is on, and each
//     of the two edge replicas, asked on its own pod with the deployable cached as calls
//     off just before the switch, serves the switch within 10 seconds of it, a third of
//     the 30 seconds its cache keeps a copy: the change feed, not the TTL, reached each
//     one. Each replica learns of a write on its own, through its own subscription, so the
//     bound is held per replica, not "every replica by the moment the first one has it"
//     (waitForEveryReplica, whose rule calls_live_wait_test.go proves in the default
//     build);
//  3. the key holder unlocks a session and starts a call, and a guest joins it by the
//     call's link, all at https://<deployable>/_memql/calls/* through the front door;
//  4. both connect to the media server with the passes the engine handed out, publish a
//     microphone each, and each receives the other's track within 10 seconds;
//  5. both seats read in_call on EVERY bff replica, each asked on its own pod: what the
//     media server reported (its participant_joined webhook, or the reconcile pass
//     behind it) reached the rows, and each replica reads the call fresh, not from what
//     it last cached. The host's roster also says the call is not locked.
//
// THE SITE IS WRITTEN AS app/access_surface_db_test.go WRITES ONE: a raw insert through an
// engine, as the cluster acting with internal origin, here over the database the cluster
// runs on. No route creates a site for a test, and none is added for this one. The
// deployable is live, owned by the person the token names, in the cluster's own
// organization (liveCallsAccount says why it needs one), and has no bundle: the calls
// routes are answered before the edge opens one.
//
// ASKING ONE REPLICA. The front door load-balances, so it cannot say which replica
// answered, nor reach a chosen one. Each edge replica is asked through a port-forward to
// its own pod (edgePodNames and startPodForward, site_edge_invalidation_test.go), with the
// deployable's Host. The roster is asked of each bff pod the same way, with the stamp the
// edge would write (the site and its owner) and the host's session cookie; the bff reads
// the site again under the stamped owner before it believes the stamp, as for every
// request the edge forwards. Each pod's own /healthz names it first.
//
// IT IS NEVER RUN BY `make test`: the build tag keeps it out, and every prerequisite it
// does not find is a skip that says what to start.
//
// PREREQUISITES, ALL SKIPPED WHEN ABSENT:
//   - MEMQL_E2E_TOKEN for a CLUSTER OWNER: the media server list is a cluster owner's
//     read, and the calls builtins need the calls part, which the owner role holds.
//   - MEMQL_E2E_ENDPOINT (default localhost:50051): the bff's gRPC, reachable.
//   - MEMQL_E2E_DATABASE_DSN: the database the cluster runs on, named explicitly (never a
//     default), for the site's row and the sweep afterwards. Locally:
//     kubectl port-forward -n memql svc/memql-db-rw 55433:5432, then
//     postgres://memql:memql_dev@127.0.0.1:55433/memql?sslmode=disable
//   - kubectl on PATH, with at least two Running bff pods and two Running edge pods
//     (make up SERVERS=2, make scale N=2).
//   - The media server (make media-up): the cluster lists an active one, and its public
//     address answers through TLS this machine trusts (the mkcert root, mkcert -install).
//   - The front door at https://<deployable>.<MEMQL_E2E_DOMAIN> (default memql.localhost),
//     trusted by this machine, or by MEMQL_E2E_CA_FILE.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<cluster owner JWT> MEMQL_E2E_DATABASE_DSN=<the cluster's database> \
//	  go test -tags clustere2e -count=1 -timeout=600s ./test/clustere2e/ \
//	  -run TestTwoPeopleHoldACallThroughTheFrontDoor -v

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// The knobs this leg reads beyond the package's own (MEMQL_E2E_ENDPOINT, MEMQL_E2E_TOKEN).
const (
	liveCallsDatabaseEnv = "MEMQL_E2E_DATABASE_DSN"
	liveCallsDomainEnv   = "MEMQL_E2E_DOMAIN"
	liveCallsCAEnv       = "MEMQL_E2E_CA_FILE"
)

// liveCallsConcepts is every concept a call writes rows of, keyed by its site: what the
// sweep takes away afterwards.
var liveCallsConcepts = []string{
	"v1:calls:key", "v1:calls:session", "v1:calls:room", "v1:calls:seat",
	"v1:calls:queueEntry", "v1:calls:scheduledCall", "v1:calls:policy",
}

// liveCallsConfirm is the three confirmations every join records.
var liveCallsConfirm = map[string]any{"adult": true, "noMinors": true, "terms": "clustere2e"}

func liveCallsDomain() string {
	if v := strings.TrimSpace(os.Getenv(liveCallsDomainEnv)); v != "" {
		return v
	}
	return "memql.localhost"
}

// liveReachable skips unless a TCP connection to addr opens: the cluster or a part of it
// is not up, which is a reason to start it, not a failure of what this leg tests.
func liveReachable(t *testing.T, what, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Skipf("%s is not reachable at %s (%v): start the cluster (make up SERVERS=2, make scale N=2) and try again", what, addr, err)
	}
	_ = conn.Close()
}

// liveBFFPods is every Running bff pod, at least two, or a skip.
func liveBFFPods(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH -- this leg asks each bff replica on its own pod")
	}
	out, err := exec.Command("kubectl", "-n", k3dNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=bff",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		t.Skipf("kubectl get pods -l app.kubernetes.io/name=bff failed (no live cluster?): %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) < 2 {
		t.Skipf("found %d Running bff pod(s) in namespace %q, want at least 2 -- run make up SERVERS=2 and make scale N=2 first",
			len(names), k3dNamespace())
	}
	return names
}

// liveClusterEngine is an engine over the database the cluster runs on, for the one row
// this leg writes as access_surface_db_test.go writes it, and for the sweep.
func liveClusterEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(liveCallsDatabaseEnv))
	if dsn == "" {
		// The example DSN is in the file header, not here: a string literal naming the
		// shared credentials is what scripts/cidb's fifteenth-copy gate refuses.
		t.Skipf("%s is unset: this leg writes its deployable's row into the database the cluster runs on, so it needs that "+
			"database named explicitly, through a port-forward to svc/memql-db-rw (this file's header has the commands)", liveCallsDatabaseEnv)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		t.Skipf("the cluster's database at %s does not answer (%v): is the port-forward running?", liveCallsDatabaseEnv, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return eng, db
}

// liveTrust is the roots this machine trusts, with MEMQL_E2E_CA_FILE, or else mkcert's
// root when mkcert is installed, added: the front door's certificate is the local mkcert
// pair.
func liveTrust(t *testing.T) *x509.CertPool {
	t.Helper()
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	path := strings.TrimSpace(os.Getenv(liveCallsCAEnv))
	if path == "" {
		if out, err := exec.Command("mkcert", "-CAROOT").Output(); err == nil {
			path = filepath.Join(strings.TrimSpace(string(out)), "rootCA.pem")
		}
	}
	if path != "" {
		if pem, err := os.ReadFile(path); err == nil {
			pool.AppendCertsFromPEM(pem)
		} else if os.Getenv(liveCallsCAEnv) != "" {
			t.Fatalf("%s names %s, which cannot be read: %v", liveCallsCAEnv, path, err)
		}
	}
	return pool
}

// liveMediaServer is the public address of the media server the cluster places calls on,
// once it answers through TLS: a skip when the cluster lists none or nothing listens
// there, a failure when something answers that is not LiveKit, or whose certificate this
// machine does not trust (the LiveKit client trusts the system's roots alone).
func liveMediaServer(ctx context.Context, t *testing.T, qc *memqlclient.QueryClient) string {
	t.Helper()
	res, err := qc.CallsMediaServers(ctx, memqlclient.CallsMediaServersArgs{})
	if err != nil {
		t.Fatalf("callsMediaServers (MEMQL_E2E_TOKEN must be a cluster owner's): %v", err)
	}
	// The list holds the active servers only, and the installation runs one.
	publicURL := memqlclient.RowString(res.Single(), "publicUrl")
	if publicURL == "" {
		t.Skip("the cluster has no active media server: make secrets, make media-up, and restart the bff so it reads the memql-calls-media Secret")
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		t.Fatalf("the media server's public URL %q is not a URL", publicURL)
	}
	probe := *u
	probe.Scheme, probe.Path = "https", "/"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(probe.String())
	if err != nil {
		var verify *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &verify) || errors.As(err, &unknown) || strings.Contains(err.Error(), "x509:") {
			t.Fatalf("the media server at %s answers with a certificate this machine does not trust (%v): the LiveKit client "+
				"trusts the system's roots alone, so run mkcert -install", probe.String(), err)
		}
		t.Skipf("the media server at %s is not reachable (%v): run make media-up", probe.String(), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "OK" {
		t.Fatalf("the media server at %s answered %d %q, not LiveKit's OK", probe.String(), resp.StatusCode, body)
	}
	return publicURL
}

// liveCallsAccount is the organization the deployable belongs to: the cluster's own
// (v1:accounts:account:self, stored bare as createSite stores it), which a cluster owner's
// writes default to (organizationDefaultAccount) and which Deployables gives what an
// operator creates. It is not optional: a person may not change an existing record that
// no organization holds, a cluster owner included (organization_required,
// validateOrganizationOwnership), so the calls switch would be refused. The first live run
// failed exactly so, before any call.
const liveCallsAccount = "self"

// liveCallsSite is the deployable this leg holds its call on.
type liveCallsSite struct {
	id, owner, host string
	trust           *x509.CertPool
}

// newLiveCallsSite writes a live deployable owned by owner, as access_surface_db_test.go
// writes one, and sweeps it and every calls row of it away afterwards, the calls switch
// turned off first so the edge drops its copy through the change feed.
func newLiveCallsSite(t *testing.T, eng *memql.MemQLEngine, db *bun.DB, qc *memqlclient.QueryClient, owner, domain string) *liveCallsSite {
	t.Helper()
	suffix := strings.ToLower(id.NewShortId())
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	s := &liveCallsSite{id: "calls-live-" + suffix, owner: owner, host: "callse2e-" + suffix + "." + domain, trust: liveTrust(t)}
	payload, err := json.Marshal(map[string]any{
		"hostname": s.host, "kind": "spa", "status": "live", "title": "clustere2e live call",
		"bundleRef": "blob://sites/" + s.id + "/v1/", "ownerUserId": "v1:identity:user:" + owner,
		"accountId": liveCallsAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("calls-live-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "calls-live-test"})
	if _, err := eng.Execute(system, fmt.Sprintf(`insert("v1:platform:site", id=%s, payload=%s)`, langparser.QuoteString(s.id), payload)); err != nil {
		t.Fatalf("write the deployable's row: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := qc.CallsSiteEnable(ctx, memqlclient.CallsSiteEnableArgs{SiteId: s.id, On: false}); err != nil {
			t.Logf("switching the deployable's calls off afterwards: %v", err)
		}
		siteRef := "v1:platform:site:" + s.id
		if _, err := db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept IN (?)", bun.In(liveCallsConcepts)).
			Where("payload->>'siteId' = ?", siteRef).Exec(ctx); err != nil {
			t.Logf("sweeping the deployable's calls rows: %v", err)
		}
		if _, err := db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", siteRef).Exec(ctx); err != nil {
			t.Logf("sweeping the deployable's row: %v", err)
		}
	})
	return s
}

// liveBrowser is one person's browser on the deployable's origin: its own cookie jar, in
// which the calls session the routes hand it lives, and a new connection for every
// request, so the front door spreads the requests over the edge replicas.
type liveBrowser struct {
	t      *testing.T
	site   *liveCallsSite
	client *http.Client
}

func (s *liveCallsSite) browser(t *testing.T) *liveBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	// No proxy: the front door is this machine's.
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: s.trust, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	return &liveBrowser{t: t, site: s, client: &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}}
}

func (s *liveCallsSite) origin() string { return "https://" + s.host }

// send is one request to the deployable's origin, a POST as a page sends it: JSON, from
// the deployable's own origin. A request that gets no answer fails the test.
func (b *liveBrowser) send(method, path string, body any) (int, []byte) {
	b.t.Helper()
	status, out, err := b.attempt(method, path, body)
	if err != nil {
		b.t.Fatalf("%s %s through the front door: %v", method, path, err)
	}
	return status, out
}

// attempt is send without failing, for the best-effort requests of a cleanup.
func (b *liveBrowser) attempt(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, b.site.origin()+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", b.site.origin())
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// sessionToken is the calls session the browser holds, read from its own jar.
func (b *liveBrowser) sessionToken() string {
	b.t.Helper()
	u, _ := url.Parse(b.site.origin() + "/")
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == memql.CallsCookieName {
			return c.Value
		}
	}
	b.t.Fatal("the browser holds no calls session")
	return ""
}

// liveJoin is a join's answer on the wire.
type liveJoin struct {
	Outcome      string `json:"outcome"`
	URL          string `json:"url"`
	Pass         string `json:"pass"`
	Identity     string `json:"identity"`
	Reason       string `json:"reason"`
	Host         bool   `json:"host"`
	RetryAfterMs int64  `json:"retryAfterMs"`
}

// liveRoster is a host's session answer, as far as this leg reads it.
type liveRoster struct {
	Kind   string `json:"kind"`
	Roster *struct {
		Locked *bool `json:"locked"`
		InCall []struct {
			Seat, Status, Identity string
			Host                   bool
		} `json:"inCall"`
	} `json:"roster"`
}

func liveDecode(t *testing.T, what string, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s is not the JSON it should be: %v: %s", what, err, raw)
	}
}

// liveParticipant is one person connected to the media server, publishing a microphone,
// and the tracks they have received, by the identity that sent each.
type liveParticipant struct {
	name  string
	mu    sync.Mutex
	heard map[string]bool
	got   chan struct{}
}

// opusSilence is one 20 ms Opus frame of silence: the microphone a test can publish.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// liveConnect connects to the media server with the pass a join handed out, publishes a
// microphone and keeps sending silence on it until the test ends.
func liveConnect(ctx context.Context, t *testing.T, name string, join liveJoin) *liveParticipant {
	t.Helper()
	p := &liveParticipant{name: name, heard: map[string]bool{}, got: make(chan struct{}, 16)}
	callback := lksdk.NewRoomCallback()
	callback.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		p.mu.Lock()
		p.heard[rp.Identity()] = true
		p.mu.Unlock()
		select {
		case p.got <- struct{}{}:
		default:
		}
	}
	room, err := lksdk.ConnectToRoomWithToken(join.URL, join.Pass, callback, lksdk.WithConnectTimeout(15*time.Second))
	if err != nil {
		t.Fatalf("%s could not connect to the media server at %s with the pass the engine handed out: %v", name, join.URL, err)
	}
	t.Cleanup(room.Disconnect)
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, name+"-mic", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: name + "-mic", Source: livekit.TrackSource_MICROPHONE}); err != nil {
		t.Fatalf("%s could not publish the microphone its pass allows: %v", name, err)
	}
	speaking, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-speaking.Done():
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{Data: opusSilence, Duration: 20 * time.Millisecond})
			}
		}
	}()
	return p
}

// hears waits until the participant has received an audio track from identity.
func (p *liveParticipant) hears(t *testing.T, identity string, within time.Duration) {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		done := p.heard[identity]
		p.mu.Unlock()
		if done {
			return
		}
		select {
		case <-p.got:
		case <-deadline.C:
			t.Fatalf("%s received no audio track from %s within %s", p.name, identity, within)
		}
	}
}

// liveBFFForward is a port-forward to one bff pod's HTTP port, the pod named by its own
// /healthz.
func liveBFFForward(ctx context.Context, t *testing.T, pod string) int {
	t.Helper()
	port := freeLocalPort(t)
	cmd := exec.CommandContext(ctx, "kubectl", "-n", k3dNamespace(), "port-forward", "pod/"+pod, fmt.Sprintf("%d:8085", port))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kubectl port-forward to pod/%s: %v", pod, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := healthzProbe(port)
		if err == nil && info.Code == http.StatusOK && info.NodeType == "bff" {
			if info.NodeId != pod {
				t.Fatalf("pod/%s's own /healthz reports nodeId %q: the forward reached another replica", pod, info.NodeId)
			}
			return port
		}
		if time.Now().After(deadline) {
			t.Fatalf("the port-forward to pod/%s never answered as a bff within 30s (last %+v, %v; kubectl: %s)", pod, info, err, stderr.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// rosterOn is the host's session as one bff replica answers it: the edge's stamp, the
// host's cookie, and nothing else.
func (s *liveCallsSite) rosterOn(t *testing.T, port int, hostSession string) liveRoster {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, memql.CallsSessionPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(memql.CallsStampSiteHeader, s.id)
	req.Header.Set(memql.CallsStampOwnerHeader, s.owner)
	req.AddCookie(&http.Cookie{Name: memql.CallsCookieName, Value: hostSession})
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s on the replica behind :%d: %v", memql.CallsSessionPath, port, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s on the replica behind :%d answered %d: %s", memql.CallsSessionPath, port, resp.StatusCode, body)
	}
	var out liveRoster
	liveDecode(t, "a host's session", body, &out)
	return out
}

// servesCallsOn asks one edge replica, through its own port-forward, for the deployable's
// session route with no cookie, as the front door would hand it the request (the
// deployable's Host), and reads the answer with edgeServesCalls: the edge's 404 while that
// replica still has calls off, the bff's 401 once it forwards.
func (s *liveCallsSite) servesCallsOn(pf *podForward) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s/session", pf.port, memql.CallsInboundPath), nil)
	if err != nil {
		return false, err
	}
	req.Host = s.host
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return edgeServesCalls(resp.StatusCode, string(body))
}

// bothInCall reports whether a roster lists exactly the two seats, by their media
// identities, both in the call.
func bothInCall(r liveRoster, identities ...string) bool {
	if r.Roster == nil || len(r.Roster.InCall) != len(identities) {
		return false
	}
	want := map[string]bool{}
	for _, identity := range identities {
		want[identity] = true
	}
	for _, seat := range r.Roster.InCall {
		if !want[seat.Identity] || seat.Status != "in_call" {
			return false
		}
		delete(want, seat.Identity)
	}
	return len(want) == 0
}

func TestTwoPeopleHoldACallThroughTheFrontDoor(t *testing.T) {
	tok := token(t)
	dial, _, err := memqlclient.ParseClusterEndpoint(endpoint())
	if err != nil {
		t.Fatalf("MEMQL_E2E_ENDPOINT: %v", err)
	}
	liveReachable(t, "the bff's gRPC endpoint (MEMQL_E2E_ENDPOINT)", dial)
	liveReachable(t, "the front door", net.JoinHostPort("api."+liveCallsDomain(), "443"))
	pods := liveBFFPods(t)
	edges := edgePodNames(t)
	eng, db := liveClusterEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	conn := openConnections(ctx, t, tok, 1)[0]
	t.Cleanup(conn.Close)
	qc := memqlclient.NewQueryClient(conn.Dispatcher())
	mediaURL := liveMediaServer(ctx, t, qc)
	t.Logf("the media server: %s", mediaURL)

	site := newLiveCallsSite(t, eng, db, qc, memql.BareShortId(userIDFromToken(t, tok)), liveCallsDomain())
	host, guest := site.browser(t), site.browser(t)
	t.Logf("the deployable: %s (%s)", site.origin(), site.id)

	// Each edge replica on its own pod, named by its own /healthz.
	edgeOn := map[string]*podForward{}
	for _, pod := range edges {
		edgeOn[pod] = startPodForward(t, ctx, pod)
	}

	// 1. A test key, minted as a person over the stream, shown once.
	minted, err := qc.CallsTestKeyCreate(ctx, memqlclient.CallsTestKeyCreateArgs{SiteId: site.id, Label: "clustere2e live call"})
	if err != nil {
		t.Fatalf("callsTestKeyCreate: %v", err)
	}
	key := memqlclient.RowString(minted.Single(), "key")
	if !strings.HasPrefix(key, "ck_") {
		t.Fatalf("callsTestKeyCreate answered no key: %v", minted.Single())
	}

	// OFF UNTIL IT IS SWITCHED ON, ON EVERY EDGE REPLICA: each answers its own 404 and
	// forwards nothing. Asked of each pod just before the switch, so each holds the
	// deployable, calls off, in its cache, and its 30 s TTL cannot run out inside the
	// 10 s bound below: only the change feed can explain a replica serving the switch.
	for _, pod := range edges {
		served, err := site.servesCallsOn(edgeOn[pod])
		if err != nil || served {
			t.Fatalf("pod/%s, asked before the switch, served calls=%v (%v), want the edge's 404", pod, served, err)
		}
	}

	// 2. The switch.
	if _, err := qc.CallsSiteEnable(ctx, memqlclient.CallsSiteEnableArgs{SiteId: site.id, On: true}); err != nil {
		t.Fatalf("callsSiteEnable: %v", err)
	}
	switched := time.Now()

	// 3. EVERY EDGE REPLICA SERVES THE SWITCH WITHIN 10 SECONDS OF IT, a third of the 30 s
	// its cache would keep the old copy without the change feed. Each replica learns of the
	// write on its own, so each is waited for against that one bound (waitForEveryReplica),
	// and none is held to the moment the first one had it. The session route answers the
	// bff's 401 once a replica forwards: asked on that route because it shares the edge's
	// general budget, where an unlock is held to 10 a minute.
	took, err := waitForEveryReplica(time.Now, time.Sleep, switched, 10*time.Second, 250*time.Millisecond, edges,
		func(pod string) (bool, error) { return site.servesCallsOn(edgeOn[pod]) })
	if err != nil {
		t.Fatalf("the calls switch did not reach every edge replica within 10s of it, a third of the 30s TTL its cache "+
			"would keep the old copy for, so the change feed did not reach the ones named: %v", err)
	}
	t.Logf("every edge replica served the switch: %s", took.describe())

	// Unlock through the front door, which every edge replica now forwards.
	status, body := host.send(http.MethodPost, memql.CallsInboundPath+"/unlock", map[string]any{"key": key})
	if status != http.StatusOK {
		t.Fatalf("unlock answered %d %s", status, body)
	}
	var sess liveRoster
	liveDecode(t, "the unlock's answer", body, &sess)
	if sess.Kind != "host" {
		t.Fatalf("a key opened a %q session, want host", sess.Kind)
	}

	status, body = host.send(http.MethodPost, memql.CallsInboundPath+"/start", map[string]any{"name": "Host", "confirm": liveCallsConfirm})
	if status != http.StatusOK {
		t.Fatalf("start answered %d %s", status, body)
	}
	var started struct {
		Call struct {
			ID, GuestToken, HostToken string
		} `json:"call"`
		Join liveJoin `json:"join"`
	}
	liveDecode(t, "the start's answer", body, &started)
	if started.Join.Outcome != "admit" || started.Join.Pass == "" || !started.Join.Host {
		t.Fatalf("the host was not let into the call it started: %+v", started.Join)
	}
	t.Cleanup(func() {
		// Best effort, for a failure part-way: the call ends, and its media room with it. A
		// call the test ended already answers that it has ended.
		if _, _, err := host.attempt(http.MethodPost, memql.CallsInboundPath+"/host", map[string]any{"action": "end"}); err != nil {
			t.Logf("ending the call afterwards: %v", err)
		}
	})

	// 4. A guest joins by the call's link, with no session of their own.
	status, body = guest.send(http.MethodPost, memql.CallsInboundPath+"/join",
		map[string]any{"token": started.Call.GuestToken, "name": "Guest", "confirm": liveCallsConfirm})
	if status != http.StatusOK {
		t.Fatalf("the guest's join answered %d %s", status, body)
	}
	var joined liveJoin
	liveDecode(t, "the guest's join", body, &joined)
	if joined.Outcome != "admit" || joined.Pass == "" || joined.Host {
		t.Fatalf("the guest was not let in as a guest: %+v", joined)
	}
	if joined.URL != started.Join.URL {
		t.Fatalf("the guest was sent to %s and the host to %s: one call is on one media server", joined.URL, started.Join.URL)
	}

	// 5. Both connect with the passes the engine handed out, and publish a microphone.
	hostP := liveConnect(ctx, t, "the host", started.Join)
	guestP := liveConnect(ctx, t, "the guest", joined)

	// 6. Each receives the other's track within 10 seconds...
	hostP.hears(t, joined.Identity, 10*time.Second)
	guestP.hears(t, started.Join.Identity, 10*time.Second)

	// ...and both seats are in the call on every bff replica, each asked on its own pod.
	// The webhook usually lands within seconds; the reconcile pass, every 30 s, is the
	// bound, so 45 s.
	hostSession := host.sessionToken()
	inCallBy := time.Now().Add(45 * time.Second)
	for _, pod := range pods {
		port := liveBFFForward(ctx, t, pod)
		for {
			roster := site.rosterOn(t, port, hostSession)
			if bothInCall(roster, started.Join.Identity, joined.Identity) {
				if roster.Roster.Locked == nil || *roster.Roster.Locked {
					t.Fatalf("pod/%s's roster does not say the call is unlocked: %+v", pod, roster.Roster)
				}
				t.Logf("pod/%s reads both seats in the call", pod)
				break
			}
			if time.Now().After(inCallBy) {
				t.Fatalf("pod/%s still reads %+v: both seats should be in_call once the media server reported them", pod, roster.Roster)
			}
			time.Sleep(time.Second)
		}
	}

	// The host ends the call for everyone in it.
	if status, body := host.send(http.MethodPost, memql.CallsInboundPath+"/host", map[string]any{"action": "end"}); status != http.StatusNoContent {
		t.Fatalf("the host's end answered %d %s", status, body)
	}
}
