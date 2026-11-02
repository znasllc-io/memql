package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// The publication recipe exactly as a product installs it: memql-znas
// dsl/znasdelivery/automations.memql at e82cfd4, renamed. It reads the draft and
// publication journals once at the top, then loops over every target three
// times. It is held here unchanged because a saved release captures whatever the
// product shipped: the engine must make this shape finish, without a new recipe.
const installedPublicationRecipe = `@template
automation fixtureBuildPublication {
  args { candidateId string! approvalId string! }
  candidate := builtin releaseGetCandidate(candidateId: args.candidateId)
  if candidate.candidateId != args.candidateId || candidate.state != "approved" ||
    candidate.approvalId != args.approvalId {
    return error("The exact candidate needs a recorded owner approval before publication.")
  }
  bindings := builtin releaseCandidateTargets(candidateId: args.candidateId)
  if bindings.candidateId != args.candidateId || bindings.targets.count() == 0 {
    return error("The approved release destinations are unavailable.")
  }
  drafts := builtin releaseCandidateDrafts(candidateId: args.candidateId)
  publications := builtin releaseCandidatePublications(candidateId: args.candidateId)
  if drafts.candidateId != args.candidateId || publications.candidateId != args.candidateId ||
    bindings.targets.distinct(target => target.targetId).count() != bindings.targets.count() ||
    bindings.targets.any(target => target.operation != "publish" || !(target.kind in ["file", "oci"])) {
    return error("The publication history does not match this approved release.")
  }
  // Validate all saved identities before any effect. A completed receipt can
  // suppress work only for the same approval and exact destination digest.
  for publication in publications.publications {
    targets := bindings.targets.where(target => target.targetId == publication.target.targetId)
    if targets.count() != 1 || publication.candidateId != args.candidateId ||
      publication.approvalId != args.approvalId || !(publication.state in ["pending", "complete"]) {
      return error("The saved publication belongs to a different approved release.")
    }
    target := targets.single()
    if publication.target.targetDigest != target.targetDigest || publication.target.component != target.component ||
      publication.target.artifact != target.artifact || publication.target.operation != target.operation ||
      publication.artifact.name != target.artifact || publication.artifact.kind != target.kind {
      return error("The saved publication differs from its approved destination.")
    }
  }
  for draft in drafts.drafts {
    if draft.candidateId != args.candidateId || draft.approvalId != args.approvalId ||
      draft.targets.count() == 0 || !(draft.state in ["prepared", "creating", "ready", "promoting", "published"]) {
      return error("The saved release draft belongs to a different approval.")
    }
    for targetId in draft.targets {
      targets := bindings.targets.where(target => target.targetId == targetId && target.kind == "file")
      if targets.count() != 1 { return error("The saved release draft has an unknown destination.") }
    }
  }
  for target in bindings.targets {
    saved := publications.publications.where(item => item.target.targetId == target.targetId)
    release := drafts.drafts.where(item => target.targetId in item.targets)
    if saved.count() > 1 || release.count() > 1 {
      return error("The release history has conflicting destination receipts.")
    }
    if release.any(item => item.state in ["promoting", "published"]) && !saved.any(item => item.state == "complete") {
      return error("The published release is missing its completed upload receipt.")
    }
  }
  // A promoted draft must never be passed back through draft creation. Pending
  // native effects retain their original identity and reconcile on the next call.
  for target in bindings.targets {
    release := drafts.drafts.where(item => target.targetId in item.targets)
    if target.kind == "file" && !release.any(item => item.state in ["ready", "promoting", "published"]) {
      builtin releaseCreateCandidateDraft(candidateId: args.candidateId, approvalId: args.approvalId, targetId: target.targetId)
    }
  }
  for target in bindings.targets {
    saved := publications.publications.where(item => item.target.targetId == target.targetId)
    if !saved.any(item => item.state == "complete") {
      builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId,
        targetId: target.targetId, component: target.component, artifact: target.artifact)
    }
  }
  for target in bindings.targets {
    release := drafts.drafts.where(item => target.targetId in item.targets)
    if target.kind == "file" && !release.any(item => item.state == "published") {
      builtin releasePromoteCandidateDraft(candidateId: args.candidateId, approvalId: args.approvalId, targetId: target.targetId)
    }
  }
  return builtin releaseSealPublishedCandidate(candidateId: args.candidateId, approvalId: args.approvalId)
}`

// manyFileLibrary is the retained-artifact store for a run that produced many
// files, counting every byte stream it opens.
type manyFileLibrary struct {
	t     *testing.T
	mu    sync.Mutex
	files map[string]*manyFile
	pins  map[string]bool
	opens int
}

type manyFile struct {
	receipt pipelinesteps.StoredFileReceipt
	body    []byte
}

func (l *manyFileLibrary) check(ctx context.Context, scope pipelinesteps.RunFileReceiptScope) {
	l.t.Helper()
	owner, err := candidateOwner(ctx)
	if err != nil || owner != scope.OwnerUserID || !auth.OriginFromContext(ctx).IsInternal() {
		l.t.Error("artifact authority changed", err)
	}
}

func (l *manyFileLibrary) ReadRunFileReceipts(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	l.check(ctx, scope)
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []pipelinesteps.StoredFileReceipt{}
	for _, intent := range ids {
		file := l.files[intent]
		if file == nil || receiptScope(file.receipt) != scope {
			return nil, errors.New("unknown artifact receipt")
		}
		out = append(out, file.receipt)
	}
	return out, nil
}

func (l *manyFileLibrary) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	rows, err := l.ReadRunFileReceipts(ctx, ref.Scope, ref.IntentIDs)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pins[ref.ReferenceID] = true
	return rows, nil
}

func (l *manyFileLibrary) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	l.check(ctx, ref.Scope)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pins, ref.ReferenceID)
	return nil
}

func (l *manyFileLibrary) RetireRunFile(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.RetiredFileReceipt, error) {
	l.t.Error("publication must not retire retained artifacts")
	return pipelinesteps.RetiredFileReceipt{}, nil
}

func (l *manyFileLibrary) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intent string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	l.check(ctx, scope)
	l.mu.Lock()
	defer l.mu.Unlock()
	file := l.files[intent]
	if file == nil || len(l.pins) == 0 || receiptScope(file.receipt) != scope {
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("read without exact pin")
	}
	l.opens++
	return file.receipt, io.NopCloser(bytes.NewReader(file.body)), nil
}

func (l *manyFileLibrary) openCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

type manyFileAsset struct {
	id    int64
	name  string
	state string
	body  []byte
}

