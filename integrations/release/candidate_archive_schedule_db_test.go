package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type archiveIdentityEngine struct {
	memql.IntegrationEngineAccess
	t     *testing.T
	role  auth.Role
	calls int
}

func (e *archiveIdentityEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.t.Helper()
	if !strings.HasPrefix(query, "query userByIdSystem(") || auth.OriginFromContext(ctx) != auth.OriginInternal {
		e.t.Fatal("maintenance used an unscoped engine read", query)
	}
	e.calls++
	return memql.NewResultWithOutput(map[string]any{"role": string(e.role)}), nil
}

func archiveScheduleHost(t *testing.T, p *candidatePreparer, db *sql.DB, trust map[string]ed25519.PublicKey, role auth.Role) (*Integration, *archiveIdentityEngine) {
	t.Helper()
	configuration := candidateOperatorConfiguration{FormatVersion: 1}
	for _, source := range p.versionReader.sources {
		configuration.Sources = append(configuration.Sources, source)
	}
	for _, target := range p.targetReader.targets {
		configuration.Registries = append(configuration.Registries, target)
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for key, value := range trust {
		keys[key] = base64.StdEncoding.EncodeToString(value)
	}
	catalog, err := json.Marshal(catalogConfiguration{FormatVersion: 1, Publisher: "fixture", PublicKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	engine := &archiveIdentityEngine{t: t, role: role}
	i := NewIntegration(nil, engine, resolver{systemVariable: func(ctx context.Context, name string) (string, error) {
		if _, err := candidateOwner(ctx); err != nil {
			t.Fatal("configuration read borrowed maintenance authority", err)
		}
		switch name {
		case CandidateConfigurationVariable:
			return string(encoded), nil
		case ReleaseCatalogConfigurationVariable:
			return string(catalog), nil
		default:
			return "", errors.New("unexpected configuration read")
		}
	}})
	if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { return db }, Library: p.library.(CandidateLibrary)}); err != nil {
		t.Fatal(err)
	}
	return i, engine
}

func archiveScheduleCleanup(t *testing.T, db *sql.DB, owner string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM release_input_archive_schedules WHERE owner_user_id=$1`, owner); err != nil {
			t.Error(err)
		}
	})
}

func TestCandidateArchiveScheduleEnforcesEnrollmentPolicyAndResumesOnAnotherHost(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	i, identity := archiveScheduleHost(t, p, db, trust, auth.RoleOwner)
	parent := auth.ContextWithInternalOrigin(auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: "system:maintenance", Role: auth.RoleOwner, Synthetic: true}))
	sweep := func(host *Integration) map[string]any {
		t.Helper()
		nodes, err := host.handleSweepPublishedInputs(parent, nil, 0)
		if err != nil || len(nodes) != 1 {
			t.Fatal("maintenance failed", err)
		}
		var result map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := provider.reads.Load()
	if result := sweep(i); result["inspected"] != float64(0) || identity.calls != 0 || provider.reads.Load() != before {
		t.Fatal("unenrolled owner reached maintenance", result)
	}
	owner, _ := candidateOwner(ownerCtx())
	archiveScheduleCleanup(t, db, owner)
	if _, err := i.handleArchiveSchedule(ownerCtx(), map[string]any{"enabled": true}, 0); err != nil {
		t.Fatal(err)
	}
	if result := sweep(i); result["inspected"] != float64(1) || result["archived"] != false || !library.pins[request.CandidateID] || provider.reads.Load() != before {
		t.Fatal("DSL age policy was bypassed", result)
	}
	// A new host must honor the same 48-hour grace just before it expires.
	// No in-process selection state is shared.
	if _, err := db.Exec(`UPDATE release_catalog SET created_at=clock_timestamp()-interval '47 hours' WHERE candidate_id=$1`, request.CandidateID); err != nil {
		t.Fatal(err)
	}
	other, _ := archiveScheduleHost(t, p, candidateTestDB(t), trust, auth.RoleOwner)
	if result := sweep(other); result["inspected"] != float64(0) {
		t.Fatal("cursor did not wrap", result)
	}
	if result := sweep(other); result["inspected"] != float64(1) || result["archived"] != false || !library.pins[request.CandidateID] || provider.reads.Load() != before {
		t.Fatal("new host bypassed the remaining grace", result)
	}
	if _, err := db.Exec(`UPDATE release_catalog SET created_at=clock_timestamp()-interval '49 hours' WHERE candidate_id=$1`, request.CandidateID); err != nil {
		t.Fatal(err)
	}
	if result := sweep(other); result["inspected"] != float64(0) {
		t.Fatal("cursor did not wrap after grace", result)
	}
	if result := sweep(other); result["archived"] != true || library.pins[request.CandidateID] {
		t.Fatal("new host did not finish the authorized archive", result)
	}
	var released bool
	if err := db.QueryRow(`SELECT inputs_released_at IS NOT NULL FROM release_candidate_archives WHERE candidate_id=$1`, request.CandidateID).Scan(&released); err != nil || !released {
		t.Fatal("completed input release did not leave durable evidence", err)
	}
	provider.unusable.Store(true)
	before = provider.reads.Load()
	for range 2 {
		if result := sweep(i); result["inspected"] != float64(0) || provider.reads.Load() != before {
			t.Fatal("completed archive re-entered scheduled verification", result)
		}
	}
}

func TestCandidateArchiveScheduleRefusesRevokedOrExpiredOwnerAuthority(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	owner, _ := candidateOwner(ownerCtx())
	archiveScheduleCleanup(t, db, owner)
	if err := p.ledger.setArchiveSchedule(ownerCtx(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE release_catalog SET created_at=clock_timestamp()-interval '15 days' WHERE candidate_id=$1`, request.CandidateID); err != nil {
		t.Fatal(err)
	}
	before := provider.reads.Load()
	for _, current := range []auth.Role{auth.RoleReader, auth.RoleAdmin} {
		i, identity := archiveScheduleHost(t, p, candidateTestDB(t), trust, current)
		if _, err := i.handleSweepPublishedInputs(auth.ContextWithInternalOrigin(t.Context()), nil, 0); err == nil || identity.calls != 1 || provider.reads.Load() != before || !library.pins[request.CandidateID] {
			t.Fatal("maintenance lent a former owner its own authority", current, err)
		}
	}
	expired := map[string]any{"roleCeiling": "owner", "credentialClass": auth.ForwardedClassBadge, "expiresAt": time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}
	body, _ := json.Marshal(expired)
	if _, err := db.Exec(`UPDATE release_input_archive_schedules SET authority=$2::jsonb WHERE owner_user_id=$1`, owner, string(body)); err != nil {
		t.Fatal(err)
	}
	i, identity := archiveScheduleHost(t, p, candidateTestDB(t), trust, auth.RoleOwner)
	if _, err := i.handleSweepPublishedInputs(auth.ContextWithInternalOrigin(t.Context()), nil, 0); err == nil || identity.calls != 0 || provider.reads.Load() != before || !library.pins[request.CandidateID] {
		t.Fatal("expired captured badge reached cleanup", err)
	}
	if err := p.ledger.setArchiveSchedule(ownerCtx(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleSweepPublishedInputs(auth.ContextWithInternalOrigin(t.Context()), nil, 0); err != nil || identity.calls != 0 {
		t.Fatal("paused schedule still resolved the owner", err)
	}
}

func TestCandidateArchiveScheduleLeaseExcludesOtherReplicasAndRecoversLoss(t *testing.T) {
	db := candidateTestDB(t)
	ledger := &candidateLedger{db: func() *sql.DB { return db }}
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: "v1:identity:user:" + owner, Role: auth.RoleOwner})
	archiveScheduleCleanup(t, db, owner)
	if err := ledger.setArchiveSchedule(ctx, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan *candidateArchiveScan, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scan, err := claimCandidateArchiveScan(t.Context(), candidateTestDB(t))
			if err != nil {
				t.Error(err)
			}
			claims <- scan
		}()
	}
	wg.Wait()
	close(claims)
	var claimed []*candidateArchiveScan
	for scan := range claims {
		if scan != nil {
			claimed = append(claimed, scan)
		}
	}
	if len(claimed) != 1 || claimed[0].owner != owner {
		t.Fatal("replicas shared an active lease", claimed)
	}
	if _, err := db.Exec(`UPDATE release_input_archive_schedules SET lease_until='epoch' WHERE owner_user_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	recovered, err := claimCandidateArchiveScan(t.Context(), candidateTestDB(t))
	if err != nil || recovered == nil || recovered.lease == claimed[0].lease {
		t.Fatal("another host could not recover an expired lease", err)
	}
	if err := finishCandidateArchiveScan(t.Context(), db, claimed[0]); err == nil {
		t.Fatal("old host overwrote the recovering host's cursor")
	}
	if err := finishCandidateArchiveScan(t.Context(), db, recovered); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateArchiveScheduleOwnerAndInternalWalls(t *testing.T) {
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{})
	i.candidateDB = func() *sql.DB { t.Fatal("unauthorized scheduler reached storage"); return nil }
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleAdmin), auth.ContextWithUserActor(t.Context(), "someone")} {
		if _, err := i.handleArchiveSchedule(ctx, map[string]any{"enabled": true}, 0); err == nil {
			t.Fatal("non-owner enrolled scheduled cleanup")
		}
	}
	if _, err := i.handleSweepPublishedInputs(ownerCtx(), nil, 0); err == nil {
		t.Fatal("wire caller entered internal maintenance")
	}
	for _, capability := range i.Capabilities() {
		if capability.Name == "releaseRetentionArchiveInputs" {
			if _, err := capability.Handler(ownerCtx(), nil, 0); err == nil {
				t.Fatal("wire caller fabricated a leased candidate scope")
			}
		}
	}
}

// sweepArchives runs one pass the way the agent's schedule does: internal origin, no user.
func sweepArchives(t *testing.T, i *Integration) map[string]any {
	t.Helper()
	parent := auth.ContextWithInternalOrigin(auth.ContextWithAccess(t.Context(), &auth.AccessContext{UserId: "system:maintenance", Role: auth.RoleOwner, Synthetic: true}))
	nodes, err := i.handleSweepPublishedInputs(parent, nil, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatal("maintenance failed", err)
	}
	var result map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func enrollArchiveSchedule(t *testing.T, i *Integration, db *sql.DB) {
	t.Helper()
	owner, _ := candidateOwner(ownerCtx())
	archiveScheduleCleanup(t, db, owner)
	if _, err := i.handleArchiveSchedule(ownerCtx(), map[string]any{"enabled": true}, 0); err != nil {
		t.Fatal(err)
	}
}

// ageCatalog moves a sealing back, for the retention policy's 48 hours.
func ageCatalog(t *testing.T, db *sql.DB, candidate, age string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE release_catalog SET created_at=clock_timestamp()-$2::interval WHERE candidate_id=$1`, candidate, age); err != nil {
		t.Fatal(err)
	}
}

