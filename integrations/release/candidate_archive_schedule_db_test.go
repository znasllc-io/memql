package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
