package identity

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// store_githubconnect_test.go -- the two contracts the GitHub Connect writes
// depend on and that nothing else can check for them (epic memql#4912).
//
//  1. EVERY @serverOnly construct is reached with INTERNAL ORIGIN stamped.
//     The engine refuses such a call otherwise, so an unstamped one does not
//     degrade -- the feature simply does not work, on any cluster, ever. The
//     DSL-side gates pass auth.OriginInternal in by hand, which is precisely
//     the thing this store is responsible for supplying, so they cannot catch
//     a missing stamp. That is the server_only_origin_test.go argument
//     (memql#2881) applied to the constructs this file added.
//
//  2. THE GRANT WRITE BORROWS THE OWNER'S ACTOR. createGithubAppGrant stamps
//     ownerUserId from actor.userId and the callback has no actor of its own,
//     so without the borrow the row lands owned by nobody -- readable by
//     nobody, including the person who just connected, who would see a success
//     and a credential that resolves for no package.
//
// The statements are also PARSED, because a builtin that writes renders MemQL
// TEXT and a fake engine that only records strings would happily accept one
// the real engine cannot read.

// connectOriginRecorder captures the origin AND the actor of the context each
// construct was executed with, keyed by the construct name in the query.
type connectOriginRecorder struct {
	mu      sync.Mutex
	origins map[string]auth.CallOrigin
	actors  map[string]string
	queries []string

	// grant, when non-nil, is what githubAppGrantByExternalId answers, so a
	// test can drive the update branch.
	grant map[string]string
}

var connectConstructNames = []string{
	"githubConnectStateByHash",
	"createGithubConnectState",
	"consumeGithubConnectState",
	"githubAppGrantByExternalId",
	"createGithubAppGrant",
	"updateGithubAppGrant",
}

func (r *connectOriginRecorder) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	r.mu.Lock()
	if r.origins == nil {
		r.origins = map[string]auth.CallOrigin{}
		r.actors = map[string]string{}
	}
	r.queries = append(r.queries, q)
	for _, name := range connectConstructNames {
		if strings.Contains(q, name+"(") {
			r.origins[name] = auth.OriginFromContext(ctx)
			if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
				r.actors[name] = ac.UserId
			}
		}
	}
	grant := r.grant
	r.mu.Unlock()

	if strings.Contains(q, "githubAppGrantByExternalId(") && grant != nil {
		fields := map[string]*structpb.Value{}
		for k, v := range grant {
			fields[k] = structpb.NewStringValue(v)
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: grant["id"], Payload: &structpb.Struct{Fields: fields}}},
		}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func (r *connectOriginRecorder) originOf(t *testing.T, construct string) auth.CallOrigin {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	got, ok := r.origins[construct]
	if !ok {
		t.Fatalf("no statement naming %q was executed -- if the construct was renamed, move this "+
			"guard with it rather than letting it silently stop checking. Executed:\n  %s",
			construct, strings.Join(r.queries, "\n  "))
	}
	return got
}

func (r *connectOriginRecorder) statement(t *testing.T, prefix string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.queries {
		if strings.HasPrefix(q, prefix) {
			return q
		}
	}
	t.Fatalf("no statement starting %q. Executed:\n  %s", prefix, strings.Join(r.queries, "\n  "))
	return ""
}

