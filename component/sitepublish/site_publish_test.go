package sitepublish

// site_publish_test.go -- memql#4345.
//
// Two levels, deliberately, because either alone is a false signal:
//
//   - BEHAVIOUR, against a fake engine: every refusal names itself, the happy
//     path uploads under a fresh version prefix and flips the row with the
//     artifactId attached.
//   - THE STATEMENTS THEMSELVES, against the REAL MemQL front end. A fake
//     engine that records query strings and parses nothing is the failure mode
//     memql#4256 documents: whole features have shipped failing at parse in
//     production with their unit tests green. TestSitePublishStatementsResolve
//     takes the strings THIS FILE'S OWN HAPPY PATH PRODUCED -- not a
//     hand-copied table that could drift from them -- and runs each through a
//     real engine's parser and function registry.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concepts "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// publishStubEngine answers the four named reads the capability issues and
// records every statement it was handed. Rows are keyed by (query name, id)
// and it applies NO authorization of its own beyond what a test seeds --
// which is the point: the cross-user case is modelled the way the engine
// really behaves, by the owner-scoped query returning ZERO rows.
type publishStubEngine struct {
	mu    sync.Mutex
	calls []string

	sites     map[string]map[string]any
	artifacts map[string]map[string]any
	files     map[string]map[string]any

	// failOn, when non-empty, makes Execute return an error for any
	// statement naming this construct.
	failOn string
}

func newPublishStubEngine() *publishStubEngine {
	return &publishStubEngine{
		sites:     map[string]map[string]any{},
		artifacts: map[string]map[string]any{},
		files:     map[string]map[string]any{},
	}
}

var publishCallNameRe = regexp.MustCompile(`^(?:query|mutation)\s+([A-Za-z0-9_]+)\(`)
var publishStringArgRe = regexp.MustCompile(`([A-Za-z0-9_]+):\s*"((?:[^"\\]|\\.)*)"`)

func (s *publishStubEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, query)
	s.mu.Unlock()

	m := publishCallNameRe.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("stub: unparseable statement %q", query)
	}
	name := m[1]
	if s.failOn != "" && name == s.failOn {
		return nil, fmt.Errorf("stub: %s failed", name)
	}
	args := map[string]string{}
	for _, a := range publishStringArgRe.FindAllStringSubmatch(query, -1) {
		if _, dup := args[a[1]]; !dup {
			args[a[1]] = strings.ReplaceAll(a[2], `\"`, `"`)
		}
	}

	switch name {
	case "siteById":
		return rowsOf(s.sites[args["siteId"]]), nil
	case "libraryArtifactById":
		return rowsOf(s.artifacts[args["artifactId"]]), nil
	case "libraryFileById":
		return rowsOf(s.files[args["fileId"]]), nil
	case "updateSiteBundle":
		row, ok := s.sites[args["siteId"]]
		if !ok {
			return nil, fmt.Errorf("stub: updateSiteBundle on unknown site %q", args["siteId"])
		}
		row["bundleRef"] = args["bundleRef"]
		row["artifactId"] = args["artifactId"]
		return rowsOf(nil), nil
	case "createAuditEvent":
		return rowsOf(nil), nil
	}
	return nil, fmt.Errorf("stub: unexpected construct %q", name)
}

func (s *publishStubEngine) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *publishStubEngine) statementsNaming(construct string) []string {
	var out []string
	for _, c := range s.statements() {
		if strings.HasPrefix(c, "query "+construct+"(") || strings.HasPrefix(c, "mutation "+construct+"(") {
			out = append(out, c)
		}
	}
	return out
}

// rowsOf wraps zero or one row in the *ExecuteResult shape extractRows walks.
func rowsOf(row map[string]any) *memql.ExecuteResult {
	if row == nil {
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
	}
	fields := map[string]any{}
	for k, v := range row {
		fields[k] = v
	}
	st, _ := structpb.NewStruct(fields)
	id, _ := row["id"].(string)
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
		Id:      id,
		Payload: st,
	}}}}
}

// publishMemStore is the in-memory object store: reader and writer over ONE
// map, so a mismatch between the key written and the key read cannot hide.
type publishMemStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newPublishMemStore() *publishMemStore {
	return &publishMemStore{objects: map[string][]byte{}}
}

