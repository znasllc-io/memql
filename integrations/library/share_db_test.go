package library

// File sharing between people of different ranks (owner-approved 2026-11-18),
// against a real database and two engines standing in for two replicas: every
// share is written on one and read, downloaded and copied on the other.
//
// The people, all in one run's prefix:
//
//	alice  owner (cluster owner)   org A
//	vera   viewer                  org A
//	adam   admin                   org A
//	mike   member (writer)         org A, and a CUSTOM group with bob
//	bob    member (writer)         org B
//
// Org A and org B are groups of kind `account`; the custom group proves a
// member's directory follows the organization and not any group.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/libraryfile"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/library/sharestore"
)

// shareBlob is the storage both replicas share: one map of objects by URL.
type shareBlob struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (b *shareBlob) url(bucket, key string) string {
	return "https://store.example/" + bucket + "/" + key
}

func (b *shareBlob) put(bucket, key string, data []byte) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.url(bucket, key)
	b.data[u] = bytes.Clone(data)
	return u
}

func (b *shareBlob) CreateVerifiedStream(_ context.Context, bucket, key string, r io.Reader, size int64, digest, _ string) (azureblob.VerifiedBlob, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return azureblob.VerifiedBlob{}, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(sum[:]) != digest {
		return azureblob.VerifiedBlob{}, fmt.Errorf("verified write mismatch")
	}
	return azureblob.VerifiedBlob{URL: b.put(bucket, key, data), ETag: "etag", SHA256: digest, Size: size}, nil
}

func (b *shareBlob) DownloadStreamURL(_ context.Context, u string) (io.ReadCloser, error) {
	data, err := b.DownloadURL(context.Background(), u)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *shareBlob) DownloadURL(_ context.Context, u string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.data[u]
	if !ok {
		return nil, fmt.Errorf("missing blob %s", u)
	}
	return bytes.Clone(data), nil
}

type shareFixture struct {
	t      *testing.T
	f      *revisionDB
	db     *bun.DB
	blob   *shareBlob
	prefix string
	ids    map[string]string // person -> bare user id
}

const shareBucket = "library"

func newShareFixture(t *testing.T, f *revisionDB) *shareFixture {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	x := &shareFixture{t: t, f: f, db: db, blob: &shareBlob{data: map[string][]byte{}},
		prefix: fmt.Sprintf("share%d", time.Now().UnixNano()), ids: map[string]string{}}
	for _, lib := range []*Integration{f.first, f.second} {
		lib.SetBlobFetcher(x.blob)
		lib.SetShareCopyStorage(x.blob, shareBucket)
		lib.SetExtractor(shareText{})
		// Each replica its own guard, as each node builds one, over one table.
		lib.SetSaveClaims(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, nil).StrictClaimer())
	}
	for _, p := range []string{"alice", "vera", "adam", "mike", "bob"} {
		x.ids[p] = x.prefix + "-" + p
		x.insert("v1:identity:user", "v1:identity:user:"+x.ids[p], map[string]any{
			"active": true, "displayName": x.name(p), "primaryEmail": x.ids[p] + "@example.test", "role": "writer",
		})
	}
	orgA, orgB, custom := x.group("orga", "account"), x.group("orgb", "account"), x.group("mixed", "custom")
	for _, p := range []string{"alice", "vera", "adam", "mike"} {
		x.member(orgA, p)
	}
	x.member(orgB, "bob")
	x.member(custom, "mike")
	x.member(custom, "bob")
	return x
}

func (x *shareFixture) name(p string) string { return strings.ToUpper(p[:1]) + p[1:] + " " + x.prefix }

