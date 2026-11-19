package calls

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	callrules "github.com/znasllc-io/memql/component/calls"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/common"
)

// operator_db_test.go -- the media bootstrap and the operator acts, against a real
// engine and a real database.
//
// WHY A REAL ENGINE. Every claim below is about what a row holds afterwards and who
// the engine took each read and write to be: the bootstrap row and the three sealed
// secrets a second boot leaves alone, a key's row holding its digest and never the
// key, the capability and organization gates a person's call crosses on the way to
// a handler, and the @serverWritten and @serverOnly gates the store's own stamps
// open. A fake engine has none of those, so it could not fail for the reason these
// tests exist.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the
// skip into a failure.

// testMasterKey seals and opens every secret this package's tests write. ONE key for
// the whole package and every run of it: a secret sealed under one key and read under
// another is undecryptable, which the bootstrap refuses to paper over for the pepper.
const testMasterKey = "8f1d1e2c3b4a59687f6e5d4c3b2a19087f6e5d4c3b2a19081726354453627180"

// lockedBuffer is a log sink a test can read while the integration writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sharedBoot is one engine per test process: a boot costs seconds.
var sharedBoot struct {
	once    sync.Once
	dsn     string
	eng     *memql.MemQLEngine
	db      *bun.DB
	gates   *sql.DB
	in      *Integration
	logs    *lockedBuffer
	pingErr error
	bootErr error
}

// liveEngine is the narrow Engine this package asks for, over the real one.
type liveEngine struct{ e *memql.MemQLEngine }

func (l liveEngine) Execute(ctx context.Context, q string) (any, error) { return l.e.Execute(ctx, q) }

func bootEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB, *Integration) {
	t.Helper()
	s := &sharedBoot
	s.once.Do(func() {
		s.dsn = dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))), pgdialect.New())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("memql.New: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("engine Init: %w", err)
			return
		}
		gates, err := database.NewGatePool(s.dsn, 0)
		if err != nil {
			s.bootErr = fmt.Errorf("gate pool: %w", err)
			return
		}
		s.logs = &lockedBuffer{}
		// THE REAL HANDLERS BEHIND THE REAL DISPATCH, so a test that goes through
		// `builtin calls...` meets the capability gate and the shared organization
		// gate on the way to them, as a person's call does.
		s.in = New(Deps{
			Engine:              liveEngine{eng},
			GateDB:              func() *sql.DB { return gates },
			ResolveSystemSecret: eng.ResolveSystemSecret,
			Logger:              slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		if err := eng.RegisterIntegration(s.in); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			return
		}
		s.eng, s.db, s.gates = eng, db, gates
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "calls integration", s.dsn, s.pingErr)
		return nil, nil, nil
	}
	require.NoError(t, s.bootErr, "the shared engine failed to boot")
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	return s.eng, s.db, s.in
}