// manyFileGitHub is one remote repository, as far as the release protocol can
// see it. Each write checks the durable fence that must already stand behind it.
type manyFileGitHub struct {
	t      *testing.T
	db     *sql.DB
	server *httptest.Server
	commit string
	tag    string

	mu         sync.Mutex
	created    bool
	published  bool
	body       string
	hideDraft  bool
	loseCreate bool
	assets     []*manyFileAsset
	nextAsset  int64
	draftPosts int
	promotes   int
	uploads    map[string]int
	downloads  int
	removed    []int64
	// beforeUpload runs outside the fixture's lock, and true means it answered.
	beforeUpload func(name string, w http.ResponseWriter, r *http.Request) bool
	// beforeDownload is the same for one read of an asset's bytes.
	beforeDownload func(name string, w http.ResponseWriter, r *http.Request) bool
	// beforeRelease is the same for one read of the release's record or asset list.
	beforeRelease func(w http.ResponseWriter, r *http.Request) bool
	// candidate scopes the fence checks to this release's own journal rows.
	candidate atomic.Value
	requests  atomic.Int64
}

func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// dropMidRead fails one asset read for real. Dropped before any response bytes, a GET
// is retried by the HTTP transport on its own and nobody ever sees the failure.
func dropMidRead(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	_, _ = w.Write([]byte("part of a file"))
	w.(http.Flusher).Flush()
	dropConnection(w)
}

func (g *manyFileGitHub) assetList() []map[string]any {
	out := []map[string]any{}
	for _, a := range g.assets {
		sum := sha256.Sum256(a.body)
		row := map[string]any{"id": a.id, "name": a.name, "state": a.state, "size": len(a.body)}
		if a.state == "uploaded" {
			row["digest"] = "sha256:" + hex.EncodeToString(sum[:])
		}
		out = append(out, row)
	}
	return out
}

func (g *manyFileGitHub) leaveIncomplete(name string) {
	g.nextAsset++
	g.assets = append(g.assets, &manyFileAsset{id: g.nextAsset, name: name, state: "starter"})
}