func (x *shareFixture) insert(concept, id string, payload map[string]any) {
	x.t.Helper()
	raw, _ := json.Marshal(payload)
	schema, _ := json.Marshal(map[string]any{"$id": concept})
	row := memorynodes.MemoryNode{ID: id, CreatedAt: time.Now().UTC(), CreatedBy: "system:share-test", Concept: concept,
		Type: memorynodes.NodeTypeObject, Schema: schema, Payload: raw, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	if _, err := x.db.NewInsert().Model(&row).Exec(context.Background()); err != nil {
		x.t.Fatal(err)
	}
	x.t.Cleanup(func() {
		_, _ = x.db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", concept).Where("id = ?", id).Exec(context.Background())
	})
}

func (x *shareFixture) group(name, kind string) string {
	id := "v1:identity:group:" + x.prefix + "-" + name
	x.insert("v1:identity:group", id, map[string]any{"ownerUserId": "", "name": name + " " + x.prefix, "kind": kind, "status": "active"})
	return id
}

func (x *shareFixture) member(group, person string) {
	id := "v1:identity:groupMembership:" + memql.BareShortId(group) + "-" + x.ids[person]
	x.insert("v1:identity:groupMembership", id, map[string]any{
		"ownerUserId": "", "groupId": group, "userId": "v1:identity:user:" + x.ids[person], "origin": "added", "status": "active",
	})
}

func (x *shareFixture) as(person string, role auth.Role) context.Context {
	return revisionActor(x.ids[person], role)
}

func (x *shareFixture) alice() context.Context { return x.as("alice", auth.RoleOwner) }
func (x *shareFixture) vera() context.Context  { return x.as("vera", auth.RoleReader) }
func (x *shareFixture) adam() context.Context  { return x.as("adam", auth.RoleAdmin) }
func (x *shareFixture) mike() context.Context  { return x.as("mike", auth.RoleWriter) }
func (x *shareFixture) bob() context.Context   { return x.as("bob", auth.RoleWriter) }

// file puts a text file in a person's Library: its bytes, its row and its index row.
func (x *shareFixture) file(ctx context.Context, person, name, body string) string {
	x.t.Helper()
	fileID := fmt.Sprintf("%s-file-%d", x.ids[person], time.Now().UnixNano())
	data := []byte(body)
	sum := sha256.Sum256(data)
	u := x.blob.put(shareBucket, "library/"+x.ids[person]+"/"+fileID+"/"+name, data)
	if err := libraryfile.NewStore(shareEngine{x.f.engine}).Create(ctx, map[string]any{
		"fileId": fileID, "name": name, "mimeType": "text/plain", "size": len(data), "sha256": hex.EncodeToString(sum[:]),
		"blobUrl": u, "source": "uploaded", "format": "text",
	}); err != nil {
		x.t.Fatal(err)
	}
	row := x.f.query(x.f.engine, ctx, "mutation", "createArtifact", map[string]any{
		"sourceConceptRef": server.LibraryFileConceptRef(fileID), "ownerUserId": x.ids[person], "lens": "artifact",
		"kind": "file", "source": "uploaded", "title": name, "format": "text", "mimeType": "text/plain", "archived": false,
	})[0]
	return memql.BareShortId(asString(row["id"]))
}

func (x *shareFixture) share(lib *Integration, ctx context.Context, artifact, recipient string, days int) (map[string]any, error) {
	rows, err := lib.handleShareFile(ctx, map[string]any{"artifactId": artifact, "recipientUserId": x.ids[recipient], "days": days}, 0)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(rows[0].Payload, &out)
	return out, nil
}

func (x *shareFixture) mustShare(lib *Integration, ctx context.Context, artifact, recipient string, days int) map[string]any {
	x.t.Helper()
	out, err := x.share(lib, ctx, artifact, recipient, days)
	if err != nil {
		x.t.Fatalf("share with %s: %v", recipient, err)
	}
	return out
}

func (x *shareFixture) rows(e *memql.MemQLEngine, ctx context.Context, name string, args map[string]any) []map[string]any {
	x.t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		x.t.Fatal(err)
	}
	raw, err := e.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		x.t.Fatalf("%s: %v", name, err)
	}
	return extractRows(raw)
}