// secondReplica is another node's integration over the same database: its own
// instance, sharing only Postgres and the gate pool's server, as a second replica
// booting beside the first does.
func secondReplica(t *testing.T, eng *memql.MemQLEngine) *Integration {
	t.Helper()
	gates, err := database.NewGatePool(sharedBoot.dsn, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gates.Close() })
	return New(Deps{
		Engine:              liveEngine{eng},
		GateDB:              func() *sql.DB { return gates },
		ResolveSystemSecret: eng.ResolveSystemSecret,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func testMediaConfig() MediaConfig {
	return MediaConfig{
		Provider: "docker-local", APIURL: "http://host.k3d.internal:7880", PublicURL: "wss://media.memql.localhost:7443",
		Seats: 50, APIKey: "APIroomkey", APISecret: testAPISecret, WebhookKey: "APIwebhookkey", WebhookSecret: testWebhookSecret,
	}
}

var bootstrapSecretNames = []string{SecretMediaBootstrap, SecretMediaWebhook, SecretKeyPepper}

// resetBootstrap takes the bootstrap row and its three secrets away, now and when the
// test ends, so a test observes a FIRST boot whatever an earlier run left behind.
func resetBootstrap(t *testing.T, db *bun.DB) {
	t.Helper()
	wipe := func() {
		ctx := context.Background()
		_, err := db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", mediaServerConcept).Where("id = ?", mediaServerConcept+":"+BootstrapMediaServerID).Exec(ctx)
		require.NoError(t, err)
		_, err = db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept = ?", globalSecretConcept).Where("payload->>'name' IN (?)", bun.In(bootstrapSecretNames)).Exec(ctx)
		require.NoError(t, err)
	}
	wipe()
	t.Cleanup(wipe)
}

// rowState is how many versions a row has and when the newest was written: what a
// write that "changes nothing" must leave as it found.
type rowState struct {
	Versions int
	Ids      int
	Newest   time.Time
}

func mediaServerState(t *testing.T, db *bun.DB) rowState {
	t.Helper()
	return stateWhere(t, db, `concept = ? AND id = ?`, mediaServerConcept, mediaServerConcept+":"+BootstrapMediaServerID)
}

func secretState(t *testing.T, db *bun.DB, name string) rowState {
	t.Helper()
	return stateWhere(t, db, `concept = ? AND payload->>'name' = ?`, globalSecretConcept, name)
}

func stateWhere(t *testing.T, db *bun.DB, where string, args ...any) rowState {
	t.Helper()
	var out struct {
		Versions int          `bun:"versions"`
		Ids      int          `bun:"ids"`
		Newest   sql.NullTime `bun:"newest"`
	}
	err := db.NewRaw(`SELECT count(*) AS versions, count(DISTINCT id) AS ids, max("createdAt") AS newest FROM "MemoryNodes" WHERE `+where, args...).
		Scan(context.Background(), &out)
	require.NoError(t, err)
	return rowState{Versions: out.Versions, Ids: out.Ids, Newest: out.Newest.Time.UTC()}
}

// latestPayload is the newest stored version of a row, straight off the table: the
// question is what was STORED, which no shape would answer.
func latestPayload(t *testing.T, db *bun.DB, concept, id string) (map[string]any, memorynodes.MemoryNode) {
	t.Helper()
	var node memorynodes.MemoryNode
	err := db.NewSelect().Model(&node).Where("concept = ?", concept).Where("id = ?", id).
		Order("createdAt DESC").Limit(1).Scan(context.Background())
	require.NoError(t, err, "reading back %s", id)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(node.Payload, &payload))
	return payload, node
}

// resolveFresh reads a sealed secret the way the integration does: through the
// engine's resolver, answered by the database rather than a cache.
func resolveFresh(t *testing.T, eng *memql.MemQLEngine, name string) string {
	t.Helper()
	value, err := eng.ResolveSystemSecret(memql.ContextWithFreshRead(context.Background()), name)
	require.NoError(t, err, "the secret %s does not resolve", name)
	return value
}

// TestBootstrapUpsertsOneMediaServerAndSealsItsSecrets: a node whose environment
// names a media server records it as the one row v1:calls:mediaServer:bootstrap,
// seals the API and webhook credentials and generates the key pepper -- and a second
// boot with the same environment WRITES NOTHING: no new version of any of the four
// rows, and the pepper every key digest depends on is the one the first boot made.
func TestBootstrapUpsertsOneMediaServerAndSealsItsSecrets(t *testing.T) {
	eng, db, in := bootEngine(t)
	if eng == nil {
		return
	}
	resetBootstrap(t, db)
	cfg := testMediaConfig()

	first, err := in.Bootstrap(context.Background(), cfg)
	require.NoError(t, err)
	require.True(t, first.ServerCreated, "the first boot did not create the media server")

	row, node := latestPayload(t, db, mediaServerConcept, mediaServerConcept+":"+BootstrapMediaServerID)
	require.Equal(t, "docker-local", row["provider"], "the provider is recorded as the environment gave it")
	require.Equal(t, cfg.APIURL, row["apiUrl"])
	require.Equal(t, cfg.PublicURL, row["publicUrl"])
	require.EqualValues(t, 50, row["seats"])
	require.Equal(t, "active", row["state"])
	require.Equal(t, SecretMediaBootstrap, row["credentialSecret"], "the row NAMES its credential")
	stored, _ := json.Marshal(row)
	requireNoSecretIn(t, string(stored)+string(node.Metadata)+string(node.Provenance))

	api, err := ParseMediaCredential(resolveFresh(t, eng, SecretMediaBootstrap))
	require.NoError(t, err)
	require.Equal(t, MediaCredential{Key: cfg.APIKey, Secret: cfg.APISecret}, api)
	webhook, err := ParseMediaCredential(resolveFresh(t, eng, SecretMediaWebhook))
	require.NoError(t, err)
	require.Equal(t, MediaCredential{Key: cfg.WebhookKey, Secret: cfg.WebhookSecret}, webhook)
	pepper := resolveFresh(t, eng, SecretKeyPepper)
	raw, err := hex.DecodeString(pepper)
	require.NoError(t, err, "the pepper is not hex")
	require.Len(t, raw, 32, "the pepper is not 32 bytes")

	before := map[string]rowState{"server": mediaServerState(t, db)}
	for _, name := range bootstrapSecretNames {
		before[name] = secretState(t, db, name)
		require.Equal(t, 1, before[name].Ids, "%s is held by more than one row", name)
		require.Equal(t, 1, before[name].Versions, "the first boot wrote %s more than once", name)
	}
	require.Equal(t, 1, before["server"].Versions)

	second, err := in.Bootstrap(context.Background(), cfg)
	require.NoError(t, err)
	require.False(t, second.ServerCreated || second.ServerUpdated || len(second.SecretsWritten) > 0,
		"a second boot with the same environment reported a write: %+v", second)
	after := map[string]rowState{"server": mediaServerState(t, db)}
	for _, name := range bootstrapSecretNames {
		after[name] = secretState(t, db, name)
	}
	require.Equal(t, before, after, "a second boot with the same environment changed a row")
	require.Equal(t, pepper, resolveFresh(t, eng, SecretKeyPepper), "the pepper was regenerated")
}

// TestBootstrapIsOneAcrossReplicasBootingAtOnce: two replicas whose environments name
// the same server boot together. The write gate and the fresh read under it leave ONE
// media server version and ONE pepper, never two peppers each half the keys depend on.
func TestBootstrapIsOneAcrossReplicasBootingAtOnce(t *testing.T) {
	eng, db, in := bootEngine(t)
	if eng == nil {
		return
	}
	resetBootstrap(t, db)
	replicas := []*Integration{in, secondReplica(t, eng)}

	start := make(chan struct{})
	errs := make([]error, len(replicas))
	var wg sync.WaitGroup
	for i, replica := range replicas {
		wg.Add(1)
		go func(i int, replica *Integration) {
			defer wg.Done()
			<-start
			_, errs[i] = replica.Bootstrap(context.Background(), testMediaConfig())
		}(i, replica)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "replica %d failed to boot", i)
	}

	require.Equal(t, 1, mediaServerState(t, db).Versions, "two replicas each wrote the media server")
	for _, name := range bootstrapSecretNames {
		state := secretState(t, db, name)
		require.Equal(t, 1, state.Ids, "%s is held by more than one row", name)
		require.Equal(t, 1, state.Versions, "two replicas each wrote %s", name)
	}
}

