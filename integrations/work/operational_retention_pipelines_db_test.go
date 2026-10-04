package work

// operational_retention_pipelines_db_test.go -- a finished pipeline's runs on
// the nightly operational retention (issue #5496, epic memql#5478).
//
// Pipelines retention is not a sweep of its own. It is two more entries on
// operationalPolicies, the closed list workJournalRetentionSweep already runs
// on the cron leader under the cluster's maintenance principal: the
// v1:pipelines:run row once it is completed, and the v1:work:run it compiled
// into once that is finished, together with the steps the run's own owner
// wrote. Both go on MEMQL_PIPELINES_RUN_RETENTION_DAYS (default 30), through
// the same retireVerified path as every other policy: every version archived
// to an object, the object read back and compared, then exactly the archived
// keys deleted in one transaction. No archive means no delete.
//
// Postgres-gated like its neighbours: each test gets a private schema from
// sweepDB, and CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1, so
// a skip there is a failure rather than a green.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// The two people these fixtures belong to, spelled as the write path stores an
// owner: canonical, because ownerUserId is an outgoing @relationship.
const (
	pipelineOwner = "v1:identity:user:pipeline-owner"
	strangerOwner = "v1:identity:user:somebody-else"
)

// seedPipelinesRun writes a v1:pipelines:run the way the seam and the driver
// write one: a version for each status it passed through, the last at `at`.
func seedPipelinesRun(t *testing.T, db *bun.DB, name, owner, status string, at time.Time) string {
	t.Helper()
	id := pipelinesRunConcept + ":" + name
	path := []string{"queued", "in_progress", "completed"}
	end := slices.Index(path, status)
	if end < 0 {
		t.Fatalf("v1:pipelines:run has no status %q", status)
	}
	for k := 0; k <= end; k++ {
		fields := map[string]any{
			"ownerUserId": owner, "accountId": "", "pipelineId": "v1:pipelines:pipeline:" + name,
			"repository": "acme/app", "sha": "3f9c2ab", "mode": "full", "event": "push",
			"runKey": "key-" + name, "attempt": 1, "trigger": "webhook", "status": path[k],
			"queuedAt": rfc(at.Add(time.Duration(-end) * time.Hour)),
		}
		if path[k] == "completed" {
			fields["conclusion"], fields["finishedAt"] = "success", rfc(at)
		}
		insertSweepRow(t, db, id, pipelinesRunConcept, at.Add(time.Duration(k-end)*time.Hour), fields)
	}
	return id
}

// seedPipelineWorkRun writes the v1:work:run a pipelines run compiled into, as
// component/workjournal writes one: running at open, then its final status at
// `at` -- triggered by "pipeline:<mode>" and owned by the pipeline's owner.
func seedPipelineWorkRun(t *testing.T, db *bun.DB, name, owner, status string, at time.Time) string {
	t.Helper()
	return seedWorkRun(t, db, name, status, at, map[string]any{
		"ownerUserId": owner, "goalId": "v1:work:goal:" + name, "automationName": "pipeline:acme-app",
		"triggeredBy": pipelines.WorkTriggerPrefix + "full", "mode": "live",
	})
}

// seedSystemRun writes the unowned scheduled run the system-run policy is for.
func seedSystemRun(t *testing.T, db *bun.DB, name, status string, at time.Time) string {
	t.Helper()
	return seedWorkRun(t, db, name, status, at, map[string]any{
		"ownerUserId": "", "goalId": "", "automationName": "workJournalRetentionSweep", "triggeredBy": "schedule",
	})
}

func seedWorkRun(t *testing.T, db *bun.DB, name, status string, at time.Time, base map[string]any) string {
	t.Helper()
	id := runConcept + ":" + name
	version := func(status string) map[string]any {
		fields := maps.Clone(base)
		fields["status"] = status
		return fields
	}
	if status != runStatusRunning {
		insertSweepRow(t, db, id, runConcept, at.Add(-time.Hour), version(runStatusRunning))
	}
	insertSweepRow(t, db, id, runConcept, at, version(status))
	return id
}

// seedWorkChild writes a step, approval, model call or observation of a run:
// two versions, the second at `at`. An owner is whatever fields says, absent
// included.
func seedWorkChild(t *testing.T, db *bun.DB, concept, name, runID string, at time.Time, fields map[string]any) string {
	t.Helper()
	id := concept + ":" + name
	payload := maps.Clone(fields)
	payload["runId"] = runID
	insertSweepRow(t, db, id, concept, at.Add(-time.Minute), payload)
	insertSweepRow(t, db, id, concept, at, payload)
	return id
}

