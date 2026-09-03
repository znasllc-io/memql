package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// credentials_db_test.go -- epic memql#4885, the half that has to run against
// a real engine.
//
// credentials_test.go proves what the resolver DOES with a row it is handed.
// It cannot prove the one thing the design turns on: that the read gate, over
// real rows, hands the resolver ANOTHER person's credential exactly never. The
// recording engine answers whatever it is canned with, so it passes the
// correct resolver -- reading as the package owner -- and the wrong one --
// reading as the caller -- equally well. Only a real engine over a real
// Postgres, with two people's rows in it, can tell them apart.
//
// One test, one story, because the cases are ORDERED: a credential is created,
// fetched under, deployed under by a cluster owner, polled under, refused to a
// second person's package, and then revoked -- and the revoke is terminal, so
// it comes last. Splitting the story into five tests would mean five
// credentials and five packages in a shared database for no additional claim.
//
// Postgres-gated like its neighbours. CI's db-tests lane runs this package
// with MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.
// Locally the throwaway database is on 15434; 5432 is the k3d load balancer,
// which accepts the connection and then EOFs, and every case here would
// silently skip against it.

var (
	dbEngineOnce sync.Once
	dbEngineEng  *memqlengine.MemQLEngine
	dbEngineDB   *bun.DB
	dbEnginePing error
	dbEngineBoot error
)

// dbEngine boots ONE real engine over the shared test database for this FILE.
// A file-level fixture rather than a package-level one, for the reason
// render_parse_test.go's realEngine states: the concept registry is
// process-global, and a fixture shared more widely leaks into every
// registry-walking test that runs after it.
func dbEngine(t *testing.T) (*memqlengine.MemQLEngine, *bun.DB) {
	t.Helper()
	dbEngineOnce.Do(func() {
		dsn := dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(pingCtx); err != nil {
			dbEnginePing = err
			_ = db.Close()
			return
		}
		if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
			dbEngineBoot = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		eng, err := memqlengine.New(db)
		if err != nil {
			dbEngineBoot = fmt.Errorf("engine: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(concept.DefaultRegistry()); err != nil {
			dbEngineBoot = fmt.Errorf("engine init: %w", err)
			return
		}
		dbEngineEng, dbEngineDB = eng, db
	})
	if dbEnginePing != nil {
		dbtest.Unreachable(t, "source-credential resolution over real rows (epic memql#4885)", dbtest.DSN(), dbEnginePing)
	}
	if dbEngineBoot != nil {
		t.Fatalf("%v", dbEngineBoot)
	}
	return dbEngineEng, dbEngineDB
}

// fakeGitHub answers the tarball and commits endpoints for ONE repository and
// 404 for everything else, recording every request it is handed. The 404 for
// strangers is deliberate: the poll walks every repo-sourced package in the
// shared database, and a fake that answered a sha for all of them would write
// upstream versions onto rows other suites own.
type fakeGitHub struct {
	repoPath string // "acme/widget-<suffix>"
	tarball  []byte
	sha      string
	mu       sync.Mutex
	requests []*http.Request
}

func (g *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req.Clone(context.Background()))
	g.mu.Unlock()
	answer := func(status int, body []byte, contentType string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}
	}
	prefix := "/repos/" + g.repoPath + "/"
	switch {
	case strings.HasPrefix(req.URL.Path, prefix+"tarball"):
		return answer(http.StatusOK, g.tarball, "application/gzip"), nil
	case strings.HasPrefix(req.URL.Path, prefix+"commits/"):
		return answer(http.StatusOK, []byte(fmt.Sprintf(`{"sha":%q}`, g.sha)), "application/json"), nil
	}
	return answer(http.StatusNotFound, []byte(`{"message":"Not Found"}`), "application/json"), nil
}

// ours returns the recorded requests for the one repository this fake serves.
func (g *fakeGitHub) ours() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*http.Request
	for _, r := range g.requests {
		if strings.Contains(r.URL.Path, "/repos/"+g.repoPath+"/") {
			out = append(out, r)
		}
	}
	return out
}

func (g *fakeGitHub) reset() {
	g.mu.Lock()
	g.requests = nil
	g.mu.Unlock()
}

// gitHubTarball builds what the tarball API returns: one synthesized
// top-level directory named <owner>-<repo>-<sha>, holding the tree.
func gitHubTarball(t *testing.T, top string, tree fs.FS) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: top + "/" + path + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		body, rerr := fs.ReadFile(tree, path)
		if rerr != nil {
			return rerr
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		_, werr := tw.Write(body)
		return werr
	}); err != nil {
		t.Fatalf("build tarball: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// clusterOwnerCtx is a person the envelope resolves as cluster owner -- the
// operator deploying a colleague's package.
func clusterOwnerCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleOwner})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