// TestBootstrapFollowsAChangedEnvironment: the environment is the truth for the
// server it names. A changed address, size or credential is written as one new
// version; what did not change is left alone, and the pepper above all.
func TestBootstrapFollowsAChangedEnvironment(t *testing.T) {
	eng, db, in := bootEngine(t)
	if eng == nil {
		return
	}
	resetBootstrap(t, db)
	cfg := testMediaConfig()
	_, err := in.Bootstrap(context.Background(), cfg)
	require.NoError(t, err)
	pepper := resolveFresh(t, eng, SecretKeyPepper)
	webhookBefore := secretState(t, db, SecretMediaWebhook)

	cfg.PublicURL = "wss://m1.media.example.test"
	cfg.Seats = 80
	cfg.Provider = "hetzner"
	cfg.APISecret = "rotated-api-secret-0123456789abcdefghijkl"
	result, err := in.Bootstrap(context.Background(), cfg)
	require.NoError(t, err)
	require.True(t, result.ServerUpdated)
	require.Equal(t, []string{SecretMediaBootstrap}, result.SecretsWritten)

	row, _ := latestPayload(t, db, mediaServerConcept, mediaServerConcept+":"+BootstrapMediaServerID)
	require.Equal(t, "wss://m1.media.example.test", row["publicUrl"])
	require.EqualValues(t, 80, row["seats"])
	require.Equal(t, "hetzner", row["provider"])
	require.Equal(t, 2, mediaServerState(t, db).Versions)

	api, err := ParseMediaCredential(resolveFresh(t, eng, SecretMediaBootstrap))
	require.NoError(t, err)
	require.Equal(t, cfg.APISecret, api.Secret)
	require.Equal(t, 2, secretState(t, db, SecretMediaBootstrap).Versions)
	require.Equal(t, webhookBefore, secretState(t, db, SecretMediaWebhook), "the unchanged webhook credential was written again")
	require.Equal(t, pepper, resolveFresh(t, eng, SecretKeyPepper), "the pepper was regenerated")
	require.Equal(t, 1, secretState(t, db, SecretKeyPepper).Versions)
}

// ---------------------------------------------------------------------------
// the world an operator acts in
// ---------------------------------------------------------------------------

// world is one test's deployable and the people around it, unique per test so a
// rerun against the same database collides with nothing a previous one left.
//
//	owner      a writer, a member of the account, and the site's owner
//	developer  holds the calls part by role, and owns nothing here
//	operator   a cluster owner, who owns nothing here
//	stranger   a writer in no group, who holds no part
type world struct {
	t   *testing.T
	eng *memql.MemQLEngine
	db  *bun.DB
	in  *Integration

	suffix    string
	owner     string
	developer string
	operator  string
	stranger  string
	account   string
	group     string
	site      string
	second    string // a second site of the same organization, owned by owner
	lone      string // a site tied to no organization, owned by owner
	unowned   string // a site with no owner at all

	rows  []string // canonical ids this test wrote and has to take away
	sites []string // every site this test wrote, whose calls rows go with it
}

func actorContext(userID string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

// seedContext is the scaffolding actor, the cluster acting on its own: internal origin
// because the writers are @serverOnly, Synthetic because the rows it writes belong to
// nobody.
func seedContext() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "calls-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "calls-seeder"})
}

func newWorld(t *testing.T, name string) *world {
	t.Helper()
	eng, db, in := bootEngine(t)
	if eng == nil {
		return nil
	}
	now := time.Now()
	s := fmt.Sprintf("%s-%d-%d", name, now.Unix(), now.Nanosecond())
	w := &world{
		t: t, eng: eng, db: db, in: in, suffix: s,
		owner: "cl-owner-" + s, developer: "cl-developer-" + s, operator: "cl-operator-" + s, stranger: "cl-stranger-" + s,
		account: "cl-acct-" + s, group: "cl-group-" + s,
		site: "cl-site-" + s, second: "cl-second-" + s, lone: "cl-lone-" + s, unowned: "cl-unowned-" + s,
	}
	t.Cleanup(w.sweep)
	w.principal(w.owner, auth.RoleWriter)
	w.principal(w.developer, auth.RoleDeveloper)
	w.principal(w.operator, auth.RoleOwner)
	w.principal(w.stranger, auth.RoleWriter)
	w.organization(w.account, w.group)
	w.join(w.group, w.owner)
	w.siteRow(w.site, nil)
	w.siteRow(w.second, nil)
	w.siteRow(w.lone, map[string]any{"accountId": nil})
	w.siteRow(w.unowned, map[string]any{"ownerUserId": nil})
	return w
}

// sweep takes away what this test wrote: the shared lane's table is not a place to
// leave rows behind. A site's keys and limits are found by their site, since the
// handlers mint their ids.
func (w *world) sweep() {
	ctx := context.Background()
	for _, id := range w.rows {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(ctx)
	}
	for _, site := range w.sites {
		_, _ = w.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept IN (?, ?)", keyConcept, policyConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(ctx)
	}
}

