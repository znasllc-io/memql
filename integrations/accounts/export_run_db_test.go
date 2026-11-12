package accounts

// export_run_db_test.go -- what a run writes, against a real Postgres and the
// registry of the engine's own tree (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections B, D,
// E and F). Storage is an in-memory store behind the run's two storage
// interfaces; the snapshot is a real maintenance session.
//
// The tests own the database they run on (MEMQL_DATABASE_DSN): each one
// empties the row table first, so they run one after another (-p=1).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/accountexport"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/librarykey"
	"github.com/znasllc-io/memql/integrations/azureblob"

	// The two hand-overs a node registers, so the file holds their members.
	_ "github.com/znasllc-io/memql/component/campaigns"
	_ "github.com/znasllc-io/memql/component/sitetraffic"
)

// vaultConcept is a kind of the fixture's own with a @secret field: no kind
// that travels in the engine's tree may declare one (a build gate), so the
// removal of a secret value is shown on this one.
const vaultConcept = "v1:testexport:vault"

// vaultSchema marks a secret at the top level, in an object block, in an
// array of objects, in a map's values and in a union branch.
const vaultSchema = `{"properties":{
  "label":{"type":"string"},
  "token":{"type":"string","x-secret":true},
  "settings":{"type":"object","properties":{"region":{"type":"string"},"apiKey":{"type":"string","x-secret":true}}},
  "entries":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"pin":{"type":"string","x-secret":true}}}},
  "extra":{"type":"object","additionalProperties":{"type":"object","properties":{"code":{"type":"string","x-secret":true}}}},
  "choice":{"oneOf":[
    {"type":"object","properties":{"kind":{"const":"basic"},"password":{"type":"string","x-secret":true}}},
    {"type":"object","properties":{"kind":{"const":"link"},"url":{"type":"string"}}}]}
}}`

var registryOnce sync.Once

