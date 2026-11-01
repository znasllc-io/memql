package release

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/work"
)

// candidate_carried_retention_db_test.go -- a candidate stays provable through the
// retention it will meet. Coverage reads every run the candidate names and every attempt
// of its release on every read, however long after, and only a run that produced one of
// its artifacts has one to keep its rows. The proof (candidate_coverage.go,
// candidate_carried.go) and the hold (retentionHeldBackSQL, integrations/work) are one
// promise kept in two packages, so these cases run the real nightly pass and then the
// real proof over what it left.

// retentionScratch is a database of the test's own, taken through one real node start. It
// needs CREATEDB, which the db-tests lane's service user has. The pass retires everything
// past its windows, so it cannot run on the package's shared database, where other cases'
// rows live and a long-lived one would use up its budget before reaching these. A private
// schema is not enough either: the engine's reads depend on triggers LIKE does not copy.
func retentionScratch(t *testing.T) *sql.DB {
	t.Helper()
	adminDSN := dbtest.DSN()
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN)))
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ping); err != nil {
		_ = admin.Close()
		dbtest.Unreachable(t, "a carried candidate through a retention pass", adminDSN, err)
		return nil
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "memql_carried_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name+` TEMPLATE template0`); err != nil {
		_ = admin.Close()
		t.Fatalf("create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		// Housekeeping, not an assertion: a drop that does not land is named so it
		// can be done by hand.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("could not drop the scratch database %s (drop it by hand): %v", name, err)
		}
		_ = admin.Close()
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.SafeDSN(adminDSN), err)
	}
	u.Path = "/" + name
	var base database.Database
	boot, err := database.NewTimescaleDBDatabase(common.ComponentName("carried-retention-test"), database.WithDSN(u.String()), base.WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	boot.Start(context.Background())
	defer boot.Stop(context.Background())
	select {
	case <-boot.Ready():
	case <-time.After(3 * time.Minute):
		t.Fatal("the scratch database did not become ready within three minutes")
	}
	if err := boot.MigrationError(); err != nil {
		t.Fatalf("boot: %v", err)
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(u.String())))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// memoryArchive is object storage that holds what it was given and reads it back.
type memoryArchive struct{ objects map[string][]byte }

func (a *memoryArchive) Upload(_ context.Context, _, object string, data []byte, _ string) (string, error) {
	a.objects[object] = slices.Clone(data)
	return object, nil
}

func (a *memoryArchive) DownloadWithLimit(_ context.Context, _, object string, _ int64) ([]byte, error) {
	return a.objects[object], nil
}

// carriedRetention is the carried fixture on a database of its own, with the work plug-in
// a node installs, so the pass has the production row gate.
type carriedRetention struct {
	*carriedDB
	db        *sql.DB
	ledger    *candidateLedger
	retention *work.Integration
}

func newCarriedRetention(t *testing.T) *carriedRetention {
	t.Helper()
	db := retentionScratch(t)
	// newCarriedDB's fixture over this database. It cannot share that fixture's engine,
	// whose database is the package's own.
	engine := assemblyEngine(t, db)
	owner, err := candidateOwner(ownerCtx())
	if err != nil {
		t.Fatal(err)
	}
	f := &carriedDB{t: t, engine: engine, reader: candidateEvidenceReader{engine: evidenceFreshRead{t, engine}}, owner: owner,
		pipeline: id.NewShortId(), runKey: id.NewShortId()}
	f.journal = workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) { return engine.Execute(ctx, q) }), slog.New(slog.DiscardHandler), "carried-producer")
	profileMutation(t, engine, "createPipeline", map[string]any{"pipelineId": f.pipeline, "packageId": id.NewShortId(), "name": "Carried fixture", "repository": carriedRepo, "defaultBranch": "main", "installationId": "1", "credentialId": id.NewShortId(), "delivery": "webhook"})
	r := &carriedRetention{carriedDB: f, db: db, ledger: &candidateLedger{db: func() *sql.DB { return db }}}
	bunDB := bun.NewDB(db, pgdialect.New())
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name != "work" {
			continue
		}
		provider, err := plugin.Factory(memql.PluginContext{Engine: f.engine, Logger: f.engine.Logger, BunDB: func() *bun.DB { return bunDB }, GateDB: func() *sql.DB { return db }, AdmitSourceRow: memql.AdmitSourceRow})
		if err != nil {
			t.Fatal(err)
		}
		r.retention = provider.(*work.Integration)
	}
	if r.retention == nil {
		t.Fatal("the work plug-in is not registered")
	}
	// Named after the factory, which would otherwise reach for real storage.
	t.Setenv(work.EnvArchiveContainer, "test-retention")
	t.Setenv(work.EnvPipelinesRunRetentionDays, "30")
	r.retention.SetArchiver(&memoryArchive{objects: map[string][]byte{}})
	return r
}