// installGrants turns on the engine's grant resolution for this test, so a grant
// row is what decides, and puts back whatever answered before when it ends.
func (w *world) installGrants() {
	w.t.Helper()
	grants, members := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.eng.InstallGrantResolution()
	w.t.Cleanup(func() { auth.SetGrantSource(grants); auth.SetMembershipSource(members) })
}

// grantCallsPart grants `execute app:deployables/calls` to a group: the part held
// through an organization, which is the only way a writer comes to hold it.
func (w *world) grantCallsPart(group string) {
	w.t.Helper()
	id := auth.GrantRowID(auth.SubjectKindGroup, group, auth.VerbExecute, "app:deployables/calls")
	w.mutate(seedContext(), "writeGrant", map[string]any{
		"grantId": id, "subjectKind": auth.SubjectKindGroup, "subjectId": group,
		"verb": auth.VerbExecute, "resourceType": "app:deployables/calls", "effect": auth.GrantAllow, "grantedBy": "calls-seeder",
	})
	w.rows = append(w.rows, "v1:rbac:grant:"+id)
}

func (w *world) insert(concept, id string, payload map[string]any) {
	w.t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(w.t, err)
	_, err = w.eng.Execute(seedContext(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(id), encoded))
	require.NoError(w.t, err, "seeding %s %s", concept, id)
	w.rows = append(w.rows, concept+":"+memql.BareShortId(id))
}

func (w *world) principal(id string, role auth.Role) {
	w.t.Helper()
	w.insert("v1:identity:user", id, map[string]any{
		"displayName": id, "primaryEmail": id + "@example.test", "role": string(role), "active": true,
	})
}

