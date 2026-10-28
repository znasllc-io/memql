package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

const buildFixturePlan = `@template
automation fixtureBuildPlan {
 args { facts object! }
 return {version: args.facts.version, tag: "v" + args.facts.version}
}`

const buildFixtureWorkflow = `@template
automation fixtureBuildWorkflow {
 tag := builtin releaseBuildEnsureTag()
 if !tag.confirmed { return }
 running := builtin releaseBuildRun()
 observed := builtin releaseBuildObserve()
 if observed.phase == "built" { builtin releaseBuildAssemble() }
}`

// This fixture intentionally cannot publish. Separate controls exercise the
// scoped publication ports; existing draft/upload/catalog tests prove effects.
const buildFixturePublication = `@template
automation fixtureBuildPublication {
 args { candidateId string! approvalId string! }
 candidate := builtin releaseGetCandidate(candidateId: args.candidateId)
 return candidate
}`

type buildPipelineFixture struct {
	source                pl.ReleaseBuildSource
	run                   *pl.ReleaseBuildRun
	starts, observations  int
	nextRunID             string
	loseStart, missingRun bool
	beforeStart           func()
}

func (f *buildPipelineFixture) InspectReleaseBuild(_ context.Context, r pl.ReleaseBuildRequest) (pl.ReleaseBuildSource, error) {
	if r.ExpectedSHA != "" && r.ExpectedSHA != f.source.SHA {
		return pl.ReleaseBuildSource{}, errors.New("selected source changed")
	}
	out := f.source
	out.ExistingRun = f.run
	return out, nil
}
func (f *buildPipelineFixture) StartReleaseBuild(ctx context.Context, r pl.ReleaseBuildRequest, fence func(context.Context) error) (pl.ReleaseBuildRun, error) {
	if f.beforeStart != nil {
		f.beforeStart()
	}
	if err := fence(ctx); err != nil {
		return pl.ReleaseBuildRun{}, err
	}
	f.starts++
	run := pl.ReleaseBuildRun{ID: id.NewShortId(), OwnerUserID: f.source.OwnerUserID, PipelineID: r.PipelineID, Repository: r.Repository, SHA: r.ExpectedSHA, Tag: r.Tag, Mode: "full", Event: "release", Status: "in_progress", Attempt: 1}
	if f.nextRunID != "" {
		run.ID = f.nextRunID
	}
	if !f.missingRun {
		f.run = &run
	}
	if f.loseStart {
		return pl.ReleaseBuildRun{}, errors.New("lost response")
	}
	return run, nil
}
func (f *buildPipelineFixture) ObserveReleaseBuild(context.Context, pl.ReleaseBuildRequest) (pl.ReleaseBuildRun, bool, error) {
	f.observations++
	if f.run == nil {
		return pl.ReleaseBuildRun{}, false, nil
	}
	return *f.run, true, nil
}

type buildDBFixture struct {
	profileDBFixture
	pipeline    *buildPipelineFixture
	key         string
	tagPosts    int
	hideTag     bool
	loseTag     bool
	workflow    string
	publication string
}