// TestConnectStateWritesStampInternalOrigin covers the three constructs over
// v1:identity:githubConnectState.
func TestConnectStateWritesStampInternalOrigin(t *testing.T) {
	rec := &connectOriginRecorder{}
	store := &Store{Engine: rec}
	ctx := context.Background()

	if _, err := store.CreateGithubConnectState(ctx, GithubConnectStateSeed{
		UserId:     "v1:identity:user:asked",
		StateHash:  HashConnectState("plain"),
		ReturnPath: "/packages/new",
		ExpiresAt:  time.Now().UTC().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateGithubConnectState: %v", err)
	}
	// The lookup returns nothing from this fake, so the consume refuses --
	// which is fine: the origin of the READ is what is under test here, and it
	// is recorded before the refusal.
	_, _ = store.ConsumeGithubConnectState(ctx, HashConnectState("plain"), "203.0.113.9")

	for _, construct := range []string{"createGithubConnectState", "githubConnectStateByHash"} {
		if got := rec.originOf(t, construct); !got.IsInternal() {
			t.Errorf("%s executed with origin %v, not internal.\n"+
				"It is @serverOnly, so an unstamped call is REFUSED at runtime -- GitHub Connect "+
				"would not work on any cluster, and the DSL-side gate cannot catch it because that "+
				"test supplies the origin this store is responsible for supplying.", construct, got)
		}
	}
}

// TestConsumeStampsInternalOriginOnTheWrite drives the consume all the way to
// its write by answering the re-read with a live row.
func TestConsumeStampsInternalOriginOnTheWrite(t *testing.T) {
	eng := &githubConnectFakeEngine{row: liveConnectStateRow()}
	origins := &connectOriginTee{inner: eng}
	store := &Store{Engine: origins}

	if _, err := store.ConsumeGithubConnectState(context.Background(),
		HashConnectState("the-plaintext-state"), "203.0.113.9"); err != nil {
		t.Fatalf("ConsumeGithubConnectState: %v", err)
	}
	got, ok := origins.seen["consumeGithubConnectState"]
	if !ok {
		t.Fatal("the consume never issued consumeGithubConnectState")
	}
	if !got.IsInternal() {
		t.Errorf("consumeGithubConnectState executed with origin %v, not internal", got)
	}
}

// connectOriginTee records the origin of each call and delegates the answer, so
// a test can use the richer fake next door and still see contexts.
type connectOriginTee struct {
	inner *githubConnectFakeEngine
	mu    sync.Mutex
	seen  map[string]auth.CallOrigin
}

func (t *connectOriginTee) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	t.mu.Lock()
	if t.seen == nil {
		t.seen = map[string]auth.CallOrigin{}
	}
	for _, name := range connectConstructNames {
		if strings.Contains(q, name+"(") {
			t.seen[name] = auth.OriginFromContext(ctx)
		}
	}
	t.mu.Unlock()
	return t.inner.Execute(ctx, q)
}

// TestGrantWriteBorrowsTheOwnersActorAndStampsOrigin is contract (2).
func TestGrantWriteBorrowsTheOwnersActorAndStampsOrigin(t *testing.T) {
	rec := &connectOriginRecorder{}
	store := &Store{Engine: rec}

	id, created, err := store.UpsertGithubAppGrant(context.Background(), GithubAppGrant{
		OwnerUserId:     "v1:identity:user:asked",
		Host:            "github.com",
		Label:           "GitHub (@octocat)",
		EncryptedValue:  "c2VhbGVk",
		Fingerprint:     "...0000",
		RefreshToken:    "cmVmcmVzaA==",
		ExpiresAt:       time.Unix(1700000000, 0).UTC(),
		Login:           "octocat",
		ExternalId:      "583231",
		InstallationIds: []string{"11111", "22222"},
	})
	if err != nil {
		t.Fatalf("UpsertGithubAppGrant: %v", err)
	}
	if !created || !strings.HasPrefix(id, "v1:platform:sourceCredential:") {
		t.Fatalf("created=%v id=%q, want a fresh sourceCredential row", created, id)
	}

	for _, construct := range []string{"githubAppGrantByExternalId", "createGithubAppGrant"} {
		if got := rec.originOf(t, construct); !got.IsInternal() {
			t.Errorf("%s executed with origin %v, not internal", construct, got)
		}
		if got := rec.actors[construct]; got != "v1:identity:user:asked" {
			t.Errorf("%s executed under actor %q, want the state row's user.\n"+
				"The mutation stamps ownerUserId from actor.userId and this call has no actor of "+
				"its own -- a browser redirect from GitHub carries no MemQL bearer -- so without "+
				"the borrow the row lands owned by NOBODY, readable by nobody, including the "+
				"person who just connected.", construct, got)
		}
	}
	// The compare and the swap must run as the SAME actor: an unborrowed read
	// filters ownerUserId==actor.userId against "", matches nothing, and every
	// reconnect mints a second row.
	if rec.actors["githubAppGrantByExternalId"] != rec.actors["createGithubAppGrant"] {
		t.Errorf("the read and the write ran as different actors (%q vs %q)",
			rec.actors["githubAppGrantByExternalId"], rec.actors["createGithubAppGrant"])
	}
}