// save records a candidate the way preparation does.
func (r *carriedRetention) save(c pl.ReleaseCandidate) string {
	r.t.Helper()
	prepared, err := r.ledger.prepare(ownerCtx(), c)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.ledger.ready(ownerCtx(), prepared.ID); err != nil {
		r.t.Fatal(err)
	}
	return prepared.ID
}

// pinArtifactOf writes the retained artifact that keeps the attempt a candidate names,
// the way a real release pins its own run. Only that attempt has one.
func (r *carriedRetention) pinArtifactOf(workRunID string) {
	r.t.Helper()
	if _, err := r.db.Exec(`INSERT INTO pipeline_artifact_uploads (intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES ($1,$2,jsonb_build_object('WorkRunID',$3::text),$4,$5,11,1,'ready')`, carriedImage, r.owner, workRunID, "file-"+workRunID, "library/"+r.owner+"/"+workRunID); err != nil {
		r.t.Fatal(err)
	}
}

// night runs one real retention pass as the maintenance principal, as it would run on
// the day days after now.
func (r *carriedRetention) night(now time.Time, days int) {
	r.t.Helper()
	r.retention.SetNow(func() time.Time { return now.AddDate(0, 0, days) })
	if _, err := r.retention.RetentionSweep(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("workJournalRetentionSweep")), false); err != nil {
		r.t.Fatal(err)
	}
}