// fenced fails the test unless this release's draft row satisfies the condition.
func (g *manyFileGitHub) fenced(condition string) {
	g.t.Helper()
	var held bool
	err := g.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM release_draft_intents WHERE candidate_id=$1 AND `+condition+`)`, g.candidate.Load()).Scan(&held)
	if err != nil || !held {
		g.t.Error("remote write came before its durable fence:", condition, err)
	}
}

func (g *manyFileGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.requests.Add(1)
	if r.Header.Get("Authorization") != "Bearer fixture-tag-token" {
		g.t.Error("unscoped release credential", r.Method, r.URL.Path)
		w.WriteHeader(401)
		return
	}
	const repo = "/repos/acme/engine"
	const release = repo + "/releases/73"
	name := r.URL.Query().Get("name")
	if r.Method == "POST" && r.URL.Path == release+"/assets" {
		g.fenced(`release_id=73 AND state='ready' AND upload_lease_until>clock_timestamp()`)
		if hook := g.beforeUpload; hook != nil && hook(name, w, r) {
			return
		}
	}
	if r.Method == "GET" && (r.URL.Path == release || r.URL.Path == release+"/assets") {
		if hook := g.beforeRelease; hook != nil && hook(w, r) {
			return
		}
	}
	if r.Method == "GET" && r.Header.Get("Accept") == "application/octet-stream" {
		if hook := g.beforeDownload; hook != nil {
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/releases/assets/"), 10, 64)
			g.mu.Lock()
			index := slices.IndexFunc(g.assets, func(a *manyFileAsset) bool { return a.id == id })
			asset := ""
			if index >= 0 {
				asset = g.assets[index].name
			}
			g.mu.Unlock()
			if hook(asset, w, r) {
				return
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	view := func() map[string]any {
		return map[string]any{"id": 73, "draft": !g.published, "tag_name": g.tag, "name": "Engine 0.25.0", "body": g.body, "prerelease": false,
			"upload_url": "http://" + r.Host + release + "/assets{?name,label}"}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == repo+"/git/ref/tags/"+g.tag:
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/" + g.tag, "object": map[string]any{"type": "commit", "sha": g.commit}})
	case r.Method == "GET" && r.URL.Path == repo+"/releases":
		rows := []any{}
		if g.created && !g.hideDraft {
			rows = append(rows, view())
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == "POST" && r.URL.Path == repo+"/releases":
		g.fenced(`state='creating' AND release_id IS NULL`)
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["draft"] != true || request["tag_name"] != g.tag || request["target_commitish"] != g.commit {
			g.t.Error("draft scope changed", request)
		}
		g.body, _ = request["body"].(string)
		g.created = true
		g.draftPosts++
		if g.loseCreate {
			dropConnection(w)
			return
		}
		w.WriteHeader(201)
	case r.Method == "GET" && r.URL.Path == release:
		_ = json.NewEncoder(w).Encode(view())
	case r.Method == "PATCH" && r.URL.Path == release:
		g.fenced(`release_id=73 AND state='promoting'`)
		g.published = true
		g.promotes++
		_ = json.NewEncoder(w).Encode(view())
	case r.Method == "GET" && r.URL.Path == release+"/assets":
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 1 {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(g.assetList())
	case r.Method == "POST" && r.URL.Path == release+"/assets":
		g.uploads[name]++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			g.t.Error(err)
		}
		if slices.ContainsFunc(g.assets, func(a *manyFileAsset) bool { return a.name == name }) {
			w.WriteHeader(422)
			return
		}
		g.nextAsset++
		g.assets = append(g.assets, &manyFileAsset{id: g.nextAsset, name: name, state: "uploaded", body: body})
		w.WriteHeader(201)
	case strings.HasPrefix(r.URL.Path, repo+"/releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/releases/assets/"), 10, 64)
		index := slices.IndexFunc(g.assets, func(a *manyFileAsset) bool { return a.id == id })
		switch {
		case index < 0:
			w.WriteHeader(404)
		case r.Method == "DELETE":
			g.removed = append(g.removed, id)
			g.assets = slices.Delete(g.assets, index, index+1)
			w.WriteHeader(204)
		case r.Method == "GET" && r.Header.Get("Accept") == "application/octet-stream":
			g.downloads++
			_, _ = w.Write(g.assets[index].body)
		default:
			g.t.Error("unexpected asset request", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	default:
		g.t.Error("unexpected release request", r.Method, r.URL.Path)
		w.WriteHeader(400)
	}
}

// counts reads every remote effect together, so a check cannot see half an upload.
func (g *manyFileGitHub) counts() (draftPosts, promotes, uploads, downloads, removed int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.uploads {
		uploads += n
	}
	return g.draftPosts, g.promotes, uploads, g.downloads, len(g.removed)
}

// manyFileRelease is a saved release of n separate files through the whole
// journey, up to an approved candidate.
type manyFileRelease struct {
	*buildDBFixture
	github    *manyFileGitHub
	library   *manyFileLibrary
	intent    string
	candidate string
	args      map[string]any
	names     []string
}

func newManyFileRelease(t *testing.T, n int) *manyFileRelease {
	t.Helper()
	f := newBuildDBFixture(t)
	m := &manyFileRelease{buildDBFixture: f}
	owner, _ := candidateOwner(ownerCtx())
	commit := f.pipeline.source.SHA
	m.library = &manyFileLibrary{t: t, files: map[string]*manyFile{}, pins: map[string]bool{}}
	intents := []string{}
	for index := range n {
		intents = append(intents, fmt.Sprintf("%064x", index+1))
	}
	run, pipelineRun := profileCompletedRun(t, f.producer, "acme/engine", commit, "0.25.0", "release", "full", intents)
	var pipelineID string
	if err := f.db.QueryRow(`SELECT payload->>'pipelineId' FROM "MemoryNodes" WHERE id=$1 ORDER BY "createdAt" DESC LIMIT 1`, "v1:pipelines:run:"+pipelineRun).Scan(&pipelineID); err != nil {
		t.Fatal(err)
	}
	pipelineID = memql.BareShortId(pipelineID)
	t.Cleanup(func() {
		if _, err := f.db.Exec(`DELETE FROM release_build_intents WHERE body->'selection'->'request'->>'PipelineID'=$1`, pipelineID); err != nil {
			t.Error(err)
		}
		if _, err := f.db.Exec(`DELETE FROM release_candidate_profile_bindings WHERE body->'runs'->>'build'=$1`, run); err != nil {
			t.Error(err)
		}
	})
	f.runs["build"], f.runID = run, pipelineRun
	f.pipeline.nextRunID, f.pipeline.source.PipelineID = pipelineRun, pipelineID
	f.pipeline.source.TagExists, f.pipeline.source.TagSHA = true, commit

	m.github = &manyFileGitHub{t: t, db: f.db, commit: commit, tag: "v0.25.0", uploads: map[string]int{}}
	m.github.server = httptest.NewServer(m.github)
	t.Cleanup(m.github.server.Close)
	url := m.github.server.URL

	profile := &f.config.Profiles[0]
	profile.Registries, profile.ReleaseAssets = nil, nil
	artifacts, publications := []string{}, []string{}
	for index, intent := range intents {
		name := fmt.Sprintf("file-%02d", index)
		m.names = append(m.names, name)
		body := []byte(fmt.Sprintf("the approved bytes of %s", name))
		sum := sha256.Sum256(body)
		m.library.files[intent] = &manyFile{body: body, receipt: pipelinesteps.StoredFileReceipt{IntentID: intent, OwnerUserID: owner, WorkRunID: run, StepKey: "build.image", Attempt: 1, Path: "build/" + name + ".tar.gz", ETag: "v1", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}
		profile.ReleaseAssets = append(profile.ReleaseAssets, candidateProfileFileAuthority{ID: name, Component: "engine", Artifact: name, Repository: "acme/engine", TagPrefix: "v", APIOrigin: url, UploadOrigin: url, CredentialSecret: "TAG_TOKEN", AllowLoopbackHTTP: true})
		artifacts = append(artifacts, fmt.Sprintf(`{name: %q, run: "build", stepKey: "build.image", path: %q, kind: "file", platform: "linux/arm64"}`, name, "build/"+name+".tar.gz"))
		publications = append(publications, fmt.Sprintf(`{targetId: %q, assetName: %q, draft: {name: "Engine 0.25.0", body: "Reviewed notes.", prerelease: false, latest: false}}`, name, name+".tar.gz"))
	}
	profile.Build.TagTargetID = m.names[0]
	*f.source = "@template\nautomation fixtureProfile {\n args { facts object! }\n if args.facts.components.0.runs.0.event != \"release\" { return {components: [], publications: []} }\n return {\n  components: [{name: \"engine\", artifacts: [" + strings.Join(artifacts, ",\n") + "]}],\n  publications: [" + strings.Join(publications, ",\n") + "]\n }\n}"
	f.publication = installedPublicationRecipe
	deps := *f.i.candidateDeps.Load()
	deps.Library = m.library
	f.i.candidateDeps.Store(&deps)

	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := json.Marshal(catalogConfiguration{FormatVersion: 1, Publisher: "fixture", KeyID: "fixture-key", SigningKeySecret: "CATALOG_KEY", PublicKeys: map[string]string{"fixture-key": base64.StdEncoding.EncodeToString(public)}})
	variables, secrets := f.i.resolver.systemVariable, f.i.resolver.systemSecret
	f.i.resolver.systemVariable = func(ctx context.Context, name string) (string, error) {
		if name == ReleaseCatalogConfigurationVariable {
			return string(catalog), nil
		}
		return variables(ctx, name)
	}
	f.i.resolver.systemSecret = func(ctx context.Context, name string) (string, error) {
		if name == "CATALOG_KEY" {
			return base64.StdEncoding.EncodeToString(key.Seed()), nil
		}
		return secrets(ctx, name)
	}

	r := f.prepare(t)
	m.intent = r.ID
	buildAction(t, f.i, r.ID, "start")
	f.pipeline.run.Status, f.pipeline.run.Conclusion, f.pipeline.run.WorkRunID = "completed", "success", run
	buildAction(t, f.i, r.ID, "observe")
	v := buildAction(t, f.i, r.ID, "resume")
	if v.CandidateID == "" {
		t.Fatal("no candidate was assembled", v)
	}
	cleanupCandidate(t, f.db, v.CandidateID)
	m.github.candidate.Store(v.CandidateID)
	m.candidate = v.CandidateID
	p, err := f.i.configuredCandidateFor(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := p.approve(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	m.args = map[string]any{"intentId": r.ID, "approvalId": approved.ApprovalID}
	return m
}

func (m *manyFileRelease) publish(ctx context.Context, i *Integration) (releaseBuildView, error) {
	var v releaseBuildView
	rows, err := i.executeBuild(ctx, m.args, "publish")
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(rows[0].Payload, &v)
}

func (m *manyFileRelease) read(t *testing.T) releaseBuildView {
	t.Helper()
	var v releaseBuildView
	rows, err := m.i.handleBuildGet(ownerCtx(), map[string]any{"intentId": m.intent}, 1)
	if err != nil {
		t.Error(err)
		return v
	}
	if err := json.Unmarshal(rows[0].Payload, &v); err != nil {
		t.Error(err)
	}
	return v
}

// The regression this file exists for. cockpit/v0.21.1 was 41 files: one Publish
// could not finish because every per-file call re-read every artifact, and each
// loop of the recipe repeated the whole release's work once per file. Here the
// lease is shorter than the action, so it finishes only if it is renewed.
func TestReleaseBuildPublishesManyFileTargetsInOneCall(t *testing.T) {
	m := newManyFileRelease(t, 41)
	setLeaseTiming(t, 2*time.Second, 200*time.Millisecond)
	m.github.beforeUpload = func(string, http.ResponseWriter, *http.Request) bool {
		time.Sleep(100 * time.Millisecond)
		return false
	}
	before := m.library.openCount()
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "published" || v.Catalog.State != "sealed" || v.ReasonCode != "" {
		t.Fatal("one publish did not finish the release", v.Phase, v.ReasonCode, v.Catalog)
	}
	draftPosts, promotes, uploads, downloads, removed := m.github.counts()
	if draftPosts != 1 || promotes != 1 || uploads != 41 || removed != 0 {
		t.Fatal("a once-only effect was repeated or lost", draftPosts, promotes, uploads, removed)
	}
	for _, name := range m.names {
		if m.github.uploads[name+".tar.gz"] != 1 {
			t.Fatal("file was not uploaded exactly once", name, m.github.uploads[name+".tar.gz"])
		}
	}
	// One whole-candidate verification before the first effect (41 streams), one
	// snapshot of each file before its upload (41), and one more verification before
	// the release goes public (41). Per-file verification was 41 streams per call.
	if opens := m.library.openCount() - before; opens != 123 {
		t.Fatal("the candidate was not verified only where the action needs it", opens)
	}
	// Each upload's readback (41) and the one promotion's two complete reads: before
	// its claim and after its PATCH (82). A promotion repeated per file read the entire
	// release at least 40 more times.
	if downloads != 123 {
		t.Fatal("the release's bytes were read more than the proof needs", downloads)
	}
	var held bool
	if err := m.db.QueryRow(`SELECT lease_until>clock_timestamp() FROM release_build_intents WHERE intent_id=$1`, m.intent).Scan(&held); err != nil || held {
		t.Fatal("a finished action kept its lease", err)
	}
}

// The other half of the evidence: a retry whose top read still says creating
// spent its whole limit calling draft creation once per file.
func TestReleaseBuildRetryWithADraftStillCreatingSpendsOneCallOnIt(t *testing.T) {
	m := newManyFileRelease(t, 8)
	m.github.loseCreate, m.github.hideDraft = true, true
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" || m.github.draftPosts != 1 {
		t.Fatal("an unseen draft did not stay uncertain", v.Phase, m.github.draftPosts)
	}
	m.github.mu.Lock()
	m.github.loseCreate, m.github.hideDraft = false, false
	m.github.mu.Unlock()
	before := m.library.openCount()
	v, err = m.publish(ownerCtx(), m.peer(t))
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "published" {
		t.Fatal("the retry did not finish", v.Phase, v.ReasonCode)
	}
	if m.github.draftPosts != 1 {
		t.Fatal("the once-only draft POST was repeated", m.github.draftPosts)
	}
	// Verified before its first effect (8), a snapshot per upload (8), and verified
	// again before going public (8).
	if opens := m.library.openCount() - before; opens != 24 {
		t.Fatal("the retry repeated its verification per file", opens)
	}
}

func TestReleaseBuildPublishRecoversAnUploadCutOffPartWay(t *testing.T) {
	m := newManyFileRelease(t, 6)
	cut := "file-02.tar.gz"
	var once sync.Once
	m.github.beforeUpload = func(name string, w http.ResponseWriter, r *http.Request) bool {
		answered := false
		if name == cut {
			once.Do(func() {
				answered = true
				_, _ = io.CopyN(io.Discard, r.Body, 4)
				m.github.mu.Lock()
				m.github.uploads[name]++
				m.github.leaveIncomplete(name)
				m.github.mu.Unlock()
				dropConnection(w)
			})
		}
		return answered
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal("a cut-off upload lost the journal", err)
	}
	if v.Phase != "uncertain" || !slices.Contains(v.Actions, "publish") {
		t.Fatal("a cut-off upload must stay open to another publish", v.Phase, v.Actions)
	}
	_, _, uploads, _, _ := m.github.counts()
	// Files sort by name and upload in target order: 00, 01, then the cut 02.
	if uploads != 3 {
		t.Fatal("the action went on past a failed upload", uploads)
	}
	before := m.library.openCount()
	v, err = m.publish(ownerCtx(), m.peer(t))
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "published" || v.Catalog.State != "sealed" {
		t.Fatal("the retry did not recover the release", v.Phase, v.ReasonCode)
	}
	draftPosts, promotes, _, _, removed := m.github.counts()
	if draftPosts != 1 || promotes != 1 || removed != 1 {
		t.Fatal("recovery changed more than the incomplete asset", draftPosts, promotes, removed)
	}
	for _, name := range m.names {
		want := 1
		if name+".tar.gz" == cut {
			want = 2
		}
		if m.github.uploads[name+".tar.gz"] != want {
			t.Fatal("completed work was repeated or lost", name, m.github.uploads[name+".tar.gz"])
		}
	}
	// A new action verifies for itself (6), snapshots the 4 files still owed, and
	// verifies again before going public (6).
	if opens := m.library.openCount() - before; opens != 16 {
		t.Fatal("the retry did not verify for itself", opens)
	}
}

func TestReleaseBuildPublishOutlivesItsCaller(t *testing.T) {
	m := newManyFileRelease(t, 3)
	ctx, leave := context.WithCancel(ownerCtx())
	defer leave()
	// The page closes while the first file is on its way.
	var once sync.Once
	m.github.beforeUpload = func(string, http.ResponseWriter, *http.Request) bool {
		once.Do(leave)
		time.Sleep(50 * time.Millisecond)
		return false
	}
	v, err := m.publish(ctx, m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "published" || m.read(t).Phase != "published" {
		t.Fatal("a caller leaving cut off the publication", v.Phase, v.ReasonCode)
	}
}

// Anything else the caller's context ends still records what it knows. An expired
// context used to fail the save too, so the phase stayed publishing and the caller
// got only "journal update unconfirmed".
func TestReleaseBuildActionCutOffByItsContextStillSavesTheJournal(t *testing.T) {
	f := newBuildDBFixture(t)
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	r := f.prepare(t)
	ctx, cut := context.WithCancel(ownerCtx())
	defer cut()
	f.pipeline.beforeStart = cut
	rows, err := f.i.executeBuild(ctx, map[string]any{"intentId": r.ID}, "start")
	if err != nil {
		t.Fatal("the outcome could not be recorded:", err)
	}
	var v releaseBuildView
	if err := json.Unmarshal(rows[0].Payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" || v.ReasonCode != "operation_unconfirmed" {
		t.Fatal("the cut-off action reported no uncertainty", v.Phase, v.ReasonCode)
	}
	stored, err := f.i.handleBuildGet(ownerCtx(), map[string]any{"intentId": r.ID}, 1)
	if err != nil || json.Unmarshal(stored[0].Payload, &v) != nil || v.Phase != "uncertain" {
		t.Fatal("the journal does not hold the uncertainty", v.Phase, err)
	}
}

func TestReleaseBuildPublishStopsWhenItsLeaseIsTaken(t *testing.T) {
	m := newManyFileRelease(t, 3)
	setLeaseTiming(t, time.Second, 100*time.Millisecond)
	var once sync.Once
	m.github.beforeUpload = func(string, http.ResponseWriter, *http.Request) bool {
		once.Do(func() {
			if _, err := m.db.Exec(`UPDATE release_build_intents SET lease_id='another-replica',lease_until=clock_timestamp()+interval '1 hour' WHERE intent_id=$1`, m.intent); err != nil {
				t.Error(err)
			}
			// Hold this upload open until the renewal has had time to notice.
			time.Sleep(5 * buildLeaseTiming.renew)
		})
		return false
	}
	if _, err := m.publish(ownerCtx(), m.i); err == nil {
		t.Fatal("an action without its lease reported an outcome")
	}
	_, _, uploads, _, _ := m.github.counts()
	if uploads > 1 {
		t.Fatal("the action kept uploading after losing its lease", uploads)
	}
	if _, err := m.db.Exec(`UPDATE release_build_intents SET lease_id='',lease_until='epoch' WHERE intent_id=$1`, m.intent); err != nil {
		t.Fatal(err)
	}
	// The other replica's upload lease lapses on its own; clear it to save the wait.
	if _, err := m.db.Exec(`UPDATE release_draft_intents SET upload_lease_id='',upload_lease_until='epoch' WHERE release_id=73`); err != nil {
		t.Fatal(err)
	}
	v, err := m.publish(ownerCtx(), m.peer(t))
	if err != nil || v.Phase != "published" {
		t.Fatal("the release could not continue after the lost lease", v.Phase, err)
	}
}

// A publication that outlives its caller still answers to the stored role. The
// admission context cannot see a role change, so this needs its own read.
func TestReleaseBuildPublicationStopsWhenTheOwnerLosesTheRole(t *testing.T) {
	m := newManyFileRelease(t, 4)
	var once sync.Once
	m.github.beforeUpload = func(string, http.ResponseWriter, *http.Request) bool {
		once.Do(func() { m.ownerRole.Store(string(auth.RoleAdmin)) })
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" {
		t.Fatal("a demoted owner's publication kept its outcome open", v.Phase)
	}
	_, promotes, uploads, _, _ := m.github.counts()
	if uploads != 1 || promotes != 0 {
		t.Fatal("effects continued after the role was lost", uploads, promotes)
	}
	m.ownerRole.Store(string(auth.RoleOwner))
	v, err = m.publish(ownerCtx(), m.i)
	if err != nil || v.Phase != "published" {
		t.Fatal("the restored owner could not finish", v.Phase, err)
	}
}

func TestReleaseBuildViewTellsARunningPublicationFromAnInterruptedOne(t *testing.T) {
	m := newManyFileRelease(t, 2)
	var seen releaseBuildView
	var once sync.Once
	m.github.beforeUpload = func(string, http.ResponseWriter, *http.Request) bool {
		once.Do(func() { seen = m.read(t) })
		return false
	}
	if v, err := m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal(v.Phase, err)
	}
	if seen.Phase != "publishing" || slices.Contains(seen.Actions, "publish") || seen.ReasonCode != "" {
		t.Fatal("a running publication was offered again", seen.Phase, seen.Actions, seen.ReasonCode)
	}
}

func TestReleaseBuildViewOffersAnInterruptedPublicationAgain(t *testing.T) {
	m := newManyFileRelease(t, 2)
	storage, err := m.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	// The way a killed pod leaves it: phase saved, nothing more written.
	lease, err := storage.ledger.claimBuild(ownerCtx(), m.intent)
	if err != nil {
		t.Fatal(err)
	}
	lease.record.ApprovalID, lease.record.PublicationStarted, lease.record.Phase = m.args["approvalId"].(string), true, "publishing"
	if err := lease.save(ownerCtx()); err != nil {
		t.Fatal(err)
	}
	if v := m.read(t); v.Phase != "publishing" || slices.Contains(v.Actions, "publish") || v.ReasonCode != "" {
		t.Fatal("a held lease must read as running", v.Phase, v.Actions, v.ReasonCode)
	}
	if _, err := m.db.Exec(`UPDATE release_build_intents SET lease_until='epoch' WHERE intent_id=$1`, m.intent); err != nil {
		t.Fatal(err)
	}
	v := m.read(t)
	if v.Phase != "uncertain" || !slices.Contains(v.Actions, "publish") || v.ReasonCode != "publication_interrupted" || v.Reason == "" {
		t.Fatal("a dead publication must say so and offer publish", v.Phase, v.Actions, v.ReasonCode)
	}
	if after, err := m.publish(ownerCtx(), m.i); err != nil || after.Phase != "published" {
		t.Fatal("publishing again did not finish the release", after.Phase, err)
	}
}

// What cannot be recovered says so plainly, and changes nothing remote.
func TestReleaseBuildNamesARealAssetConflictAndLeavesIt(t *testing.T) {
	m := newManyFileRelease(t, 2)
	m.github.mu.Lock()
	m.github.nextAsset++
	m.github.assets = append(m.github.assets, &manyFileAsset{id: m.github.nextAsset, name: "file-00.tar.gz", state: "uploaded", body: []byte("somebody else's bytes")})
	m.github.mu.Unlock()
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" || v.ReasonCode != "release_asset_conflict" || !strings.Contains(v.Reason, "file-00.tar.gz") {
		t.Fatal("a real conflict was not named", v.Phase, v.ReasonCode, v.Reason)
	}
	_, _, uploads, _, removed := m.github.counts()
	if uploads != 0 || removed != 0 {
		t.Fatal("a conflict changed the release", uploads, removed)
	}
}

// receipts counts this release's finished uploads, so a hook can act once every
// upload is done and promotion has begun.
func (m *manyFileRelease) receipts() int {
	var complete int
	if err := m.db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id=$1 AND state='complete'`, m.candidate).Scan(&complete); err != nil {
		m.github.t.Error(err)
	}
	return complete
}

