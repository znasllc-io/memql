package packages

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

// grant_db_test.go -- epic memql#4912's half that has to run against a real
// engine, the same split credentials_db_test.go states.
//
// grant_test.go proves what the resolver, the fetcher and the picker DO with a
// row they are handed. It cannot prove the thing the design turns on: that the
// read gate, over real rows, hands one person's grant to nobody else. The
// recording engine answers whatever it is canned with, so it passes the
// correct owner-scoped read and an unscoped one equally well -- and
// githubAppGrantForCaller takes NO ARGUMENTS, so its whole authorization IS
// the owner term in its filter. Only a real engine over real Postgres, with
// two people's rows in it, can tell a correct filter from a missing one.
//
// ONE TEST, ONE STORY, because the cases are ordered: a grant is landed,
// resolved, fetched under, probed under, listed under, refused to a second
// person, and then disconnected -- and the disconnect is terminal, so it comes
// last.
//
// Postgres-gated like its neighbours. Locally the throwaway database is on
// 15434; 5432 is the k3d load balancer, which accepts the connection and then
// EOFs, and every case here would silently skip against it.

// grantFakeHub answers the app and user endpoints for ONE repository and one
// installation, and 404 for everything else. The 404 for strangers matters in
// a SHARED database: the poll walks every repo-sourced package in it, and a
// fake that answered for all of them would write upstream versions onto rows
// other suites own.
type grantFakeHub struct {
	*grantHub
	repoPath string
}

func (g *grantFakeHub) ours() []*http.Request {
	var out []*http.Request
	for _, r := range g.seen() {
		if r.URL.Path == "/repos/"+g.repoPath || strings.HasPrefix(r.URL.Path, "/repos/"+g.repoPath+"/") {
			out = append(out, r)
		}
	}
	return out
}

func (g *grantFakeHub) reset() {
	g.mu.Lock()
	g.requests = nil
	g.sent = nil
	g.mu.Unlock()
}