// TestAGrantWithNoOwnerIsRefusedBeforeAnythingIsExecuted.
//
// auth.ContextWithUserActor returns the context UNCHANGED on a blank id, so
// there is no error to notice downstream -- the row simply lands owned by
// nobody. The refusal has to be here, before the borrow.
func TestAGrantWithNoOwnerIsRefusedBeforeAnythingIsExecuted(t *testing.T) {
	rec := &connectOriginRecorder{}
	store := &Store{Engine: rec}

	_, _, err := store.UpsertGithubAppGrant(context.Background(), GithubAppGrant{
		OwnerUserId: "  ",
		ExternalId:  "583231",
	})
	if err == nil {
		t.Fatal("a grant with no owner was written")
	}
	if len(rec.queries) != 0 {
		t.Errorf("the engine was reached %d time(s) before the refusal: %v", len(rec.queries), rec.queries)
	}
}

// TestConnectStatementsParse is the "render MemQL TEXT" guard: a fake engine
// that only records strings accepts a statement the real engine cannot read,
// and the failure would then arrive at the first live callback.
func TestConnectStatementsParse(t *testing.T) {
	rec := &connectOriginRecorder{}
	store := &Store{Engine: rec}
	ctx := context.Background()

	if _, err := store.CreateGithubConnectState(ctx, GithubConnectStateSeed{
		// Hostile-ish values: a quote, a backslash and a control byte are all
		// reachable from a client-supplied return path or a proxy header.
		UserId:     `v1:identity:user:as"ked`,
		StateHash:  HashConnectState("plain"),
		ReturnPath: "/packages/new?q=a%20b",
		SourceIP:   "203.0.113.9\t",
		ExpiresAt:  time.Unix(1700000000, 0).UTC(),
	}); err != nil {
		t.Fatalf("CreateGithubConnectState: %v", err)
	}
	if _, _, err := store.UpsertGithubAppGrant(ctx, GithubAppGrant{
		OwnerUserId:     "v1:identity:user:asked",
		Host:            "github.com",
		Label:           `GitHub (@oct"cat)`,
		EncryptedValue:  "c2VhbGVk",
		Fingerprint:     "...0000",
		Login:           "octocat",
		ExternalId:      "583231",
		InstallationIds: []string{"11111", `2"2`},
	}); err != nil {
		t.Fatalf("UpsertGithubAppGrant: %v", err)
	}

	for _, prefix := range []string{
		"mutation createGithubConnectState(",
		"query githubAppGrantByExternalId(",
		"mutation createGithubAppGrant(",
	} {
		q := rec.statement(t, prefix)
		expr, err := langparser.ParseExpression(q)
		if err != nil {
			t.Fatalf("the engine cannot parse the statement this store emits, so the row is never "+
				"written:\n  %s\n  %v", q, err)
		}
		if _, ok := expr.(*langparser.FunctionCallExpr); !ok {
			t.Fatalf("statement parsed as %T, want a call:\n  %s", expr, q)
		}
	}

	// The list literal is the one shape not covered by a scalar argument: an
	// empty list must render as a list rather than as nothing, because an
	// installation somebody removed has to leave the row.
	empty := renderEmptyInstallationList(t, store, rec)
	if !strings.Contains(empty, "installationIds: []") {
		t.Errorf("an empty installation list did not render as []:\n  %s", empty)
	}
}

func renderEmptyInstallationList(t *testing.T, store *Store, rec *connectOriginRecorder) string {
	t.Helper()
	rec.mu.Lock()
	rec.queries = nil
	rec.mu.Unlock()
	if _, _, err := store.UpsertGithubAppGrant(context.Background(), GithubAppGrant{
		OwnerUserId:    "v1:identity:user:asked",
		Host:           "github.com",
		Label:          "GitHub (@octocat)",
		EncryptedValue: "c2VhbGVk",
		Fingerprint:    "...0000",
		Login:          "octocat",
		ExternalId:     "583231",
	}); err != nil {
		t.Fatalf("UpsertGithubAppGrant: %v", err)
	}
	q := rec.statement(t, "mutation createGithubAppGrant(")
	if _, err := langparser.ParseExpression(q); err != nil {
		t.Fatalf("an empty installation list does not parse:\n  %s\n  %v", q, err)
	}
	return q
}