func (m *manyFileRelease) draftState() string {
	var state string
	if err := m.db.QueryRow(`SELECT state FROM release_draft_intents WHERE candidate_id=$1`, m.candidate).Scan(&state); err != nil {
		m.github.t.Error(err)
	}
	return state
}

// A release going public is the one effect nobody can take back, so what a call
// read at its start is not enough for it. Without this read the release went public
// and then failed to seal: a public release with no signed catalog.
func TestReleaseBuildPublicationRereadsTheEvidenceBeforeGoingPublic(t *testing.T) {
	m := newManyFileRelease(t, 3)
	var once sync.Once
	m.github.beforeUpload = func(name string, _ http.ResponseWriter, _ *http.Request) bool {
		if name == "file-02.tar.gz" {
			once.Do(func() {
				owner, _ := candidateOwner(ownerCtx())
				ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ownerCtx(), owner))
				if _, err := m.producer.Execute(ctx, "mutation "+renderCall("updatePipelineRun", map[string]any{"runId": m.runID, "conclusion": "failure"})); err != nil {
					t.Error(err)
				}
			})
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	_, promotes, uploads, _, _ := m.github.counts()
	if v.Phase != "uncertain" || promotes != 0 || uploads != 3 {
		t.Fatal("the release went public on evidence that had changed", v.Phase, promotes, uploads)
	}
}