func (w *world) organization(account, group string) {
	w.t.Helper()
	w.insert("v1:accounts:account", account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	w.mutate(seedContext(), "writeGroup", map[string]any{
		"groupId": group, "name": group, "kind": "account", "accountId": account, "status": "active",
	})
	w.rows = append(w.rows, "v1:identity:group:"+group)
}

func (w *world) join(group, user string) {
	w.t.Helper()
	parent, err := w.eng.Execute(seedContext(), `query groupById(groupId: `+langparser.QuoteString(group)+`)`)
	require.NoError(w.t, err)
	rows := memql.MaterializeRows(parent)
	require.Len(w.t, rows, 1, "joining a group that is not there")
	membership := group + "-" + user
	w.mutate(seedContext(), "writeGroupMembership", map[string]any{
		"membershipId": membership, "groupId": group, "userId": user, "origin": "added", "status": "active",
		"accountId": rows[0]["accountId"],
	})
	w.rows = append(w.rows, "v1:identity:groupMembership:"+membership)
}

// siteRow writes a site. A RAW INSERT rather than createSite, because the fixtures
// are sites the flow would not make (no owner, no organization). A nil value in
// `over` takes the field away.
func (w *world) siteRow(id string, over map[string]any) {
	w.t.Helper()
	payload := map[string]any{
		"hostname": id + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://sites/" + id + "/v1/",
		"ownerUserId": "v1:identity:user:" + w.owner, "accountId": w.account, "packageDeployableName": "desk-" + w.suffix,
	}
	for key, value := range over {
		if value == nil {
			delete(payload, key)
		} else {
			payload[key] = value
		}
	}
	w.insert(siteConcept, id, payload)
	w.sites = append(w.sites, id)
}

func (w *world) mutate(ctx context.Context, name string, args map[string]any) {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	_, err = w.eng.Execute(ctx, "mutation "+call)
	require.NoError(w.t, err, "mutation %s", name)
}

// builtin is a call as a person's stream makes it: through the engine's dispatch,
// with whatever gates that crosses.
func (w *world) builtin(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	w.t.Helper()
	call, err := langparser.RenderCall(name, args)
	require.NoError(w.t, err)
	res, err := w.eng.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

func (w *world) bootstrap() {
	w.t.Helper()
	_, err := w.in.Bootstrap(context.Background(), testMediaConfig())
	require.NoError(w.t, err)
}

func (w *world) pepper() []byte {
	w.t.Helper()
	raw, err := hex.DecodeString(resolveFresh(w.t, w.eng, SecretKeyPepper))
	require.NoError(w.t, err)
	return raw
}

// rowsContaining counts the stored rows, of any concept, whose payload, metadata or
// provenance carry the text anywhere.
func (w *world) rowsContaining(text string) int {
	w.t.Helper()
	n, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).
		Where(`payload::text LIKE ? OR metadata::text LIKE ? OR provenance::text LIKE ?`, "%"+text+"%", "%"+text+"%", "%"+text+"%").
		Count(context.Background())
	require.NoError(w.t, err)
	return int(n)
}

func bare(v any) string { return memql.BareShortId(fmt.Sprint(v)) }

// ---------------------------------------------------------------------------
// a test key
// ---------------------------------------------------------------------------

// TestTestKeyIsShownOnceAndStoredAsDigest: a person holding the calls part mints a
// test key through the real dispatch. The reply is the ONE place the key exists: the
// row holds the digest of its sixteen raw bytes under the pepper, the site's owner and
// the test kind, and no stored row and no log line anywhere carries the key.
func TestTestKeyIsShownOnceAndStoredAsDigest(t *testing.T) {
	w := newWorld(t, "shown")
	if w == nil {
		return
	}
	resetBootstrap(t, w.db)
	w.bootstrap()

	rows, err := w.builtin(actorContext(w.developer, auth.RoleDeveloper), "callsTestKeyCreate",
		map[string]any{"siteId": w.site, "label": "Reviewers"})
	require.NoError(t, err, "a developer, who holds the calls part, was refused")
	require.Len(t, rows, 1)
	reply, _ := rows[0]["payload"].(map[string]any)
	require.ElementsMatch(t, []string{"keyId", "key"}, mapKeys(reply), "the reply carries more than the key and its id")
	keyID, _ := reply["keyId"].(string)
	key, _ := reply["key"].(string)
	require.NotEmpty(t, keyID)
	require.True(t, strings.HasPrefix(key, callrules.KeyPrefix), "the key is not spelled as a key: %q", key)
	raw, err := callrules.ParseKey(key)
	require.NoError(t, err, "the key does not parse back")

	stored, node := latestPayload(t, w.db, keyConcept, keyConcept+":"+keyID)
	digest := callrules.Digest(w.pepper(), raw[:])
	require.Equal(t, digest, stored["digest"], "the row does not hold the digest of the key's raw bytes under the pepper")
	require.Equal(t, w.site, bare(stored["siteId"]))
	require.Equal(t, w.owner, bare(stored["ownerUserId"]), "the key belongs to the site's owner, whoever asked")
	require.Equal(t, "test", stored["kind"])
	require.Equal(t, "active", stored["status"])
	require.Equal(t, "Reviewers", stored["label"])
	require.NotContains(t, string(node.Payload)+string(node.Metadata)+string(node.Provenance), key)

	// NOWHERE ELSE: no row of any concept, in any column, holds the key, its body or
	// its raw bytes, and neither does anything the integration logged.
	for _, needle := range []string{key, strings.TrimPrefix(key, callrules.KeyPrefix), hex.EncodeToString(raw[:])} {
		require.Zero(t, w.rowsContaining(needle), "a stored row carries the key")
		require.NotContains(t, sharedBoot.logs.String(), needle, "a log line carries the key")
	}

	// The digest finds the key on its own site and on no other.
	found, err := w.in.store.KeyByDigest(context.Background(), w.owner, w.site, digest)
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, keyID, found.ID)
	elsewhere, err := w.in.store.KeyByDigest(context.Background(), w.owner, w.lone, digest)
	require.NoError(t, err)
	require.Nil(t, elsewhere, "a key minted for one deployable resolved at another")

	// Every key is a key of its own.
	again, err := w.builtin(actorContext(w.developer, auth.RoleDeveloper), "callsTestKeyCreate", map[string]any{"siteId": w.site})
	require.NoError(t, err)
	againReply, _ := again[0]["payload"].(map[string]any)
	require.NotEqual(t, key, againReply["key"])
	require.NotEqual(t, keyID, againReply["keyId"])
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTestKeyCreateRefusesBeforeItMints: every refusal is a sentence a person can act
// on, and none of them wrote a key.
func TestTestKeyCreateRefusesBeforeItMints(t *testing.T) {
	w := newWorld(t, "refuses")
	if w == nil {
		return
	}
	developer := actorContext(w.developer, auth.RoleDeveloper)
	keysOn := func(site string) int {
		n, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).Where("concept = ?", keyConcept).
			Where("payload->>'siteId' = ?", siteConcept+":"+site).Count(context.Background())
		require.NoError(t, err)
		return int(n)
	}

	t.Run("a person without the calls part is refused at the dispatch", func(t *testing.T) {
		resetBootstrap(t, w.db)
		w.bootstrap()
		_, err := w.builtin(actorContext(w.owner, auth.RoleWriter), "callsTestKeyCreate", map[string]any{"siteId": w.site})
		require.Error(t, err)
		require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld)
	})

	t.Run("a cluster with no media server has no pepper and mints nothing", func(t *testing.T) {
		resetBootstrap(t, w.db)
		_, err := w.builtin(developer, "callsTestKeyCreate", map[string]any{"siteId": w.site})
		requireSentence(t, err)
		require.Contains(t, err.Error(), "not set up")
	})

	resetBootstrap(t, w.db)
	w.bootstrap()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		args map[string]any
		want string
	}{
		{"inside a run", common.ContextWithRun(developer, common.RunContext{RunId: "run-" + w.suffix}), map[string]any{"siteId": w.site}, "run"},
		{"a deployable the person cannot read", developer, map[string]any{"siteId": w.lone}, "No deployable you can read"},
		{"a deployable with no owner", developer, map[string]any{"siteId": w.unowned}, "no owner"},
		{"a name over 80 characters", developer, map[string]any{"siteId": w.site, "label": strings.Repeat("x", 81)}, "80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.builtin(tc.ctx, "callsTestKeyCreate", tc.args)
			msg := requireSentence(t, err)
			require.Contains(t, msg, tc.want)
		})
	}
	require.Zero(t, keysOn(w.site)+keysOn(w.lone)+keysOn(w.unowned), "a refused mint wrote a key")
}