func (m *publishMemStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("no object at %q", key)
	}
	return append([]byte(nil), data...), nil
}

func (m *publishMemStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *publishMemStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	fixtureUser     = "u-owner"
	fixtureSiteId   = "site-1"
	fixtureArtifact = "artifact-1"
	fixtureFileId   = "file-1"
	fixtureBlobKey  = "library/u-owner/file-1/dist.zip"
)

// zipOf builds a zip in memory. Entries are written in map-key order after a
// sort, so the bytes are deterministic across runs.
func zipOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("zip create %q: %v", n, err)
		}
		if _, err := io.WriteString(w, entries[n]); err != nil {
			t.Fatalf("zip write %q: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// publishFixture is a wired-up capability plus the three fake backends, with
// one owner, one site, one file-kind artifact and one zip already in storage.
type publishFixture struct {
	integration *SitePublishIntegration
	engine      *publishStubEngine
	store       *publishMemStore
	ctx         context.Context
}

func newPublishFixture(t *testing.T, siteKind string, zipBytes []byte) *publishFixture {
	t.Helper()
	eng := newPublishStubEngine()
	eng.sites[fixtureSiteId] = map[string]any{
		"id":          "v1:platform:site:" + fixtureSiteId,
		"ownerUserId": fixtureUser,
		"hostname":    "shop.example.com",
		"kind":        siteKind,
		"status":      "live",
		"bundleRef":   "blob://sites/site-1/vprevious/",
	}
	eng.artifacts[fixtureArtifact] = map[string]any{
		"id":               "v1:library:artifact:" + fixtureArtifact,
		"ownerUserId":      fixtureUser,
		"kind":             "file",
		"sourceConceptRef": "v1:library:file:" + fixtureFileId,
		"archived":         false,
	}
	eng.files[fixtureFileId] = map[string]any{
		"id":          "v1:library:file:" + fixtureFileId,
		"ownerUserId": fixtureUser,
		"name":        "dist.zip",
		"mimeType":    "application/zip",
		"blobUrl":     fixtureBlobKey,
		"archived":    false,
	}

	store := newPublishMemStore()
	if zipBytes != nil {
		store.objects[fixtureBlobKey] = zipBytes
	}

	i := NewSitePublishIntegration(eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	i.newStore = func(context.Context) (siteObjectStore, error) {
		return siteObjectStore{reader: store, writer: store}, nil
	}

	return &publishFixture{
		integration: i,
		engine:      eng,
		store:       store,
		ctx:         auth.ContextWithUserActor(context.Background(), fixtureUser),
	}
}

func (f *publishFixture) run(t *testing.T) ([]map[string]any, error) {
	t.Helper()
	nodes, err := f.integration.handlePublishFromArtifact(f.ctx, map[string]any{
		"siteId":     fixtureSiteId,
		"artifactId": fixtureArtifact,
	}, 0)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, n := range nodes {
		out = append(out, map[string]any{"id": n.ID, "payload": string(n.Payload)})
	}
	return out, nil
}

// wantRefusal asserts the error is a refusal with exactly the named reason.
func wantRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s refusal, got no error", reason)
	}
	if got := refusalReason(err); got != reason {
		t.Fatalf("refusal reason = %q, want %q (error: %v)", got, reason, err)
	}
}

// goodBundle is a minimal but realistic spa build.
func goodBundle() map[string]string {
	return map[string]string{
		"index.html":     "<!doctype html><title>fixture</title>",
		"assets/app.js":  "console.log('hi')",
		"assets/app.css": "body{}",
	}
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestSitePublishFromArtifact_PublishesAndStampsTheArtifact(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))

	if _, err := f.run(t); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// The row was flipped AND carries the artifact provenance, from the same
	// write -- if artifactId were omitted, the previous artifact's id would
	// silently stay on a row now serving different bytes.
	row := f.engine.sites[fixtureSiteId]
	ref, _ := row["bundleRef"].(string)
	if !strings.HasPrefix(ref, "blob://sites/"+fixtureSiteId+"/v") {
		t.Fatalf("bundleRef = %q, want the new version's ref", ref)
	}

	// Every file landed under the version prefix the row now names, and the
	// store holds NOTHING else beyond the source zip -- so a key the serving
	// path would look for and a key the publisher wrote cannot silently be
	// two different strings.
	prefix := strings.TrimPrefix(ref, "blob://")
	wantKeys := []string{fixtureBlobKey}
	for _, name := range []string{"assets/app.css", "assets/app.js", "index.html"} {
		wantKeys = append(wantKeys, prefix+name)
	}
	sort.Strings(wantKeys)
	if got := f.store.keys(); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("stored objects =\n  %v\nwant\n  %v", got, wantKeys)
	}
	if row["artifactId"] != fixtureArtifact {
		t.Errorf("artifactId on the row = %v, want %q", row["artifactId"], fixtureArtifact)
	}

	// And it was audited.
	audits := f.engine.statementsNaming("createAuditEvent")
	if len(audits) != 1 {
		t.Fatalf("wrote %d audit events, want 1", len(audits))
	}
	for _, want := range []string{`action: "site_publish_from_artifact"`, `outcome: "success"`, fixtureSiteId, fixtureArtifact} {
		if !strings.Contains(audits[0], want) {
			t.Errorf("the audit statement does not carry %q:\n  %s", want, audits[0])
		}
	}
}