func assertInputsReleased(t *testing.T, db *sql.DB, candidate string) {
	t.Helper()
	var state string
	var released bool
	if err := db.QueryRow(`SELECT c.state,a.inputs_released_at IS NOT NULL FROM release_candidates c JOIN release_candidate_archives a ON a.candidate_id=c.candidate_id WHERE c.candidate_id=$1`, candidate).Scan(&state, &released); err != nil {
		t.Fatal(err)
	}
	if state != "archived" || !released {
		t.Fatal("the candidate left no evidence of its archive", state, released)
	}
}

// moveEngine is what the deployment a release exists for does to its own binding: the next
// replica runs another commit, and here another resolver as well.
func moveEngine(t *testing.T, f *buildDBFixture) *Integration {
	t.Helper()
	f.i.profileRevision = func() string { return strings.Repeat("e", 40) }
	*f.source += "\n// The resolver the next engine installs.\n"
	return f.peer(t)
}

// The deployment a release exists for changes the engine revision its binding recorded, so
// on production every bound candidate was refused from its own rollout on, from 2026-10-27,
// and none was ever archived. A new effect stays bound to the engine that prepared it, which
// is what the refusal is for. Archival adds no effect, and it must also not fail a young
// candidate that the policy would only have kept.
func TestCandidateArchiveScheduleReleasesABoundCandidateAfterItsOwnRollout(t *testing.T) {
	m := newManyFileRelease(t, 3)
	if v, err := m.publish(ownerCtx(), m.i); err != nil || v.Catalog.State != "sealed" {
		t.Fatal("the release did not reach its catalog", v.Phase, err)
	}
	enrollArchiveSchedule(t, m.i, m.db)
	peer := moveEngine(t, m.buildDBFixture)
	if _, err := peer.configuredCandidateFor(ownerCtx(), m.candidate); err == nil || !strings.Contains(err.Error(), "installed resolver changed") {
		t.Fatal("a new effect outlived the engine that prepared it", err)
	}
	if result := sweepArchives(t, peer); result["inspected"] != float64(1) || result["archived"] != false || !m.library.pins[m.candidate] {
		t.Fatal("a young candidate did not wait out its 48 hours", result)
	}
	if result := sweepArchives(t, peer); result["inspected"] != float64(0) {
		t.Fatal("cursor did not wrap", result)
	}
	ageCatalog(t, m.db, m.candidate, "49 hours")
	draftPosts, promotes, uploads, _, removed := m.github.counts()
	if result := sweepArchives(t, peer); result["inspected"] != float64(1) || result["archived"] != true || m.library.pins[m.candidate] {
		t.Fatal("the candidate's own rollout kept its inputs", result)
	}
	assertInputsReleased(t, m.db, m.candidate)
	afterPosts, afterPromotes, afterUploads, _, afterRemoved := m.github.counts()
	if afterPosts != draftPosts || afterPromotes != promotes || afterUploads != uploads || afterRemoved != removed {
		t.Fatal("archival wrote to the published release")
	}
	if v := m.read(t); v.Phase != "published" || v.Catalog.State != "sealed" || !slices.Equal(v.Actions, []string{"view_candidate"}) {
		t.Fatal("the saved release stopped reading as published once archived", v.Phase, v.Catalog, v.Actions)
	}
}