// download asks the content route on the given replica, as ctx's caller.
func (x *shareFixture) download(e *memql.MemQLEngine, ctx context.Context, artifact, query string) (int, []byte) {
	h := server.NewArtifactHandler(server.ArtifactHandlerOptions{
		Store: server.NewEngineLibraryStore(shareEngine{e}), Downloader: x.blob, Bucket: shareBucket,
	})
	req := httptest.NewRequest(http.MethodGet, "/artifacts/"+artifact+"/content"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// receipt unwraps a handler's one-row answer.
func receipt(t *testing.T) func([]memorynodes.MemoryNode, error) map[string]any {
	return func(rows []memorynodes.MemoryNode, err error) map[string]any { return historyResult(t, rows, err) }
}

func (x *shareFixture) directory(ctx context.Context) map[string]string {
	x.t.Helper()
	raw, err := x.f.other.Execute(ctx, "builtin libraryShareDirectory()")
	if err != nil {
		x.t.Fatal(err)
	}
	out := map[string]string{}
	for _, row := range extractRows(raw) {
		// By display name only: no email reaches anybody, whatever their rank.
		blob, _ := json.Marshal(row)
		_, primary := row["primaryEmail"]
		_, email := row["email"]
		_, detail := row["detail"]
		// The fixture's own people only: other rows in a shared database are
		// other tests', and their display names are theirs to choose.
		ours := strings.HasPrefix(memql.BareShortId(asString(row["userId"])), x.prefix)
		if (ours && strings.Contains(string(blob), "@example.test")) || primary || email || detail {
			x.t.Fatalf("the directory carries an email: %s", blob)
		}
		out[memql.BareShortId(asString(row["userId"]))] = asString(row["displayName"])
	}
	return out
}

func noticesFor(rows []map[string]any, artifact string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if memql.BareShortId(asString(r["artifactId"])) == artifact {
			out = append(out, r)
		}
	}
	return out
}

func TestFileSharingBetweenRanksDB(t *testing.T) {
	f := newRevisionDB(t)
	if f == nil {
		return
	}
	x := newShareFixture(t, f)

	t.Run("OwnerSharesWithViewerAcrossReplicas", func(t *testing.T) { x.t = t; testOwnerSharesWithViewer(t, x) })
	t.Run("ViewerSharesWithAdminInTheirOrganization", func(t *testing.T) { x.t = t; testViewerSharesWithAdmin(t, x) })
	t.Run("DirectoryFollowsTheOrganization", func(t *testing.T) { x.t = t; testDirectoryFollowsTheOrganization(t, x) })
	t.Run("RefusedShares", func(t *testing.T) { x.t = t; testRefusedShares(t, x) })
	t.Run("RenewAndShareAgainAfterStop", func(t *testing.T) { x.t = t; testRenewAndShareAgain(t, x) })
	t.Run("ExpiredShareRefuses", func(t *testing.T) { x.t = t; testExpiredShareRefuses(t, x) })
	t.Run("ArchiveEndsAccessAndNotice", func(t *testing.T) { x.t = t; testArchiveEndsShares(t, x) })
	t.Run("ABurstOfSavesMakesOneCopy", func(t *testing.T) { x.t = t; testBurstOfSavesMakesOneCopy(t, x) })
	t.Run("ABurstOfSharesMakesOneShare", func(t *testing.T) { x.t = t; testBurstOfSharesMakesOneShare(t, x) })
}

// burst fires n calls at once, alternating the two replicas, and answers how
// many succeeded and the errors of the rest.
func burst(n int, libs [2]*Integration, call func(*Integration) error) (int, []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	ok, refused := 0, []error{}
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := call(libs[k%2])
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				refused = append(refused, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return ok, refused
}

// José, Nov 19: twenty quick clicks on Save to my Library made eighteen
// copies; "even at 1,000 clicks a second it should only count as one click".
// The same save fired at once on both replicas makes one copy; one asked for
// after it finished is a new copy (no file is remembered).
func testBurstOfSavesMakesOneCopy(t *testing.T, x *shareFixture) {
	f := x.f
	name := "burst-" + x.prefix + ".txt"
	artifact := x.file(x.alice(), "alice", name, "saved once per click")
	x.mustShare(f.first, x.alice(), artifact, "vera", 7)
	copies := func() int {
		n := 0
		for _, row := range x.rows(f.other, x.vera(), "libraryArtifacts", nil) {
			if asString(row["title"]) == name {
				n++
			}
		}
		return n
	}
	save := func(lib *Integration) error {
		_, err := lib.handleSaveSharedCopy(x.vera(), map[string]any{"artifactId": artifact}, 0)
		return err
	}
	// The copy that wins is held at its write until the other nineteen have
	// answered, so none of them can arrive after it finished and count as a
	// later click (which would be a new save, rightly).
	hold, refusedSoFar := make(chan struct{}), 0
	var once sync.Once
	var mu sync.Mutex
	for _, lib := range []*Integration{f.first, f.second} {
		lib.SetShareCopyStorage(heldCopies{x.blob, hold}, shareBucket)
	}
	ok, refused := burst(20, [2]*Integration{f.first, f.second}, func(lib *Integration) error {
		err := save(lib)
		if err != nil {
			mu.Lock()
			if refusedSoFar++; refusedSoFar == 19 {
				once.Do(func() { close(hold) })
			}
			mu.Unlock()
		}
		return err
	})
	for _, lib := range []*Integration{f.first, f.second} {
		lib.SetShareCopyStorage(x.blob, shareBucket)
	}
	if ok != 1 || copies() != 1 {
		t.Fatalf("twenty saves at once: %d succeeded, %d copies in vera's Library; want 1 and 1 (refusals: %v)", ok, copies(), refused)
	}
	for _, err := range refused {
		if err.Error() != errSaveInFlight.Error() {
			t.Fatalf("a save refused for another reason than the one running: %v", err)
		}
	}
	// The burst is over, so its claim is given back: the next click is a new save.
	if err := save(f.second); err != nil || copies() != 2 {
		t.Fatalf("a save after the burst finished: %v, %d copies; want a second copy", err, copies())
	}
	// Another person's save of their own share is not held by vera's.
	x.mustShare(f.first, x.alice(), artifact, "adam", 7)
	release, err := f.first.oneSaveAtATime(x.vera(), x.ids["vera"], artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := f.second.handleSaveSharedCopy(x.adam(), map[string]any{"artifactId": artifact}, 0); err != nil {
		t.Fatalf("adam's save was held by vera's: %v", err)
	}
}

// libraryShareFile already answers a burst with one share (the file's version
// gate, then renew-or-new); this holds it to that.
func testBurstOfSharesMakesOneShare(t *testing.T, x *shareFixture) {
	f := x.f
	artifact := x.file(x.alice(), "alice", "burst-share.txt", "shared once")
	var mu sync.Mutex
	ids := map[string]bool{}
	ok, refused := burst(8, [2]*Integration{f.first, f.second}, func(lib *Integration) error {
		got, err := x.share(lib, x.alice(), artifact, "vera", 7)
		if err == nil {
			mu.Lock()
			ids[memql.BareShortId(asString(got["shareId"]))] = true
			mu.Unlock()
		}
		return err
	})
	shares := x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})
	if ok != 8 || len(ids) != 1 || len(shares) != 1 {
		t.Fatalf("eight shares at once: %d answered, %d share ids, %d active shares; want 8, 1, 1 (refusals: %v)", ok, len(ids), len(shares), refused)
	}
}

func testOwnerSharesWithViewer(t *testing.T, x *shareFixture) {
	f := x.f
	body := "quarterly numbers, owner to viewer"
	artifact := x.file(x.alice(), "alice", "q3.txt", body)
	got := x.mustShare(f.first, x.alice(), artifact, "vera", 7)
	shareID := asString(got["shareId"])
	expires, err := time.Parse(time.RFC3339Nano, asString(got["expiresAt"]))
	if err != nil || shareID == "" || got["days"] != float64(7) {
		t.Fatalf("share receipt: %v (%v)", got, err)
	}
	if d := time.Until(expires); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("a 7-day share expires in %v", d)
	}

	// The recipient's notice is on their own list, read on the OTHER replica.
	notices := noticesFor(x.rows(f.other, x.vera(), "librarySharedWithMe", nil), artifact)
	if len(notices) != 1 || asString(notices[0]["title"]) != "q3.txt" || asString(notices[0]["sharerDisplayName"]) != x.name("alice") ||
		memql.BareShortId(asString(notices[0]["shareId"])) != memql.BareShortId(shareID) {
		t.Fatalf("vera's shared-with-me list on the second replica: %v", notices)
	}
	// Nobody else sees the notice: it is vera's row.
	if len(noticesFor(x.rows(f.other, x.mike(), "librarySharedWithMe", nil), artifact)) != 0 {
		t.Fatal("a stranger sees vera's notice")
	}
	// The sharer's list says who has it.
	shares := x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})
	if len(shares) != 1 || memql.BareShortId(asString(shares[0]["recipientUserId"])) != x.ids["vera"] ||
		asString(shares[0]["recipientDisplayName"]) != x.name("vera") || shares[0]["days"] != float64(7) ||
		memql.BareShortId(asString(shares[0]["id"])) != memql.BareShortId(shareID) {
		t.Fatalf("alice's share list: %v", shares)
	}

	// The recipient downloads it through the content route on the second replica.
	if code, data := x.download(f.other, x.vera(), artifact, ""); code != http.StatusOK || string(data) != body {
		t.Fatalf("vera's download: %d %q", code, data)
	}
	if code, _ := x.download(f.other, x.vera(), artifact, "?version=1"); code != http.StatusNotFound {
		t.Fatalf("a shared read must not open the version read: %d", code)
	}
	// Strangers cannot, whatever their rank below cluster owner.
	for name, ctx := range map[string]context.Context{"mike": x.mike(), "adam": x.adam(), "bob": x.bob()} {
		if code, _ := x.download(f.other, ctx, artifact, ""); code != http.StatusNotFound {
			t.Fatalf("%s, who was not shared the file, downloaded it: %d", name, code)
		}
		if _, err := f.second.handleSaveSharedCopy(ctx, map[string]any{"artifactId": artifact}, 0); err == nil {
			t.Fatalf("%s, who was not shared the file, saved a copy", name)
		}
	}

	// What the recipient cannot do.
	if rows := x.rows(f.other, x.vera(), "libraryArtifactById", map[string]any{"artifactId": artifact}); len(rows) != 0 {
		t.Fatal("vera reads the owner's artifact through an ordinary Library query")
	}
	for _, r := range x.rows(f.other, x.vera(), "libraryArtifacts", nil) {
		if memql.BareShortId(asString(r["id"])) == artifact {
			t.Fatal("the shared file appears in vera's own Library list")
		}
	}
	sourceRef := asString(x.rows(f.engine, x.alice(), "libraryArtifactById", map[string]any{"artifactId": artifact})[0]["sourceConceptRef"])
	if rows := x.rows(f.other, x.vera(), "libraryFileById", map[string]any{"fileId": sourceRef}); len(rows) != 0 {
		t.Fatal("vera reads the owner's file row through an ordinary Library query")
	}
	for _, m := range []struct {
		name string
		args map[string]any
	}{
		{"archiveArtifact", map[string]any{"artifactId": artifact}},
		{"moveArtifactToFolder", map[string]any{"artifactId": artifact, "folderId": "elsewhere"}},
	} {
		call, _ := langparser.RenderCall(m.name, m.args)
		_, _ = f.other.Execute(x.vera(), "mutation "+call)
	}
	owned := x.rows(f.engine, x.alice(), "libraryArtifactById", map[string]any{"artifactId": artifact})
	if len(owned) != 1 || boolField(owned[0], "archived") || asString(owned[0]["folderId"]) == "elsewhere" {
		t.Fatalf("vera changed alice's artifact: %v", owned)
	}
	if _, err := x.share(f.second, x.vera(), artifact, "adam", 7); err == nil {
		t.Fatal("vera shared alice's file onward")
	}
	if _, err := f.second.handleStopShare(x.vera(), map[string]any{"shareId": shareID}, 0); err == nil {
		t.Fatal("the recipient stopped (and so could rewrite) the sharer's share")
	}
	// No client path writes the share or the notice: the named writes are
	// @serverOnly and the raw insert is refused by @serverWritten -- for the
	// recipient, and for the sharer too.
	far := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	for who, ctx := range map[string]context.Context{"vera": x.vera(), "alice": x.alice()} {
		for _, attempt := range []struct{ call, refusal string }{
			{fmt.Sprintf(`mutation putLibraryShare(shareId: %q, recipientUserId: %q, recipientDisplayName: "x", artifactId: %q, days: 30, expiresAt: %q)`, shareID, x.ids["vera"], artifact, far), "server-only"},
			{fmt.Sprintf(`mutation endLibraryShareNotice(shareId: %q)`, shareID), "server-only"},
			{fmt.Sprintf(`mutation putLibraryShareNotice(shareId: %q, artifactId: %q, sharerUserId: %q, sharerDisplayName: "x", title: "x", expiresAt: %q)`, shareID, artifact, x.ids["alice"], far), "server-only"},
			{fmt.Sprintf(`insert("v1:library:share", id=%q, payload={"recipientUserId":%q,"recipientDisplayName":"x","artifactId":%q,"days":30,"expiresAt":%q})`, "v1:library:share:"+shareID, x.ids["vera"], artifact, far), "@serverWritten"},
			{fmt.Sprintf(`insert("v1:library:shareNotice", id=%q, payload={"shareId":%q,"artifactId":%q,"sharerUserId":%q,"sharerDisplayName":"x","title":"x","expiresAt":%q})`, "v1:library:shareNotice:"+shareID, shareID, artifact, x.ids["alice"], far), "@serverWritten"},
		} {
			_, err := f.other.Execute(ctx, attempt.call)
			if err == nil || !strings.Contains(err.Error(), attempt.refusal) {
				t.Fatalf("%s's client write was not refused by %s: %v\n  %s", who, attempt.refusal, err, attempt.call)
			}
		}
	}
	shares = x.rows(f.engine, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})
	if len(shares) != 1 || shares[0]["days"] != float64(7) || asString(shares[0]["expiresAt"]) != asString(got["expiresAt"]) {
		t.Fatalf("a refused write changed the share: %v", shares)
	}

	// The recipient saves a copy: theirs, readable after the share ends.
	saved := receipt(t)(f.second.handleSaveSharedCopy(x.vera(), map[string]any{"artifactId": artifact}, 0))
	copyID := asString(saved["artifactId"])
	copyRows := x.rows(f.engine, x.vera(), "libraryArtifactById", map[string]any{"artifactId": copyID})
	if copyID == "" || copyID == artifact || len(copyRows) != 1 || memql.BareShortId(asString(copyRows[0]["ownerUserId"])) != x.ids["vera"] {
		t.Fatalf("vera's copy: %v %v", saved, copyRows)
	}
	if len(x.rows(f.engine, x.mike(), "libraryArtifactById", map[string]any{"artifactId": copyID})) != 0 {
		t.Fatal("somebody else reads vera's copy")
	}
	// The copy is read like an upload (Nov 18), so search by meaning finds it:
	// pieces of vera's own, and the file ends ready.
	copyFile := asString(saved["fileId"])
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		chunks := x.rows(f.engine, x.vera(), "libraryFileChunksForFile", map[string]any{"fileId": copyFile})
		file := x.rows(f.engine, x.vera(), "libraryFileById", map[string]any{"fileId": copyFile})
		if len(chunks) > 0 && len(file) == 1 && file[0]["status"] == "ready" {
			if !strings.Contains(asString(chunks[0]["text"]), body) {
				t.Fatalf("vera's copy was read as %q, want its text %q", chunks[0]["text"], body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("vera's copy was never read like an upload: %d piece(s), file %v", len(chunks), file)
		}
	}

	// Stop: access ends at once, on both replicas.
	stopped := receipt(t)(f.first.handleStopShare(x.alice(), map[string]any{"shareId": shareID}, 0))
	if stopped["stopped"] != true {
		t.Fatalf("stop receipt: %v", stopped)
	}
	for i, e := range []*memql.MemQLEngine{f.engine, f.other} {
		if code, _ := x.download(e, x.vera(), artifact, ""); code != http.StatusNotFound {
			t.Fatalf("replica %d served a stopped share: %d", i, code)
		}
		if len(noticesFor(x.rows(e, x.vera(), "librarySharedWithMe", nil), artifact)) != 0 {
			t.Fatalf("replica %d still lists a stopped share to its recipient", i)
		}
		if len(x.rows(e, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
			t.Fatalf("replica %d still lists a stopped share to its sharer", i)
		}
	}
	if _, err := f.second.handleSaveSharedCopy(x.vera(), map[string]any{"artifactId": artifact}, 0); err == nil {
		t.Fatal("vera saved a copy after the share was stopped")
	}
	if code, data := x.download(f.other, x.vera(), copyID, ""); code != http.StatusOK || string(data) != body {
		t.Fatalf("vera's own copy after the share stopped: %d %q", code, data)
	}
}

func testViewerSharesWithAdmin(t *testing.T, x *shareFixture) {
	f := x.f
	body := "a viewer's own notes"
	artifact := x.file(x.vera(), "vera", "notes.txt", body)
	if _, ok := x.directory(x.vera())[x.ids["adam"]]; !ok {
		t.Fatal("vera's directory does not offer adam, a member of her organization")
	}
	x.mustShare(f.first, x.vera(), artifact, "adam", 1)
	if code, data := x.download(f.other, x.adam(), artifact, ""); code != http.StatusOK || string(data) != body {
		t.Fatalf("adam's download of vera's file: %d %q", code, data)
	}
	if len(noticesFor(x.rows(f.other, x.adam(), "librarySharedWithMe", nil), artifact)) != 1 {
		t.Fatal("adam's shared-with-me list does not show vera's file")
	}
}

func testDirectoryFollowsTheOrganization(t *testing.T, x *shareFixture) {
	mine := x.directory(x.mike())
	want := map[string]string{x.ids["alice"]: x.name("alice"), x.ids["vera"]: x.name("vera"), x.ids["adam"]: x.name("adam")}
	if len(mine) != len(want) {
		t.Fatalf("mike's directory: %v, want exactly his organization %v", mine, want)
	}
	for id, name := range want {
		if mine[id] != name {
			t.Fatalf("mike's directory: %v, want %v", mine, want)
		}
	}
	everyone := x.directory(x.adam())
	for _, p := range []string{"alice", "vera", "mike", "bob"} {
		if everyone[x.ids[p]] != x.name(p) {
			t.Fatalf("an admin's directory is missing %s: %d people", p, len(everyone))
		}
	}
	if _, self := everyone[x.ids["adam"]]; self {
		t.Fatal("a directory offers its caller")
	}

	artifact := x.file(x.mike(), "mike", "plan.txt", "member's plan")
	if _, err := x.share(x.f.first, x.mike(), artifact, "bob", 7); err == nil || !strings.Contains(err.Error(), "people offered") {
		t.Fatalf("a member shared with someone outside their organization: %v", err)
	}
	if len(x.rows(x.f.engine, x.mike(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
		t.Fatal("a refused share wrote a share row")
	}
	if len(noticesFor(x.rows(x.f.engine, x.bob(), "librarySharedWithMe", nil), artifact)) != 0 {
		t.Fatal("a refused share wrote a notice")
	}
}

func testRefusedShares(t *testing.T, x *shareFixture) {
	f := x.f
	artifact := x.file(x.alice(), "alice", "refusals.txt", "refusals")
	for _, days := range []int{0, 2, 3, 14, 31, 365} {
		if _, err := x.share(f.first, x.alice(), artifact, "vera", days); err == nil {
			t.Fatalf("a %d-day share was accepted", days)
		}
	}
	if _, err := x.share(f.first, x.mike(), artifact, "vera", 7); err == nil {
		t.Fatal("mike shared a file he does not own")
	}
	if _, err := x.share(f.first, x.alice(), artifact, "alice", 7); err == nil {
		t.Fatal("alice shared a file with herself")
	}
	// Borrowed authority is not a person deciding to share.
	if _, err := x.share(f.first, auth.ContextWithUserActor(context.Background(), x.ids["alice"]), artifact, "vera", 7); err == nil {
		t.Fatal("borrowed authority shared a file")
	}
	if len(x.rows(f.engine, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
		t.Fatal("a refused share wrote a row")
	}
	x.f.query(f.engine, x.alice(), "mutation", "archiveArtifact", map[string]any{"artifactId": artifact})
	if _, err := x.share(f.first, x.alice(), artifact, "vera", 7); err == nil {
		t.Fatal("an archived file was shared")
	}
}

func testRenewAndShareAgain(t *testing.T, x *shareFixture) {
	f := x.f
	artifact := x.file(x.alice(), "alice", "renew.txt", "renewable")
	first := x.mustShare(f.first, x.alice(), artifact, "vera", 1)
	time.Sleep(10 * time.Millisecond)
	renewed := x.mustShare(f.second, x.alice(), artifact, "vera", 30)
	if renewed["shareId"] != first["shareId"] {
		t.Fatalf("a renewal made a second share: %v then %v", first, renewed)
	}
	if asString(renewed["expiresAt"]) <= asString(first["expiresAt"]) {
		t.Fatalf("a renewal did not move the expiry forward: %v -> %v", first["expiresAt"], renewed["expiresAt"])
	}
	// Renewing with the same lifetime moves the expiry forward too.
	time.Sleep(10 * time.Millisecond)
	again := x.mustShare(f.first, x.alice(), artifact, "vera", 30)
	if asString(again["expiresAt"]) <= asString(renewed["expiresAt"]) || again["shareId"] != first["shareId"] {
		t.Fatalf("renewing with the same days: %v -> %v", renewed, again)
	}
	shares := x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})
	if len(shares) != 1 || shares[0]["days"] != float64(30) || asString(shares[0]["expiresAt"]) != asString(again["expiresAt"]) {
		t.Fatalf("after renewals, alice's list: %v", shares)
	}
	notices := noticesFor(x.rows(f.other, x.vera(), "librarySharedWithMe", nil), artifact)
	if len(notices) != 1 || asString(notices[0]["expiresAt"]) != asString(again["expiresAt"]) {
		t.Fatalf("vera's notice after renewals: %v", notices)
	}

	// Stop, then share again: a FRESH share, the only active one; the stopped
	// one stays as the record, still stopped.
	receipt(t)(f.first.handleStopShare(x.alice(), map[string]any{"shareId": asString(first["shareId"])}, 0))
	if len(x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
		t.Fatal("a stopped share is still listed")
	}
	if code, _ := x.download(f.other, x.vera(), artifact, ""); code != http.StatusNotFound {
		t.Fatalf("download after the stop: %d", code)
	}
	back := x.mustShare(f.second, x.alice(), artifact, "vera", 10)
	if back["shareId"] == first["shareId"] {
		t.Fatalf("sharing again after a stop reused the stopped share: %v", back)
	}
	shares = x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})
	if len(shares) != 1 || shares[0]["days"] != float64(10) || asString(shares[0]["revokedAt"]) != "" ||
		memql.BareShortId(asString(shares[0]["id"])) != memql.BareShortId(asString(back["shareId"])) {
		t.Fatalf("after sharing again: %v", shares)
	}
	old := x.rows(f.other, x.alice(), "libraryShareById", map[string]any{"shareId": asString(first["shareId"])})
	if len(old) != 1 || asString(old[0]["revokedAt"]) == "" {
		t.Fatalf("the stopped share is not kept as stopped: %v", old)
	}
	notices = noticesFor(x.rows(f.other, x.vera(), "librarySharedWithMe", nil), artifact)
	if len(notices) != 1 || memql.BareShortId(asString(notices[0]["shareId"])) != memql.BareShortId(asString(back["shareId"])) {
		t.Fatalf("after sharing again, vera's list: %v", notices)
	}
	if code, _ := x.download(f.other, x.vera(), artifact, ""); code != http.StatusOK {
		t.Fatalf("download after sharing again: %d", code)
	}
}