// The zip's __MACOSX sidecar tree and .DS_Store are dropped rather than
// published or refused -- a person zipping a build folder on a Mac has not
// done anything wrong.
func TestSitePublishFromArtifact_DropsArchiverCruft(t *testing.T) {
	entries := goodBundle()
	entries["__MACOSX/._index.html"] = "resource fork"
	entries[".DS_Store"] = "finder"
	entries["assets/.DS_Store"] = "finder"
	f := newPublishFixture(t, "spa", zipOf(t, entries))

	if _, err := f.run(t); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for _, k := range f.store.keys() {
		if strings.Contains(k, "__MACOSX") || strings.HasSuffix(k, ".DS_Store") {
			t.Errorf("archiver cruft was published: %q", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Refusals -- each one NAMED
// ---------------------------------------------------------------------------

// A caller who owns neither row sees the owner-scoped query return zero rows,
// which is what the real engine does. Two independent ownership questions, so
// both directions are exercised.
func TestSitePublishFromArtifact_CrossUserCallsAreRefused(t *testing.T) {
	t.Run("another user's site", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		delete(f.engine.sites, fixtureSiteId) // owner-scoped read: not visible
		_, err := f.run(t)
		wantRefusal(t, err, reasonSiteNotFound)
		if got := len(f.store.keys()); got != 1 {
			t.Errorf("a refused publish uploaded %d extra object(s)", got-1)
		}
	})
	t.Run("another user's artifact", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		delete(f.engine.artifacts, fixtureArtifact)
		_, err := f.run(t)
		wantRefusal(t, err, reasonArtifactNotFound)
	})
	t.Run("another user's backing file", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		delete(f.engine.files, fixtureFileId)
		_, err := f.run(t)
		wantRefusal(t, err, reasonFileNotFound)
	})
}

// A refusal is audited too, carrying the reason -- otherwise a run of failed
// cross-user attempts leaves no trace at all.
func TestSitePublishFromArtifact_RefusalsAreAudited(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	delete(f.engine.sites, fixtureSiteId)

	if _, err := f.run(t); err == nil {
		t.Fatal("want a refusal")
	}
	audits := f.engine.statementsNaming("createAuditEvent")
	if len(audits) != 1 {
		t.Fatalf("wrote %d audit events for a refusal, want 1", len(audits))
	}
	for _, want := range []string{`outcome: "blocked"`, `failureReason: "` + reasonSiteNotFound + `"`} {
		if !strings.Contains(audits[0], want) {
			t.Errorf("the audit statement does not carry %q:\n  %s", want, audits[0])
		}
	}
}

func TestSitePublishFromArtifact_NonZipArtifactIsRefused(t *testing.T) {
	for _, mime := range []string{"application/pdf", "text/plain", "image/png", "application/octet-stream", ""} {
		t.Run(mime, func(t *testing.T) {
			f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
			f.engine.files[fixtureFileId]["mimeType"] = mime
			_, err := f.run(t)
			wantRefusal(t, err, reasonArtifactNotAZip)
		})
	}
	// The instrument moves: the same fixture with a zip MIME publishes, so
	// the refusals above are about the type and not about the fixture being
	// broken in some other way.
	t.Run("application/zip publishes", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		if _, err := f.run(t); err != nil {
			t.Fatalf("the control case must publish: %v", err)
		}
	})
	// The common client-sent variants are accepted.
	for _, mime := range []string{"application/x-zip-compressed", "APPLICATION/ZIP", "application/zip; charset=binary"} {
		t.Run(mime, func(t *testing.T) {
			f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
			f.engine.files[fixtureFileId]["mimeType"] = mime
			if _, err := f.run(t); err != nil {
				t.Errorf("mimeType %q was refused: %v", mime, err)
			}
		})
	}
}