func mustExecute(t *testing.T, eng *memqlengine.MemQLEngine, ctx context.Context, statement string) []map[string]any {
	t.Helper()
	res, err := eng.Execute(ctx, statement)
	if err != nil {
		t.Fatalf("%s: %v", firstToken(statement), err)
	}
	return memqlRows(res)
}

func cardFor(t *testing.T, eng *memqlengine.MemQLEngine, ctx context.Context, credentialId string) map[string]any {
	t.Helper()
	rows := mustExecute(t, eng, ctx, fmt.Sprintf("query sourceCredentialById(credentialId: %s)", langparser.QuoteString(credentialId)))
	if len(rows) != 1 {
		t.Fatalf("want one credential card, got %d", len(rows))
	}
	return rows[0]
}

// TestCredentialResolutionIsOwnerScopedOverRealRows is the acceptance run for
// D10 over a real read gate.
func TestCredentialResolutionIsOwnerScopedOverRealRows(t *testing.T) {
	eng, db := dbEngine(t)
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	userA := "v1:identity:user:cred-a-" + suffix
	userB := "v1:identity:user:cred-b-" + suffix
	operator := "v1:identity:user:cred-op-" + suffix
	token := "ghp_DBTOKEN" + suffix + "wxyz"
	repoPath := "acme/widget-" + suffix
	repoUrl := "https://github.com/" + repoPath
	headSha := "0123456789abcdef0123456789abcdef01234567"

	ctxA := auth.ContextWithUserActor(context.Background(), userA)
	ctxB := auth.ContextWithUserActor(context.Background(), userB)
	ctxOp := clusterOwnerCtx(operator)

	logger := discardLogger()
	i, s := credentialHarness(t, eng, logger)
	gh := &fakeGitHub{
		repoPath: repoPath,
		tarball:  gitHubTarball(t, "acme-widget-"+suffix+"-"+headSha, spaOnlyPackage()),
		sha:      headSha,
	}
	i.deps.HTTP = &http.Client{Transport: gh}
	i.deps.Fetcher = &githubFetcher{http: i.deps.HTTP, credentials: s.resolveCredential, tempDir: t.TempDir()}

	// ---- A stores a credential through the capability ----
	nodes, err := i.handleSourceCredentialCreate(ctxA, map[string]any{
		"host": "github.com", "label": "acme deploy token", "token": token,
	}, 0)
	if err != nil {
		t.Fatalf("sourceCredentialCreate: %v", err)
	}
	credentialId, _ := replyPayload(t, nodes)["credentialId"].(string)
	if credentialId == "" {
		t.Fatal("no credentialId in the reply")
	}
	pkgA := "v1:platform:package:cred-a-" + suffix
	pkgB := "v1:platform:package:cred-b-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{credentialId, pkgA, pkgB} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})

	// The card A reads back carries the fingerprint and never the ciphertext;
	// the sealed read is refused to a client-origin call, even A's own.
	card := cardFor(t, eng, ctxA, credentialId)
	if _, leaked := card["encryptedValue"]; leaked {
		t.Fatal("the card projection returned encryptedValue")
	}
	if got := rowString(card, "fingerprint"); got != secret.Fingerprint(token) {
		t.Fatalf("fingerprint %q, want %q", got, secret.Fingerprint(token))
	}
	if rowString(card, "lastUsedAt") != "" {
		t.Fatal("lastUsedAt must be empty before the first use")
	}
	if _, serr := eng.Execute(ctxA, fmt.Sprintf("query sourceCredentialSealedById(credentialId: %s)", langparser.QuoteString(credentialId))); serr == nil || !strings.Contains(serr.Error(), "server-only") {
		t.Fatalf("the sealed read must be refused to a client-origin call, got: %v", serr)
	}

	// ---- A registers a package fetching under it ----
	mustExecute(t, eng, ctxA, fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: "acme", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(pkgA), langparser.QuoteString(repoUrl), langparser.QuoteString(credentialId)))
	rowA, err := s.packageById(ctxA, pkgA)
	if err != nil || rowA == nil {
		t.Fatalf("A cannot read its own package: %v %v", rowA, err)
	}
	if got := rowString(rowA, "credentialId"); !strings.HasSuffix(got, strings.TrimPrefix(credentialId, sourceCredentialConcept+":")) {
		t.Fatalf("the package row does not name the credential: %q", got)
	}

	// (1) A fetches: the credential resolves, GitHub sees the bearer, the
	// tree arrives, and the heartbeat lands on the credential row.
	snap, ferr := i.deps.fetch(ctxA, rowA)
	if ferr != nil {
		t.Fatalf("A's own fetch must succeed: %v", ferr)
	}
	snap.Close()
	if reqs := gh.ours(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("want one tarball request carrying the bearer, got %d request(s): %v", len(reqs), authHeaders(reqs))
	}
	if got := rowString(cardFor(t, eng, ctxA, credentialId), "lastUsedAt"); got == "" {
		t.Fatal("a successful fetch must stamp lastUsedAt on the credential")
	}

	// (4) A cluster owner deploying A's package fetches under A's credential:
	// the resolver runs under the PACKAGE owner, not the caller.
	gh.reset()
	rowAsOp, err := s.packageById(ctxOp, pkgA)
	if err != nil || rowAsOp == nil {
		t.Fatalf("the composite tier must let a cluster owner read A's package: %v %v", rowAsOp, err)
	}
	snap, ferr = i.deps.fetch(ctxOp, rowAsOp)
	if ferr != nil {
		t.Fatalf("a cluster owner deploying A's package must fetch under A's credential: %v", ferr)
	}
	snap.Close()
	if reqs := gh.ours(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("the operator's fetch must carry A's bearer, got: %v", authHeaders(reqs))
	}

	// (5) The poll, under the same rule.
	gh.reset()
	if _, perr := i.handlePollUpstream(ctxOp, nil, 0); perr != nil {
		t.Fatalf("poll: %v", perr)
	}
	if reqs := gh.ours(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("the poll must ask GitHub for A's repository under A's credential, got: %v", authHeaders(reqs))
	}
	if got := rowString(mustPackage(t, s, ctxA, pkgA), "latestKnownVersion"); got != headSha {
		t.Fatalf("the poll must record the head it read, got %q", got)
	}

	// (2) B registers a package naming A's credential: refused by name,
	// before any request, and skipped by the poll.
	mustExecute(t, eng, ctxB, fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: "acme-b", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(pkgB), langparser.QuoteString(repoUrl+"-b"), langparser.QuoteString(credentialId)))
	rowB := mustPackage(t, s, ctxB, pkgB)
	gh.reset()
	if _, ferr := i.deps.fetch(ctxB, rowB); RefusalCode(ferr) != CodeCredentialNotFound {
		t.Fatalf("B's package naming A's credential must refuse with %s, got %v", CodeCredentialNotFound, ferr)
	}
	if n := len(gh.requests); n != 0 {
		t.Fatalf("%d request(s) left the cluster for a package whose credential its owner cannot read", n)
	}
	if _, perr := i.handlePollUpstream(ctxOp, nil, 0); perr != nil {
		t.Fatalf("poll: %v", perr)
	}
	for _, r := range gh.requests {
		if strings.Contains(r.URL.Path, repoPath+"-b") {
			t.Fatalf("the poll asked GitHub for B's repository under a credential B cannot read: %s", r.URL)
		}
	}
	// And B cannot revoke it either: the owned write guard is the decision.
	if _, rerr := i.handleSourceCredentialRevoke(ctxB, map[string]any{"credentialId": credentialId}, 0); rerr == nil {
		t.Fatal("B revoked A's credential -- the owned write guard did not decide")
	}
	if got := rowString(cardFor(t, eng, ctxA, credentialId), "status"); got != credentialStatusActive {
		t.Fatalf("B's refused revoke changed the row: status %q", got)
	}

	// (3) A revokes: A's own package now refuses with credential_revoked,
	// no request is made, and the row survives as history.
	if _, rerr := i.handleSourceCredentialRevoke(ctxA, map[string]any{"credentialId": credentialId}, 0); rerr != nil {
		t.Fatalf("A's own revoke: %v", rerr)
	}
	gh.reset()
	if _, ferr := i.deps.fetch(ctxA, rowA); RefusalCode(ferr) != CodeCredentialRevoked {
		t.Fatalf("a fetch under a revoked credential must refuse with %s, got %v", CodeCredentialRevoked, ferr)
	}
	if n := len(gh.requests); n != 0 {
		t.Fatalf("%d request(s) left the cluster under a revoked credential", n)
	}
	card = cardFor(t, eng, ctxA, credentialId)
	if rowString(card, "status") != credentialStatusRevoked || rowString(card, "revokedAt") == "" {
		t.Fatalf("the revoked row must stay, marked: %v", card)
	}
	// A cluster owner still reads it -- the oversight the composite tier
	// exists for -- and still without the ciphertext.
	opCard := cardFor(t, eng, ctxOp, credentialId)
	if _, leaked := opCard["encryptedValue"]; leaked {
		t.Fatal("the cluster owner's card read returned encryptedValue")
	}
}

func mustPackage(t *testing.T, s *store, ctx context.Context, packageId string) map[string]any {
	t.Helper()
	row, err := s.packageById(ctx, packageId)
	if err != nil || row == nil {
		t.Fatalf("package %s is not readable by this caller: %v", packageId, err)
	}
	return row
}

func authHeaders(reqs []*http.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.URL.Path+" Authorization="+r.Header.Get("Authorization"))
	}
	return out
}