// requireSentence holds a refusal to what the OS shows it as: verbatim, so it opens
// with a capital, closes like a sentence and carries no frame a person cannot act on.
func requireSentence(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	require.NotEmpty(t, msg)
	require.True(t, strings.ToUpper(msg[:1]) == msg[:1], "a refusal the OS shows verbatim opens in lower case: %q", msg)
	require.True(t, strings.HasSuffix(msg, "."), "a refusal the OS shows verbatim does not read as a sentence: %q", msg)
	for _, frame := range []string{"integrations/calls", "calls:", "row version", "not implemented"} {
		require.NotContains(t, msg, frame, "a refusal carries a frame a person cannot act on: %q", msg)
	}
	return msg
}

// ---------------------------------------------------------------------------
// the policy
// ---------------------------------------------------------------------------

// policyRowOf is the newest stored version of a site's limits, or nil when the site
// has none: found by the site, since a policy is keyed by its site.
func (w *world) policyRowOf(site string) map[string]any {
	w.t.Helper()
	var nodes []memorynodes.MemoryNode
	err := w.db.NewSelect().Model(&nodes).Where("concept = ?", policyConcept).
		Where("payload->>'siteId' = ?", siteConcept+":"+site).Order("createdAt DESC").Limit(1).Scan(context.Background())
	require.NoError(w.t, err)
	if len(nodes) == 0 {
		return nil
	}
	var payload map[string]any
	require.NoError(w.t, json.Unmarshal(nodes[0].Payload, &payload))
	return payload
}

// TestPolicySetStoresSecondsAndReadsBackDurations: the limits of one site are stored as
// whole numbers, durations in SECONDS, on a row the site's owner owns, and read back as
// the rules module's Policy with the shipped defaults under every value nobody set. A
// later call changes only what it names, on the same row.
func TestPolicySetStoresSecondsAndReadsBackDurations(t *testing.T) {
	w := newWorld(t, "policy")
	if w == nil {
		return
	}
	developer := actorContext(w.developer, auth.RoleDeveloper)

	rows, err := w.builtin(developer, "callsPolicySet", map[string]any{"siteId": w.site, "maxPeople": 4, "passTtl": 60})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	reply, _ := rows[0]["payload"].(map[string]any)
	require.Equal(t, w.site, reply["siteId"])
	require.EqualValues(t, 4, reply["maxPeople"])
	require.EqualValues(t, 60, reply["passTtl"])
	require.EqualValues(t, 2, reply["maxAgents"], "an unset value answers the shipped default")
	require.EqualValues(t, 300, reply["hostLeftGrace"], "an unset duration answers the shipped default, in seconds")

	stored := w.policyRowOf(w.site)
	require.NotNil(t, stored, "no limits were stored for the site")
	require.EqualValues(t, 4, stored["maxPeople"])
	require.EqualValues(t, 60, stored["passTtl"], "a duration is stored in whole seconds")
	require.Equal(t, w.owner, bare(stored["ownerUserId"]), "the limits belong to the site's owner, whoever set them")
	_, defaulted := stored["maxAgents"]
	require.False(t, defaulted, "a value nobody set was written as if somebody had")

	effective, err := w.in.store.PolicyFor(context.Background(), w.owner, w.site)
	require.NoError(t, err)
	want := callrules.DefaultPolicy()
	want.MaxPeople = 4
	want.PassTTL = 60 * time.Second
	require.Equal(t, want, effective)

	_, err = w.builtin(developer, "callsPolicySet", map[string]any{"siteId": w.site, "maxAgents": 0, "queuePoll": 5})
	require.NoError(t, err)
	effective, err = w.in.store.PolicyFor(context.Background(), w.owner, w.site)
	require.NoError(t, err)
	want.MaxAgents = 0
	want.QueuePoll = 5 * time.Second
	require.Equal(t, want, effective, "a later call lost a value it did not name")
	require.Equal(t, 1, stateWhere(t, w.db, `concept = ? AND payload->>'siteId' = ?`, policyConcept, siteConcept+":"+w.site).Ids,
		"a site's limits are spread over more than one row")

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no people at all", map[string]any{"maxPeople": 0}, "maxPeople"},
		{"more people than the cluster allows", map[string]any{"maxPeople": 1001}, "maxPeople"},
		{"a pass too short to connect with", map[string]any{"passTtl": 5}, "passTtl"},
		{"a pass longer than the ceiling", map[string]any{"passTtl": 901}, "passTtl"},
		{"a waiter that expires before it can poll", map[string]any{"queueEntryTtl": 10, "queuePoll": 10}, "queueEntryTtl"},
		{"a value that is not a whole number", map[string]any{"maxPeople": 2.5}, "maxPeople"},
		{"no change at all", map[string]any{}, "Name at least one limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"siteId": w.site}
			for k, v := range tc.args {
				args[k] = v
			}
			_, err := w.builtin(developer, "callsPolicySet", args)
			msg := requireSentence(t, err)
			require.Contains(t, msg, tc.want)
		})
	}

	_, err = w.builtin(actorContext(w.owner, auth.RoleWriter), "callsPolicySet", map[string]any{"siteId": w.site, "maxPeople": 3})
	require.Error(t, err)
	require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld, "a person without the calls part set a limit")
	inRun := common.ContextWithRun(developer, common.RunContext{RunId: "run-" + w.suffix})
	_, err = w.builtin(inRun, "callsPolicySet", map[string]any{"siteId": w.site, "maxPeople": 3})
	requireSentence(t, err)
	_, err = w.builtin(developer, "callsPolicySet", map[string]any{"siteId": w.lone, "maxPeople": 3})
	require.Contains(t, requireSentence(t, err), "No deployable you can read", "a site the caller cannot read was given limits")
	require.Nil(t, w.policyRowOf(w.lone), "a site the caller cannot read was given limits")

	effective, err = w.in.store.PolicyFor(context.Background(), w.owner, w.site)
	require.NoError(t, err)
	require.Equal(t, want, effective, "a refused change was written")
}