// ownedBy is a child's payload carrying an owner.
func ownedBy(owner string, extra ...string) map[string]any {
	fields := map[string]any{"ownerUserId": owner}
	for k := 0; k+1 < len(extra); k += 2 {
		fields[extra[k]] = extra[k+1]
	}
	return fields
}

// versionKey names one stored version the way the delete is bound to it.
func versionKey(concept, id string, at time.Time) string {
	return concept + "|" + id + "|" + at.UTC().Format(time.RFC3339Nano)
}

// storedKeys is every stored version of the ids.
func storedKeys(t *testing.T, db *bun.DB, ids ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	rows, err := db.QueryContext(context.Background(), `SELECT concept, id, "createdAt" FROM "MemoryNodes" WHERE id IN (?)`, bun.In(ids))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var concept, id string
		var at time.Time
		if err := rows.Scan(&concept, &id, &at); err != nil {
			t.Fatal(err)
		}
		out[versionKey(concept, id, at)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// archivedKeys reads archive objects back into the versions they hold.
func archivedKeys(t *testing.T, blobs ...[]byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, blob := range blobs {
		r, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("an archive object is not gzip: %v", err)
		}
		dec := json.NewDecoder(r)
		for {
			var row struct {
				ID        string    `json:"id"`
				Concept   string    `json:"concept"`
				CreatedAt time.Time `json:"createdAt"`
			}
			err := dec.Decode(&row)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("an archive object does not decode: %v", err)
			}
			out[versionKey(row.Concept, row.ID, row.CreatedAt)] = true
		}
		_ = r.Close()
	}
	return out
}

func idsOf(keys map[string]bool) []string {
	seen := map[string]bool{}
	for key := range keys {
		parts := strings.SplitN(key, "|", 3)
		seen[parts[1]] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// assertKeptWhole fails unless every version stored BEFORE the sweep is still
// stored: a kept record keeps its whole history, not its newest row.
func assertKeptWhole(t *testing.T, db *bun.DB, before map[string]bool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		want := map[string]bool{}
		for key := range before {
			if strings.SplitN(key, "|", 3)[1] == id {
				want[key] = true
			}
		}
		if len(want) == 0 {
			t.Fatalf("%s was never seeded; the assertion would pass over nothing", id)
		}
		if got := storedKeys(t, db, id); !maps.Equal(got, want) {
			t.Errorf("%s should have been kept whole: stored %d of its %d versions", id, len(got), len(want))
		}
	}
}

// assertRetired fails unless no version of the ids is stored.
func assertRetired(t *testing.T, db *bun.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if got := storedKeys(t, db, id); len(got) != 0 {
			t.Errorf("%s should have been retired: %d versions are still stored", id, len(got))
		}
	}
}