func newBuildDBFixture(t *testing.T) *buildDBFixture {
	t.Helper()
	f := &buildDBFixture{profileDBFixture: newProfileDBFixture(t, "release", "full"), workflow: buildFixtureWorkflow, publication: buildFixturePublication}
	owner, _ := candidateOwner(ownerCtx())
	var pipelineID string
	if err := f.db.QueryRow(`SELECT payload->>'pipelineId' FROM "MemoryNodes" WHERE id=$1 ORDER BY "createdAt" DESC LIMIT 1`, "v1:pipelines:run:"+f.runID).Scan(&pipelineID); err != nil {
		t.Fatal(err)
	}
	pipelineID = memql.BareShortId(pipelineID)
	f.pipeline = &buildPipelineFixture{nextRunID: f.runID, source: pl.ReleaseBuildSource{OwnerUserID: owner, PipelineID: pipelineID, SourceID: id.NewShortId(), CredentialID: id.NewShortId(), Repository: "acme/engine", Branch: "main", HeadSHA: strings.Repeat("b", 40), SHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), ProofSHA: strings.Repeat("b", 40), InstallationID: 1, RepositoryID: 2, Checks: []pl.ReleaseBuildCheck{{Context: "required", AppID: 3, SHA: strings.Repeat("b", 40), Status: "completed", Conclusion: "success"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-tag-token" {
			t.Error("unscoped tag credential")
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/engine/git/ref/tags/v0.25.0":
			if !f.pipeline.source.TagExists || f.hideTag {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/v0.25.0", "object": map[string]any{"type": "commit", "sha": f.pipeline.source.TagSHA}})
		case r.Method == "POST" && r.URL.Path == "/repos/acme/engine/git/refs":
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["ref"] != "refs/tags/v0.25.0" || request["sha"] != f.pipeline.source.SHA {
				t.Error("tag identity changed")
			}
			var attempted bool
			if err := f.db.QueryRow(`SELECT (body->>'tagAttempted')::boolean FROM release_build_intents WHERE intent_id=$1`, f.key).Scan(&attempted); err != nil || !attempted {
				t.Error("tag POST preceded durable attempt", err)
			}
			f.tagPosts++
			f.pipeline.source.TagExists = true
			f.pipeline.source.TagSHA = f.pipeline.source.SHA
			if f.loseTag {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
				} else {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(201)
		default:
			t.Error("unexpected tag protocol", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	p := &f.config.Profiles[0]
	p.ReleaseAssets = []candidateProfileFileAuthority{{ID: "tag", Component: "engine", Artifact: "docs", Repository: "acme/engine", TagPrefix: "v", APIOrigin: server.URL, UploadOrigin: server.URL, CredentialSecret: "TAG_TOKEN", AllowLoopbackHTTP: true}}
	p.Build = &candidateBuildConfiguration{Component: "engine", Run: "build", Branch: "main", TagPrefix: "v", TagTargetID: "tag", Plan: "fixtureBuildPlan", Workflow: "fixtureBuildWorkflow", PublicationWorkflow: "fixtureBuildPublication"}
	f.i.resolver.systemSecret = func(_ context.Context, name string) (string, error) {
		if name != "TAG_TOKEN" {
			return "", errors.New("wrong tag secret")
		}
		return "fixture-tag-token", nil
	}
	loader := f.i.profileSource
	f.i.profileSource = func(kind, name string) (string, error) {
		if kind == "automation" {
			switch name {
			case "fixtureBuildPlan":
				return buildFixturePlan, nil
			case "fixtureBuildWorkflow":
				return f.workflow, nil
			case "fixtureBuildPublication":
				return f.publication, nil
			}
		}
		return loader(kind, name)
	}
	if err := f.i.ConfigureReleaseBuilds(f.pipeline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.db.Exec(`DELETE FROM release_build_intents WHERE body->'selection'->'request'->>'PipelineID'=$1`, pipelineID); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *buildDBFixture) prepare(t *testing.T) releaseBuildRecord {
	t.Helper()
	_, _, digest, err := f.i.candidateProfile(ownerCtx(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.i.prepareBuild(ownerCtx(), "engine", f.pipeline.source.PipelineID, digest)
	if err != nil {
		t.Fatal(err)
	}
	f.key = r.ID
	return r
}
func (f *buildDBFixture) peer(t *testing.T) *Integration {
	t.Helper()
	i := NewIntegration(nil, assemblyEngine(t, f.db), f.i.resolver)
	i.profileSource, i.profileRevision = f.i.profileSource, f.i.profileRevision
	if err := i.ConfigureCandidates(*f.i.candidateDeps.Load()); err != nil {
		t.Fatal(err)
	}
	if err := i.ConfigureReleaseBuilds(f.pipeline); err != nil {
		t.Fatal(err)
	}
	return i
}
func buildAction(t *testing.T, i *Integration, key, action string) releaseBuildView {
	t.Helper()
	rows, err := i.executeBuild(ownerCtx(), map[string]any{"intentId": key}, action)
	if err != nil || len(rows) != 1 {
		t.Fatal(action, err)
	}
	var v releaseBuildView
	if json.Unmarshal(rows[0].Payload, &v) != nil {
		t.Fatal("invalid build projection")
	}
	return v
}

func TestReleaseBuildTagLostReplyRecoversOnAnotherReplicaWithoutReplay(t *testing.T) {
	f := newBuildDBFixture(t)
	r := f.prepare(t)
	if r.Phase != "prepared" || f.tagPosts != 0 || f.pipeline.starts != 0 || len(f.library.pins) != 0 {
		t.Fatal("prepare caused an external effect")
	}
	f.loseTag, f.hideTag = true, true
	v := buildAction(t, f.i, r.ID, "start")
	if v.Phase != "uncertain" || f.tagPosts != 1 || f.pipeline.starts != 0 {
		t.Fatal("lost tag was not retained", v)
	}
	// Observe reports the provider's actual visibility. The first response is
	// still absent, so a fresh replica must not issue another tag POST.
	f.pipeline.source.TagExists = false
	peer := f.peer(t)
	v = buildAction(t, peer, r.ID, "resume")
	if v.Phase != "uncertain" || f.tagPosts != 1 || f.pipeline.starts != 0 {
		t.Fatal("missing tag was replayed", v)
	}
	f.hideTag = false
	f.pipeline.source.TagExists = true
	v = buildAction(t, peer, r.ID, "resume")
	if v.Phase != "building" || f.tagPosts != 1 || f.pipeline.starts != 1 {
		t.Fatal("confirmed tag did not resume exactly once", v)
	}
	view, err := peer.handleBuildGet(ownerCtx(), map[string]any{"intentId": r.ID}, 1)
	if err != nil || len(view) != 1 {
		t.Fatal("reentry lost owner history", err)
	}
}

func TestReleaseBuildLostDispatchNeverReplacesUnknownRun(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "found", true: "absent"}[missing], func(t *testing.T) {
			f := newBuildDBFixture(t)
			f.pipeline.source.TagExists = true
			f.pipeline.source.TagSHA = f.pipeline.source.SHA
			r := f.prepare(t)
			f.pipeline.loseStart, f.pipeline.missingRun = true, missing
			if v := buildAction(t, f.i, r.ID, "start"); v.Phase != "uncertain" {
				t.Fatal(v)
			}
			peer := f.peer(t)
			v := buildAction(t, peer, r.ID, "resume")
			if f.pipeline.starts != 1 || f.tagPosts != 0 {
				t.Fatal("dispatch replayed")
			}
			want := "building"
			if missing {
				want = "uncertain"
			}
			if v.Phase != want {
				t.Fatal(v)
			}
			if again := f.prepare(t); again.ID != r.ID || again.SelectionDigest != r.SelectionDigest {
				t.Fatal("new observation reset immutable intent")
			}
		})
	}
}

func TestReleaseBuildCompletedJournalAssemblyAndSeparateApprovalAcrossReplicas(t *testing.T) {
	f := newBuildDBFixture(t)
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	r := f.prepare(t)
	v := buildAction(t, f.i, r.ID, "start")
	if v.Phase != "building" {
		t.Fatal(v)
	}
	f.pipeline.run.Status, f.pipeline.run.Conclusion, f.pipeline.run.WorkRunID = "completed", "success", f.runs["build"]
	peer := f.peer(t)
	v = buildAction(t, peer, r.ID, "observe")
	if v.Phase != "built" || v.CandidateID != "" || len(f.library.pins) != 0 {
		t.Fatal("observation assembled or lost completion", v)
	}
	v = buildAction(t, peer, r.ID, "resume")
	if v.Phase != "ready" || v.CandidateID == "" || v.ApprovalID != "" || !slices.Contains(v.Actions, "approve") {
		t.Fatal("candidate not separately reviewable", v)
	}
	cleanupCandidate(t, f.db, v.CandidateID)
	p, err := peer.configuredCandidateFor(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := p.approve(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.i.handleBuildGet(ownerCtx(), map[string]any{"intentId": r.ID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(rows[0].Payload, &v) != nil || v.ApprovalID != approved.ApprovalID || !slices.Contains(v.Actions, "publish") {
		t.Fatal("read did not observe separate approval", v)
	}
	if f.pipeline.starts != 1 || f.tagPosts != 0 {
		t.Fatal("assembly repeated source effects")
	}
}

func TestReleaseBuildAuthorityLeaseAndAttemptFences(t *testing.T) {
	f := newBuildDBFixture(t)
	r := f.prepare(t)
	storage, err := f.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	l, err := storage.ledger.claimBuild(ownerCtx(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ledger.claimBuild(ownerCtx(), r.ID); err == nil {
		t.Fatal("second replica claimed live lease")
	}
	l.record.TagAttempted = true
	if err := l.save(ownerCtx()); err != nil {
		t.Fatal(err)
	}
	l.record.TagAttempted = false
	if err := l.save(ownerCtx()); err == nil {
		t.Fatal("attempt fence cleared")
	}
	l.record.TagAttempted = true
	if _, err := f.db.Exec(`UPDATE release_build_intents SET lease_until='epoch' WHERE intent_id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := (&releaseBuildScope{f.i, l}).ensureTag(ownerCtx(), nil); err == nil || f.tagPosts != 0 {
		t.Fatal("expired lease dispatched")
	}
	l.close(ownerCtx())
	// Config changes cannot open a second durable fence for the same source.
	f.config.Profiles[0].ReleaseAssets[0].CredentialSecret = "ROTATED_REFERENCE"
	again := f.prepare(t)
	if again.ID != r.ID || again.SelectionDigest != r.SelectionDigest {
		t.Fatal("authority change reset selection")
	}
	if _, err := f.i.executeBuild(ownerCtx(), map[string]any{"intentId": r.ID}, "start"); err == nil {
		t.Fatal("changed authority executed")
	}
	if f.tagPosts != 0 || f.pipeline.starts != 0 {
		t.Fatal("authority refusal had effects")
	}
}

func TestReleaseBuildRefusesCollisionsAndForeignOwnerBeforeEffects(t *testing.T) {
	for _, kind := range []string{"tag collision", "run collision", "foreign owner", "revoked before opening"} {
		t.Run(kind, func(t *testing.T) {
			f := newBuildDBFixture(t)
			switch kind {
			case "tag collision":
				f.pipeline.source.TagExists = true
				f.pipeline.source.TagSHA = strings.Repeat("e", 40)
			case "run collision":
				f.pipeline.run = &pl.ReleaseBuildRun{ID: id.NewShortId(), Tag: "other/v1"}
			case "foreign owner":
				f.pipeline.source.OwnerUserID = id.NewShortId()
			case "revoked before opening":
				f.pipeline.source.TagExists = true
				f.pipeline.source.TagSHA = f.pipeline.source.SHA
				f.pipeline.beforeStart = func() { f.config.Profiles[0].Build.TagPrefix = "changed/" }
			}
			if kind == "foreign owner" {
				_, _, digest, err := f.i.candidateProfile(ownerCtx(), "engine")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.i.prepareBuild(ownerCtx(), "engine", f.pipeline.source.PipelineID, digest); err == nil {
					t.Fatal("foreign source accepted")
				}
				return
			}
			r := f.prepare(t)
			if kind == "revoked before opening" {
				v := buildAction(t, f.i, r.ID, "start")
				if v.Phase != "uncertain" {
					t.Fatal(v)
				}
			} else {
				if r.Phase != "blocked" {
					t.Fatal(r.Phase)
				}
				if _, err := f.i.executeBuild(ownerCtx(), map[string]any{"intentId": r.ID}, "start"); err == nil {
					t.Fatal("blocked source started")
				}
			}
			if f.tagPosts != 0 || f.pipeline.starts != 0 {
				t.Fatal("refusal reached effect")
			}
		})
	}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		i := NewIntegration(nil, nil, resolver{})
		if _, err := i.handleBuildGet(ctx, map[string]any{"intentId": "anything"}, 1); err == nil {
			t.Fatal("non-owner read journal")
		}
		if _, err := i.prepareBuild(ctx, "engine", "pipeline", "configuration"); err == nil {
			t.Fatal("non-owner prepared release")
		}
	}
}

func TestReleaseBuildOptionalConfigPreservesProfileIdentityAndPrivateScope(t *testing.T) {
	for _, publication := range []bool{false, true} {
		for name, operation := range buildCaptureOperations(publication) {
			if operation == nil {
				t.Fatal("capture operation is missing", name)
			}
			if _, err := operation(ownerCtx(), nil); err == nil {
				t.Fatal("capture operation executed", name)
			}
		}
	}
	f := newProfileDBFixture(t, "release", "full")
	p, snapshot, digest, err := f.i.candidateProfile(ownerCtx(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := profileDigest(struct {
		Profile          candidateProfile
		Resolver, Engine string
	}{p, snapshot.Version, f.i.profileRevision()})
	if err != nil || digest != legacy {
		t.Fatal("absent build changed existing profile identity", err)
	}
	for _, name := range []string{"releaseBuildFacts", "releaseBuildEnsureTag", "releaseBuildRun", "releaseBuildObserve", "releaseBuildAssemble"} {
		found := false
		for _, capability := range f.i.Capabilities() {
			if capability.Name == name {
				found = true
				if _, err := capability.Handler(ownerCtx(), nil, 1); err == nil {
					t.Fatal("unscoped private port accepted", name)
				}
			}
		}
		if !found {
			t.Fatal("private port missing", name)
		}
	}
}

func TestReleaseBuildPublicationCannotBorrowAnotherApprovalOrCandidate(t *testing.T) {
	f := newBuildDBFixture(t)
	r := f.prepare(t)
	p, err := f.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.ledger.claimBuild(ownerCtx(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close(ownerCtx())
	s := &releaseBuildScope{f.i, l}
	if err := s.publish(ownerCtx(), "unapproved"); err == nil {
		t.Fatal("publication without separately approved candidate")
	}
	for name, op := range s.publicationOperations() {
		if _, err := op(ownerCtx(), map[string]any{"candidateId": "sha256:" + strings.Repeat("a", 64), "approvalId": "foreign"}); err == nil {
			t.Fatal("borrowed candidate reached native operation", name)
		}
	}
	// Capturing a publication recipe cannot introduce approval or arbitrary
	// installed operations into the release's bound authority.
	loader := func(kind, name string) (string, error) {
		if kind == "automation" {
			return "@template\nautomation badPublication { builtin releaseApproveCandidate(candidateId: \"foreign\") }", nil
		}
		return "", workflowhost.ErrSourceNotFound
	}
	if _, err := workflowhost.Capture("badPublication", buildPublicationContract, loader, buildCaptureOperations(true)); err == nil {
		t.Fatal("publication recipe captured automatic approval")
	}
}

func TestReleaseBuildPublicationRecipeResumesCompletedEffectsOnAnotherReplica(t *testing.T) {
	f := newBuildDBFixture(t)
	var writes atomic.Int64
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.config.Profiles[0].Registries[0].Origin = server.URL
	f.config.Profiles[0].Registries[0].Repository = "candidate/" + f.runID
	f.config.Profiles[0].Registries[0].AllowLoopbackHTTP = true
	f.publication = `@template
automation fixtureBuildPublication {
 args { candidateId string! approvalId string! }
 candidate := builtin releaseGetCandidate(candidateId: args.candidateId)
 targets := builtin releaseCandidateTargets(candidateId: args.candidateId)
 if candidate.candidateId != args.candidateId || candidate.approvalId != args.approvalId || targets.candidateId != args.candidateId { return }
 builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId, component: "engine", artifact: "image", targetId: "registry")
 builtin releaseSealPublishedCandidate(candidateId: args.candidateId, approvalId: args.approvalId)
}`
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := catalogConfiguration{FormatVersion: 1, Publisher: "fixture", KeyID: "fixture-key", SigningKeySecret: "CATALOG_KEY", PublicKeys: map[string]string{"fixture-key": base64.StdEncoding.EncodeToString(public)}}
	body, _ := json.Marshal(cfg)
	variables, secrets := f.i.resolver.systemVariable, f.i.resolver.systemSecret
	f.i.resolver.systemVariable = func(ctx context.Context, name string) (string, error) {
		if name == ReleaseCatalogConfigurationVariable {
			return string(body), nil
		}
		return variables(ctx, name)
	}
	signingUnavailable := true
	f.i.resolver.systemSecret = func(ctx context.Context, name string) (string, error) {
		if name == "CATALOG_KEY" {
			if signingUnavailable {
				return "", errors.New("fixture signing temporarily unavailable")
			}
			return base64.StdEncoding.EncodeToString(key.Seed()), nil
		}
		return secrets(ctx, name)
	}
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	r := f.prepare(t)
	buildAction(t, f.i, r.ID, "start")
	f.pipeline.run.Status, f.pipeline.run.Conclusion, f.pipeline.run.WorkRunID = "completed", "success", f.runs["build"]
	buildAction(t, f.i, r.ID, "observe")
	v := buildAction(t, f.i, r.ID, "resume")
	cleanupCandidate(t, f.db, v.CandidateID)
	p, err := f.i.configuredCandidateFor(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := p.approve(ownerCtx(), v.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"intentId": r.ID, "approvalId": approved.ApprovalID}
	rows, err := f.i.executeBuild(ownerCtx(), args, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(rows[0].Payload, &v) != nil || v.Phase != "uncertain" || v.ApprovalID != approved.ApprovalID || v.Catalog.State != "absent" || writes.Load() == 0 {
		t.Fatal("publication did not retain verified upload and uncertain seal", v)
	}
	before := writes.Load()
	signingUnavailable = false
	peer := f.peer(t)
	rows, err = peer.executeBuild(ownerCtx(), args, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(rows[0].Payload, &v) != nil || v.Phase != "published" || v.Catalog.State != "sealed" || v.Catalog.Digest == "" || v.ApprovalID != approved.ApprovalID {
		t.Fatal("captured publication did not reach signed catalog", v)
	}
	if writes.Load() != before {
		t.Fatal("completed native publication was replayed", before, writes.Load())
	}
	if !slices.Equal(v.Actions, []string{"view_candidate"}) {
		t.Fatal("sealed release retained an effect action", v.Actions)
	}
	verified, err := PublishedCatalog(ownerCtx(), f.db, v.CandidateID, "fixture", map[string]ed25519.PublicKey{"fixture-key": public})
	if err != nil {
		t.Fatal(err)
	}
	published, err := verified.Release()
	if err != nil || published.ApprovalID != approved.ApprovalID || published.Components[0].Commit != r.Selection.Request.ExpectedSHA {
		t.Fatal("catalog lost exact approved source", err)
	}
	if _, err := peer.executeBuild(ownerCtx(), args, "publish"); err == nil {
		t.Fatal("sealed release offered another publication")
	}
}

// TestReleaseBuildAdoptsAFailedOnlyRerunOfItsRun drives a saved release through
// a running attempt it keeps, a failed attempt that blocks it, an unrelated run
// it refuses and the re-run of its own attempt it adopts (build_adopt.go). A
// refused observation keeps the recorded run and is saved as unconfirmed, the
// way executeBuild records every failed operation.
func TestReleaseBuildAdoptsAFailedOnlyRerunOfItsRun(t *testing.T) {
	f := newBuildDBFixture(t)
	f.pipeline.source.TagExists = true
	f.pipeline.source.TagSHA = f.pipeline.source.SHA
	r := f.prepare(t)
	if v := buildAction(t, f.i, r.ID, "start"); v.Phase != "building" {
		t.Fatal(v)
	}
	first := *f.pipeline.run
	observe := func(run pl.ReleaseBuildRun) releaseBuildView {
		f.pipeline.run = &run
		return buildAction(t, f.i, r.ID, "observe")
	}
	refused := func(v releaseBuildView, why string) {
		t.Helper()
		if v.PipelineRunID != first.ID || v.ReasonCode != "operation_unconfirmed" {
			t.Fatal(why, v)
		}
	}

	early := first
	early.ID, early.Attempt, early.RerunOf = id.NewShortId(), 2, first.ID
	early.Chain = []pl.ReleaseBuildAttempt{{ID: first.ID, Attempt: 1, Status: "in_progress"}}
	refused(observe(early), "a running recorded attempt was replaced")

	first.Status, first.Conclusion = "completed", "failure"
	if v := observe(first); v.Phase != "blocked" || !strings.Contains(v.Reason, "Re-run its failed steps") {
		t.Fatal("a failed release does not say how to recover:", v)
	}

	unrelated := first
	unrelated.ID, unrelated.Attempt, unrelated.Status, unrelated.Conclusion = id.NewShortId(), 2, "in_progress", ""
	refused(observe(unrelated), "an attempt outside the recorded run's chain was adopted")

	rerun := unrelated
	rerun.RerunOf = first.ID
	rerun.Chain = []pl.ReleaseBuildAttempt{{ID: first.ID, Attempt: 1, Status: "completed", Conclusion: "failure"}}
	if v := observe(rerun); v.Phase != "building" || v.PipelineRunID != rerun.ID || v.ReasonCode != "" {
		t.Fatal("the re-run of the recorded attempt was not adopted:", v)
	}

	// The ledger holds the same line: a replica with an older view cannot
	// write the failed attempt back over the adopted re-run.
	storage, err := f.i.candidateStorage(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := storage.ledger.claimBuild(ownerCtx(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close(ownerCtx())
	// Both answers are the fence's own refusal, not a database error: a stale
	// run with no chain, and one whose chain does not name the adopted re-run.
	stale := first
	unrelatedChain := first
	unrelatedChain.ID = id.NewShortId()
	unrelatedChain.Chain = []pl.ReleaseBuildAttempt{{ID: id.NewShortId(), Attempt: 1, Status: "completed", Conclusion: "failure"}}
	for _, run := range []pl.ReleaseBuildRun{stale, unrelatedChain} {
		lease.record.Run = &run
		if err := lease.save(ownerCtx()); err == nil || !strings.Contains(err.Error(), "release update lost its immutable selection or lease") {
			t.Fatalf("a run that does not descend from the adopted re-run was not refused by the fence: %+v %v", run, err)
		}
	}
}