func testExpiredShareRefuses(t *testing.T, x *shareFixture) {
	f := x.f
	artifact := x.file(x.alice(), "alice", "expired.txt", "expired")
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	shareID := "share-expired-" + x.prefix
	// An expired SHARE beside a notice that still claims an hour: the share decides.
	if err := sharestore.Write(x.alice(), f.engine, "putLibraryShare", map[string]any{
		"shareId": shareID, "recipientUserId": x.ids["vera"], "recipientDisplayName": x.name("vera"),
		"artifactId": artifact, "days": 1, "expiresAt": past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sharestore.Write(auth.ContextWithUserActor(x.alice(), x.ids["vera"]), f.engine, "putLibraryShareNotice", map[string]any{
		"shareId": shareID, "artifactId": artifact, "sharerUserId": x.ids["alice"], "sharerDisplayName": x.name("alice"),
		"title": "expired.txt", "expiresAt": future,
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := x.download(f.other, x.vera(), artifact, ""); code != http.StatusNotFound {
		t.Fatalf("an expired share served its file: %d", code)
	}
	if _, err := f.second.handleSaveSharedCopy(x.vera(), map[string]any{"artifactId": artifact}, 0); err == nil {
		t.Fatal("an expired share let its recipient save a copy")
	}
	if len(x.rows(f.other, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
		t.Fatal("an expired share is listed as active")
	}
}

func testArchiveEndsShares(t *testing.T, x *shareFixture) {
	f := x.f
	artifact := x.file(x.alice(), "alice", "archived.txt", "soon archived")
	x.mustShare(f.first, x.alice(), artifact, "vera", 7)
	x.mustShare(f.first, x.alice(), artifact, "adam", 7)
	x.f.query(f.engine, x.alice(), "mutation", "archiveArtifact", map[string]any{"artifactId": artifact})
	// Access ends with the archive itself, before any automation runs.
	if code, _ := x.download(f.other, x.vera(), artifact, ""); code != http.StatusNotFound {
		t.Fatalf("an archived file was served to its recipient: %d", code)
	}
	if _, err := f.second.handleSaveSharedCopy(x.vera(), map[string]any{"artifactId": artifact}, 0); err == nil {
		t.Fatal("an archived file was copied")
	}
	args := map[string]any{"artifactId": artifact, "ownerUserId": x.ids["alice"]}
	// Only the engine's own automation runs the cascade.
	for who, ctx := range map[string]context.Context{"alice": auth.ContextWithInternalOrigin(x.alice()),
		"another automation": auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginEngine, "archiveFileOnArtifactArchive")))} {
		if _, err := f.second.handleEndSharesForArtifact(ctx, args, 0); err == nil {
			t.Fatalf("%s ran the archive cascade", who)
		}
	}
	automation := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginEngine, endSharesAutomation)))
	ended := receipt(t)(f.second.handleEndSharesForArtifact(automation, args, 0))
	if ended["ended"] != float64(2) {
		t.Fatalf("the cascade ended %v shares, want 2", ended["ended"])
	}
	for _, p := range []string{"vera", "adam"} {
		if len(noticesFor(x.rows(f.engine, x.as(p, auth.RoleReader), "librarySharedWithMe", nil), artifact)) != 0 {
			t.Fatalf("%s still lists an archived file", p)
		}
	}
	if len(x.rows(f.engine, x.alice(), "librarySharesForArtifact", map[string]any{"artifactId": artifact})) != 0 {
		t.Fatal("an archived file still has active shares")
	}
	// A file that is not archived is left alone, whatever the automation is told.
	live := x.file(x.alice(), "alice", "live.txt", "still here")
	x.mustShare(f.first, x.alice(), live, "vera", 7)
	if got := receipt(t)(f.second.handleEndSharesForArtifact(automation, map[string]any{"artifactId": live, "ownerUserId": x.ids["alice"]}, 0)); got["ended"] != float64(0) {
		t.Fatalf("the cascade ended shares of a live file: %v", got)
	}
	if len(noticesFor(x.rows(f.engine, x.vera(), "librarySharedWithMe", nil), live)) != 1 {
		t.Fatal("the cascade touched a live file's share")
	}
}

// heldCopies writes a copy only once hold is closed (or ten seconds pass, so a
// bug that lets two saves through fails the test instead of hanging it).
type heldCopies struct {
	*shareBlob
	hold <-chan struct{}
}

func (h heldCopies) CreateVerifiedStream(ctx context.Context, bucket, key string, r io.Reader, size int64, digest, mimeType string) (azureblob.VerifiedBlob, error) {
	select {
	case <-h.hold:
	case <-time.After(10 * time.Second):
	}
	return h.shareBlob.CreateVerifiedStream(ctx, bucket, key, r, size, digest, mimeType)
}

// shareText reads a stored file as the text it holds, so a saved copy's
// reading pass has pieces to write.
type shareText struct{}

func (shareText) Extract(_ context.Context, _ string, data []byte) (string, error) {
	return string(data), nil
}