// An artifact whose index row is not a file, or whose backing ref names some
// other concept, cannot carry bytes.
func TestSitePublishFromArtifact_NonFileArtifactIsRefused(t *testing.T) {
	t.Run("kind is not file", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		f.engine.artifacts[fixtureArtifact]["kind"] = "note"
		_, err := f.run(t)
		wantRefusal(t, err, reasonArtifactNotAFile)
	})
	t.Run("sourceConceptRef names another concept", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		f.engine.artifacts[fixtureArtifact]["sourceConceptRef"] = "v1:notes:note:n-1"
		_, err := f.run(t)
		wantRefusal(t, err, reasonArtifactNotAFile)
	})
	t.Run("archived artifact", func(t *testing.T) {
		f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
		f.engine.artifacts[fixtureArtifact]["archived"] = true
		_, err := f.run(t)
		wantRefusal(t, err, reasonArtifactArchived)
	})
}

// index.html at the ROOT, for the two kinds that need it. The commonest
// mistake -- zipping the folder rather than its contents -- must be named.
func TestSitePublishFromArtifact_MissingRootIndexIsRefused(t *testing.T) {
	nested := map[string]string{
		"dist/index.html":    "<!doctype html>",
		"dist/assets/app.js": "console.log(1)",
	}
	for _, kind := range []string{"spa", "shopify_storefront"} {
		t.Run(kind, func(t *testing.T) {
			f := newPublishFixture(t, kind, zipOf(t, nested))
			_, err := f.run(t)
			wantRefusal(t, err, reasonBundleNoIndex)
			if !strings.Contains(err.Error(), "dist/") {
				t.Errorf("the refusal should name what the zip's top level actually holds: %v", err)
			}
		})
	}
	// A storefront bundle WITH a root index publishes, so the refusals above
	// are about the missing index and not about the kind being rejected.
	t.Run("storefront with a root index publishes", func(t *testing.T) {
		f := newPublishFixture(t, "shopify_storefront", zipOf(t, goodBundle()))
		if _, err := f.run(t); err != nil {
			t.Fatalf("the control case must publish: %v", err)
		}
	})
}

func TestSitePublishFromArtifact_OversizedBundlesAreRefused(t *testing.T) {
	t.Run("one file over the per-file cap", func(t *testing.T) {
		entries := goodBundle()
		// Highly compressible, so the ZIP stays small while the EXPANSION is
		// over the cap -- which is the shape of the attack the check exists
		// for, not merely a big file.
		entries["assets/huge.bin"] = strings.Repeat("A", sitePublishMaxFileBytes+1)
		f := newPublishFixture(t, "spa", zipOf(t, entries))
		_, err := f.run(t)
		wantRefusal(t, err, reasonBundleFileTooBig)
		if got := len(f.store.keys()); got != 1 {
			t.Errorf("a refused publish uploaded %d object(s); nothing may be written before validation passes", got-1)
		}
	})
	t.Run("too many files", func(t *testing.T) {
		entries := map[string]string{"index.html": "<!doctype html>"}
		for n := 0; n < sitePublishMaxFileCount+5; n++ {
			entries[fmt.Sprintf("a/%05d.txt", n)] = "x"
		}
		f := newPublishFixture(t, "spa", zipOf(t, entries))
		_, err := f.run(t)
		wantRefusal(t, err, reasonBundleTooManyFile)
	})
}

// A path that escapes its own bundle is refused rather than sanitised: there
// is no legitimate bundle entry that needs to write outside its prefix.
func TestSitePublishFromArtifact_TraversalPathIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"index.html", "../../etc/passwd"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	f := newPublishFixture(t, "spa", buf.Bytes())
	_, err := f.run(t)
	wantRefusal(t, err, reasonBundlePathInvalid)
	if got := len(f.store.keys()); got != 1 {
		t.Errorf("a refused publish wrote %d object(s)", got-1)
	}
}