// TestPolicyIsPerSite: two deployables keep independent limits, here two of one
// organization whose package deployable has the same name. A policy is keyed by its
// SITE, because a package deployable's name is unique only together with its package.
func TestPolicyIsPerSite(t *testing.T) {
	w := newWorld(t, "persite")
	if w == nil {
		return
	}
	developer := actorContext(w.developer, auth.RoleDeveloper)

	_, err := w.builtin(developer, "callsPolicySet", map[string]any{"siteId": w.site, "maxPeople": 3})
	require.NoError(t, err)
	_, err = w.builtin(developer, "callsPolicySet", map[string]any{"siteId": w.second, "maxAgents": 0, "hostLeftGrace": 60})
	require.NoError(t, err)

	first, err := w.in.store.PolicyFor(context.Background(), w.owner, w.site)
	require.NoError(t, err)
	second, err := w.in.store.PolicyFor(context.Background(), w.owner, w.second)
	require.NoError(t, err)
	wantFirst, wantSecond := callrules.DefaultPolicy(), callrules.DefaultPolicy()
	wantFirst.MaxPeople = 3
	wantSecond.MaxAgents, wantSecond.HostLeftGrace = 0, time.Minute
	require.Equal(t, wantFirst, first, "the first site's limits took the second's")
	require.Equal(t, wantSecond, second, "the second site's limits took the first's")

	untouched, err := w.in.store.PolicyFor(context.Background(), w.owner, w.lone)
	require.NoError(t, err)
	require.Equal(t, callrules.DefaultPolicy(), untouched, "a site nobody set limits for does not have the defaults")
	firstRow, secondRow := w.policyRowOf(w.site), w.policyRowOf(w.second)
	require.NotNil(t, firstRow)
	require.NotNil(t, secondRow)
	require.NotEqual(t, firstRow["siteId"], secondRow["siteId"])
}

// ---------------------------------------------------------------------------
// the part, held through one organization
// ---------------------------------------------------------------------------

// TestTheCallsPartHeldInOneOrganizationActsOnNoOther: a person who holds
// `execute app:deployables/calls` ONLY through organization A's group, and owns a
// deployable in A and another in B, may mint a key, switch calls and set limits on A's
// and is refused all three on B's -- by the organization gate, since owning B's site
// lets them read it. Nothing is written on B's.
func TestTheCallsPartHeldInOneOrganizationActsOnNoOther(t *testing.T) {
	w := newWorld(t, "orgpart")
	if w == nil {
		return
	}
	resetBootstrap(t, w.db)
	w.bootstrap()
	w.installGrants()
	otherAccount, otherGroup, otherSite := "cl-acct-b-"+w.suffix, "cl-group-b-"+w.suffix, "cl-site-b-"+w.suffix
	w.organization(otherAccount, otherGroup)
	w.siteRow(otherSite, map[string]any{"accountId": otherAccount})
	// The owner is a writer, a member of A's group and of no group of B's; the part
	// is granted to A's group and to nobody else.
	w.grantCallsPart(w.group)
	person := actorContext(w.owner, auth.RoleWriter)

	acts := []struct {
		builtin string
		args    func(site string) map[string]any
	}{
		{"callsTestKeyCreate", func(site string) map[string]any { return map[string]any{"siteId": site, "label": "From A"} }},
		{"callsSiteEnable", func(site string) map[string]any { return map[string]any{"siteId": site, "on": true} }},
		{"callsPolicySet", func(site string) map[string]any { return map[string]any{"siteId": site, "maxPeople": 2} }},
	}
	for _, act := range acts {
		t.Run(act.builtin+" on the other organization's deployable", func(t *testing.T) {
			_, err := w.builtin(person, act.builtin, act.args(otherSite))
			require.Error(t, err, "the part held in A acted on B's deployable")
			require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld)
		})
	}
	keys, err := w.db.NewSelect().TableExpr(`"MemoryNodes"`).Where("concept = ?", keyConcept).
		Where("payload->>'siteId' = ?", siteConcept+":"+otherSite).Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, keys, "a key was minted on B's deployable")
	require.Nil(t, w.policyRowOf(otherSite), "limits were set on B's deployable")
	stored, _ := latestPayload(t, w.db, siteConcept, siteConcept+":"+otherSite)
	require.NotEqual(t, true, stored["calls"], "calls were switched on for B's deployable")

	for _, act := range acts {
		t.Run(act.builtin+" on the person's own organization's deployable", func(t *testing.T) {
			_, err := w.builtin(person, act.builtin, act.args(w.site))
			require.NoError(t, err, "the part held in A was refused on A's deployable")
		})
	}
	require.NotNil(t, w.policyRowOf(w.site))
	stored, _ = latestPayload(t, w.db, siteConcept, siteConcept+":"+w.site)
	require.Equal(t, true, stored["calls"])
}

// ---------------------------------------------------------------------------
// the media servers
// ---------------------------------------------------------------------------