// The role read sits before every inner operation, not only each recipe step: one
// promotion spends minutes reading bytes before its claim and its PATCH.
func TestReleaseBuildPromotionStopsWhenTheOwnerLosesTheRole(t *testing.T) {
	m := newManyFileRelease(t, 2)
	var once sync.Once
	m.github.beforeDownload = func(string, http.ResponseWriter, *http.Request) bool {
		if m.receipts() == 2 {
			once.Do(func() { m.ownerRole.Store(string(auth.RoleAdmin)) })
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	_, promotes, _, _, _ := m.github.counts()
	if v.Phase != "uncertain" || promotes != 0 || m.draftState() != "ready" {
		t.Fatal("a demoted owner's release went public", v.Phase, promotes, m.draftState())
	}
	m.ownerRole.Store(string(auth.RoleOwner))
	m.github.beforeDownload = nil
	if v, err = m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal("the restored owner could not finish", v.Phase, err)
	}
}

// Once a promotion is claimed it never repeats on a guess, so a read that still fails
// between the claim and the PATCH cannot be fixed by pressing again. This is the window
// that remains, and it is a few reads whatever the release's size. The owner has to be
// told what will fix it.
func TestReleaseBuildPublishExplainsAPromotionItCannotRepeat(t *testing.T) {
	m := newManyFileRelease(t, 2)
	m.github.beforeRelease = func(w http.ResponseWriter, _ *http.Request) bool {
		if m.draftState() == "promoting" {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	_, promotes, _, _, _ := m.github.counts()
	if v.Phase != "uncertain" || v.ReasonCode != "promotion_unconfirmed" || promotes != 0 || m.draftState() != "promoting" {
		t.Fatal("a stopped promotion did not say what it needs", v.Phase, v.ReasonCode, promotes, m.draftState())
	}
	// What stopped it may be a change on GitHub, and publishing that by hand is no fix.
	if !strings.Contains(v.Reason, "still the approved ones") {
		t.Fatal("the reason sends the owner to publish without checking the draft", v.Reason)
	}
	m.github.beforeRelease = nil
	if v, err = m.publish(ownerCtx(), m.i); err != nil {
		t.Fatal(err)
	}
	if _, promotes, _, _, _ = m.github.counts(); v.ReasonCode != "promotion_unconfirmed" || promotes != 0 {
		t.Fatal("pressing again guessed", v.ReasonCode, promotes)
	}
	// The owner makes it public on GitHub, as the reason says, and presses again.
	m.github.mu.Lock()
	m.github.published = true
	m.github.mu.Unlock()
	if v, err = m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" || v.Catalog.State != "sealed" {
		t.Fatal("the release was not recorded once public", v.Phase, v.ReasonCode, err)
	}
	if _, promotes, _, _, _ = m.github.counts(); promotes != 0 {
		t.Fatal("the PATCH was sent after all", promotes)
	}
}

// The regression. Whatever can fail between a promotion's claim and its PATCH strands
// the release until the owner publishes on GitHub, and that used to include a read of
// every byte, however large the release. A dropped read there now has nothing to drop.
func TestReleaseBuildPromotionReadsNoBytesBetweenItsClaimAndItsPatch(t *testing.T) {
	m := newManyFileRelease(t, 2)
	var inWindow atomic.Int64
	m.github.beforeDownload = func(_ string, w http.ResponseWriter, _ *http.Request) bool {
		if _, promotes, _, _, _ := m.github.counts(); promotes == 0 && m.draftState() == "promoting" {
			inWindow.Add(1)
			dropMidRead(w)
			return true
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "published" || v.Catalog.State != "sealed" {
		t.Fatal("a read in the window stranded the release", v.Phase, v.ReasonCode, m.draftState(), inWindow.Load())
	}
	if _, promotes, _, _, _ := m.github.counts(); promotes != 1 || inWindow.Load() != 0 {
		t.Fatal("the window was not free of byte reads, or the PATCH repeated", promotes, inWindow.Load())
	}
}

// A credential that cannot be had costs nothing before the claim and strands the
// release after it, so a promotion gets its credential first.
func TestReleaseBuildPromotionHoldsItsCredentialBeforeItsClaim(t *testing.T) {
	m := newManyFileRelease(t, 2)
	secrets := m.i.resolver.systemSecret
	var unavailable atomic.Bool
	m.i.resolver.systemSecret = func(ctx context.Context, name string) (string, error) {
		if name == "TAG_TOKEN" && unavailable.Load() {
			return "", errors.New("the secret store is unreachable")
		}
		return secrets(ctx, name)
	}
	// The promotion's first read holds a credential already, so from here the next
	// one asked for is the one its PATCH would use.
	m.github.beforeDownload = func(string, http.ResponseWriter, *http.Request) bool {
		if m.receipts() == 2 {
			unavailable.Store(true)
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if _, promotes, _, _, _ := m.github.counts(); v.Phase != "uncertain" || promotes != 0 || m.draftState() != "ready" {
		t.Fatal("a credential failure spent the claim", v.Phase, v.ReasonCode, promotes, m.draftState())
	}
	m.github.beforeDownload = nil
	unavailable.Store(false)
	if v, err = m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal("pressing again did not finish", v.Phase, v.ReasonCode, err)
	}
	if _, promotes, _, _, _ := m.github.counts(); promotes != 1 {
		t.Fatal("promotion did not send exactly one PATCH", promotes)
	}
}

// Once a promotion is claimed the reason cannot say nothing changed: the release may be
// public, and an edited file on it is a different problem. Either way the owner is led
// through the same steps: the approved file back, then whatever each press then says.
func TestReleaseBuildConflictFoundAfterTheClaimSaysThePromotionStarted(t *testing.T) {
	for _, afterPatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "at the last look", true: "after the PATCH"}[afterPatch], func(t *testing.T) {
			m := newManyFileRelease(t, 2)
			var approved []byte
			var once sync.Once
			m.github.beforeRelease = func(http.ResponseWriter, *http.Request) bool {
				_, promotes, _, _, _ := m.github.counts()
				if m.draftState() == "promoting" && (promotes == 1) == afterPatch {
					once.Do(func() {
						m.github.mu.Lock()
						defer m.github.mu.Unlock()
						approved = m.github.assets[1].body
						m.github.assets[1].body = []byte("edited after its upload")
					})
				}
				return false
			}
			v, err := m.publish(ownerCtx(), m.i)
			if err != nil {
				t.Fatal(err)
			}
			m.github.beforeRelease = nil
			wantPromotes := 0
			if afterPatch {
				// The read after the PATCH cannot tell an edit from a PATCH that did not
				// land, so it reports the promotion. The next look names the file.
				wantPromotes = 1
				if v.ReasonCode != "promotion_unconfirmed" {
					t.Fatal("wrong bytes after the PATCH were reported as something else", v.Phase, v.ReasonCode)
				}
				if v, err = m.publish(ownerCtx(), m.i); err != nil {
					t.Fatal(err)
				}
			}
			if v.Phase != "uncertain" || v.ReasonCode != "release_asset_conflict" || !strings.Contains(v.Reason, "file-01.tar.gz") || m.draftState() != "promoting" {
				t.Fatal("the changed file was not named", v.Phase, v.ReasonCode, v.Reason, m.draftState())
			}
			if strings.Contains(v.Reason, "nothing was changed") || strings.Contains(v.Reason, "in the draft") {
				t.Fatal("a claimed promotion was reported as if nothing had happened", v.Reason)
			}
			if _, promotes, _, _, _ := m.github.counts(); promotes != wantPromotes {
				t.Fatal("the PATCH count is wrong", promotes, wantPromotes)
			}
			m.github.mu.Lock()
			m.github.assets[1].body = approved
			m.github.mu.Unlock()
			if v, err = m.publish(ownerCtx(), m.i); err != nil {
				t.Fatal(err)
			}
			if !afterPatch {
				// The claim is spent, so a restored file still needs the owner's hand.
				if v.ReasonCode != "promotion_unconfirmed" {
					t.Fatal("pressing again guessed", v.Phase, v.ReasonCode)
				}
				m.github.mu.Lock()
				m.github.published = true
				m.github.mu.Unlock()
				if v, err = m.publish(ownerCtx(), m.i); err != nil {
					t.Fatal(err)
				}
			}
			if v.Phase != "published" || v.Catalog.State != "sealed" {
				t.Fatal("the restored release was not recorded", v.Phase, v.ReasonCode)
			}
			if _, promotes, _, _, _ := m.github.counts(); promotes != wantPromotes {
				t.Fatal("the PATCH was repeated", promotes, wantPromotes)
			}
		})
	}
}

// A failed read proves nothing about the bytes. Calling it a conflict told the owner
// to remove a correct file, and after a receipt nothing would ever send that file again.
func TestReleaseBuildDoesNotCallADroppedReadAConflict(t *testing.T) {
	m := newManyFileRelease(t, 2)
	var once sync.Once
	m.github.beforeDownload = func(_ string, w http.ResponseWriter, _ *http.Request) bool {
		dropped := false
		once.Do(func() {
			dropped = true
			dropMidRead(w)
		})
		return dropped
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" || v.ReasonCode != "operation_unconfirmed" {
		t.Fatal("a dropped read was reported as something else", v.Phase, v.ReasonCode)
	}
	if v, err = m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal("pressing again did not finish", v.Phase, v.ReasonCode, err)
	}
	if _, _, _, _, removed := m.github.counts(); removed != 0 {
		t.Fatal("a correct file was removed", removed)
	}
}

func TestReleaseBuildNamesAConflictFoundAtPromotion(t *testing.T) {
	m := newManyFileRelease(t, 2)
	var approved []byte
	var once sync.Once
	m.github.beforeDownload = func(string, http.ResponseWriter, *http.Request) bool {
		if m.receipts() == 2 {
			once.Do(func() {
				m.github.mu.Lock()
				defer m.github.mu.Unlock()
				approved = m.github.assets[1].body
				m.github.assets[1].body = []byte("edited after its upload")
			})
		}
		return false
	}
	v, err := m.publish(ownerCtx(), m.i)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "uncertain" || v.ReasonCode != "release_asset_conflict" || !strings.Contains(v.Reason, "file-01.tar.gz") || m.draftState() != "ready" {
		t.Fatal("a changed file was not named before the claim", v.Phase, v.ReasonCode, v.Reason, m.draftState())
	}
	// Removing it would strand the release, because its receipt keeps it from being
	// sent again. So the reason asks for the approved file back, and that works.
	if strings.Contains(strings.ToLower(v.Reason), "remove") {
		t.Fatal("the reason gives advice that strands the release", v.Reason)
	}
	m.github.mu.Lock()
	m.github.assets[1].body = approved
	m.github.mu.Unlock()
	if v, err = m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal("the restored release did not finish", v.Phase, v.ReasonCode, err)
	}
}