// resultFor is the one result a policy reported, by concept and window.
func resultFor(t *testing.T, results []OperationalRetentionResult, concept, env string) OperationalRetentionResult {
	t.Helper()
	var found []OperationalRetentionResult
	for _, r := range results {
		if r.Concept == concept && r.Env == env {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one result for %s on %s, got %d in %+v", concept, env, len(found), results)
	}
	return found[0]
}

// witnessArchive asks the database, at every upload and at every read-back,
// whether each version the object holds is still stored. A sweep that deleted
// before it archived -- or before it verified -- shows its rows missing at the
// moment they were archived.
type witnessArchive struct {
	sweepDBArchive
	db                *bun.DB
	uploads, verifies int
	missingAtUpload   []string
	missingAtVerify   []string
}

func (a *witnessArchive) Upload(ctx context.Context, container, object string, data []byte, kind string) (string, error) {
	a.uploads++
	a.missingAtUpload = append(a.missingAtUpload, a.missing(data)...)
	return a.sweepDBArchive.Upload(ctx, container, object, data, kind)
}

func (a *witnessArchive) DownloadWithLimit(ctx context.Context, container, object string, limit int64) ([]byte, error) {
	data, err := a.sweepDBArchive.DownloadWithLimit(ctx, container, object, limit)
	a.verifies++
	a.missingAtVerify = append(a.missingAtVerify, a.missing(data)...)
	return data, err
}

func (a *witnessArchive) missing(blob []byte) []string {
	r, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return []string{"<an object that is not gzip>"}
	}
	defer func() { _ = r.Close() }()
	var out []string
	dec := json.NewDecoder(r)
	for {
		var row struct {
			ID        string    `json:"id"`
			Concept   string    `json:"concept"`
			CreatedAt time.Time `json:"createdAt"`
		}
		if err := dec.Decode(&row); err != nil {
			if !errors.Is(err, io.EOF) {
				out = append(out, "<an object that does not decode>")
			}
			return out
		}
		var n int
		if err := a.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND id=? AND "createdAt"=?`, row.Concept, row.ID, row.CreatedAt).Scan(&n); err != nil || n != 1 {
			out = append(out, versionKey(row.Concept, row.ID, row.CreatedAt))
		}
	}
}

func (a *witnessArchive) objectBlobs() [][]byte {
	names := slices.Sorted(maps.Keys(a.objects))
	out := make([][]byte, 0, len(names))
	for _, name := range names {
		out = append(out, a.objects[name])
	}
	return out
}

// TestRetentionArchivesBeforeItDeletes is the rule's headline: a finished
// pipeline's run -- the pipelines run row, the work run it compiled into and
// that run's steps -- is archived, every version of it, the archive read back
// and found intact, and only THEN deleted. What the run leaves that is not the
// run's stays: the goal, and the Library file its step's log was archived to,
// which is the owner's.
func TestRetentionArchivesBeforeItDeletes(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "") // the documented default
	finished := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))

	pipelinesRun := seedPipelinesRun(t, db, "finished", pipelineOwner, "completed", finished)
	workRun := seedPipelineWorkRun(t, db, "finished", pipelineOwner, "succeeded", finished)
	logFile := "v1:library:file:finished-checks-log"
	checks := seedWorkChild(t, db, "v1:work:step", "finished-checks", workRun, finished, ownedBy(pipelineOwner, "status", "done", "logFileId", logFile))
	tests := seedWorkChild(t, db, "v1:work:step", "finished-tests", workRun, finished, ownedBy(pipelineOwner, "status", "skipped"))
	goal := "v1:work:goal:finished"
	insertSweepRow(t, db, goal, goalConcept, finished, map[string]any{"ownerUserId": pipelineOwner, "status": "closed", "requestedVia": "pipeline"})
	insertSweepRow(t, db, logFile, "v1:library:file", finished, map[string]any{"ownerUserId": pipelineOwner, "name": "checks.log"})

	retired := []string{pipelinesRun, workRun, checks, tests}
	before := storedKeys(t, db, retired...)
	kept := storedKeys(t, db, goal, logFile)
	archive := &witnessArchive{db: db}
	i.SetArchiver(archive)

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}

	// ARCHIVED: every version of every row of the run, nothing else.
	if got := archivedKeys(t, archive.objectBlobs()...); !maps.Equal(got, before) {
		t.Fatalf("the archive holds %d versions of %v, want the run's %d versions of %v", len(got), idsOf(got), len(before), idsOf(before))
	}
	// VERIFIED: each object read back once it was written.
	if archive.uploads == 0 || archive.verifies != archive.uploads {
		t.Fatalf("%d objects written and %d read back; every object is read back before anything is deleted", archive.uploads, archive.verifies)
	}
	// ...and every row was still stored at both moments: archived FIRST.
	if len(archive.missingAtUpload) != 0 || len(archive.missingAtVerify) != 0 {
		t.Fatalf("rows were deleted before they were archived: missing at upload %v, at read-back %v", archive.missingAtUpload, archive.missingAtVerify)
	}
	// DELETED SECOND: nothing of the run remains.
	assertRetired(t, db, retired...)
	// What is not the run's stays.
	assertKeptWhole(t, db, kept, goal, logFile)

	deleted := 0
	for _, concept := range []string{runConcept, pipelinesRunConcept} {
		r := resultFor(t, results, concept, EnvPipelinesRunRetentionDays)
		if r.RetentionDays != DefaultPipelinesRunRetentionDays {
			t.Errorf("%s retained %d days, want the default %d", concept, r.RetentionDays, DefaultPipelinesRunRetentionDays)
		}
		if r.ArchivedVersions != r.DeletedVersions {
			t.Errorf("%s archived %d versions and deleted %d", concept, r.ArchivedVersions, r.DeletedVersions)
		}
		deleted += r.DeletedVersions
	}
	if deleted != len(before) {
		t.Errorf("deleted %d versions, want the run's %d", deleted, len(before))
	}
}

// TestRetentionRefusesToDeleteWithoutAnArchive: with nowhere to archive to, an
// archive that will not take the object, or one whose read-back is missing or
// different, a finished pipeline run is kept whole and the sweep says why.
//
// Each case ends on the REACHABLE POSITIVE: the same rows, with the archive
// repaired, are retired. So a kept run was kept because of the archive, never
// because the policy did not match it.
func TestRetentionRefusesToDeleteWithoutAnArchive(t *testing.T) {
	for _, mode := range []string{"no archive container", "no archiver", "upload fails", "read-back fails", "read-back differs"} {
		t.Run(mode, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			now := time.Now().UTC().Truncate(time.Second)
			i.SetNow(func() time.Time { return now })
			t.Setenv(EnvPipelinesRunRetentionDays, "")
			t.Setenv(EnvArchiveContainer, "test-retention")
			finished := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))
			workRun := seedPipelineWorkRun(t, db, "refused", pipelineOwner, "failed", finished)
			ids := []string{
				seedPipelinesRun(t, db, "refused", pipelineOwner, "completed", finished),
				workRun,
				seedWorkChild(t, db, "v1:work:step", "refused-tests", workRun, finished, ownedBy(pipelineOwner, "status", "failed")),
			}
			before := storedKeys(t, db, ids...)

			archive := &verifyingArchive{}
			i.SetArchiver(archive)
			switch mode {
			case "no archive container":
				t.Setenv(EnvArchiveContainer, "")
				t.Setenv(envBlobContainer, "")
			case "no archiver":
				i.SetArchiver(nil)
			case "upload fails":
				archive.fail = true
			case "read-back fails":
				archive.downloadFail = true
			case "read-back differs":
				archive.corrupt = true
			}
			_, err := i.operationalRetention(ctx, false)
			if err == nil || !strings.Contains(err.Error(), "records preserved") {
				t.Fatalf("want a refusal saying the records were preserved, got %v", err)
			}
			if after := storedKeys(t, db, ids...); !maps.Equal(after, before) {
				t.Fatalf("deleted without a verified archive: %d of %d versions left", len(after), len(before))
			}
			if mode != "read-back differs" && mode != "read-back fails" && len(archive.objects) != 0 {
				t.Fatalf("an object was written with %s", mode)
			}

			// The archive repaired, the same rows go.
			t.Setenv(EnvArchiveContainer, "test-retention")
			i.SetArchiver(&verifyingArchive{})
			if _, err := i.operationalRetention(ctx, false); err != nil {
				t.Fatalf("with a working archive: %v", err)
			}
			assertRetired(t, db, ids...)
		})
	}
}

// TestRetentionKeepsRunsInsideTheWindowAndNonTerminalRuns: age is measured on
// a record's LATEST version, and only a finished run is a candidate -- a
// v1:pipelines:run at completed (its conclusion is not the test; a refused run
// is completed too), a pipeline's work run at succeeded, failed or cancelled.
// A finished work run whose step was written inside the window waits for it.
func TestRetentionKeepsRunsInsideTheWindowAndNonTerminalRuns(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	old := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))
	recent := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays - 10))

	// The reachable positives: one of each finished shape, past the window.
	retired := []string{
		seedPipelinesRun(t, db, "done-old", pipelineOwner, "completed", old),
		seedPipelineWorkRun(t, db, "succeeded-old", pipelineOwner, "succeeded", old),
		seedPipelineWorkRun(t, db, "failed-old", pipelineOwner, "failed", old),
		seedPipelineWorkRun(t, db, "cancelled-old", pipelineOwner, "cancelled", old),
	}
	touched := seedPipelinesRun(t, db, "done-old-touched", pipelineOwner, "completed", old)
	// A later write -- the check run's state, say -- moves the run's age.
	insertSweepRow(t, db, touched, pipelinesRunConcept, now.AddDate(0, 0, -2), map[string]any{"ownerUserId": pipelineOwner, "status": "completed", "conclusion": "success", "checkRunState": "written"})
	waitingOnStep := seedPipelineWorkRun(t, db, "succeeded-recent-step", pipelineOwner, "succeeded", old)
	kept := []string{
		seedPipelinesRun(t, db, "done-recent", pipelineOwner, "completed", recent),
		seedPipelinesRun(t, db, "in-progress-old", pipelineOwner, "in_progress", old),
		seedPipelinesRun(t, db, "queued-old", pipelineOwner, "queued", old),
		touched,
		seedPipelineWorkRun(t, db, "succeeded-recent", pipelineOwner, "succeeded", recent),
		seedPipelineWorkRun(t, db, "running-old", pipelineOwner, runStatusRunning, old),
		seedPipelineWorkRun(t, db, "waiting-old", pipelineOwner, runStatusWaiting, old),
		// No writer closes a pipeline's work run abandoned, and nothing judges
		// one (runnerOwnsRecovery), so the status is not one this policy was
		// told about: kept rather than guessed at.
		seedPipelineWorkRun(t, db, "abandoned-old", pipelineOwner, runStatusAbandoned, old),
		waitingOnStep,
		seedWorkChild(t, db, "v1:work:step", "recent-step", waitingOnStep, now.AddDate(0, 0, -2), ownedBy(pipelineOwner, "status", "done")),
	}
	before := storedKeys(t, db, kept...)

	if _, err := i.operationalRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)
}

// TestRetentionSystemRunPolicyStillRefusesOwnedChildren: a system run is
// unowned, so a child that carries an owner -- or carries none at all -- is
// not the run's, and the run waits. Unchanged by the pipelines policy, which
// is the point: the rule "a child goes with its run when it is the run's own"
// reads the same for both, and for an unowned run that is an unowned child.
func TestRetentionSystemRunPolicyStillRefusesOwnedChildren(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	old := now.AddDate(0, 0, -(DefaultSystemRunRetentionDays + 10))

	// The reachable positive: an unowned run with an unowned step goes.
	unowned := seedSystemRun(t, db, "system-unowned-step", runStatusSucceeded, old)
	retired := []string{unowned, seedWorkChild(t, db, "v1:work:step", "system-unowned-step", unowned, old, ownedBy(""))}

	owned := seedSystemRun(t, db, "system-owned-step", runStatusSucceeded, old)
	ownerless := seedSystemRun(t, db, "system-ownerless-step", runStatusSucceeded, old)
	kept := []string{
		owned, seedWorkChild(t, db, "v1:work:step", "system-owned-step", owned, old, ownedBy(pipelineOwner)),
		ownerless, seedWorkChild(t, db, "v1:work:step", "system-ownerless-step", ownerless, old, map[string]any{"status": "done"}),
	}
	before := storedKeys(t, db, kept...)

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)
	// The SQL half of the rule: the kept runs are not even candidates, so a
	// population of them can never fill the candidate LIMIT ahead of a run
	// that could go.
	if r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays); r.Candidates != 1 {
		t.Errorf("the system-run policy read %d candidates, want only the run whose child is unowned", r.Candidates)
	}
}

// TestRetentionKeepsAPipelineWorkRunWithAChildOwnedBySomeoneElse: a pipeline's
// work run takes its steps and closed approvals with it when they are its
// owner's -- the journal writes every one of them under that owner -- and is
// kept whole when any child is somebody else's, carries no owner, is a pending
// approval, or is model-call or observation detail on its own longer window.
func TestRetentionKeepsAPipelineWorkRunWithAChildOwnedBySomeoneElse(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	old := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))

	// The reachable positive: every child the owner's, a decided approval among them.
	own := seedPipelineWorkRun(t, db, "own-children", pipelineOwner, "succeeded", old)
	retired := []string{
		own,
		seedWorkChild(t, db, "v1:work:step", "own-children-a", own, old, ownedBy(pipelineOwner, "status", "done")),
		seedWorkChild(t, db, "v1:work:step", "own-children-b", own, old, ownedBy(pipelineOwner, "status", "done")),
		seedWorkChild(t, db, approvalConcept, "own-children", own, old, ownedBy(pipelineOwner, "decision", "approved")),
	}

	var kept []string
	keep := func(name string, children ...func(run string) string) {
		run := seedPipelineWorkRun(t, db, name, pipelineOwner, "succeeded", old)
		kept = append(kept, run, seedWorkChild(t, db, "v1:work:step", name+"-own", run, old, ownedBy(pipelineOwner, "status", "done")))
		for _, child := range children {
			kept = append(kept, child(run))
		}
	}
	keep("stranger-step", func(run string) string {
		return seedWorkChild(t, db, "v1:work:step", "stranger-step", run, old, ownedBy(strangerOwner, "status", "done"))
	})
	keep("ownerless-step", func(run string) string {
		return seedWorkChild(t, db, "v1:work:step", "ownerless-step", run, old, map[string]any{"status": "done"})
	})
	keep("stranger-approval", func(run string) string {
		return seedWorkChild(t, db, approvalConcept, "stranger-approval", run, old, ownedBy(strangerOwner, "decision", "approved"))
	})
	keep("pending-approval", func(run string) string {
		return seedWorkChild(t, db, approvalConcept, "pending-approval", run, old, ownedBy(pipelineOwner, "decision", ""))
	})
	keep("observation", func(run string) string {
		return seedWorkChild(t, db, observationConcept, "observation", run, old, ownedBy(pipelineOwner))
	})
	keep("model-call", func(run string) string {
		return seedWorkChild(t, db, modelCallConcept, "model-call", run, old, ownedBy(pipelineOwner))
	})
	before := storedKeys(t, db, kept...)

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)
	if r := resultFor(t, results, runConcept, EnvPipelinesRunRetentionDays); r.Candidates != 1 {
		t.Errorf("the pipeline work-run policy read %d candidates, want only the run whose children are all its own", r.Candidates)
	}
}

// TestRetireVerifiedKeepsARunWhoseChildIsNotItsOwn holds the Go half of the
// ownership rule on its own, past the SQL prefilter that normally keeps such a
// run from being a candidate at all: a child can change between the candidate
// read and the archive, and retireVerified is the last word on what goes.
func TestRetireVerifiedKeepsARunWhoseChildIsNotItsOwn(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))
	cutoff := now.AddDate(0, 0, -DefaultPipelinesRunRetentionDays)
	for _, tc := range []struct {
		name       string
		system     bool
		childOwner any // nil: the child carries no owner at all
		retired    bool
	}{
		{"a pipeline run's own step goes with it", false, pipelineOwner, true},
		{"a step somebody else owns keeps a pipeline run", false, strangerOwner, false},
		{"a step with no owner keeps a pipeline run", false, nil, false},
		{"an unowned step goes with a system run", true, "", true},
		{"an owned step keeps a system run", true, pipelineOwner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			t.Setenv(EnvArchiveContainer, "test-retention")
			i.SetArchiver(&verifyingArchive{})
			var run string
			if tc.system {
				run = seedSystemRun(t, db, "direct", runStatusSucceeded, old)
			} else {
				run = seedPipelineWorkRun(t, db, "direct", pipelineOwner, runStatusSucceeded, old)
			}
			fields := map[string]any{"status": "done"}
			if tc.childOwner != nil {
				fields["ownerUserId"] = tc.childOwner
			}
			step := seedWorkChild(t, db, "v1:work:step", "direct", run, old, fields)
			before := storedKeys(t, db, run, step)

			_, deleted, _, err := i.retireVerified(ctx, runConcept, []map[string]any{{"id": run, "createdAt": rfc(old)}}, true, cutoff, false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.retired {
				if deleted != len(before) {
					t.Fatalf("deleted %d versions, want the run and its step's %d", deleted, len(before))
				}
				assertRetired(t, db, run, step)
				return
			}
			if deleted != 0 {
				t.Fatalf("deleted %d versions of a run whose child is not its own", deleted)
			}
			assertKeptWhole(t, db, before, run, step)
		})
	}
}

// TestRetentionPoliciesSharingTheWorkRunConceptUseTheirOwnWindows: two
// policies read v1:work:run -- the system run's and the pipeline's -- and
// never the same run. Each is measured against its OWN window, in both
// directions, so neither a shared window nor the larger or smaller of the two
// can pass for it.
func TestRetentionPoliciesSharingTheWorkRunConceptUseTheirOwnWindows(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		systemDays, pipeDays    int
		retiredSys, retiredPipe []int // ages in days
		keptSys, keptPipe       []int
	}{
		{"pipelines keep longer", 10, 60, []int{20}, []int{70}, []int{5}, []int{20}},
		{"system runs keep longer", 60, 10, []int{70}, []int{20}, []int{20}, []int{5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			now := time.Now().UTC().Truncate(time.Second)
			i.SetNow(func() time.Time { return now })
			t.Setenv(EnvArchiveContainer, "test-retention")
			t.Setenv(EnvSystemRunRetentionDays, strconv.Itoa(tc.systemDays))
			t.Setenv(EnvPipelinesRunRetentionDays, strconv.Itoa(tc.pipeDays))
			i.SetArchiver(&verifyingArchive{})
			var retired, kept []string
			for _, age := range tc.retiredSys {
				retired = append(retired, seedSystemRun(t, db, "system-"+strconv.Itoa(age), runStatusSucceeded, now.AddDate(0, 0, -age)))
			}
			for _, age := range tc.retiredPipe {
				retired = append(retired, seedPipelineWorkRun(t, db, "pipeline-"+strconv.Itoa(age), pipelineOwner, "succeeded", now.AddDate(0, 0, -age)))
			}
			for _, age := range tc.keptSys {
				kept = append(kept, seedSystemRun(t, db, "system-"+strconv.Itoa(age), runStatusSucceeded, now.AddDate(0, 0, -age)))
			}
			for _, age := range tc.keptPipe {
				kept = append(kept, seedPipelineWorkRun(t, db, "pipeline-"+strconv.Itoa(age), pipelineOwner, "succeeded", now.AddDate(0, 0, -age)))
			}
			before := storedKeys(t, db, kept...)

			results, err := i.operationalRetention(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			assertRetired(t, db, retired...)
			assertKeptWhole(t, db, before, kept...)
			if r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays); r.RetentionDays != tc.systemDays || r.DeletedVersions == 0 {
				t.Errorf("the system-run policy reported %+v, want %d days and its run deleted", r, tc.systemDays)
			}
			if r := resultFor(t, results, runConcept, EnvPipelinesRunRetentionDays); r.RetentionDays != tc.pipeDays || r.DeletedVersions == 0 {
				t.Errorf("the pipeline work-run policy reported %+v, want %d days and its run deleted", r, tc.pipeDays)
			}
		})
	}
}

// TestRetentionPipelinesWindowHonorsAStoredGlobalVariable: the new window is
// read like every other operational window -- explicit process configuration
// first, then a stored, active v1:platform:globalVariable of the same name,
// then the default.
func TestRetentionPipelinesWindowHonorsAStoredGlobalVariable(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})

	windows, err := i.operationalRetentionWindows(ctx)
	if err != nil || windows[EnvPipelinesRunRetentionDays] != DefaultPipelinesRunRetentionDays {
		t.Fatalf("with nothing set: %v %v, want the default %d", windows, err, DefaultPipelinesRunRetentionDays)
	}
	insertSweepRow(t, db, "v1:platform:globalVariable:pipelines-retention", "v1:platform:globalVariable", now, map[string]any{"name": EnvPipelinesRunRetentionDays, "value": "45", "active": true})
	windows, err = i.operationalRetentionWindows(ctx)
	if err != nil || windows[EnvPipelinesRunRetentionDays] != 45 {
		t.Fatalf("stored policy lost: %v %v", windows, err)
	}

	// It is the window both pipelines policies run on.
	kept := []string{
		seedPipelinesRun(t, db, "stored-40", pipelineOwner, "completed", now.AddDate(0, 0, -40)),
		seedPipelineWorkRun(t, db, "stored-40", pipelineOwner, "succeeded", now.AddDate(0, 0, -40)),
	}
	retired := []string{
		seedPipelinesRun(t, db, "stored-50", pipelineOwner, "completed", now.AddDate(0, 0, -50)),
		seedPipelineWorkRun(t, db, "stored-50", pipelineOwner, "succeeded", now.AddDate(0, 0, -50)),
	}
	before := storedKeys(t, db, kept...)
	if _, err := i.operationalRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)

	t.Setenv(EnvPipelinesRunRetentionDays, "12")
	windows, err = i.operationalRetentionWindows(ctx)
	if err != nil || windows[EnvPipelinesRunRetentionDays] != 12 {
		t.Fatalf("explicit configuration must override the stored policy: %v %v", windows, err)
	}
}

// TestRetentionSeesPipelinesRunsOnlyUnderTheMaintenancePrincipal: the reads
// pass the PRODUCTION row gate here, not the admit-all the other tests use.
// v1:pipelines:run declares the composite owner tier with the account
// argument, so the sweep sees a person's run only as the cluster's
// maintenance principal -- the one component/auth/maintenance_actor.go
// already names for workJournalRetentionSweep. The negative controls are what
// make the pass mean something: an unenforced tier would admit every caller.
func TestRetentionSeesPipelinesRunsOnlyUnderTheMaintenancePrincipal(t *testing.T) {
	db, i := sweepDB(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	i.admitRow = memqlengine.AdmitSourceRow
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	old := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))

	run := seedPipelinesRun(t, db, "tiered", pipelineOwner, "completed", old)
	// Tied to an account too, so the tier's account branch has something to match.
	insertSweepRow(t, db, run, pipelinesRunConcept, old.Add(time.Second), map[string]any{"ownerUserId": pipelineOwner, "accountId": "v1:accounts:account:acme", "status": "completed", "conclusion": "success"})
	workRun := seedPipelineWorkRun(t, db, "tiered", pipelineOwner, "succeeded", old)
	step := seedWorkChild(t, db, "v1:work:step", "tiered", workRun, old, ownedBy(pipelineOwner, "status", "done"))
	before := storedKeys(t, db, run, workRun, step)

	var payload []byte
	if err := db.QueryRowContext(context.Background(), `SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, run).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	row := memorynodes.MemoryNode{ID: run, Concept: pipelinesRunConcept, Type: memorynodes.NodeTypeObject, CreatedAt: old, Payload: payload}

	// THE NEGATIVE CONTROLS. An automation that is not on the maintenance
	// list runs as a reader: the handler floor refuses it, and the tier would
	// hide the row from it anyway.
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:workJournalRetentionSweep", Role: auth.RoleReader, Unranked: true, Synthetic: true})
	if _, err := i.operationalRetention(reader, false); err == nil {
		t.Fatal("a reader actor ran the sweep")
	}
	if memqlengine.AdmitSourceRow(reader, row) {
		t.Fatal("the tier admitted a reader to a person's pipelines run; the maintenance principal would prove nothing")
	}
	if memqlengine.AdmitSourceRow(actorCtx("somebody-else"), row) {
		t.Fatal("the tier admitted a stranger to a person's pipelines run")
	}
	if after := storedKeys(t, db, run, workRun, step); !maps.Equal(after, before) {
		t.Fatal("a refused sweep deleted rows")
	}

	// THE POSITIVE: the principal the nightly automation runs under.
	ctx := retentionOwner()
	if auth.MaintenanceActor("workJournalRetentionSweep") == nil {
		t.Fatal("workJournalRetentionSweep is not on the maintenance list")
	}
	if !memqlengine.AdmitSourceRow(ctx, row) {
		t.Fatal("the maintenance principal cannot see a person's pipelines run; the sweep would retire nothing, silently")
	}
	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, run, workRun, step)
	if r := resultFor(t, results, pipelinesRunConcept, EnvPipelinesRunRetentionDays); r.Candidates != 1 {
		t.Errorf("the pipelines-run policy saw %d candidates, want 1", r.Candidates)
	}
}