// stored says whether a pipeline run's row is still there.
func (r *carriedRetention) stored(runID string) bool {
	r.t.Helper()
	rows, err := r.reader.rows(ownerCtx(), "pipelineRunForOwner", map[string]any{"runId": runID}, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(rows) == 1
}

// The chain a proof walks can begin with an attempt refused before it opened any work:
// carriedChain reads that row to reach attempt 1, though it has no journal and names no
// goal. Forty days on, the proof holds while the candidate is live. Once the candidate is
// retired the attempts leave and the same proof refuses, naming what it could not read,
// which is what says the proof was not loosened to stand without them, and that the pass
// is able to retire these rows at all.
func TestACandidateFromAFailedOnlyRerunStaysProvablePastRetention(t *testing.T) {
	r := newCarriedRetention(t)
	now := time.Now()
	refused, _ := r.run(carriedRunSpec{attempt: 1, queuedAt: now.Add(-2 * time.Hour), refused: true})
	second, secondWork := r.attempt(2, refused, now.Add(-time.Hour), false, map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": executed})
	third, thirdWork := r.attempt(3, second, now, true, map[string]carriedStep{"build.image": image, "test.full": carried})
	candidate := r.candidateRecording(thirdWork, nil)
	key := r.save(candidate)
	r.pinArtifactOf(thirdWork)
	if _, err := r.reader.coverage(ownerCtx(), candidate); err != nil {
		t.Fatalf("before any retention: %v", err)
	}

	r.night(now, 40)
	got, err := r.reader.coverage(ownerCtx(), candidate)
	if err != nil {
		t.Fatalf("a live candidate stopped being provable once its release's earlier attempts passed retention: %v", err)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].CarriedFrom == nil || got.Skipped[0].CarriedFrom.WorkRunID != secondWork {
		t.Fatalf("the carried pass is not proven against the attempt that executed it: %+v", got.Skipped)
	}

	if _, err := r.ledger.retire(ownerCtx(), key); err != nil {
		t.Fatal(err)
	}
	r.night(now, 40)
	for name, run := range map[string]string{"attempt 1": refused, "attempt 2": second} {
		if r.stored(run) {
			t.Errorf("%s outlived the candidate that held it", name)
		}
	}
	if !r.stored(third) {
		t.Error("the named attempt left while its artifact is pinned")
	}
	if _, err := r.reader.coverage(ownerCtx(), candidate); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("the proof stood without the attempts it walks: %v", err)
	}
}

// A candidate can name a run that is not a release and pins nothing: a component's runs
// may cover different check suites as long as they share its repository and commit.
// Verification and coverage read that run and its steps on every read, and only the
// candidate can keep them. Forty days on, the proof holds while the candidate is live, and
// a push of the same commit that no candidate names has gone. Once the candidate is retired
// the check suite goes too and the same proof refuses, which says the proof was not
// loosened to stand without the run.
func TestACheckSuiteACandidateNamesStaysProvablePastRetention(t *testing.T) {
	r := newCarriedRetention(t)
	now := time.Now()
	_, release := r.run(carriedRunSpec{queuedAt: now, succeeded: true, steps: map[string]carriedStep{"build.image": image, "test.full": executed}})
	suiteRun, suite := r.run(carriedRunSpec{event: "push", runKey: id.NewShortId(), queuedAt: now, succeeded: true, steps: map[string]carriedStep{"test.suite": executed}})
	unnamed, _ := r.run(carriedRunSpec{event: "push", runKey: id.NewShortId(), queuedAt: now, succeeded: true, steps: map[string]carriedStep{"test.suite": executed}})
	candidate := r.candidateRecording(release, nil)
	receipt, err := r.reader.readReceipt(ownerCtx(), suite, "test.suite", true)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Evidence = append(candidate.Evidence, pl.ReleaseEvidence{Name: "test.suite", Component: "engine", WorkRunID: suite, StepKey: "test.suite", Attempt: receipt.Attempt,
		ReceiptID: receipt.ReceiptID, ReceiptDigest: receipt.ReceiptDigest, DefinitionDigest: receipt.DefinitionDigest, ArtifactIntentIDs: receipt.ArtifactIntentIDs})
	key := r.save(candidate)
	r.pinArtifactOf(release)
	// What an overlay asks of a candidate, in the order it asks (nativeCandidate).
	proven := func(moment string) {
		t.Helper()
		if err := r.reader.verify(ownerCtx(), candidate); err != nil {
			t.Fatalf("%s: verification: %v", moment, err)
		}
		if _, err := r.reader.coverage(ownerCtx(), candidate); err != nil {
			t.Fatalf("%s: coverage: %v", moment, err)
		}
	}
	proven("before any retention")

	r.night(now, 40)
	proven("a live candidate stopped being provable once a check suite it names passed retention")
	if r.stored(unnamed) {
		t.Error("the candidate held a push of its commit that it does not name")
	}

	if _, err := r.ledger.retire(ownerCtx(), key); err != nil {
		t.Fatal(err)
	}
	r.night(now, 40)
	if r.stored(suiteRun) {
		t.Error("the check suite outlived the candidate that held it")
	}
	if _, err := r.reader.coverage(ownerCtx(), candidate); err == nil || !strings.Contains(err.Error(), "release run is unavailable") {
		t.Fatalf("the proof stood without the run it names: %v", err)
	}
}

// The two features together: attempt 1 reused a step from the pull request that tested
// the tree, and attempt 2 carried that reuse. The candidate records only the source, and
// the proof walks through attempt 1 to reach it, so holding the source alone is not
// enough. A source left behind by a retired candidate goes with the attempts.
func TestACarriedReuseStaysProvablePastRetention(t *testing.T) {
	r := newCarriedRetention(t)
	now := time.Now()
	firstStart := now.Add(-time.Hour)
	source, sourceWork := r.pullRequest(now, func(s *carriedRunSpec) { s.finishedAt = firstStart.Add(-time.Hour) })
	first, _ := r.run(carriedRunSpec{attempt: 1, queuedAt: firstStart, startedAt: firstStart, tree: reusedTree,
		steps: map[string]carriedStep{"build.image": {definition: carriedDefinition, failed: true}, "test.full": reusedOf(source)}})
	_, secondWork := r.run(carriedRunSpec{attempt: 2, rerunOf: first, queuedAt: now, startedAt: now, succeeded: true, tree: reusedTree,
		steps: map[string]carriedStep{"build.image": image, "test.full": {definition: carriedDefinition, packages: reusedPackages, carried: true, treeFingerprint: reusedFingerprint}}})
	want := r.provenance(t, source, sourceWork, "pull_request", reusedPullCommit, 1)
	candidate := r.candidateRecording(secondWork, map[string]*pl.ReleaseReusedFrom{"test.full": want})
	key := r.save(candidate)
	r.pinArtifactOf(secondWork)

	r.night(now, 40)
	got, err := r.reader.coverage(ownerCtx(), candidate)
	if err != nil {
		t.Fatalf("a live candidate stopped being provable once the attempt that reused the step passed retention: %v", err)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].ReusedFrom == nil || *got.Skipped[0].ReusedFrom != *want {
		t.Fatalf("the carried reuse is not proven against its source: %+v", got.Skipped)
	}

	if _, err := r.ledger.retire(ownerCtx(), key); err != nil {
		t.Fatal(err)
	}
	r.night(now, 40)
	for name, run := range map[string]string{"the attempt that reused the step": first, "the source": source} {
		if r.stored(run) {
			t.Errorf("%s outlived the candidate that held it", name)
		}
	}
}