func TestSitePublishFromArtifact_NotAZipFileIsRefused(t *testing.T) {
	f := newPublishFixture(t, "spa", []byte("this is not a zip, whatever the row says"))
	_, err := f.run(t)
	wantRefusal(t, err, reasonBundleNotAZip)
}

func TestSitePublishFromArtifact_MissingBytesAreRefused(t *testing.T) {
	f := newPublishFixture(t, "spa", nil) // nothing at the blob key
	_, err := f.run(t)
	wantRefusal(t, err, reasonBundleUnreadable)
}

func TestSitePublishFromArtifact_UnconfiguredStorageIsRefused(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	f.integration = NewSitePublishIntegration(f.engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.integration.newStore = func(context.Context) (siteObjectStore, error) {
		return siteObjectStore{}, fmt.Errorf("MEMQL_AZURE_BLOB_CONTAINER is not set on this node")
	}
	_, err := f.run(t)
	wantRefusal(t, err, reasonStorageNotReady)
}

func TestSitePublishFromArtifact_MissingArgumentsAreRefused(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	for _, args := range []map[string]any{
		{"artifactId": fixtureArtifact},
		{"siteId": fixtureSiteId},
		{"siteId": "  ", "artifactId": fixtureArtifact},
	} {
		_, err := f.integration.handlePublishFromArtifact(f.ctx, args, 0)
		wantRefusal(t, err, reasonMissingArgument)
	}
}

// A failed row flip must NOT be reported as a success. The bytes are already
// uploaded and orphaned at that point, which is the right failure -- the site
// keeps serving what it was serving.
func TestSitePublishFromArtifact_FailedRowFlipIsRefused(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	f.engine.failOn = "updateSiteBundle"
	_, err := f.run(t)
	wantRefusal(t, err, reasonPublishFailed)
}

// ---------------------------------------------------------------------------
// The statements themselves, against the REAL front end
// ---------------------------------------------------------------------------

// TestSitePublishStatementsResolve is the anti-false-signal gate. Every
// statement the happy path above produced is run through a REAL engine with
// the full embedded DSL loaded: the parser and named-construct resolution are
// live, so a retired call form, a typo'd construct name or a construct that
// exists in no .memql file fails HERE rather than at execute in production.
//
// The statements come from the fake engine's recording, not from a table
// written by hand, so this cannot drift from what the code actually renders.
func TestSitePublishStatementsResolve(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	if _, err := f.run(t); err != nil {
		t.Fatalf("publish: %v", err)
	}
	statements := f.engine.statements()
	if len(statements) < 5 {
		t.Fatalf("the happy path issued %d statements; expected at least 5 "+
			"(siteById, libraryArtifactById, libraryFileById, updateSiteBundle, createAuditEvent) "+
			"-- a shrunken set would make this gate cover less than it claims", len(statements))
	}

	eng := newRealEngineForPublish(t)
	seen := map[string]bool{}
	for _, stmt := range statements {
		if _, err := eng.Parse(stmt); err != nil {
			t.Errorf("the engine refused a rendered statement:\n  %s\n  --> %v", stmt, err)
			continue
		}
		if m := publishCallNameRe.FindStringSubmatch(stmt); m != nil {
			seen[m[1]] = true
		}
	}
	for _, want := range []string{"siteById", "libraryArtifactById", "libraryFileById", "updateSiteBundle", "createAuditEvent"} {
		if !seen[want] {
			t.Errorf("the happy path never issued %s; this gate did not cover it", want)
		}
	}
}

// TestSitePublishArgumentsAreDeclared is the other direction of memql#3626's
// "declared and used, in both directions" rule at the Go call boundary.
// Resolution does not cover it: validateFunctionArgs iterates DECLARED fields
// and rejectUnknownArgs is gated behind the MCP boundary, so an argument this
// package invents is silently DISCARDED -- the write believes it wrote a
// field, the reader believes the same, and the row never receives it.
func TestSitePublishArgumentsAreDeclared(t *testing.T) {
	f := newPublishFixture(t, "spa", zipOf(t, goodBundle()))
	if _, err := f.run(t); err != nil {
		t.Fatalf("publish: %v", err)
	}
	eng := newRealEngineForPublish(t)

	var checked int
	for _, stmt := range f.engine.statements() {
		m := publishCallNameRe.FindStringSubmatch(stmt)
		if m == nil {
			t.Errorf("could not read a construct name out of %q", stmt)
			continue
		}
		name := m[1]
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", name, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, field := range fn.ArgsSchema.Fields {
				declared[field.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block, yet this package calls it with arguments", name)
			continue
		}
		checked++
		for _, a := range publishStringArgRe.FindAllStringSubmatch(stmt, -1) {
			if declared[a[1]] {
				continue
			}
			t.Errorf("%s: this package passes %q, which the construct does not declare. "+
				"It is NOT refused -- the value is silently discarded and the row never "+
				"receives it (memql#3626, memql#4258).\n  %s", name, a[1], stmt)
		}
	}
	if checked == 0 {
		t.Fatal("checked no statements; this gate proved nothing")
	}
}

// newRealEngineForPublish boots a real MemQLEngine over the full embedded DSL
// tree, no database. Mirrors component/grpc's newRealDSLEngine.
func newRealEngineForPublish(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concepts.DefaultRegistry()
	if registry == nil {
		t.Fatal("concept.DefaultRegistry() is nil")
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng
}

// TestSitePublishBuiltinIsDeclaredAndResolvable closes the last gap between
// the Go capability and the DSL: the builtin must exist in the tree, name
// THIS integration's executor FQN, and declare exactly the two arguments the
// handler reads.
func TestSitePublishBuiltinIsDeclaredAndResolvable(t *testing.T) {
	eng := newRealEngineForPublish(t)
	fn, err := eng.Functions().Get("sitePublishFromArtifact")
	if err != nil || fn == nil {
		t.Fatalf("sitePublishFromArtifact is not in the function registry: %v", err)
	}
	wantExecutor := "integration." + sitePublishIntegrationName + "." + sitePublishCapability
	if fn.Executor != wantExecutor {
		t.Errorf("builtin executor = %q, want %q -- the DSL declaration and the Go "+
			"registration must name the same capability or every call fails at dispatch",
			fn.Executor, wantExecutor)
	}
	if fn.BuiltinArgs == nil {
		t.Fatal("the builtin declares no arg contract")
	}
	got := map[string]string{}
	for k, v := range fn.BuiltinArgs.Properties {
		got[k] = v
	}
	for _, want := range []string{"siteId", "artifactId"} {
		if _, ok := got[want]; !ok {
			t.Errorf("the builtin does not declare %q; the handler reads it", want)
		}
	}
	if len(got) != 2 {
		t.Errorf("the builtin declares %v; the handler reads only siteId + artifactId", got)
	}
	required := map[string]bool{}
	for _, r := range fn.BuiltinArgs.Required {
		required[r] = true
	}
	for _, want := range []string{"siteId", "artifactId"} {
		if !required[want] {
			t.Errorf("%q must be required: the handler refuses a blank one anyway, and a "+
				"declaration that says otherwise misleads every caller", want)
		}
	}

	// And the boot-time audit the engine itself runs. With the provider
	// REGISTERED, a declared executor naming a capability it does not expose
	// is FATAL rather than a warning (an absent integration only warns, which
	// is why registering first is what makes this check mean anything). This
	// is the exact check that fires at node startup.
	if err := eng.RegisterIntegration(NewSitePublishIntegration(newPublishStubEngine(), nil)); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the engine's boot-time executor audit refuses this wiring: %v", err)
	}
}

// TestSitePublishCapabilityIsRegisteredUnderItsDeclaredName pins the other
// half of the same wiring: the Go provider really does expose the capability
// the FQN names.
func TestSitePublishCapabilityIsRegisteredUnderItsDeclaredName(t *testing.T) {
	i := NewSitePublishIntegration(newPublishStubEngine(), nil)
	if i.IntegrationName() != sitePublishIntegrationName {
		t.Errorf("IntegrationName = %q, want %q", i.IntegrationName(), sitePublishIntegrationName)
	}
	caps := i.Capabilities()
	if len(caps) != 1 || caps[0].Name != sitePublishCapability {
		t.Fatalf("Capabilities = %+v, want exactly %q", caps, sitePublishCapability)
	}
	if caps[0].Handler == nil {
		t.Error("the capability registers a nil handler")
	}
}

// ---------------------------------------------------------------------------
// The duplicated limits
// ---------------------------------------------------------------------------

// TestSiteBundleLimitsMatchTheCIRoute is the drift guard on the one thing this
// file DUPLICATES rather than imports: the three bundle limits, which live
// unexported in component/server (a tiered module with its own go.mod that
// integrations/library cannot import them from).
//
// A duplicated constant with a comment saying "keep in step with X" is a
// convention, not a check. This reads X and fails when the numbers part --
// and it FAILS LOUDLY if it cannot find them at all, because a gate that
// silently examines nothing is worse than no gate: its pass would be a claim
// about the regexp rather than about the code.
func TestSiteBundleLimitsMatchTheCIRoute(t *testing.T) {
	const source = "../../component/server/site_bundle_handler.go"
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("cannot read %s, so this gate can prove nothing: %v", source, err)
	}

	want := map[string]int{
		"maxBundleFileBytes":  sitePublishMaxFileBytes,
		"maxBundleTotalBytes": sitePublishMaxTotalBytes,
		"maxBundleFileCount":  sitePublishMaxFileCount,
	}
	found := map[string]bool{}
	for name, mirrored := range want {
		re := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*([0-9*\s]+?)\s*(?://.*)?$`)
		m := re.FindSubmatch(body)
		if m == nil {
			t.Errorf("could not find the constant %q in %s -- this gate is not examining "+
				"what it claims to; find where the limit moved to and re-point it", name, source)
			continue
		}
		found[name] = true
		if got := evalProduct(t, string(m[1])); got != mirrored {
			t.Errorf("%s in %s is %d, but this package mirrors it as %d. The CI route and "+
				"the portal deploy publish into the same storage and are served by the same "+
				"edge; a bundle one accepts and the other refuses is a defect a person "+
				"experiences as \"it deploys from CI but not from the portal\".",
				name, source, got, mirrored)
		}
	}
	if len(found) != len(want) {
		t.Fatalf("located %d of %d limits; a partial read makes the pass meaningless", len(found), len(want))
	}
}

// evalProduct evaluates the "25 * 1024 * 1024" form the constants are written
// in. Deliberately tiny: it accepts a product of integers and nothing else,
// so it cannot silently mis-read an expression it does not understand.
func evalProduct(t *testing.T, expr string) int {
	t.Helper()
	out := 1
	for _, part := range strings.Split(expr, "*") {
		part = strings.TrimSpace(part)
		n := 0
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil || n <= 0 {
			t.Fatalf("cannot evaluate %q as a product of positive integers: %v", expr, err)
		}
		out *= n
	}
	return out
}

// TestSitePublishRegistrationNameIsTheLiteralTheGateScansFor closes the one
// drift the registration's own comment creates: the plugin name is written
// twice, as the literal memql.RegisterPlugin needs for the classification gate
// to SEE this plugin at all, and as the constant IntegrationName() returns and
// the executor FQN is built from. If those two ever part, the classification
// gate keeps classifying a name nothing dispatches under while every call to
// the builtin fails to resolve its capability.
//
// It FAILS rather than skips when it cannot find the registration, because a
// gate that quietly examines nothing turns its pass into a claim about the
// regexp instead of about the code.
func TestSitePublishRegistrationNameIsTheLiteralTheGateScansFor(t *testing.T) {
	const source = "site_publish.go"
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("cannot read %s, so this gate can prove nothing: %v", source, err)
	}
	re := regexp.MustCompile(`memql\.RegisterPlugin\(\s*"([A-Za-z0-9_]+)"`)
	m := re.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no memql.RegisterPlugin(\"<literal>\") in %s -- module_taxonomy_test.go finds "+
			"plugins by exactly this pattern, so a non-literal name makes this plugin invisible "+
			"to the classification gate", source)
	}
	if got := string(m[1]); got != sitePublishIntegrationName {
		t.Errorf("RegisterPlugin registers %q but sitePublishIntegrationName is %q; "+
			"IntegrationName() and the executor FQN follow the constant, the taxonomy gate "+
			"follows the literal, and a mismatch breaks dispatch while the gate stays green",
			got, sitePublishIntegrationName)
	}
}