// TestGithubAppGrantResolutionIsOwnerScopedOverRealRows is the acceptance run
// for the grant kind over a real read gate.
func TestGithubAppGrantResolutionIsOwnerScopedOverRealRows(t *testing.T) {
	eng, db := dbEngine(t)
	t.Setenv(secret.EnvMasterKey, testMasterKey)

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	userA := "v1:identity:user:grant-a-" + suffix
	userB := "v1:identity:user:grant-b-" + suffix
	credentialId := "v1:platform:sourceCredential:grant-" + suffix
	packageId := "v1:platform:package:grant-" + suffix
	repoPath := "acme/grant-" + suffix
	repoUrl := "https://github.com/" + repoPath
	headSha := "abcdef0123456789abcdef0123456789abcdef01"
	userToken := "ghu_DBUSER" + suffix
	refreshToken := "ghr_DBREFRESH" + suffix
	installToken := "ghs_DBINSTALL" + suffix

	ctxA := auth.ContextWithUserActor(context.Background(), userA)
	ctxB := auth.ContextWithUserActor(context.Background(), userB)

	t.Cleanup(func() {
		for _, id := range []string{credentialId, packageId} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})

	// ---- the callback's write, landed as the identity node lands it ----
	//
	// createGithubAppGrant is @serverOnly and stamps ownerUserId from the
	// actor, so it runs under A's borrowed actor and an internal-origin stamp
	// -- which is exactly what the connect callback does on the identity node.
	// (A stamp in a _test.go file is outside both origin gates by
	// construction: each skips files ending in _test.go.)
	sealedValue, fingerprint, serr := secret.Encrypt(userToken)
	if serr != nil {
		t.Fatalf("seal: %v", serr)
	}
	sealedRefresh, _, rerr := secret.Encrypt(refreshToken)
	if rerr != nil {
		t.Fatalf("seal: %v", rerr)
	}
	create := fmt.Sprintf(
		`mutation createGithubAppGrant(credentialId: %s, host: "github.com", label: "GitHub (@alice-%s)", encryptedValue: %s, fingerprint: %s, refreshToken: %s, login: %s, externalId: %s, installationIds: ["42"])`,
		langparser.QuoteString(credentialId), suffix,
		langparser.QuoteString(sealedValue), langparser.QuoteString(fingerprint),
		langparser.QuoteString(sealedRefresh),
		langparser.QuoteString("alice-"+suffix), langparser.QuoteString("5150"))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctxA), create); err != nil {
		t.Fatalf("createGithubAppGrant: %v", err)
	}

	// ---- the card: kind, login and installations, and NEITHER sealed field
	card := cardFor(t, eng, ctxA, credentialId)
	if got := rowString(card, "kind"); got != credentialKindGithubApp {
		t.Fatalf("kind %q, want %q", got, credentialKindGithubApp)
	}
	if got := rowString(card, "login"); got != "alice-"+suffix {
		t.Fatalf("login %q", got)
	}
	if strings.Join(rowStrings(card, "installationIds"), ",") != "42" {
		t.Fatalf("installationIds %v", card["installationIds"])
	}
	for _, sealedField := range []string{"encryptedValue", "refreshToken"} {
		if _, leaked := card[sealedField]; leaked {
			t.Fatalf("the card projection returned %s", sealedField)
		}
	}

	// ---- the no-argument read is owner-scoped BY ITS FILTER ----
	//
	// githubAppGrantForCaller takes no arguments at all, so there is no value
	// anybody could send to make it answer with somebody else's grant, and its
	// owner term is the whole of its authorization. This is the pair the fake
	// engine cannot tell apart.
	hub := &grantFakeHub{grantHub: newGrantHub(), repoPath: repoPath}
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	s := &store{engine: eng, logger: discardLogger(), github: client}

	mine, err := s.githubAppGrantForCaller(ctxA)
	if err != nil {
		t.Fatalf("grant read: %v", err)
	}
	if mine == nil || rowString(mine, "id") != credentialId {
		t.Fatalf("A must read its own grant, got %v", mine)
	}
	theirs, err := s.githubAppGrantForCaller(ctxB)
	if err != nil {
		t.Fatalf("grant read as B: %v", err)
	}
	if theirs != nil {
		t.Fatalf("B read A's grant through a no-argument query: %v", theirs)
	}

	// ---- a fetch under the grant carries an INSTALLATION token ----
	hub.body("/repos/"+repoPath+"/installation", http.StatusOK, `{"id":42}`)
	hub.body("/app/installations/42/access_tokens", http.StatusCreated,
		`{"token":"`+installToken+`","expires_at":"`+time.Now().UTC().Add(time.Hour).Format(time.RFC3339)+`"}`)
	tarball := string(gitHubTarball(t, "acme-grant-"+suffix+"-"+headSha, spaOnlyPackage()))
	hub.on("/repos/"+repoPath+"/tarball", func(*http.Request) (int, string) { return http.StatusOK, tarball })
	hub.body("/repos/"+repoPath+"/commits/HEAD", http.StatusOK, `{"sha":"`+headSha+`"}`)
	hub.body("/repos/"+repoPath, http.StatusOK, `{"private":true,"default_branch":"main"}`)
	hub.body("/repos/"+repoPath+"/branches", http.StatusOK, `[{"name":"topic"},{"name":"main"}]`)

	deps := &Deps{
		Store:           s,
		Credentials:     s.resolveCredential,
		PeekCredentials: s.peekCredential,
		GitHubApp:       client,
		HTTP:            &http.Client{Transport: hub},
		Logger:          discardLogger(),
		Limits:          DefaultLimits(),
		Fetcher: &githubFetcher{
			http:        &http.Client{Transport: hub},
			credentials: s.resolveCredential,
			github:      client,
			tempDir:     t.TempDir(),
		},
	}
	i := NewIntegration(eng, discardLogger())
	i.depsOnce.Do(func() { i.deps = deps })

	mustExecute(t, eng, ctxA, fmt.Sprintf(
		`mutation createPackage(packageId: %s, name: "acme", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(packageId), langparser.QuoteString(repoUrl), langparser.QuoteString(credentialId)))
	pkgRow := mustPackage(t, s, ctxA, packageId)

	hub.reset()
	snap, ferr := deps.fetch(ctxA, pkgRow)
	if ferr != nil {
		t.Fatalf("a fetch under A's grant must succeed: %v", ferr)
	}
	snap.Close()
	tar := ""
	for _, r := range hub.ours() {
		if strings.HasSuffix(r.URL.Path, "/tarball") {
			tar = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
	}
	if tar != installToken {
		t.Fatalf("the fetch must carry the INSTALLATION token, got %q", tar)
	}
	if tar == userToken {
		t.Fatal("the fetch carried the person's own user token, so a deploy would depend on them being signed in")
	}
	// The heartbeat lands on the grant exactly as on a pasted credential.
	if rowString(cardFor(t, eng, ctxA, credentialId), "lastUsedAt") == "" {
		t.Fatal("a successful fetch must stamp lastUsedAt on the grant")
	}

	// ---- the poll, under the same rule ----
	hub.reset()
	if _, perr := i.handlePollUpstream(ctxA, nil, 0); perr != nil {
		t.Fatalf("poll: %v", perr)
	}
	commits := ""
	for _, r := range hub.ours() {
		if strings.Contains(r.URL.Path, "/commits/") {
			commits = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
	}
	if commits != installToken {
		t.Fatalf("the poll must carry the installation token, got %q", commits)
	}
	if got := rowString(mustPackage(t, s, ctxA, packageId), "latestKnownVersion"); got != headSha {
		t.Fatalf("the poll must record the head it read, got %q", got)
	}

	// ---- a probe naming NOTHING resolves A's own grant ----
	hub.reset()
	probe, prerr := ProbeSource(ctxA, deps, repoUrl, "")
	if prerr != nil {
		t.Fatalf("probe: %v", prerr)
	}
	if probe.Reason != ProbeReasonOK || !probe.Private || probe.DefaultBranch != "main" {
		t.Fatalf("probe answered %+v", probe)
	}
	repoBearer := ""
	for _, r := range hub.ours() {
		if r.URL.Path == "/repos/"+repoPath {
			repoBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
	}
	if repoBearer != userToken {
		t.Fatalf("a probe asks what the PERSON can see and must carry their token, got %q", repoBearer)
	}
	if strings.Join(probe.Branches, ",") != "main,topic" {
		t.Fatalf("branches %v, want the default first", probe.Branches)
	}
	// And B, probing the same repository with no credential, gets no grant of
	// their own and probes anonymously -- the owner term again.
	hub.reset()
	if _, berr := ProbeSource(ctxB, deps, repoUrl, ""); berr != nil {
		t.Fatalf("B's anonymous probe: %v", berr)
	}
	for _, r := range hub.ours() {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("B's probe carried a bearer it has no grant for: %s", r.URL.Path)
		}
	}

	// ---- the picker refreshes the grant's installation ids ----
	hub.body("/user/installations", http.StatusOK,
		`{"total_count":1,"installations":[{"id":77,"account":{"login":"acme"},"repository_selection":"all"}]}`)
	hub.body("/user/installations/77/repositories", http.StatusOK,
		`{"total_count":1,"repositories":[{"full_name":"`+repoPath+`","name":"grant-`+suffix+`","owner":{"login":"acme"},"default_branch":"main"}]}`)
	list, lerr := SourceRepositories(ctxA, deps, credentialId, 0)
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if list.Reason != RepositoriesReasonOK || len(list.Repositories) != 1 {
		t.Fatalf("got %+v", list)
	}
	// THE WRITE LANDED, over a real row, under A's own actor: the ids moved
	// from the connect callback's 42 to what GitHub just said.
	if got := strings.Join(rowStrings(cardFor(t, eng, ctxA, credentialId), "installationIds"), ","); got != "77" {
		t.Fatalf("the picker must refresh the grant's installation ids, got %q", got)
	}

	// ---- B naming A's grant is refused by name, before any request ----
	hub.reset()
	bList, lerr := SourceRepositories(ctxB, deps, credentialId, 0)
	if lerr != nil {
		t.Fatalf("a credential B cannot read is a typed reason, not an error: %v", lerr)
	}
	if bList.Reason != RepositoriesReasonCredentialMissing {
		t.Fatalf("B listing under A's grant must answer %s, got %s", RepositoriesReasonCredentialMissing, bList.Reason)
	}
	bProbe, bperr := ProbeSource(ctxB, deps, repoUrl, credentialId)
	if bperr != nil {
		t.Fatalf("B's probe under A's grant is a typed reason, not an error: %v", bperr)
	}
	if bProbe.Reason != ProbeReasonCredentialNotFound {
		t.Fatalf("B's probe under A's grant must answer %s, got %s", ProbeReasonCredentialNotFound, bProbe.Reason)
	}
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left the cluster for a grant the caller cannot read", n)
	}

	// ---- disconnect ----
	hub.reset()
	hub.body("/applications/Iv1.memqlconnect/grant", http.StatusNoContent, ``)
	nodes, rvErr := i.handleSourceCredentialRevoke(ctxA, map[string]any{"credentialId": credentialId}, 0)
	if rvErr != nil {
		t.Fatalf("disconnect: %v", rvErr)
	}
	if replyPayload(t, nodes)["remoteRevoked"] != true {
		t.Fatal("a grant must be ended at GitHub as well as locally")
	}
	if hub.hits("/applications/Iv1.memqlconnect/grant") != 1 {
		t.Fatal("the revoke must reach GitHub exactly once")
	}
	// A DISCONNECTED GRANT READS AS ABSENT, which is what makes the surface
	// offer Connect rather than a picker that would refuse every repository.
	gone, gerr := s.githubAppGrantForCaller(ctxA)
	if gerr != nil {
		t.Fatalf("grant read: %v", gerr)
	}
	if gone != nil {
		t.Fatalf("a revoked grant must read as absent, got %v", gone)
	}
	// And a fetch under it refuses by name, with no request made.
	hub.reset()
	if _, ferr := deps.fetch(ctxA, pkgRow); RefusalCode(ferr) != CodeCredentialRevoked {
		t.Fatalf("a fetch under a disconnected grant must refuse with %s, got %v", CodeCredentialRevoked, ferr)
	}
	if n := len(hub.ours()); n != 0 {
		t.Fatalf("%d request(s) left the cluster under a disconnected grant", n)
	}
	// The row stays as history, marked.
	final := cardFor(t, eng, ctxA, credentialId)
	if rowString(final, "status") != credentialStatusRevoked || rowString(final, "revokedAt") == "" {
		t.Fatalf("the disconnected grant must stay, marked: %v", final)
	}
	if rowString(final, "login") == "" {
		t.Fatal("a disconnected grant keeps the login it was connected as -- the card says who it WAS")
	}
}