// TestRetentionSplitsABatchLargerThanOneArchiveObject. A batch is counted in
// CANDIDATES and an archive object is bounded in VERSIONS, and a pipeline's
// run is many versions: both its rows take one per 30-second heartbeat, and
// its steps three apiece. A batch that overflows one object is refused whole
// before anything is written, so on its own it would be refused every night,
// holding every run behind it for good. It is halved until each half fits; a
// record that ALONE does not fit is kept whole, named, and passed over.
func TestRetentionSplitsABatchLargerThanOneArchiveObject(t *testing.T) {
	seedWide := func(t *testing.T, db *bun.DB, prefix string, runs, versions int, finished time.Time) {
		t.Helper()
		_, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT ?||r, ?, ?::timestamptz - (? - v) * interval '1 second', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', ?::text, 'status', CASE WHEN v = ? THEN 'completed' ELSE 'in_progress' END)
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, ?) v`,
			pipelinesRunConcept+":"+prefix, pipelinesRunConcept, finished, versions, pipelineOwner, versions, runs, versions)
		if err != nil {
			t.Fatal(err)
		}
	}
	stored := func(t *testing.T, db *bun.DB, prefix string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id LIKE ?`, pipelinesRunConcept+":"+prefix+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	setup := func(t *testing.T) (*bun.DB, *Integration, *verifyingArchive, time.Time) {
		db, i := sweepDB(t)
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvPipelinesRunRetentionDays, "")
		archive := &verifyingArchive{}
		i.SetArchiver(archive)
		return db, i, archive, now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))
	}

	t.Run("a full batch of runs that overflows one object is retired in halves", func(t *testing.T) {
		db, i, archive, finished := setup(t)
		perRun := retirementMaxVersions/retirementBatchSize + 1
		seedWide(t, db, "wide-", retirementBatchSize, perRun, finished)
		if n := stored(t, db, "wide-"); n <= retirementMaxVersions {
			t.Fatalf("the fixture holds %d versions, which one object takes; it would prove nothing", n)
		}

		results, err := i.operationalRetention(retentionOwner(), false)
		if err != nil {
			t.Fatalf("an overflowing batch stopped the sweep: %v", err)
		}
		if n := stored(t, db, "wide-"); n != 0 {
			t.Fatalf("%d versions were left behind", n)
		}
		r := resultFor(t, results, pipelinesRunConcept, EnvPipelinesRunRetentionDays)
		if r.DeletedVersions != retirementBatchSize*perRun || len(r.Objects) < 2 || len(archive.objects) != len(r.Objects) || len(r.Oversized) != 0 {
			t.Fatalf("result %+v with %d objects stored, want %d versions deleted across at least two objects", r, len(archive.objects), retirementBatchSize*perRun)
		}
	})

	t.Run("a record too large for one object is kept whole and the rest go", func(t *testing.T) {
		db, i, _, finished := setup(t)
		seedWide(t, db, "small-", 3, 2, finished)
		seedWide(t, db, "oversized", 1, retirementMaxVersions+1, finished)

		results, err := i.operationalRetention(retentionOwner(), false)
		if err != nil {
			t.Fatalf("one oversized record stopped the sweep: %v", err)
		}
		if n := stored(t, db, "small-"); n != 0 {
			t.Fatalf("%d versions of the ordinary runs were left behind the oversized one", n)
		}
		if n := stored(t, db, "oversized"); n != retirementMaxVersions+1 {
			t.Fatalf("the oversized run kept %d of its %d versions; it has no archive and must be kept whole", n, retirementMaxVersions+1)
		}
		r := resultFor(t, results, pipelinesRunConcept, EnvPipelinesRunRetentionDays)
		if want := []string{pipelinesRunConcept + ":oversized1"}; !slices.Equal(r.Oversized, want) {
			t.Fatalf("oversized = %v, want %v", r.Oversized, want)
		}
	})
}