// loadRegistry loads the engine's own concepts once, and the fixture's vault.
func loadRegistry(t *testing.T) {
	t.Helper()
	var err error
	registryOnce.Do(func() {
		if _, err = memql.LoadUnifiedConcepts(nil); err != nil {
			return
		}
		memorynodes.MergeAll(map[string]*memorynodes.Concept{vaultConcept: {
			Name:      vaultConcept,
			BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount},
			Schemas:   map[string]json.RawMessage{"definition": json.RawMessage(vaultSchema)},
		}})
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// In-memory storage behind the run's two interfaces

type memStore struct {
	mu        sync.Mutex
	committed map[string][]byte
	aborted   []string
	deleted   []string
	created   []string
	blobs     map[string][]byte
	// reported overrides the size Stat and List report for a key, so a test
	// can make storage disagree with the bytes it serves.
	reported map[string]int64
	opened   []string
	listed   []string
	// lax answers every reference whatever place it is given, so a test sees
	// the run's own judgment and not this store's.
	lax bool
	// counted names keys a counted field points at: opening one fails the
	// test, because a counted field is sized and never read.
	counted map[string]bool
	t       *testing.T
	// refuse makes the store refuse a key as the real reader refuses one
	// below its floor; broken makes it fail as storage fails.
	refuse, broken map[string]bool
	// failOpen and panicOpen make reading the committed file back fail.
	failOpen, panicOpen bool
	// corrupt changes one byte of every object as it is committed, the way
	// storage that does not keep what it was given would.
	corrupt bool
}

func newMemStore() *memStore {
	return &memStore{committed: map[string][]byte{}, blobs: map[string][]byte{}, reported: map[string]int64{}, counted: map[string]bool{}, refuse: map[string]bool{}, broken: map[string]bool{}}
}

type memStream struct {
	s   *memStore
	key string
	buf bytes.Buffer
}

func (s *memStore) Create(_ context.Context, key string) (exportFileStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, key)
	return &memStream{s: s, key: key}, nil
}

func (m *memStream) Write(p []byte) (int, error) { return m.buf.Write(p) }

func (m *memStream) Commit(ctx context.Context) (int64, string, error) {
	// As the real stream: a cancelled context commits nothing.
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	b := append([]byte(nil), m.buf.Bytes()...)
	d := sha256.Sum256(b)
	if m.s.corrupt {
		stored := append([]byte(nil), b...)
		stored[len(stored)/2] ^= 0x01
		m.s.committed[m.key] = stored
	} else {
		m.s.committed[m.key] = b
	}
	return int64(len(b)), hex.EncodeToString(d[:]), nil
}

func (m *memStream) Abort(context.Context) error {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	m.s.aborted = append(m.s.aborted, m.key)
	return nil
}

func (s *memStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panicOpen {
		panic("storage blew up")
	}
	if s.failOpen {
		return nil, errors.New("storage is unavailable")
	}
	b, ok := s.committed[key]
	if !ok {
		return nil, errors.New("no such export")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.committed, key)
	s.deleted = append(s.deleted, key)
	return nil
}

// inside is the blob reader's rule: the key strictly inside one of under.
func (s *memStore) inside(key string, under []string) bool {
	if s.lax {
		return true
	}
	return inside(key, under)
}

func inside(key string, under []string) bool {
	for _, u := range under {
		if strings.HasPrefix(key, u+"/") && len(key) > len(u)+1 {
			return true
		}
	}
	return false
}

func refusal(ref string) error {
	return fmt.Errorf("%w: %q", azureblob.ErrBytesReferenceRefused, ref)
}

func (s *memStore) size(key string) int64 {
	if n, ok := s.reported[key]; ok {
		return n
	}
	return int64(len(s.blobs[key]))
}

func (s *memStore) Stat(ctx context.Context, ref string, under []string) (azureblob.BlobInfo, bool, bool, error) {
	if strings.HasPrefix(ref, "file://") {
		return azureblob.BlobInfo{}, false, true, nil
	}
	if strings.HasPrefix(ref, "blob://") {
		objects, err := s.List(ctx, ref, under)
		if err != nil {
			return azureblob.BlobInfo{}, false, false, err
		}
		info := azureblob.BlobInfo{Key: strings.TrimSuffix(strings.TrimPrefix(ref, "blob://"), "/") + "/"}
		for _, o := range objects {
			info.Size += o.Size
		}
		return info, len(objects) > 0, false, nil
	}
	key, _, _, err := s.Resolve(ref)
	if err != nil || !s.inside(key, under) || s.refuse[key] {
		return azureblob.BlobInfo{}, false, false, refusal(ref)
	}
	if s.broken[key] {
		return azureblob.BlobInfo{}, false, false, errors.New("storage is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[key]; !ok {
		return azureblob.BlobInfo{}, false, false, nil
	}
	return azureblob.BlobInfo{Key: key, Size: s.size(key)}, true, false, nil
}

func (s *memStore) List(_ context.Context, treeRef string, under []string) ([]azureblob.BlobInfo, error) {
	prefix := strings.TrimSuffix(strings.TrimPrefix(treeRef, "blob://"), "/")
	if !s.inside(prefix, under) {
		return nil, refusal(treeRef)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = append(s.listed, treeRef)
	var out []azureblob.BlobInfo
	for k := range s.blobs {
		if strings.HasPrefix(k, prefix+"/") {
			out = append(out, azureblob.BlobInfo{Key: k, Size: s.size(k)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *memStore) OpenBytes(key string, under []string) (io.ReadCloser, error) {
	if !s.inside(key, under) {
		return nil, refusal(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counted[key] && s.t != nil {
		s.t.Errorf("the counted object %s was opened", key)
	}
	s.opened = append(s.opened, key)
	b, ok := s.blobs[key]
	if !ok {
		return nil, errors.New("gone")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Resolve is the store's own parse of a stored reference. It understands one
// scheme the Library's own parse does not, `store://<key>`, so a test can tell
// whose parse the run judged a key by.
func (s *memStore) Resolve(ref string) (key string, tree, shipped bool, err error) {
	switch {
	case strings.HasPrefix(ref, "file://"):
		return "", false, true, nil
	case strings.HasPrefix(ref, "blob://"):
		return strings.TrimSuffix(strings.TrimPrefix(ref, "blob://"), "/"), true, false, nil
	case strings.HasPrefix(ref, "store://"):
		return strings.TrimPrefix(ref, "store://"), false, false, nil
	}
	_, key, ok := librarykey.Split(ref)
	if !ok {
		return "", false, false, refusal(ref)
	}
	return key, false, false, nil
}

// memBytes is the bytes half of the store: Open there takes a place.
type memBytes struct{ *memStore }

func (m memBytes) Open(_ context.Context, key string, under []string) (io.ReadCloser, error) {
	return m.OpenBytes(key, under)
}

// ---------------------------------------------------------------------------
// The fixture

type runFixture struct {
	db    *bun.DB
	x     *Exports
	work  *exportWork
	store *memStore

	mu        sync.Mutex
	states    []memql.AccountAssignmentState
	still     func() map[string]int64
	requested [][]string
	progress  []ExportProgress
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	loadRegistry(t)
	db := exportPool(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM "MemoryNodes"`,
		`DELETE FROM memory_liveness`,
		`DELETE FROM edge_stats_daily`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	f := &runFixture{db: db, store: newMemStore(), still: func() map[string]int64 { return nil }}
	f.store.t = t
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inv := New(func() *bun.DB { return db }, memorynodes.List, false, logger)
	f.x = NewExports(inv, &recordingExecutor{}, logger)
	f.x.assignmentStates = f.readStates
	f.x.undeclared = func([]*memorynodes.Concept) []string { return nil }
	f.x.followPending = func(context.Context, memql.AccountQuerier, string) (int64, error) { return 0, nil }
	f.work = &exportWork{
		exports:  f.x,
		concepts: memorynodes.List,
		db:       inv.database,
		openSession: func(ctx context.Context) (*sql.Conn, func(), error) {
			return database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
		},
		states: f.readStates,
		stillAssignable: func(context.Context, memql.AccountQuerier, time.Time, []string) (map[string]int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.still(), nil
		},
		requestFullPass: func(_ context.Context, concepts []string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.requested = append(f.requested, concepts)
			for i := range f.states {
				for _, c := range concepts {
					if f.states[i].Concept == c {
						f.states[i].Waiting = 0
						f.states[i].VerifiedThrough = time.Now().Add(time.Hour)
					}
				}
			}
			return nil
		},
		census:          memql.AccountCensus,
		handOvers:       accountexport.HandOvers,
		files:           func() (exportFiles, error) { return f.store, nil },
		bytes:           func() (accountBytesSource, error) { return memBytes{f.store}, nil },
		engineVersion:   "0.32.0-test",
		domain:          "memql.example",
		caughtUpTimeout: 5 * time.Second,
		caughtUpPoll:    20 * time.Millisecond,
		rowsTimeout:     2 * time.Minute,
		logger:          logger,
	}
	// Every engine-managed kind finished and verified well after now.
	for _, c := range memql.AccountKinds(memorynodes.List(), false) {
		if meta, _ := memorynodes.Get(c); meta.AccountEngineManaged() {
			f.states = append(f.states, memql.AccountAssignmentState{Concept: c, FirstPassFinished: true, VerifiedThrough: time.Now().Add(time.Hour)})
		}
	}
	return f
}

func (f *runFixture) readStates(context.Context, memql.AccountQuerier) ([]memql.AccountAssignmentState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]memql.AccountAssignmentState(nil), f.states...), nil
}

var runClock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// put writes one version; minute orders the versions of one id.
func (f *runFixture) put(t *testing.T, concept, id string, minute int, createdBy string, payload, metadata map[string]any) {
	t.Helper()
	p, err := json.Marshal(payload)
	require.NoError(t, err)
	if metadata == nil {
		metadata = map[string]any{}
	}
	m, err := json.Marshal(metadata)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		 VALUES (?, ?, ?, ?, 'object', '{"$ref":"memql-schema:fp0001"}', ?, ?, '{"source":"test"}')`,
		id, runClock.Add(time.Duration(minute)*time.Minute), createdBy, concept, string(p), string(m))
	require.NoError(t, err)
}

// putUnder writes one version under the given row schema (inline, or a
// fingerprint reference).
func (f *runFixture) putUnder(t *testing.T, concept, id string, minute int, schema string, payload map[string]any) {
	t.Helper()
	p, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		 VALUES (?, ?, 'system', ?, 'object', ?, ?, '{}', '{"source":"test"}')`,
		id, runClock.Add(time.Duration(minute)*time.Minute), concept, schema, string(p))
	require.NoError(t, err)
}

func (f *runFixture) schema(t *testing.T) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(),
		`INSERT INTO memory_node_schemas (fingerprint, document) VALUES ('fp0001', '{"type":"object"}') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
}

func (f *runFixture) user(t *testing.T, short, name, role string) {
	t.Helper()
	f.put(t, userConcept, userConcept+":"+short, 0, "system", map[string]any{
		"displayName": name, "primaryEmail": short + "@example.test", "role": role, "active": true,
	}, nil)
}

func (f *runFixture) export(account string, protected bool) Export {
	return Export{ID: "exp-" + account, AccountID: account, AccountName: account, Protected: protected, Attempt: 1}
}

func (f *runFixture) run(t *testing.T, x Export, passphrase string) (ExportResult, error) {
	t.Helper()
	return f.work.run(context.Background(), x, passphrase, func(p ExportProgress) {
		f.mu.Lock()
		f.progress = append(f.progress, p)
		f.mu.Unlock()
	})
}

// member is one member of a committed file, by its path in the folder.
type member struct {
	path string
	data []byte
}

func members(t *testing.T, file []byte) []member {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(file))
	var out []member
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		_, rel, _ := strings.Cut(h.Name, "/")
		out = append(out, member{rel, data})
	}
}

func memberData(t *testing.T, ms []member, path string) []byte {
	t.Helper()
	for _, m := range ms {
		if m.path == path {
			return m.data
		}
	}
	t.Fatalf("no member %s", path)
	return nil
}

func jsonLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return out
}

// partLines reads every part of a folder.
func partLines(t *testing.T, ms []member, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range ms {
		if !strings.HasPrefix(m.path, dir+"/part-") {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(m.data))
		require.NoError(t, err)
		body, err := io.ReadAll(zr)
		require.NoError(t, err)
		out = append(out, jsonLines(t, body)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// The fixture account

const (
	acme     = "acme"
	other    = "other"
	siteID   = "v1:platform:site:s1"
	fileID   = "v1:library:file:f1"
	noteID   = "v1:todos:todo:t1"
	storeID  = "v1:shopify:store:st1"
	vaultID  = vaultConcept + ":vault1"
	secretOf = "never-travels-7f3a"
)

func acmeIn(extra map[string]any) map[string]any {
	out := map[string]any{"accountId": acme}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// seedAcme writes the account the file tests export, and one row of another.
func (f *runFixture) seedAcme(t *testing.T) {
	t.Helper()
	f.schema(t)
	f.user(t, "u-member", "Mia Member", "user")
	f.user(t, "u-former", "Fred Former", "user")
	f.user(t, "u-op", "Olga Operator", "owner")
	f.user(t, "u-other", "Otto Other", "user")
	f.user(t, "u-nobody", "Nora Nobody", "developer")

	f.put(t, accountConcept, canonicalPrefix+acme, 1, "system", map[string]any{"name": "Acme"}, nil)
	f.put(t, accountConcept, canonicalPrefix+acme, 2, "system", map[string]any{"name": "Acme Ltd"}, nil)

	// The membership rows: a member, and a former member (active, then removed).
	f.put(t, membershipConcept, membershipConcept+":m1", 3, "system", acmeIn(map[string]any{"userId": "v1:identity:user:u-member", "status": "active", "groupId": "v1:identity:group:g1"}), nil)
	f.put(t, membershipConcept, membershipConcept+":m2", 3, "system", acmeIn(map[string]any{"userId": "v1:identity:user:u-former", "status": "active", "groupId": "v1:identity:group:g1"}), nil)
	f.put(t, membershipConcept, membershipConcept+":m2", 4, "system", acmeIn(map[string]any{"userId": "v1:identity:user:u-former", "status": "removed", "groupId": "v1:identity:group:g1"}), nil)

	// A note, owned by someone with no membership.
	f.put(t, "v1:todos:todo", noteID, 5, "v1:identity:user:u-member", acmeIn(map[string]any{"title": "Ship", "ownerUserId": "v1:identity:user:u-other"}), nil)
	// A note in no account, and another account's note.
	f.put(t, "v1:todos:todo", "v1:todos:todo:t-none", 5, "system", map[string]any{"title": "Nobody's"}, nil)
	f.put(t, "v1:todos:todo", "v1:todos:todo:t-other", 5, "system", map[string]any{"accountId": other, "title": "Theirs"}, nil)

	// A file with two versions; the second written by the operator, with the
	// request context the engine records.
	f.put(t, fileConcept, fileID, 6, "v1:identity:user:u-member", acmeIn(map[string]any{
		"ownerUserId": "v1:identity:user:u-member", "name": "a.txt", "blobUrl": "library/u-member/f1/a.txt",
	}), map[string]any{"identity.subject": "v1:identity:user:u-member", "identity.role": "user"})
	f.put(t, fileConcept, fileID, 7, "u-op@example.test", acmeIn(map[string]any{
		"ownerUserId": "v1:identity:user:u-member", "name": "b.txt", "blobUrl": "library/u-member/f1/b.txt",
	}), map[string]any{
		"identity.subject": "v1:identity:user:u-op", "identity.role": "owner",
		"identity.displayName": "Olga Operator", "identity.email": "u-op@example.test",
		"geo.ip": "203.0.113.9", "geo.country": "XX", "request.userAgent": "Browser/1",
		"server.nodeId": "bff-1", "request.id": "req-1",
	})
	f.store.blobs["library/u-member/f1/a.txt"] = []byte("the first version")
	f.store.blobs["library/u-member/f1/b.txt"] = []byte("the second version, carried")

	// A site with a live bundle tree, a candidate that is another site's
	// bundle, and a package that does not exist.
	// Both versions written by the operator.
	for _, minute := range []int{8, 15} {
		f.put(t, "v1:platform:site", siteID, minute, "u-op@example.test", acmeIn(map[string]any{
			"ownerUserId": "v1:identity:user:u-member", "name": "Landing",
			"bundleRef": "blob://sites/s1/v3/", "candidateRef": "blob://sites/s2/v1/", "packageId": "v1:platform:package:p-gone",
		}), map[string]any{"identity.subject": "v1:identity:user:u-op", "identity.role": "owner", "geo.ip": "203.0.113.9", "server.nodeId": "bff-1"})
	}
	f.store.blobs["sites/s1/v3/index.html"] = []byte("<html></html>")
	f.store.blobs["sites/s1/v3/app.js"] = []byte("console.log(1)")
	f.store.blobs["sites/s2/v1/index.html"] = []byte("another site")
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO edge_stats_daily
		(site_id, day, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
		VALUES (?, '2026-11-11', 3, 4, 1, 9, 12, 0, 1, 2048, 2, '{1,2,3}')`, siteID)
	require.NoError(t, err)

	// A store naming its secret, and a mirrored row of it.
	f.put(t, "v1:shopify:store", storeID, 9, "system", acmeIn(map[string]any{"shop": "acme.myshopify.com", "adminTokenRef": "SHOPIFY_ACME_ADMIN"}), nil)
	f.put(t, "v1:shopify:catalog", "v1:shopify:catalog:c1", 10, "system", acmeIn(map[string]any{"gid": "gid://shopify/Catalog/1", "storeId": storeID}), nil)

	// A package fetched under a cluster-wide credential.
	f.put(t, "v1:platform:sourceCredential", "v1:platform:sourceCredential:cred1", 11, "system", map[string]any{"name": "Deploy token", "ownerUserId": "v1:identity:user:u-member"}, nil)
	f.put(t, "v1:platform:package", "v1:platform:package:p1", 11, "system", acmeIn(map[string]any{"name": "Site source", "credentialId": "v1:platform:sourceCredential:cred1"}), nil)

	// A kind that stays.
	f.put(t, "v1:platform:siteAccessSession", "v1:platform:siteAccessSession:sess1", 12, "system", acmeIn(map[string]any{"siteId": siteID}), nil)

	// @secret fields at every depth, in two versions.
	for i, minute := range []int{13, 14} {
		v := fmt.Sprint(i)
		f.put(t, vaultConcept, vaultID, minute, "system", acmeIn(map[string]any{
			"label": "Keys", "token": secretOf + "-top-" + v,
			"settings": map[string]any{"region": "eu", "apiKey": secretOf + "-block-" + v},
			"entries":  []any{map[string]any{"name": "door", "pin": secretOf + "-list-" + v}},
			"extra":    map[string]any{"a": map[string]any{"code": secretOf + "-map-" + v}},
			"choice":   map[string]any{"kind": "basic", "password": secretOf + "-union-" + v},
		}), nil)
	}
}

func TestExportRunWritesTheAccountsFile(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)

	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.Equal(t, "exports/accounts/exp-acme/1/"+res.FileName, res.ObjectKey)
	require.True(t, strings.HasPrefix(res.FileName, "acme-") && strings.HasSuffix(res.FileName, ".memql-account.tar"), res.FileName)
	file := f.store.committed[res.ObjectKey]
	require.NotEmpty(t, file)
	require.Equal(t, int64(len(file)), res.SizeBytes)
	sum := sha256.Sum256(file)
	require.Equal(t, hex.EncodeToString(sum[:]), res.SHA256)

	vr, err := accountexport.Verify(bytes.NewReader(file))
	require.NoError(t, err)

	ms := members(t, file)
	var paths []string
	for _, m := range ms {
		paths = append(paths, m.path)
	}
	// The exact layout of the fixture file.
	wantRows := []string{
		"rows/identity/groupMembership/part-000001.jsonl.gz",
		"rows/library/file/part-000001.jsonl.gz",
		"rows/platform/package/part-000001.jsonl.gz",
		"rows/platform/site/part-000001.jsonl.gz",
		"rows/shopify/catalog/part-000001.jsonl.gz",
		"rows/shopify/store/part-000001.jsonl.gz",
		"rows/testexport/vault/part-000001.jsonl.gz",
		"rows/todos/todo/part-000001.jsonl.gz",
	}
	want := append([]string{"manifest.json", "account.jsonl"}, wantRows...)
	want = append(want,
		"schemas/fp0001.json",
		"people.jsonl",
		"references.jsonl",
		"connections.jsonl",
		"tables/site-statistics/edge_stats_daily/part-000001.jsonl.gz",
		"bytes/000001",
		"bytes/000002",
		"bytes/000003",
		"bytes.jsonl",
		"report.json",
		"README.txt",
		"SHA256SUMS",
	)
	require.Equal(t, want, paths)

	// The manifest.
	var m accountexport.Manifest
	require.NoError(t, json.Unmarshal(memberData(t, ms, "manifest.json"), &m))
	require.Equal(t, accountexport.Account{ID: acme, Name: "Acme Ltd", Primary: false}, m.Account)
	require.False(t, m.Protected)
	require.Equal(t, "memql.example", m.Source.Domain)
	decl := map[string]accountexport.KindDecl{}
	for _, k := range m.Kinds {
		decl[k.Concept] = k
	}
	require.Equal(t, "shopify", decl["v1:shopify:catalog"].Origin, "a mirrored kind carries its origin")
	require.NotContains(t, decl, "v1:platform:siteAccessSession", "a kind that stays is not declared")
	require.Equal(t, vr.Manifest.CreatedAt, m.CreatedAt)

	// The account's own row, every version.
	acct := jsonLines(t, memberData(t, ms, "account.jsonl"))
	require.Len(t, acct, 2)

	// The census: every kind's rows and versions, expected and written.
	var report accountexport.Report
	require.NoError(t, json.Unmarshal(memberData(t, ms, "report.json"), &report))
	counts := map[string]accountexport.KindCount{}
	for _, k := range report.Kinds {
		counts[k.Concept] = k
		require.Equal(t, k.ExpectedRows, k.WrittenRows, k.Concept)
		require.Equal(t, k.ExpectedVersions, k.WrittenVersions, k.Concept)
	}
	for concept, rv := range map[string][2]int64{
		accountConcept: {1, 2}, membershipConcept: {2, 3}, fileConcept: {1, 2}, "v1:todos:todo": {1, 1},
		"v1:platform:site": {1, 2}, "v1:shopify:store": {1, 1}, "v1:shopify:catalog": {1, 1},
		"v1:platform:package": {1, 1}, vaultConcept: {1, 2},
	} {
		require.Equal(t, rv[0], counts[concept].WrittenRows, concept)
		require.Equal(t, rv[1], counts[concept].WrittenVersions, concept)
	}
	todos := partLines(t, ms, "rows/todos/todo")
	require.Len(t, todos, 1, "the other account's note and the note in no account are not carried")
	require.Equal(t, noteID, todos[0]["id"])

	// The left out.
	require.Contains(t, report.LeftOut, accountexport.LeftOut{Concept: "v1:platform:siteAccessSession", Reason: "signin", Rows: 1})
	require.Contains(t, report.InNoAccount, accountexport.KindRows{Concept: "v1:todos:todo", Rows: 1})
	require.Equal(t, []accountexport.ScrubbedField{
		{Concept: vaultConcept, Field: "choice.password"},
		{Concept: vaultConcept, Field: "entries[].pin"},
		{Concept: vaultConcept, Field: "extra.*.code"},
		{Concept: vaultConcept, Field: "settings.apiKey"},
		{Concept: vaultConcept, Field: "token"},
	}, report.ScrubbedFields)
	require.Equal(t, rebuiltOnTarget, report.Rebuilt)

	// The secret value is in no member of the file.
	require.NotContains(t, string(file), secretOf)
	vault := partLines(t, ms, "rows/testexport/vault")
	require.Len(t, vault, 2)
	for _, v := range vault {
		require.Equal(t, map[string]any{
			"accountId": acme, "label": "Keys",
			"settings": map[string]any{"region": "eu"},
			"entries":  []any{map[string]any{"name": "door"}},
			"extra":    map[string]any{"a": map[string]any{}},
			"choice":   map[string]any{"kind": "basic"},
		}, v["payload"], "the older version too")
	}

	// The people.
	people := jsonLines(t, memberData(t, ms, "people.jsonl"))
	require.Equal(t, []map[string]any{
		{"id": "v1:identity:user:u-former", "kind": "former", "name": "Fred Former", "email": "u-former@example.test", "role": "user", "active": false},
		{"id": "v1:identity:user:u-member", "kind": "member", "name": "Mia Member", "email": "u-member@example.test", "role": "user", "active": true},
		{"id": "v1:identity:user:u-op", "kind": "operator"},
		{"id": "v1:identity:user:u-other", "kind": "other", "name": "Otto Other", "email": "u-other@example.test"},
	}, people)

	// The operator's version, scrubbed; the member's version as stored.
	files := partLines(t, ms, "rows/library/file")
	require.Len(t, files, 2)
	require.Equal(t, "v1:identity:user:u-member", files[0]["createdBy"])
	require.Equal(t, map[string]any{"identity.subject": "v1:identity:user:u-member", "identity.role": "user"}, files[0]["metadata"])
	require.Equal(t, "user:u-op", files[1]["createdBy"])
	require.Equal(t, map[string]any{"identity.subject": "v1:identity:user:u-op", "identity.role": "owner", "request.id": "req-1"}, files[1]["metadata"])
	for _, v := range partLines(t, ms, "rows/platform/site") {
		require.Equal(t, "user:u-op", v["createdBy"])
		require.Equal(t, map[string]any{"identity.subject": "v1:identity:user:u-op", "identity.role": "owner"}, v["metadata"])
	}
	require.NotContains(t, string(file), "203.0.113.9")
	require.NotContains(t, string(file), "Olga Operator")

	// The references.
	refs := jsonLines(t, memberData(t, ms, "references.jsonl"))
	byTarget := map[string]map[string]any{}
	for _, r := range refs {
		byTarget[r["to"].(map[string]any)["id"].(string)] = r
	}
	require.Equal(t, "outside", byTarget["v1:platform:package:p-gone"]["provided"])
	require.NotContains(t, byTarget["v1:platform:package:p-gone"], "name")
	require.Equal(t, "cluster", byTarget["v1:platform:sourceCredential:cred1"]["provided"])
	require.NotContains(t, byTarget["v1:platform:sourceCredential:cred1"], "name", "a cluster-wide row is never read, so it has no name")
	require.Equal(t, []any{map[string]any{"concept": "v1:platform:package", "field": "credentialId", "rows": float64(1)}},
		byTarget["v1:platform:sourceCredential:cred1"]["from"])
	require.NotContains(t, byTarget, storeID, "a carried row is not a reference")
	require.NotContains(t, byTarget, canonicalPrefix+acme, "the account itself is in the file")
	require.NotContains(t, byTarget, "v1:identity:user:u-member", "a person is not a reference")
	require.Equal(t, "outside", byTarget["v1:identity:group:g1"]["provided"])

	// The connections.
	conns := jsonLines(t, memberData(t, ms, "connections.jsonl"))
	var secrets, creds []map[string]any
	stays := map[string]float64{}
	for _, c := range conns {
		switch c["kind"] {
		case "secret":
			secrets = append(secrets, c)
		case "credential":
			creds = append(creds, c)
		case "stays":
			stays[c["concept"].(string)] = c["rows"].(float64)
		}
	}
	require.Equal(t, []map[string]any{{"kind": "secret", "name": "SHOPIFY_ACME_ADMIN",
		"from": []any{map[string]any{"concept": "v1:shopify:store", "field": "adminTokenRef", "rows": float64(1)}}}}, secrets)
	require.Equal(t, []map[string]any{{"kind": "credential", "to": map[string]any{"concept": "v1:platform:sourceCredential", "id": "v1:platform:sourceCredential:cred1"}}}, creds)
	require.Equal(t, float64(1), stays["v1:platform:siteAccessSession"])
	require.Contains(t, stays, "v1:workbench:workspace", "every kind that stays has its line, with its count")

	// The bytes: the file's newest object and the site's tree, each once;
	// the other site's bundle refused and never opened; the older version's
	// object counted, never opened.
	bl := jsonLines(t, memberData(t, ms, "bytes.jsonl"))
	require.Len(t, bl, 4)
	require.Equal(t, "library/u-member/f1/b.txt", bl[0]["key"])
	require.Equal(t, "sites/s1/v3/app.js", bl[1]["key"])
	require.Equal(t, "blob://sites/s1/v3/", bl[1]["treeOf"])
	require.Equal(t, "app.js", bl[1]["path"])
	require.Equal(t, "sites/s1/v3/index.html", bl[2]["key"])
	require.Equal(t, "blob://sites/s2/v1/", bl[3]["key"])
	require.Equal(t, true, bl[3]["refused"])
	require.NotContains(t, bl[3], "file")
	require.Equal(t, []byte("the second version, carried"), memberData(t, ms, bl[0]["file"].(string)))
	for _, k := range f.store.opened {
		require.False(t, strings.HasPrefix(k, "sites/s2/"), "a refused reference was opened: %s", k)
		require.NotEqual(t, "library/u-member/f1/a.txt", k, "an older version's bytes were opened")
	}
	require.Equal(t, accountexport.BytesReport{
		Objects: 3, Bytes: int64(len("the second version, carried") + len("<html></html>") + len("console.log(1)")),
		Refused: 1, NotCarriedObjects: 1, NotCarriedBytes: int64(len("the first version")),
	}, report.Bytes)

	// The hand-over's folder is under tables/.
	stats := partLines(t, ms, "tables/site-statistics/edge_stats_daily")
	require.Len(t, stats, 1)
	require.Equal(t, siteID, stats[0]["site_id"])
	require.Contains(t, report.Members, accountexport.MemberCount{Name: "tables/site-statistics/edge_stats_daily", Lines: 1})

	// The record's report is the file's.
	var recorded accountexport.Report
	require.NoError(t, json.Unmarshal(res.Report, &recorded))
	require.Equal(t, report, recorded)
	require.Equal(t, report, vr.Report)

	// Every phase was reported, in order.
	var phases []string
	for _, p := range f.progress {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}
	require.Equal(t, []string{phaseChecking, phaseRows, phaseBytes, phaseSealing, phaseVerifying}, phases)
	require.Empty(t, f.store.aborted)
}

func TestExportRunRowPartsAreTheSameOnASecondRun(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	first, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	x := f.export(acme, false)
	x.Attempt = 2
	second, err := f.run(t, x, "")
	require.NoError(t, err)
	a, b := members(t, f.store.committed[first.ObjectKey]), members(t, f.store.committed[second.ObjectKey])
	require.Equal(t, len(a), len(b))
	for i := range a {
		require.Equal(t, a[i].path, b[i].path)
		switch a[i].path {
		case "manifest.json", "report.json", "README.txt", "SHA256SUMS":
			continue
		}
		require.Equal(t, a[i].data, b[i].data, "%s differs between two runs of an unchanged account", a[i].path)
	}
}

func TestExportRunProtectedFileOpensWithItsPassphraseOnly(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	const pass = "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ2"
	res, err := f.run(t, f.export(acme, true), pass)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(res.FileName, ".tar.age"), res.FileName)
	file := f.store.committed[res.ObjectKey]

	_, err = accountexport.Verify(bytes.NewReader(file))
	require.Error(t, err, "a protected file is not a tar stream without its passphrase")
	_, err = accountexport.Unprotect(bytes.NewReader(file), "WRONG-WRONG-WRONG-WRONG-WRONG")
	require.Error(t, err)

	r, err := accountexport.Unprotect(bytes.NewReader(file), pass)
	require.NoError(t, err)
	vr, err := accountexport.Verify(r)
	require.NoError(t, err)
	require.True(t, vr.Manifest.Protected)
	require.NotContains(t, string(res.Report), pass)
}

func TestExportRunSnapshotIsOneReadOnlyInstantOnANamedSession(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	var app, iso, ro string
	f.work.inSnapshot = func(ctx context.Context, q memql.AccountQuerier) error {
		rows, err := q.QueryContext(ctx, `SELECT current_setting('application_name'), current_setting('transaction_isolation'), current_setting('transaction_read_only')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		rows.Next()
		return rows.Scan(&app, &iso, &ro)
	}
	_, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.Equal(t, memql.AccountExportApplicationName, app)
	require.Equal(t, "repeatable read", iso)
	require.Equal(t, "on", ro)
}

func TestExportRunPrimaryAccountNamesItsOwnersAndDevelopers(t *testing.T) {
	f := newRunFixture(t)
	f.schema(t)
	f.user(t, "u-op", "Olga Operator", "owner")
	f.put(t, accountConcept, canonicalPrefix+primaryAccount, 1, "system", map[string]any{"name": "Our company"}, nil)
	f.put(t, fileConcept, "v1:library:file:own", 2, "u-op@example.test", map[string]any{
		"accountId": primaryAccount, "ownerUserId": "v1:identity:user:u-op", "name": "x", "blobUrl": "file:///app/os/x",
	}, map[string]any{"identity.subject": "v1:identity:user:u-op", "identity.role": "owner", "geo.ip": "203.0.113.9"})

	res, err := f.run(t, f.export(primaryAccount, false), "")
	require.NoError(t, err)
	ms := members(t, f.store.committed[res.ObjectKey])
	var m accountexport.Manifest
	require.NoError(t, json.Unmarshal(memberData(t, ms, "manifest.json"), &m))
	require.True(t, m.Account.Primary)
	require.Equal(t, []map[string]any{
		{"id": "v1:identity:user:u-op", "kind": "other", "name": "Olga Operator", "email": "u-op@example.test"},
	}, jsonLines(t, memberData(t, ms, "people.jsonl")))
	files := partLines(t, ms, "rows/library/file")
	require.Equal(t, "u-op@example.test", files[0]["createdBy"], "nothing is scrubbed in the primary account's file")
	require.Equal(t, "203.0.113.9", files[0]["metadata"].(map[string]any)["geo.ip"])
	// A shipped reference is neither read nor counted.
	require.Empty(t, jsonLines(t, memberData(t, ms, "bytes.jsonl")))
}

func TestExportRunRefusesABytesReferenceItsDeclarationDoesNotAdmit(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	// A note-taker's file naming another person's object, with storage that
	// would hand over anything it is asked for.
	f.store.lax = true
	f.put(t, fileConcept, "v1:library:file:f2", 20, "system", acmeIn(map[string]any{
		"ownerUserId": "v1:identity:user:u-member", "name": "x", "blobUrl": "library/u-other/f9/theirs.txt",
	}), nil)
	f.store.blobs["library/u-other/f9/theirs.txt"] = []byte("not this account's")
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	file := f.store.committed[res.ObjectKey]
	require.NotContains(t, string(file), "not this account's")
	for _, k := range f.store.opened {
		require.NotEqual(t, "library/u-other/f9/theirs.txt", k)
		require.False(t, strings.HasPrefix(k, "sites/s2/"), k)
	}
	require.NotContains(t, f.store.listed, "blob://sites/s2/v1/")
	refused := map[string]bool{}
	for _, l := range jsonLines(t, memberData(t, members(t, file), "bytes.jsonl")) {
		if l["refused"] == true {
			refused[l["key"].(string)] = true
		}
	}
	require.Equal(t, map[string]bool{"library/u-other/f9/theirs.txt": true, "blob://sites/s2/v1/": true}, refused)
}

// ---------------------------------------------------------------------------
// Secrets, references, people and counted fields

// withConcepts runs the export over the registry with some kinds replaced.
func (f *runFixture) withConcepts(replace ...*memorynodes.Concept) {
	byName := map[string]*memorynodes.Concept{}
	for _, c := range replace {
		byName[c.Name] = c
	}
	f.work.concepts = func() []*memorynodes.Concept {
		var out []*memorynodes.Concept
		for _, c := range memorynodes.List() {
			if r, ok := byName[c.Name]; ok {
				out = append(out, r)
				continue
			}
			out = append(out, c)
		}
		return out
	}
}

// markedSecret is a copy of a registered kind whose named top-level fields
// are declared @secret.
func markedSecret(t *testing.T, concept string, fields ...string) *memorynodes.Concept {
	t.Helper()
	c, err := memorynodes.Get(concept)
	require.NoError(t, err)
	clone := *c
	var schema map[string]any
	require.NoError(t, json.Unmarshal(c.Schemas["definition"], &schema))
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	for _, f := range fields {
		p, _ := props[f].(map[string]any)
		if p == nil {
			p = map[string]any{"type": "string"}
		}
		p["x-secret"] = true
		props[f] = p
	}
	raw, err := json.Marshal(schema)
	require.NoError(t, err)
	clone.Schemas = map[string]json.RawMessage{}
	for k, v := range c.Schemas {
		clone.Schemas[k] = v
	}
	clone.Schemas["definition"] = raw
	return &clone
}

func TestExportRunRemovesSecretsFromTheAccountRowAndAnObservedVersion(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.withConcepts(markedSecret(t, accountConcept, "notes"), markedSecret(t, "v1:pipelines:run", "token"))
	f.put(t, accountConcept, canonicalPrefix+acme, 30, "system", map[string]any{"name": "Acme Ltd", "notes": "acct-secret-1"}, nil)
	f.put(t, "v1:pipelines:run", "v1:pipelines:run:r1", 30, "system", acmeIn(map[string]any{"token": "run-secret-older"}), nil)
	_, err := f.db.ExecContext(context.Background(),
		`INSERT INTO memory_liveness (id, "createdAt", "createdBy", concept, schema, payload) VALUES (?, ?, 'system', 'v1:pipelines:run', '{}', ?)`,
		"v1:pipelines:run:r1", runClock.Add(40*time.Minute), `{"accountId":"acme","token":"run-secret-observed"}`)
	require.NoError(t, err)

	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	file := string(f.store.committed[res.ObjectKey])
	for _, secret := range []string{"acct-secret-1", "run-secret-older", "run-secret-observed"} {
		require.NotContains(t, file, secret)
	}
	ms := members(t, f.store.committed[res.ObjectKey])
	runs := partLines(t, ms, "rows/pipelines/run")
	require.Len(t, runs, 2)
	require.Equal(t, true, runs[1]["observed"])
	var report accountexport.Report
	require.NoError(t, json.Unmarshal(memberData(t, ms, "report.json"), &report))
	require.Contains(t, report.ScrubbedFields, accountexport.ScrubbedField{Concept: accountConcept, Field: "notes"})
	require.Contains(t, report.ScrubbedFields, accountexport.ScrubbedField{Concept: "v1:pipelines:run", Field: "token"})
}

// countingQuerier records the kinds every statement of the snapshot names.
type countingQuerier struct {
	inner memql.AccountQuerier
	mu    sync.Mutex
	args  []string
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	for _, a := range args {
		c.args = append(c.args, fmt.Sprint(a))
	}
	c.mu.Unlock()
	return c.inner.QueryContext(ctx, query, args...)
}

func (c *countingQuerier) named(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.args {
		if strings.Contains(a, kind) {
			return true
		}
	}
	return false
}

func refsByID(t *testing.T, ms []member) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range jsonLines(t, memberData(t, ms, "references.jsonl")) {
		out[r["to"].(map[string]any)["id"].(string)] = r
	}
	return out
}

func TestExportRunReadsNoRowAReferenceNamesThroughAnotherKind(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	cq := &countingQuerier{}
	f.work.wrapQuerier = func(q memql.AccountQuerier) memql.AccountQuerier { cq.inner = q; return cq }
	// A site whose package field names a sign-in row, and whose artifact
	// field names a cluster-wide row holding a secret. Both rows exist.
	f.put(t, "v1:identity:authSession", "v1:identity:authSession:sess-x", 1, "system", map[string]any{"name": "SESSION-TITLE"}, nil)
	f.put(t, "v1:platform:globalSecret", "v1:platform:globalSecret:gs1", 1, "system", map[string]any{"name": "SECRET-TITLE"}, nil)
	f.put(t, "v1:platform:site", "v1:platform:site:s9", 20, "system", acmeIn(map[string]any{
		"name": "Odd", "packageId": "v1:identity:authSession:sess-x", "artifactId": "v1:platform:globalSecret:gs1",
	}), nil)

	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	ms := members(t, f.store.committed[res.ObjectKey])
	refs := refsByID(t, ms)
	require.Equal(t, map[string]any{"concept": "v1:identity:authSession", "id": "v1:identity:authSession:sess-x"}, refs["v1:identity:authSession:sess-x"]["to"])
	require.Equal(t, "outside", refs["v1:identity:authSession:sess-x"]["provided"])
	require.Equal(t, "outside", refs["v1:platform:globalSecret:gs1"]["provided"])
	file := string(f.store.committed[res.ObjectKey])
	require.NotContains(t, file, "SESSION-TITLE")
	require.NotContains(t, file, "SECRET-TITLE")
	for _, c := range jsonLines(t, memberData(t, ms, "connections.jsonl")) {
		if c["kind"] == "credential" {
			require.NotEqual(t, "v1:platform:globalSecret:gs1", c["to"].(map[string]any)["id"], "a value of another kind than the field's makes no credential")
		}
	}
	require.False(t, cq.named("v1:identity:authSession"), "a sign-in kind was read")
	require.False(t, cq.named("v1:platform:globalSecret"), "a kind no field declares was read")
}

func TestExportRunReferencesToOtherAccountsAndSharedRows(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	cq := &countingQuerier{}
	f.work.wrapQuerier = func(q memql.AccountQuerier) memql.AccountQuerier { cq.inner = q; return cq }
	// Another account's package, which exists.
	f.put(t, "v1:platform:package", "v1:platform:package:p-theirs", 1, "system", map[string]any{"accountId": other, "name": "THEIR-PACKAGE"}, nil)
	f.put(t, "v1:platform:site", "v1:platform:site:s8", 20, "system", acmeIn(map[string]any{"name": "Two", "packageId": "v1:platform:package:p-theirs"}), nil)
	// Agents, a shared kind: the cluster's own, and another account's.
	f.put(t, "v1:agents:agent", "v1:agents:agent:a-cluster", 1, "system", map[string]any{"name": "Cluster helper"}, nil)
	f.put(t, "v1:agents:agent", "v1:agents:agent:a-theirs", 1, "system", map[string]any{"accountId": other, "name": "THEIR-AGENT"}, nil)
	f.put(t, "v1:agents:agentAuthorization", "v1:agents:agentAuthorization:z1", 20, "system", acmeIn(map[string]any{"agentId": "v1:agents:agent:a-cluster"}), nil)
	f.put(t, "v1:agents:agentAuthorization", "v1:agents:agentAuthorization:z2", 20, "system", acmeIn(map[string]any{"agentId": "v1:agents:agent:a-theirs"}), nil)

	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	file := string(f.store.committed[res.ObjectKey])
	require.NotContains(t, file, "THEIR-PACKAGE")
	require.NotContains(t, file, "THEIR-AGENT")
	refs := refsByID(t, members(t, f.store.committed[res.ObjectKey]))
	require.Equal(t, "outside", refs["v1:platform:package:p-theirs"]["provided"])
	require.NotContains(t, refs["v1:platform:package:p-theirs"], "name")
	require.Equal(t, "outside", refs["v1:agents:agent:a-theirs"]["provided"])
	require.NotContains(t, refs["v1:agents:agent:a-theirs"], "name")
	require.Equal(t, "cluster", refs["v1:agents:agent:a-cluster"]["provided"])
	require.Equal(t, "Cluster helper", refs["v1:agents:agent:a-cluster"]["name"])
	// A cluster-wide kind is listed and never read.
	require.Equal(t, "cluster", refs["v1:platform:sourceCredential:cred1"]["provided"])
	require.NotContains(t, refs["v1:platform:sourceCredential:cred1"], "name")
	require.False(t, cq.named("v1:platform:sourceCredential"), "a cluster-wide kind was read")
	require.False(t, cq.named("p-theirs"), "another account's row of a kind that is not shared was read")
}

func TestExportRunScrubsAnUnresolvedDeveloperOnAClusterWithNoOperator(t *testing.T) {
	f := newRunFixture(t)
	f.schema(t)
	f.user(t, "u-member", "Mia Member", "user")
	f.put(t, accountConcept, canonicalPrefix+acme, 1, "system", map[string]any{"name": "Acme"}, nil)
	f.put(t, membershipConcept, membershipConcept+":m1", 2, "system", acmeIn(map[string]any{"userId": "v1:identity:user:u-member", "status": "active"}), nil)
	f.put(t, "v1:todos:todo", noteID, 3, "gone@example.test", acmeIn(map[string]any{"title": "Ship", "ownerUserId": "v1:identity:user:u-member"}),
		map[string]any{"identity.subject": "v1:identity:user:u-gone", "identity.role": "developer", "identity.email": "gone@example.test", "geo.ip": "203.0.113.7", "request.id": "r9"})
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	todos := partLines(t, members(t, f.store.committed[res.ObjectKey]), "rows/todos/todo")
	require.Equal(t, map[string]any{"identity.subject": "v1:identity:user:u-gone", "identity.role": "developer", "request.id": "r9"}, todos[0]["metadata"])
	require.Equal(t, "operator", todos[0]["createdBy"])
}

func TestExportRunJudgesTheKeyTheReaderReads(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	// A reference only the reader's own parse understands.
	f.put(t, fileConcept, "v1:library:file:f3", 20, "system", acmeIn(map[string]any{
		"ownerUserId": "v1:identity:user:u-member", "name": "c.txt", "blobUrl": "store://library/u-member/f3/c.txt",
	}), nil)
	f.store.blobs["library/u-member/f3/c.txt"] = []byte("read by the reader's key")
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.Contains(t, string(f.store.committed[res.ObjectKey]), "read by the reader's key")
}

func TestExportRunNeverUsesASecretFieldAsAReferenceAPersonOrBytes(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.withConcepts(
		markedSecret(t, "v1:platform:site", "packageId"),
		markedSecret(t, "v1:todos:todo", "ownerUserId"),
		markedSecret(t, fileConcept, "blobUrl"),
	)
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	ms := members(t, f.store.committed[res.ObjectKey])
	require.NotContains(t, refsByID(t, ms), "v1:platform:package:p-gone")
	for _, p := range jsonLines(t, memberData(t, ms, "people.jsonl")) {
		require.NotEqual(t, "v1:identity:user:u-other", p["id"], "a secret owner field named a person")
	}
	for _, b := range jsonLines(t, memberData(t, ms, "bytes.jsonl")) {
		require.False(t, strings.HasPrefix(b["key"].(string), "library/"), "a secret field's bytes were read: %v", b["key"])
	}
}

func TestExportRunNeverOpensACountedField(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.put(t, "v1:common:attachment", "v1:common:attachment:at1", 20, "system", acmeIn(map[string]any{"blobUrl": "spaces/sp1/att.bin"}), nil)
	f.store.blobs["spaces/sp1/att.bin"] = []byte("attachment bytes")
	f.store.counted["spaces/sp1/att.bin"] = true
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	var report accountexport.Report
	require.NoError(t, json.Unmarshal(res.Report, &report))
	require.Equal(t, int64(2), report.Bytes.NotCarriedObjects)
	require.Equal(t, int64(len("the first version")+len("attachment bytes")), report.Bytes.NotCarriedBytes)
}

func TestSecretPathsFindsASecretAtEveryDepth(t *testing.T) {
	loadRegistry(t)
	c, err := memorynodes.Get(vaultConcept)
	require.NoError(t, err)
	require.Equal(t, []string{"choice.password", "entries[].pin", "extra.*.code", "settings.apiKey", "token"}, SecretPaths(c))
	plain, err := memorynodes.Get("v1:todos:todo")
	require.NoError(t, err)
	require.Empty(t, SecretPaths(plain))
}

func TestExportRunRecordsAReferenceTheReaderRefusesAndFailsOnAStorageError(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.store.refuse["library/u-member/f1/b.txt"] = true
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	refused := false
	for _, l := range jsonLines(t, memberData(t, members(t, f.store.committed[res.ObjectKey]), "bytes.jsonl")) {
		if l["key"] == "library/u-member/f1/b.txt" {
			refused = l["refused"] == true
		}
	}
	require.True(t, refused, "the reader's refusal is recorded")

	g := newRunFixture(t)
	g.seedAcme(t)
	g.store.broken["library/u-member/f1/b.txt"] = true
	_, err = g.run(t, g.export(acme, false), "")
	require.ErrorContains(t, err, "storage is unavailable")
	g.requireNothingLeft(t)
}

// ---------------------------------------------------------------------------
// A version's own schema, kinds that stay, and rows the file carries

func TestExportRunRemovesASecretTheVersionsOwnSchemaMarked(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	// The vault's older shape: a top-level secret and a nested one the
	// current definition does not have at all.
	older := `{"properties":{"label":{"type":"string"},"legacyKey":{"type":"string","x-secret":true},` +
		`"old":{"type":"object","properties":{"note":{"type":"string"},"pin":{"type":"string","x-secret":true}}}}}`
	_, err := f.db.ExecContext(context.Background(),
		`INSERT INTO memory_node_schemas (fingerprint, document) VALUES ('fp-vault-old', ?) ON CONFLICT DO NOTHING`, older)
	require.NoError(t, err)
	f.putUnder(t, vaultConcept, vaultConcept+":vault-old", 1, `{"$ref":"memql-schema:fp-vault-old"}`,
		acmeIn(map[string]any{"label": "Old", "legacyKey": "legacy-secret-by-ref", "old": map[string]any{"note": "kept", "pin": "legacy-pin-by-ref"}}))
	// The same shape inline, as a row written before schemas were shared.
	f.putUnder(t, vaultConcept, vaultConcept+":vault-inline", 1, older,
		acmeIn(map[string]any{"label": "Inline", "legacyKey": "legacy-secret-inline", "old": map[string]any{"note": "kept", "pin": "legacy-pin-inline"}}))
	// A later version of the first, under the current schema.
	f.put(t, vaultConcept, vaultConcept+":vault-old", 2, "system", acmeIn(map[string]any{"label": "Now"}), nil)

	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	file := string(f.store.committed[res.ObjectKey])
	for _, secret := range []string{"legacy-secret-by-ref", "legacy-pin-by-ref", "legacy-secret-inline", "legacy-pin-inline"} {
		require.NotContains(t, file, secret)
	}
	var report accountexport.Report
	require.NoError(t, json.Unmarshal(res.Report, &report))
	require.Contains(t, report.ScrubbedFields, accountexport.ScrubbedField{Concept: vaultConcept, Field: "legacyKey"})
	require.Contains(t, report.ScrubbedFields, accountexport.ScrubbedField{Concept: vaultConcept, Field: "old.pin"})
	for _, v := range partLines(t, members(t, f.store.committed[res.ObjectKey]), "rows/testexport/vault") {
		if old, ok := v["payload"].(map[string]any)["old"]; ok {
			require.Equal(t, map[string]any{"note": "kept"}, old)
		}
	}
}

func TestExportRunFailsOnAVersionWhoseSchemaIsNotStored(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.putUnder(t, vaultConcept, vaultConcept+":vault-lost", 1, `{"$ref":"memql-schema:fp-nowhere"}`, acmeIn(map[string]any{"label": "Lost"}))
	_, err := f.run(t, f.export(acme, false), "")
	require.ErrorContains(t, err, "fp-nowhere")
	f.requireNothingLeft(t)
}

func TestExportRunNeverTakesATitleFromASecretField(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	f.withConcepts(markedSecret(t, "v1:agents:agent", "name"))
	f.put(t, "v1:agents:agent", "v1:agents:agent:a-cluster", 1, "system", map[string]any{"name": "SECRET-AGENT-NAME"}, nil)
	f.put(t, "v1:agents:agentAuthorization", "v1:agents:agentAuthorization:z1", 20, "system", acmeIn(map[string]any{"agentId": "v1:agents:agent:a-cluster"}), nil)
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.NotContains(t, string(f.store.committed[res.ObjectKey]), "SECRET-AGENT-NAME")
	ref := refsByID(t, members(t, f.store.committed[res.ObjectKey]))["v1:agents:agent:a-cluster"]
	require.Equal(t, "cluster", ref["provided"])
	require.NotContains(t, ref, "name")
}

func TestExportRunNeverReadsAKindThatStays(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	agent, err := memorynodes.Get("v1:agents:agent")
	require.NoError(t, err)
	stays := *agent
	decl := *agent.BelongsTo
	decl.Stays = "signin"
	stays.BelongsTo = &decl
	require.True(t, stays.SharedRows())
	f.withConcepts(&stays)
	cq := &countingQuerier{}
	f.work.wrapQuerier = func(q memql.AccountQuerier) memql.AccountQuerier { cq.inner = q; return cq }
	f.put(t, "v1:agents:agent", "v1:agents:agent:a-cluster", 1, "system", map[string]any{"name": "STAYING-AGENT"}, nil)
	f.put(t, "v1:agents:agentAuthorization", "v1:agents:agentAuthorization:z1", 20, "system", acmeIn(map[string]any{"agentId": "v1:agents:agent:a-cluster"}), nil)
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.NotContains(t, string(f.store.committed[res.ObjectKey]), "STAYING-AGENT")
	ref := refsByID(t, members(t, f.store.committed[res.ObjectKey]))["v1:agents:agent:a-cluster"]
	require.Equal(t, map[string]any{"concept": "v1:agents:agent", "id": "v1:agents:agent:a-cluster"}, ref["to"])
	require.Equal(t, "outside", ref["provided"])
	require.False(t, cq.named("a-cluster"), "a row of a kind that stays was read")
}

func TestExportRunListsNoReferenceToACarriedRowWhateverItsKind(t *testing.T) {
	f := newRunFixture(t)
	f.seedAcme(t)
	// The package field names the carried note: another kind than its target.
	f.put(t, "v1:platform:site", "v1:platform:site:s7", 20, "system", acmeIn(map[string]any{"name": "Points home", "packageId": noteID}), nil)
	res, err := f.run(t, f.export(acme, false), "")
	require.NoError(t, err)
	require.NotContains(t, refsByID(t, members(t, f.store.committed[res.ObjectKey])), noteID)
}