// TestMediaServersAreTheClusterOwnersAndShowNoCredential: the media servers are the
// cluster's, so only a cluster owner lists them; a person holding the calls part, by
// role or through an organization, is refused. The list says which servers rooms are
// placed on and where browsers reach them, and never a server's control address,
// which secret holds its credentials, or the credentials.
func TestMediaServersAreTheClusterOwnersAndShowNoCredential(t *testing.T) {
	w := newWorld(t, "servers")
	if w == nil {
		return
	}
	resetBootstrap(t, w.db)
	w.bootstrap()
	w.installGrants()
	w.grantCallsPart(w.group)

	for name, ctx := range map[string]context.Context{
		"a developer, who holds the part by role":           actorContext(w.developer, auth.RoleDeveloper),
		"a writer holding the part through an organization": actorContext(w.owner, auth.RoleWriter),
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := w.builtin(ctx, "callsMediaServers", map[string]any{})
			require.Nil(t, rows, "%s listed the media servers", name)
			require.Contains(t, requireSentence(t, err), "cluster owner")
		})
	}
	_, err := w.builtin(actorContext(w.stranger, auth.RoleWriter), "callsMediaServers", map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld, "a person without the calls part reached the handler")

	rows, err := w.builtin(actorContext(w.operator, auth.RoleOwner), "callsMediaServers", map[string]any{})
	require.NoError(t, err, "the cluster owner was refused the media servers")
	var server map[string]any
	for _, row := range rows {
		if payload, _ := row["payload"].(map[string]any); bare(payload["id"]) == BootstrapMediaServerID {
			server = payload
		}
	}
	require.NotNil(t, server, "the bootstrap server is not listed: %v", rows)
	require.Equal(t, "docker-local", server["provider"])
	require.Equal(t, "wss://media.memql.localhost:7443", server["publicUrl"])
	require.EqualValues(t, 50, server["seats"])
	require.Equal(t, "active", server["state"])
	for _, withheld := range []string{"apiUrl", "credentialSecret"} {
		_, present := server[withheld]
		require.False(t, present, "the list carries %s", withheld)
	}
	listed, _ := json.Marshal(rows)
	require.NotContains(t, string(listed), SecretMediaBootstrap)
	require.NotContains(t, string(listed), testMediaConfig().APIURL, "the list carries the server's control address")
	requireNoSecretIn(t, string(listed))
}

// ---------------------------------------------------------------------------
// the site switch
// ---------------------------------------------------------------------------

// TestSiteEnableWritesTheSwitchAsThePerson: turning calls on and off is a write of the
// site's `calls` field AS THE PERSON who asked, so the engine judges it as any write
// of theirs: the part, the deployable's organization and the row's own tier.
func TestSiteEnableWritesTheSwitchAsThePerson(t *testing.T) {
	w := newWorld(t, "switch")
	if w == nil {
		return
	}
	developer := actorContext(w.developer, auth.RoleDeveloper)

	rows, err := w.builtin(developer, "callsSiteEnable", map[string]any{"siteId": w.site, "on": true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	reply, _ := rows[0]["payload"].(map[string]any)
	require.Equal(t, true, reply["calls"])
	stored, node := latestPayload(t, w.db, siteConcept, siteConcept+":"+w.site)
	require.Equal(t, true, stored["calls"])
	require.Equal(t, w.developer, bare(node.CreatedBy), "the switch was not written as the person who asked")

	_, err = w.builtin(developer, "callsSiteEnable", map[string]any{"siteId": w.site, "on": false})
	require.NoError(t, err)
	stored, _ = latestPayload(t, w.db, siteConcept, siteConcept+":"+w.site)
	require.Equal(t, false, stored["calls"])

	_, err = w.builtin(actorContext(w.owner, auth.RoleWriter), "callsSiteEnable", map[string]any{"siteId": w.site, "on": true})
	require.Error(t, err)
	require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld, "a person without the calls part turned calls on")

	inRun := common.ContextWithRun(developer, common.RunContext{RunId: "run-" + w.suffix})
	_, err = w.builtin(inRun, "callsSiteEnable", map[string]any{"siteId": w.site, "on": true})
	requireSentence(t, err)
	_, err = w.builtin(developer, "callsSiteEnable", map[string]any{"siteId": w.lone, "on": true})
	requireSentence(t, err)
	// A deployable no organization holds, switched by a cluster owner, who can read it: the
	// engine refuses a person's change to any existing record that has no organization
	// (organization_required, validateOrganizationOwnership), and the person is told THAT,
	// not that something "did not complete" -- the sentence the first live run of
	// TestTwoPeopleHoldACallThroughTheFrontDoor failed on, with nothing in the log to say why.
	operator := actorContext(w.operator, auth.RoleOwner)
	msg := requireSentence(t, mustFail(w.builtin(operator, "callsSiteEnable", map[string]any{"siteId": w.lone, "on": true})))
	require.Contains(t, msg, "no organization", "a deployable with no organization was refused without saying so: %q", msg)
	stored, _ = latestPayload(t, w.db, siteConcept, siteConcept+":"+w.lone)
	require.NotEqual(t, true, stored["calls"], "the switch was written to a deployable with no organization")
	stored, _ = latestPayload(t, w.db, siteConcept, siteConcept+":"+w.site)
	require.Equal(t, false, stored["calls"], "a refused switch was written")
}

// mustFail is the error of a call that returned rows and an error, for requireSentence.
func mustFail(_ []map[string]any, err error) error { return err }