// Production's bound engine candidates publish images as well as files, and an image's
// destination carries the version tag its binding selected.
func TestCandidateArchiveReleasesABoundImageAfterItsOwnRollout(t *testing.T) {
	f := newBuildDBFixture(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	f.config.Profiles[0].Registries[0].Origin = server.URL
	f.config.Profiles[0].Registries[0].Repository = "candidate/" + f.runID
	f.config.Profiles[0].Registries[0].AllowLoopbackHTTP = true
	f.publication = `@template
automation fixtureBuildPublication {
 args { candidateId string! approvalId string! }
 builtin releasePublishCandidate(candidateId: args.candidateId, approvalId: args.approvalId, component: "engine", artifact: "image", targetId: "registry")
 builtin releaseSealPublishedCandidate(candidateId: args.candidateId, approvalId: args.approvalId)
}`
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
	rows, err := f.i.executeBuild(ownerCtx(), map[string]any{"intentId": r.ID, "approvalId": approved.ApprovalID}, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(rows[0].Payload, &v) != nil || v.Catalog.State != "sealed" {
		t.Fatal("the release did not reach its catalog", v.Phase)
	}
	peer := moveEngine(t, f)
	// The owner's own call takes the same path as the schedule.
	if _, err := peer.handleCandidateArchive(ownerCtx(), map[string]any{"candidateId": v.CandidateID}, 0); err != nil {
		t.Fatal("the candidate's own rollout refused its archive", err)
	}
	assertInputsReleased(t, f.db, v.CandidateID)
	if _, err := PublishedCatalog(ownerCtx(), f.db, v.CandidateID, "fixture", map[string]ed25519.PublicKey{"fixture-key": public}); err != nil {
		t.Fatal("archival broke the published catalog", err)
	}
}

// Past the engine's identity, archival still reads only what the candidate was approved to
// publish to: a profile that is gone or a destination that moved leaves its inputs in place.
func TestCandidateArchiveOfABoundCandidateStillRequiresItsDestination(t *testing.T) {
	m := newManyFileRelease(t, 2)
	if v, err := m.publish(ownerCtx(), m.i); err != nil || v.Catalog.State != "sealed" {
		t.Fatal("the release did not reach its catalog", v.Phase, err)
	}
	peer := moveEngine(t, m.buildDBFixture)
	refuse := func(reason string) {
		t.Helper()
		before := m.github.requests.Load()
		_, err := peer.handleCandidateArchive(ownerCtx(), map[string]any{"candidateId": m.candidate}, 0)
		if err == nil || !strings.Contains(err.Error(), reason) || !m.library.pins[m.candidate] || m.github.requests.Load() != before {
			t.Fatal("archival went past its destination", reason, err)
		}
		var state string
		if err := m.db.QueryRow(`SELECT state FROM release_candidates WHERE candidate_id=$1`, m.candidate).Scan(&state); err != nil || state != "approved" {
			t.Fatal("a refused archive changed the candidate", state, err)
		}
	}
	profile := &m.config.Profiles[0]
	secret := profile.ReleaseAssets[1].CredentialSecret
	profile.ReleaseAssets[1].CredentialSecret = "ANOTHER_TOKEN"
	refuse("changed since candidate review")
	profile.ReleaseAssets[1].CredentialSecret = secret
	profile.ReleaseAssets = profile.ReleaseAssets[:1]
	refuse("outside its fixed destination authority")
	m.config.Profiles = nil
	refuse("no longer authorized")
}