func credentialContext(t *testing.T, class string) context.Context {
	t.Helper()
	access, ok := auth.AccessFromContext(ownerCtx())
	if !ok {
		t.Fatal("no owner access")
	}
	authority, err := auth.ForwardedAuthorityForUser(access, class, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return auth.ContextWithExecutionAuthority(auth.ContextWithForwardedAuthority(ownerCtx(), authority), authority, nil)
}

// The operator key and a troubleshooting cluster are owners with no stored row. They
// could always publish, and detaching from the caller must not change that.
func TestStoredOwnerAuthorityByCredential(t *testing.T) {
	for _, tc := range []struct {
		class string
		read  bool
	}{
		{class: auth.ForwardedClassUser, read: true},
		{class: auth.ForwardedClassPat, read: true},
		{class: auth.ForwardedClassOperator},
		{class: auth.ForwardedClassLocalDev},
	} {
		t.Run(tc.class, func(t *testing.T) {
			authority, err := captureStoredOwnerAuthority(credentialContext(t, tc.class))
			if err != nil || (authority != nil) != tc.read {
				t.Fatal("wrong treatment of this credential", authority, err)
			}
		})
	}
	refused := auth.ContextWithExecutionAuthority(ownerCtx(), auth.ForwardedAuthority{}, errors.New("rotation failed"))
	if _, err := captureStoredOwnerAuthority(refused); err == nil {
		t.Fatal("a stream whose credential failed verification started a publication")
	}
}

func TestReleaseBuildPublishesUnderTheOperatorCredential(t *testing.T) {
	m := newManyFileRelease(t, 2)
	// The stored role is not this credential's authority, so it must not be read.
	m.ownerRole.Store(string(auth.RoleAdmin))
	v, err := m.publish(credentialContext(t, auth.ForwardedClassOperator), m.i)
	if err != nil || v.Phase != "published" {
		t.Fatal("the operator credential could not publish", v.Phase, v.ReasonCode, err)
	}
}

// The other tests stand in for this read so they can change a role without touching
// who owns the shared test database. This one runs it against the real engine.
func TestStoredOwnerCheckReadsTheRealIdentityRow(t *testing.T) {
	db := candidateTestDB(t)
	engine := assemblyEngine(t, db)
	i := NewIntegration(nil, engine, resolver{})
	owner, demoted := id.NewShortId(), id.NewShortId()
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM "MemoryNodes" WHERE id IN ($1,$2)`, "v1:identity:user:"+owner, "v1:identity:user:"+demoted); err != nil {
			t.Error(err)
		}
	})
	for user, role := range map[string]auth.Role{owner: auth.RoleOwner, demoted: auth.RoleAdmin} {
		profileMutation(t, engine, "createUser", map[string]any{"userId": "v1:identity:user:" + user, "displayName": "Fixture", "primaryEmail": user + "@example.invalid", "role": string(role)})
	}
	authority, err := captureStoredOwnerAuthority(ownerCtx())
	if err != nil || authority == nil {
		t.Fatal(authority, err)
	}
	if err := i.checkStoredOwner(ownerCtx(), owner, authority); err != nil {
		t.Fatal("a stored owner was refused", err)
	}
	if err := i.checkStoredOwner(ownerCtx(), demoted, authority); err == nil {
		t.Fatal("a stored admin passed as owner")
	}
	if err := i.checkStoredOwner(ownerCtx(), id.NewShortId(), authority); err == nil {
		t.Fatal("an owner with no stored row passed")
	}
}

// Draft creation has nothing left to do once its group is ready, so siblings cost no
// remote read. Promotion is not settled by ready.
func TestPublicationActionAnswersASettledDraftWithoutRemoteWork(t *testing.T) {
	m := newManyFileRelease(t, 3)
	var once sync.Once
	m.github.beforeUpload = func(_ string, w http.ResponseWriter, _ *http.Request) bool {
		dropped := false
		once.Do(func() {
			dropped = true
			dropConnection(w)
		})
		return dropped
	}
	if v, err := m.publish(ownerCtx(), m.i); err != nil || v.Phase != "uncertain" || m.draftState() != "ready" {
		t.Fatal("the fixture did not stop with a ready draft", v.Phase, err)
	}
	storage, err := m.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := storage.ledger.claimBuild(ownerCtx(), m.intent)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close(ownerCtx())
	operations := (&releaseBuildScope{m.i, lease}).publicationOperations()
	args := map[string]any{"candidateId": m.candidate, "approvalId": m.args["approvalId"], "targetId": "file-02"}
	before := m.github.requests.Load()
	out, err := operations["releaseCreateCandidateDraft"](ownerCtx(), args)
	if err != nil || out.(map[string]any)["state"] != "ready" {
		t.Fatal("a ready group did not answer from its journal", out, err)
	}
	if m.github.requests.Load() != before {
		t.Fatal("a settled draft still went to GitHub")
	}
	if _, err := operations["releasePromoteCandidateDraft"](ownerCtx(), args); err == nil {
		t.Fatal("a ready draft was treated as already public")
	}
}

// A registry write is public the moment it lands, unlike an upload to a draft, so each
// one still verifies the candidate for itself.
func TestReleaseBuildVerifiesAfreshBeforeEveryRegistryWrite(t *testing.T) {
	f := newBuildDBFixture(t)
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	profile := &f.config.Profiles[0]
	first := profile.Registries[0]
	first.ID, first.Origin, first.Repository, first.AllowLoopbackHTTP = "registry-a", server.URL, "candidate/"+f.runID+"/a", true
	second := first
	second.ID, second.Repository = "registry-b", "candidate/"+f.runID+"/b"
	profile.Registries = []candidateRegistryTarget{first, second}
	*f.source = strings.Replace(profileFixtureSource, `publications: [{targetId: "registry", tag: facts.components.0.version}]`, `publications: [{targetId: "registry-a", tag: facts.components.0.version}, {targetId: "registry-b", tag: facts.components.0.version}]`, 1)
	f.publication = `@template
automation fixtureBuildPublication {
 args { candidateId string! approvalId string! }
 builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId, component: "engine", artifact: "image", targetId: "registry-a")
 builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId, component: "engine", artifact: "image", targetId: "registry-b")
}`
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	r := f.prepare(t)
	buildAction(t, f.i, r.ID, "start")
	f.pipeline.run.Status, f.pipeline.run.Conclusion, f.pipeline.run.WorkRunID = "completed", "success", f.runs["build"]
	buildAction(t, f.i, r.ID, "observe")
	v := buildAction(t, f.i, r.ID, "resume")
	if v.CandidateID == "" {
		t.Fatal("no candidate was assembled", v)
	}
	cleanupCandidate(t, f.db, v.CandidateID)
	p, err := f.i.configuredCandidateFor(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := p.approve(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	before := f.library.opens
	// This recipe seals nothing, so the action ends uncertain. Only its reads count.
	if _, err := f.i.executeBuild(ownerCtx(), map[string]any{"intentId": r.ID, "approvalId": approved.ApprovalID}, "publish"); err != nil {
		t.Fatal(err)
	}
	// Each write: its own whole-candidate verification, then its own image snapshot.
	// The single artifact opens once for each. Remembering would give 3.
	if opens := f.library.opens - before; opens != 4 {
		t.Fatal("a public registry write relied on an earlier verification", opens)
	}
}

func TestPublicationActionCannotSkipApprovalForASettledGroup(t *testing.T) {
	m := newManyFileRelease(t, 2)
	if v, err := m.publish(ownerCtx(), m.i); err != nil || v.Phase != "published" {
		t.Fatal(v.Phase, err)
	}
	storage, err := m.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := storage.ledger.claimBuild(ownerCtx(), m.intent)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close(ownerCtx())
	operations := (&releaseBuildScope{m.i, lease}).publicationOperations()
	args := map[string]any{"candidateId": lease.record.CandidateID, "approvalId": "another-approval", "targetId": "file-00"}
	for _, name := range []string{"releaseCreateCandidateDraft", "releasePromoteCandidateDraft"} {
		if _, err := operations[name](ownerCtx(), args); err == nil {
			t.Fatal("a settled group answered without the exact approval", name)
		}
	}
	args["approvalId"] = m.args["approvalId"]
	for _, name := range []string{"releaseCreateCandidateDraft", "releasePromoteCandidateDraft"} {
		out, err := operations[name](ownerCtx(), args)
		if err != nil {
			t.Fatal(name, err)
		}
		if out.(map[string]any)["state"] != "published" {
			t.Fatal("a settled group did not answer from its durable record", name, out)
		}
	}
}

func TestCandidateVerificationIsRememberedOnlyForItsExactIdentity(t *testing.T) {
	db := candidateTestDB(t)
	p, c, library, _ := candidateFilePublicationFixture(t, db)
	pub := preparePublication(t, db, p, c)
	record, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	remembering := *p
	remembering.verified = &candidateVerification{}
	verify := func(host *candidatePreparer) (int, error) {
		before := library.opens
		_, err := host.prepareWithExpectedIdentity(ownerCtx(), record.Manifest, candidatePrepareDefinition(t), record.ID, nil)
		return library.opens - before, err
	}
	if opens, err := verify(&remembering); err != nil || opens != 1 {
		t.Fatal("the first call must verify the bytes", opens, err)
	}
	if opens, err := verify(&remembering); err != nil || opens != 0 {
		t.Fatal("the action verified twice", opens, err)
	}
	// The identity is still computed: a remembered verification never excuses a
	// changed engine, recipe or target.
	changed := remembering
	changed.engineRevision = func() string { return strings.Repeat("e", 40) }
	if _, err := verify(&changed); err == nil {
		t.Fatal("a remembered verification hid a changed identity")
	}
	// Every direct owner call keeps its own fresh verification.
	if opens, err := verify(p); err != nil || opens != 1 {
		t.Fatal("a call outside an action skipped verification", opens, err)
	}
}

func candidatePrepareDefinition(t *testing.T) *automations.Automation {
	t.Helper()
	definition, err := workflowhost.Load(candidatePrepareWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
