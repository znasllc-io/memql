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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
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
	"github.com/znasllc-io/memql/component/workjournal"
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

// pipelineGoal is the payload of the v1:work:goal the pipelines driver opens
// for a run key: component/workjournal's Begin writes origin "system" and the
// driver's RequestedVia, "pipeline".
func pipelineGoal(owner string) map[string]any {
	return map[string]any{"ownerUserId": owner, "statement": "Run acme-app on 3f9c2ab (push)", "origin": "system", "requestedVia": "pipeline"}
}

// seedGoal writes a v1:work:goal as the journal does over one run: open at
// Begin, active once the run is opened, closed with the run -- the last at
// `at`. Its id is canonical, as the write path stores a row id, and
// seedPipelineWorkRun(name) names it.
func seedGoal(t *testing.T, db *bun.DB, name string, at time.Time, base map[string]any) string {
	t.Helper()
	id := goalConcept + ":" + name
	for k, status := range []string{"open", "active", "closed"} {
		fields := maps.Clone(base)
		fields["status"] = status
		insertSweepRow(t, db, id, goalConcept, at.Add(time.Duration(k-2)*time.Minute), fields)
	}
	return id
}

// assertStillStored fails unless every version stored BEFORE is still stored;
// a version written since is allowed.
func assertStillStored(t *testing.T, db *bun.DB, before map[string]bool) {
	t.Helper()
	if len(before) == 0 {
		t.Fatal("nothing was seeded; the assertion would pass over nothing")
	}
	now := storedKeys(t, db, idsOf(before)...)
	for key := range before {
		if !now[key] {
			t.Errorf("version %s was deleted", key)
		}
	}
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
// pipeline's run -- the pipelines run row, the work run it compiled into, that
// run's steps, and (ruling R28) the goal once no run of it remains -- is
// archived, every version of it, the archive read back and found intact, and
// only THEN deleted, all in one sweep. What the run leaves that is not the
// run's stays: the Library file its step's log was archived to, which is the
// owner's.
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
	goal := seedGoal(t, db, "finished", finished, pipelineGoal(pipelineOwner))
	insertSweepRow(t, db, logFile, "v1:library:file", finished, map[string]any{"ownerUserId": pipelineOwner, "name": "checks.log"})

	retired := []string{pipelinesRun, workRun, checks, tests, goal}
	before := storedKeys(t, db, retired...)
	kept := storedKeys(t, db, logFile)
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
	assertKeptWhole(t, db, kept, logFile)

	deleted := 0
	for _, concept := range []string{runConcept, pipelinesRunConcept, goalConcept} {
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
// Each population is seeded ALONE, so each of the three pipelines policies is
// refused by its own archive step: the work run with its steps, the run row on
// its own, and the goal on its own (no run of it left). The failing policy is
// the last one reported. And each case ends on the REACHABLE POSITIVE: the same
// rows, with the archive repaired, are retired -- so a kept row was kept
// because of the archive, never because the policy did not match it.
func TestRetentionRefusesToDeleteWithoutAnArchive(t *testing.T) {
	populations := []struct {
		name, concept string
		seed          func(t *testing.T, db *bun.DB, at time.Time) []string
	}{
		{"work run and steps", runConcept, func(t *testing.T, db *bun.DB, at time.Time) []string {
			workRun := seedPipelineWorkRun(t, db, "refused", pipelineOwner, "failed", at)
			return []string{workRun, seedWorkChild(t, db, "v1:work:step", "refused-tests", workRun, at, ownedBy(pipelineOwner, "status", "failed"))}
		}},
		{"run row alone", pipelinesRunConcept, func(t *testing.T, db *bun.DB, at time.Time) []string {
			return []string{seedPipelinesRun(t, db, "refused", pipelineOwner, "completed", at)}
		}},
		{"goal alone", goalConcept, func(t *testing.T, db *bun.DB, at time.Time) []string {
			return []string{seedGoal(t, db, "refused", at, pipelineGoal(pipelineOwner))}
		}},
	}
	for _, population := range populations {
		for _, mode := range []string{"no archive container", "no archiver", "upload fails", "read-back fails", "read-back differs"} {
			t.Run(population.name+"/"+mode, func(t *testing.T) {
				db, i := sweepDB(t)
				ctx := retentionOwner()
				now := time.Now().UTC().Truncate(time.Second)
				i.SetNow(func() time.Time { return now })
				t.Setenv(EnvPipelinesRunRetentionDays, "")
				t.Setenv(EnvArchiveContainer, "test-retention")
				ids := population.seed(t, db, now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays+10)))
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
				results, err := i.operationalRetention(ctx, false)
				if err == nil || !strings.Contains(err.Error(), "records preserved") {
					t.Fatalf("want a refusal saying the records were preserved, got %v", err)
				}
				// Every policy runs and reports its row whatever an earlier one did,
				// so the refusal is told by the error, which names its policy.
				own := "retention policy " + population.concept + " on " + EnvPipelinesRunRetentionDays + ":"
				if !strings.Contains(err.Error(), own) || strings.Count(err.Error(), "retention policy ") != 1 {
					t.Fatalf("refused with %q, want %s's own policy alone", err, population.concept)
				}
				if len(results) != len(operationalPolicies) {
					t.Fatalf("%d result rows, want one for each of the %d policies", len(results), len(operationalPolicies))
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

func TestPipelineArtifactCleanupKeepsItsAuthorizingJournals(t *testing.T) {
	db, i := sweepDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	work := seedPipelineWorkRun(t, db, "artifact-authority", pipelineOwner, runStatusSucceeded, old)
	run := seedPipelinesRun(t, db, "artifact-authority", pipelineOwner, "completed", old)
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('workRunId',?::text) WHERE id=?`, work, run); err != nil {
		t.Fatal(err)
	}
	before := storedKeys(t, db, work, run)
	if _, err := db.Exec(`INSERT INTO pipeline_artifact_uploads
(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES(?,'pipeline-owner','{"WorkRunID":"artifact-authority"}'::jsonb,'legacy-authority','library/pipeline-owner/legacy-authority/test',11,1,'ready')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})
	for _, state := range []string{"ready", "retired"} {
		if _, err := db.Exec(`UPDATE pipeline_artifact_uploads SET state=?`, state); err != nil {
			t.Fatal(err)
		}
		for _, record := range []struct{ concept, id string }{{runConcept, work}, {pipelinesRunConcept, run}} {
			// Exercise both prefilter and the final transactional guard. Even a
			// candidate read before the upload existed cannot delete its authority.
			p := operationalPolicy{concept: record.concept, predicate: "TRUE"}
			q, args := retentionCandidateRead(p, cutoff, old.Add(-24*time.Hour), cutoff, nil)
			var candidates []map[string]any
			if err := db.NewRaw(q, args...).Scan(t.Context(), &candidates); err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 0 {
				t.Fatal("artifact authority admitted to history retention", candidates)
			}
			_, deleted, _, err := i.retireVerified(retentionOwner(), record.concept, []map[string]any{{"id": record.id, "createdAt": rfc(old)}}, record.concept == runConcept, cutoff, false)
			if err != nil || deleted != 0 {
				t.Fatalf("deleted authority: %d %v", deleted, err)
			}
		}
		assertStillStored(t, db, before)
	}
	if _, err := db.Exec(`UPDATE pipeline_artifact_uploads SET library_purged_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct{ concept, id string }{{runConcept, work}, {pipelinesRunConcept, run}} {
		_, deleted, _, err := i.retireVerified(retentionOwner(), record.concept, []map[string]any{{"id": record.id, "createdAt": rfc(old)}}, record.concept == runConcept, cutoff, false)
		if err != nil || deleted == 0 {
			t.Fatalf("confirmed cleanup held history: %d %v", deleted, err)
		}
	}
	assertRetired(t, db, work, run)
}

// A candidate proves each reused step against the run that executed it, on every
// read: at publication, and whenever an overlay is built from it, however long
// after. A release's own rows stay while its artifacts are pinned, and the pull
// request it stood on has no artifact to keep it. So a live candidate holds that run
// the way an artifact holds its producer, or a release would become unprovable a
// month after it was cut. A retired candidate is read no more, and an archived one's
// own rows leave on their usual window, so neither holds anything.
func TestRetentionKeepsARunALiveCandidateReusedFrom(t *testing.T) {
	db, i := sweepDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	work := seedPipelineWorkRun(t, db, "reused-source", pipelineOwner, runStatusSucceeded, old)
	run := seedPipelinesRun(t, db, "reused-source", pipelineOwner, "completed", old)
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('workRunId',?::text) WHERE id=?`, work, run); err != nil {
		t.Fatal(err)
	}
	unrelated := seedPipelineWorkRun(t, db, "unrelated", pipelineOwner, runStatusSucceeded, old)
	// Refused before it opened any work: it names no work run, and must not be
	// kept for that. NULL NOT IN a list that is not empty is not true.
	noWork := seedPipelinesRun(t, db, "no-work", pipelineOwner, "completed", old)
	before := storedKeys(t, db, work, run)
	// The manifest's own type, so a renamed field fails here rather than holding
	// nothing. A candidate records a work run's id bare.
	manifest, err := json.Marshal(pipelines.ReleaseCandidate{Evidence: []pipelines.ReleaseEvidence{
		{Name: "ran", WorkRunID: "the-release"},
		{Name: "reused", WorkRunID: "the-release", ReusedFrom: &pipelines.ReleaseReusedFrom{WorkRunID: "reused-source"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state) VALUES (?,'pipeline-owner',?,'ready')`,
		"sha256:"+strings.Repeat("a", 64), string(manifest)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})
	admitted := func(concept string) []string {
		t.Helper()
		q, args := retentionCandidateRead(operationalPolicy{concept: concept, predicate: "TRUE"}, cutoff, old.Add(-24*time.Hour), cutoff, nil)
		var candidates []map[string]any
		if err := db.NewRaw(q, args...).Scan(t.Context(), &candidates); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(candidates))
		for _, row := range candidates {
			ids = append(ids, fmt.Sprint(row["id"]))
		}
		return ids
	}
	for _, state := range []string{"ready", "approved"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=?`, state); err != nil {
			t.Fatal(err)
		}
		for _, record := range []struct{ concept, id string }{{runConcept, work}, {pipelinesRunConcept, run}} {
			if got := admitted(record.concept); slices.Contains(got, record.id) {
				t.Fatalf("%s: a run a live candidate reused was admitted to history retention: %v", state, got)
			}
			// The final transactional guard too: a read made before the release was
			// prepared cannot delete what it now stands on.
			_, deleted, _, err := i.retireVerified(retentionOwner(), record.concept, []map[string]any{{"id": record.id, "createdAt": rfc(old)}}, record.concept == runConcept, cutoff, false)
			if err != nil || deleted != 0 {
				t.Fatalf("%s: deleted the run a live candidate reused: %d %v", state, deleted, err)
			}
		}
		assertStillStored(t, db, before)
	}
	// Only the run it names is held.
	if got := admitted(runConcept); !slices.Contains(got, unrelated) {
		t.Fatalf("the candidate held another run too: %v", got)
	}
	if got := admitted(pipelinesRunConcept); !slices.Contains(got, noWork) {
		t.Fatalf("the candidate held a run that names no work: %v", got)
	}
	for _, state := range []string{"retired", "archived"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=?`, state); err != nil {
			t.Fatal(err)
		}
		for _, record := range []struct{ concept, id string }{{runConcept, work}, {pipelinesRunConcept, run}} {
			if got := admitted(record.concept); !slices.Contains(got, record.id) {
				t.Fatalf("a %s candidate still holds its source: %v", state, got)
			}
		}
	}
	for _, record := range []struct{ concept, id string }{{runConcept, work}, {pipelinesRunConcept, run}} {
		_, deleted, _, err := i.retireVerified(retentionOwner(), record.concept, []map[string]any{{"id": record.id, "createdAt": rfc(old)}}, record.concept == runConcept, cutoff, false)
		if err != nil || deleted == 0 {
			t.Fatalf("an archived candidate held history: %d %v", deleted, err)
		}
	}
	assertRetired(t, db, work, run)
}

// seedReleaseAttempt writes one attempt's two rows at a (repository, commit, event), as
// the driver writes them: the run row carries the three itself, and the work run in its
// input.
func seedReleaseAttempt(t *testing.T, db *bun.DB, name, repository, sha, event string, at time.Time) (work, run string) {
	t.Helper()
	work = seedPipelineWorkRun(t, db, name, pipelineOwner, runStatusSucceeded, at)
	run = seedPipelinesRun(t, db, name, pipelineOwner, "completed", at)
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('input',jsonb_build_object('repository',?::text,'sha',?::text,'event',?::text)) WHERE id=?`, repository, sha, event, work); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('repository',?::text,'sha',?::text,'event',?::text,'workRunId',?::text) WHERE id=?`, repository, sha, event, work, run); err != nil {
		t.Fatal(err)
	}
	return work, run
}

// A failed-only re-run's candidate names one attempt, and coverage walks back through
// every earlier one on every read (carriedChain, integrations/release). Only the named
// attempt has an artifact to keep it, so a live candidate holds every release attempt at
// the repository and commit of a component it names: the identity a release run key is
// built from, and the identity every link of that chain must share. It takes no new
// manifest field, so a candidate saved before this hold is covered as well.
//
// An attempt refused before it opened any work is held too. It names no work run and no
// goal, and the walk still reads its row to reach attempt 1.
func TestRetentionKeepsTheAttemptsOfALiveCandidatesRelease(t *testing.T) {
	db, i := sweepDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	engineCommit, cockpitCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	const release = "release"

	type row struct{ concept, id string }
	var held []row
	attempt := func(name, repository, sha string) {
		work, run := seedReleaseAttempt(t, db, name, repository, sha, release, old)
		held = append(held, row{runConcept, work}, row{pipelinesRunConcept, run})
	}
	attempt("first-attempt", "acme/engine", engineCommit)
	attempt("second-attempt", "acme/engine", engineCommit)
	attempt("other-component", "acme/cockpit", cockpitCommit)
	refused := seedPipelinesRun(t, db, "refused-attempt", pipelineOwner, "completed", old)
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('repository','acme/engine','sha',?::text,'event','release') WHERE id=?`, engineCommit, refused); err != nil {
		t.Fatal(err)
	}
	held = append(held, row{pipelinesRunConcept, refused})

	var free []row
	unheld := func(name, repository, sha, event string) {
		work, run := seedReleaseAttempt(t, db, name, repository, sha, event, old)
		free = append(free, row{runConcept, work}, row{pipelinesRunConcept, run})
	}
	unheld("another-commit", "acme/engine", strings.Repeat("c", 40), release)
	unheld("another-repository", "acme/other", engineCommit, release)
	// The pair a component names, not one repository crossed with another's commit.
	unheld("crossed-components", "acme/engine", cockpitCommit, release)
	// The chain is a release's alone, so a push of the same commit goes on its window.
	unheld("a-push-of-the-commit", "acme/engine", engineCommit, "push")
	free = append(free, row{runConcept, seedSystemRun(t, db, "a-system-run", runStatusSucceeded, old)})
	// A release row naming no repository or commit matches no pair, and must not be kept
	// for that: NULL NOT IN a list that is not empty is not true.
	unnamed := seedPipelineWorkRun(t, db, "names-no-source", pipelineOwner, runStatusSucceeded, old)
	if _, err := db.Exec(`UPDATE "MemoryNodes" SET payload=payload||jsonb_build_object('input',jsonb_build_object('event','release')) WHERE id=?`, unnamed); err != nil {
		t.Fatal(err)
	}
	free = append(free, row{runConcept, unnamed})

	before := map[string]bool{}
	for _, r := range held {
		maps.Copy(before, storedKeys(t, db, r.id))
	}
	// The manifest's own type, so a renamed field fails here rather than holding nothing.
	manifest, err := json.Marshal(pipelines.ReleaseCandidate{Components: []pipelines.ReleaseComponent{
		{Name: "engine", Repository: "acme/engine", Commit: engineCommit},
		{Name: "cockpit", Repository: "acme/cockpit", Commit: cockpitCommit},
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := "sha256:" + strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state) VALUES (?,'pipeline-owner',?,'ready')`, candidate, string(manifest)); err != nil {
		t.Fatal(err)
	}
	// Live candidates that name no pair. Neither may hold anything, and neither may stop
	// the statement: a NULL in a NOT IN list would keep every release row for good, and
	// jsonb_array_elements refuses a scalar.
	for n, malformed := range []string{`{"components":[{"repository":"acme/engine"}]}`, `{"components":[{"commit":"` + engineCommit + `"}]}`, `{"components":null}`, `{}`} {
		if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state) VALUES (?,'pipeline-owner',?,'ready')`,
			"sha256:"+strings.Repeat(strconv.Itoa(n), 64), malformed); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})
	admitted := func(concept string) []string {
		t.Helper()
		q, args := retentionCandidateRead(operationalPolicy{concept: concept, predicate: "TRUE"}, cutoff, old.Add(-24*time.Hour), cutoff, nil)
		var candidates []map[string]any
		if err := db.NewRaw(q, args...).Scan(t.Context(), &candidates); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(candidates))
		for _, row := range candidates {
			ids = append(ids, fmt.Sprint(row["id"]))
		}
		return ids
	}
	retire := func(r row) (int, error) {
		t.Helper()
		_, deleted, _, err := i.retireVerified(retentionOwner(), r.concept, []map[string]any{{"id": r.id, "createdAt": rfc(old)}}, r.concept == runConcept, cutoff, false)
		return deleted, err
	}
	for _, state := range []string{"preparing", "ready", "approved"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=? WHERE candidate_id=?`, state, candidate); err != nil {
			t.Fatal(err)
		}
		for _, r := range held {
			if slices.Contains(admitted(r.concept), r.id) {
				t.Fatalf("%s: %s was admitted to history retention while a live candidate needs it", state, r.id)
			}
			// The final transactional guard too: a read made before the candidate was
			// prepared cannot delete an attempt it now stands on.
			if deleted, err := retire(r); err != nil || deleted != 0 {
				t.Fatalf("%s: deleted %s under a live candidate: %d %v", state, r.id, deleted, err)
			}
		}
		for _, r := range free {
			if !slices.Contains(admitted(r.concept), r.id) {
				t.Errorf("%s: the candidate held %s as well", state, r.id)
			}
		}
		assertStillStored(t, db, before)
	}
	for _, state := range []string{"retired", "archived"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=? WHERE candidate_id=?`, state, candidate); err != nil {
			t.Fatal(err)
		}
		for _, r := range held {
			if !slices.Contains(admitted(r.concept), r.id) {
				t.Errorf("a %s candidate still holds %s", state, r.id)
			}
		}
	}
	for _, r := range held {
		if deleted, err := retire(r); err != nil || deleted == 0 {
			t.Errorf("an archived candidate held %s: %d %v", r.id, deleted, err)
		}
		assertRetired(t, db, r.id)
	}
}

// A candidate's evidence names complete runs, and coverage reads each of them with its
// steps on every read (completeRun, integrations/release). A named run need not be a
// release: a component's runs may cover different check suites as long as they share its
// repository and commit, and such a run has no artifact to keep it. So a live candidate
// holds every run its evidence names, whatever the run's event, and by that name alone:
// a push of the same commit that no candidate names goes on its window.
func TestRetentionKeepsEveryRunALiveCandidateNames(t *testing.T) {
	db, i := sweepDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	commit := strings.Repeat("a", 40)

	type row struct{ concept, id string }
	suiteWork, suiteRun := seedReleaseAttempt(t, db, "a-check-suite", "acme/engine", commit, "push", old)
	scanWork, scanRun := seedReleaseAttempt(t, db, "a-scheduled-scan", "acme/engine", commit, "schedule", old)
	held := []row{{runConcept, suiteWork}, {pipelinesRunConcept, suiteRun}, {runConcept, scanWork}, {pipelinesRunConcept, scanRun}}

	var free []row
	unheld := func(name, event string) {
		work, run := seedReleaseAttempt(t, db, name, "acme/engine", commit, event, old)
		free = append(free, row{runConcept, work}, row{pipelinesRunConcept, run})
	}
	unheld("another-push-of-the-commit", "push")
	unheld("a-pull-request-at-the-commit", "pull_request")
	free = append(free, row{runConcept, seedSystemRun(t, db, "a-system-run", runStatusSucceeded, old)})
	// Refused before it opened any work: it names no work run, and a candidate naming a
	// blank one must not keep it. The hold compares such a row as "".
	free = append(free, row{pipelinesRunConcept, seedPipelinesRun(t, db, "no-work", pipelineOwner, "completed", old)})

	before := map[string]bool{}
	for _, r := range held {
		maps.Copy(before, storedKeys(t, db, r.id))
	}
	// The manifest's own type, so a renamed field fails here rather than holding nothing.
	// A candidate records a work run's id bare (completeRun compares it with a bare row).
	manifest, err := json.Marshal(pipelines.ReleaseCandidate{
		Components: []pipelines.ReleaseComponent{{Name: "engine", Repository: "acme/engine", Commit: commit}},
		Evidence: []pipelines.ReleaseEvidence{
			{Name: "unit", WorkRunID: "a-check-suite", StepKey: "tests.unit"},
			{Name: "lint", WorkRunID: "a-check-suite", StepKey: "tests.lint"},
			{Name: "scan", WorkRunID: "a-scheduled-scan", StepKey: "scan.all"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := "sha256:" + strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state) VALUES (?,'pipeline-owner',?,'ready')`, candidate, string(manifest)); err != nil {
		t.Fatal(err)
	}
	// Live candidates that name no run. None may hold anything, and none may stop the
	// statement: a NULL in a NOT IN list would keep every row for good, a blank would
	// keep every run that opened no work, and jsonb_array_elements refuses a scalar.
	for n, malformed := range []string{`{"evidence":[{"name":"no-run"}]}`, `{"evidence":[{"workRunId":""}]}`, `{"evidence":[{"reusedFrom":{"workRunId":""}}]}`, `{"evidence":[{"workRunId":null}]}`, `{"evidence":["a-check-suite",null]}`, `{"evidence":"a-check-suite"}`, `{"evidence":null}`, `{}`} {
		if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state) VALUES (?,'pipeline-owner',?,'ready')`,
			fmt.Sprintf("sha256:%064d", n), malformed); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})
	admitted := func(concept string) []string {
		t.Helper()
		q, args := retentionCandidateRead(operationalPolicy{concept: concept, predicate: "TRUE"}, cutoff, old.Add(-24*time.Hour), cutoff, nil)
		var candidates []map[string]any
		if err := db.NewRaw(q, args...).Scan(t.Context(), &candidates); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(candidates))
		for _, row := range candidates {
			ids = append(ids, fmt.Sprint(row["id"]))
		}
		return ids
	}
	retire := func(r row) (int, error) {
		t.Helper()
		_, deleted, _, err := i.retireVerified(retentionOwner(), r.concept, []map[string]any{{"id": r.id, "createdAt": rfc(old)}}, r.concept == runConcept, cutoff, false)
		return deleted, err
	}
	for _, state := range []string{"preparing", "ready", "approved"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=? WHERE candidate_id=?`, state, candidate); err != nil {
			t.Fatal(err)
		}
		for _, r := range held {
			if slices.Contains(admitted(r.concept), r.id) {
				t.Fatalf("%s: %s was admitted to history retention while a live candidate names it", state, r.id)
			}
			// The final transactional guard too: a read made before the candidate was
			// prepared cannot delete a run it now names.
			if deleted, err := retire(r); err != nil || deleted != 0 {
				t.Fatalf("%s: deleted %s under a live candidate: %d %v", state, r.id, deleted, err)
			}
		}
		for _, r := range free {
			if !slices.Contains(admitted(r.concept), r.id) {
				t.Errorf("%s: the candidate held %s, which it does not name", state, r.id)
			}
		}
		assertStillStored(t, db, before)
	}
	for _, state := range []string{"retired", "archived"} {
		if _, err := db.Exec(`UPDATE release_candidates SET state=? WHERE candidate_id=?`, state, candidate); err != nil {
			t.Fatal(err)
		}
		for _, r := range held {
			if !slices.Contains(admitted(r.concept), r.id) {
				t.Errorf("a %s candidate still holds %s", state, r.id)
			}
		}
	}
	for _, r := range held {
		if deleted, err := retire(r); err != nil || deleted == 0 {
			t.Errorf("an archived candidate held %s: %d %v", r.id, deleted, err)
		}
		assertRetired(t, db, r.id)
	}
}

// The hold reads the candidates once per statement, however many versions the
// statement looks at. Read per row it detoasted every live manifest once per version,
// which at a hundred or so candidates took a delete past its 8 s statement timeout,
// so the same page failed every night and retention stopped (retentionHeldBackSQL).
//
// Two reads, and each one hashed. A third read is the cost of every manifest again. A list
// the planner does not hash is read once too, so the loops cannot show it, but every row
// then walks the whole list, and that took a page read past 10 s at 1,000 candidates.
// work_mem is the Postgres default, so what the planner chooses does not depend on the
// database the test runs on.
func TestRetentionHoldReadsTheCandidatesOncePerStatement(t *testing.T) {
	db, _ := sweepDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	if _, err := db.Exec(`INSERT INTO release_candidates (candidate_id,owner_user_id,manifest,state)
SELECT 'sha256:'||encode(sha256(g::text::bytea),'hex'),'pipeline-owner',
 jsonb_build_object('evidence',(SELECT jsonb_agg(jsonb_build_object('name','step-'||e,'workRunId','named-'||g,'reusedFrom',jsonb_build_object('workRunId','source-'||g))) FROM generate_series(1,30) e),
  'components',jsonb_build_array(jsonb_build_object('repository','acme/engine','commit','commit-'||g))),'approved'
FROM generate_series(1,200) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE release_candidates`); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, 50)
	raw := make([]retirementKey, 0, 50)
	for n := range 50 {
		// A release attempt nothing names, so every row reaches both holds.
		work, _ := seedReleaseAttempt(t, db, fmt.Sprintf("many-%d", n), "acme/engine", "not-named", "release", old)
		keys = append(keys, work)
		raw = append(raw, retirementKey{keys[n], runConcept, old})
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	read, args := retentionCandidateRead(operationalPolicy{concept: runConcept, predicate: "TRUE"}, cutoff, old.Add(-24*time.Hour), cutoff, nil)
	for name, statement := range map[string]retirementStatement{
		"the candidate read": {query: read, args: args},
		"the guarded delete": retirementDelete(runConcept, false, keys, string(body)),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(context.Background(), `SET LOCAL work_mem='4MB'`); err != nil {
				t.Fatal(err)
			}
			var plan []byte
			if err := tx.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE, FORMAT JSON) `+statement.query, statement.args...).Scan(&plan); err != nil {
				t.Fatalf("explain: %v\n%.400s", err, statement.query)
			}
			var plans []struct {
				Plan explainNode `json:"Plan"`
			}
			if err := json.Unmarshal(plan, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("explain output: %v %s", err, plan)
			}
			scans, loops := relationLoops(plans[0].Plan, "release_candidates")
			if scans != 2 {
				t.Fatalf("the plan holds %d reads of the candidates, want one for the runs they name and one for their components: %s", scans, plan)
			}
			if loops != float64(scans) {
				t.Errorf("the candidates were read %.0f times across %d scans: the hold went back to the manifests per row", loops, scans)
			}
			if lists := unhashedLists(plan, plans[0].Plan, "release_candidates"); len(lists) != 0 {
				t.Errorf("%v read the candidates and are not hashed, so every row of the statement walks their list: %s", lists, plan)
			}
		})
	}
}

// unhashedLists names the subplans that read a relation and that the plan does not probe
// by hash. Only the text of the filter using a subplan says so. An InitPlan runs once, so
// it is no list: the delete's guard is one, holding the lists it probes.
func unhashedLists(plan []byte, n explainNode, relation string) []string {
	var lists []string
	if strings.HasPrefix(n.SubplanName, "SubPlan ") {
		if scans, _ := relationLoops(n, relation); scans > 0 && !regexp.MustCompile(`hashed `+regexp.QuoteMeta(n.SubplanName)+`\b`).Match(plan) {
			lists = append(lists, n.SubplanName)
		}
	}
	for _, child := range n.Plans {
		lists = append(lists, unhashedLists(plan, child, relation)...)
	}
	return lists
}

// relationLoops is how many scans of a relation a plan holds, and their loops.
func relationLoops(n explainNode, relation string) (scans int, loops float64) {
	if n.Relation == relation && strings.Contains(n.NodeType, "Scan") {
		scans, loops = 1, n.ActualLoops
	}
	for _, child := range n.Plans {
		s, l := relationLoops(child, relation)
		scans, loops = scans+s, loops+l
	}
	return scans, loops
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
// v1:pipelines:run and v1:work:goal declare the composite owner tier with the
// account argument, so the sweep sees a person's run and goal only as the
// cluster's maintenance principal -- the one component/auth/maintenance_actor.go
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
	goalFields := pipelineGoal(pipelineOwner)
	goalFields["accountIds"] = []string{"v1:accounts:account:acme"}
	goal := seedGoal(t, db, "tiered", old, goalFields)
	before := storedKeys(t, db, run, workRun, step, goal)

	latest := func(id, concept string) memorynodes.MemoryNode {
		t.Helper()
		var payload []byte
		if err := db.QueryRowContext(context.Background(), `SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, id).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		return memorynodes.MemoryNode{ID: id, Concept: concept, Type: memorynodes.NodeTypeObject, CreatedAt: old, Payload: payload}
	}
	rows := []memorynodes.MemoryNode{latest(run, pipelinesRunConcept), latest(goal, goalConcept)}

	// THE NEGATIVE CONTROLS. An automation that is not on the maintenance
	// list runs as a reader: the handler floor refuses it, and the tier would
	// hide the rows from it anyway.
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:workJournalRetentionSweep", Role: auth.RoleReader, Unranked: true, Synthetic: true})
	if _, err := i.operationalRetention(reader, false); err == nil {
		t.Fatal("a reader actor ran the sweep")
	}
	for _, row := range rows {
		if memqlengine.AdmitSourceRow(reader, row) {
			t.Fatalf("the tier admitted a reader to a person's %s; the maintenance principal would prove nothing", row.Concept)
		}
		if memqlengine.AdmitSourceRow(actorCtx("somebody-else"), row) {
			t.Fatalf("the tier admitted a stranger to a person's %s", row.Concept)
		}
	}
	if after := storedKeys(t, db, run, workRun, step, goal); !maps.Equal(after, before) {
		t.Fatal("a refused sweep deleted rows")
	}

	// THE POSITIVE: the principal the nightly automation runs under.
	ctx := retentionOwner()
	if auth.MaintenanceActor("workJournalRetentionSweep") == nil {
		t.Fatal("workJournalRetentionSweep is not on the maintenance list")
	}
	for _, row := range rows {
		if !memqlengine.AdmitSourceRow(ctx, row) {
			t.Fatalf("the maintenance principal cannot see a person's %s; the sweep would retire nothing, silently", row.Concept)
		}
	}
	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, run, workRun, step, goal)
	for _, concept := range []string{pipelinesRunConcept, goalConcept} {
		if r := resultFor(t, results, concept, EnvPipelinesRunRetentionDays); r.Candidates != 1 {
			t.Errorf("the %s policy saw %d candidates, want 1", concept, r.Candidates)
		}
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

	t.Run("a full batch of work runs with their steps is retired in halves, and no step goes without its run", func(t *testing.T) {
		db, i, archive, finished := setup(t)
		// Two run versions and three per step: a hundred runs of 35 steps is
		// 10,700 versions in one candidate batch.
		const steps = 35
		perRun := 2 + 3*steps
		if retirementBatchSize*perRun <= retirementMaxVersions {
			t.Fatalf("a batch of %d runs of %d versions fits one object; it would prove nothing", retirementBatchSize, perRun)
		}
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT ?||r, ?, ?::timestamptz - (2 - v) * interval '1 minute', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', ?::text, 'triggeredBy', ?::text, 'status', CASE WHEN v = 2 THEN 'succeeded' ELSE 'running' END)
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, 2) v`,
			runConcept+":wide-", runConcept, finished, pipelineOwner, pipelines.WorkTriggerPrefix+"full", retirementBatchSize); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT 'v1:work:step:wide-'||r||'-'||s, 'v1:work:step', ?::timestamptz - (3 - v) * interval '1 second', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', ?::text, 'runId', ?||r, 'status', (ARRAY['pending','running','done'])[v])
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, ?) s CROSS JOIN generate_series(1, 3) v`,
			finished, pipelineOwner, runConcept+":wide-", retirementBatchSize, steps); err != nil {
			t.Fatal(err)
		}
		// Beside them, two runs a stranger's step keeps: each must keep EVERY
		// one of its own steps too.
		var held []string
		for _, name := range []string{"held-1", "held-2"} {
			run := seedPipelineWorkRun(t, db, name, pipelineOwner, "succeeded", finished)
			held = append(held, run,
				seedWorkChild(t, db, "v1:work:step", name+"-a", run, finished, ownedBy(pipelineOwner, "status", "done")),
				seedWorkChild(t, db, "v1:work:step", name+"-b", run, finished, ownedBy(pipelineOwner, "status", "done")),
				seedWorkChild(t, db, "v1:work:step", name+"-stranger", run, finished, ownedBy(strangerOwner, "status", "done")))
		}
		before := storedKeys(t, db, held...)

		results, err := i.operationalRetention(retentionOwner(), false)
		if err != nil {
			t.Fatalf("an overflowing batch stopped the sweep: %v", err)
		}
		var wide, orphaned int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id LIKE 'v1:work:run:wide-%' OR id LIKE 'v1:work:step:wide-%'`).Scan(&wide); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" s WHERE s.concept='v1:work:step' AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" r WHERE r.concept='v1:work:run' AND r.id=s.payload->>'runId')`).Scan(&orphaned); err != nil {
			t.Fatal(err)
		}
		if wide != 0 || orphaned != 0 {
			t.Fatalf("%d versions of the wide runs and their steps were left, and %d step versions outlived their run", wide, orphaned)
		}
		assertKeptWhole(t, db, before, held...)
		r := resultFor(t, results, runConcept, EnvPipelinesRunRetentionDays)
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

// TestJournalRetentionSplitsABatchLargerThanOneArchiveObject (ruling R27b).
// RetentionSweep runs the journal's model-call and observation detail FIRST
// and returned on its error, so one journal batch over an archive object's
// budget failed the night AND skipped every operational policy after it, the
// pipelines ones included. The journal loop now splits through the same
// retireSplitting; a finished pipelines run seeded beside the journal rows is
// the proof that the night goes on past them.
func TestJournalRetentionSplitsABatchLargerThanOneArchiveObject(t *testing.T) {
	seedJournal := func(t *testing.T, db *bun.DB, prefix string, ids, versions int, at time.Time) {
		t.Helper()
		// No runId: the summary fold reads nothing, so no engine is needed.
		if _, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT ?||r, ?, ?::timestamptz - (? - v) * interval '1 second', 'sweep-test', 'object', '{}', jsonb_build_object('ownerUserId', '', 'inputTokens', v)
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, ?) v`,
			modelCallConcept+":"+prefix, modelCallConcept, at, versions, ids, versions); err != nil {
			t.Fatal(err)
		}
	}
	stored := func(t *testing.T, db *bun.DB, prefix string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id LIKE ?`, modelCallConcept+":"+prefix+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	setup := func(t *testing.T) (*bun.DB, *Integration, time.Time, string) {
		db, i := sweepDB(t)
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvModelCallRetentionDays, "")
		t.Setenv(EnvObservationRetentionDays, "")
		t.Setenv(EnvPipelinesRunRetentionDays, "")
		i.SetArchiver(&verifyingArchive{})
		after := seedPipelinesRun(t, db, "after-the-journal", pipelineOwner, "completed", now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays+10)))
		return db, i, now.AddDate(0, 0, -(DefaultModelCallRetentionDays + 10)), after
	}

	t.Run("a full batch that overflows one object is retired in halves, and the night goes on", func(t *testing.T) {
		db, i, old, after := setup(t)
		perRow := retirementMaxVersions/retirementBatchSize + 1
		seedJournal(t, db, "wide-", retirementBatchSize, perRow, old)
		if n := stored(t, db, "wide-"); n <= retirementMaxVersions {
			t.Fatalf("the fixture holds %d versions, which one object takes; it would prove nothing", n)
		}

		res, err := i.RetentionSweep(retentionOwner(), false)
		if err != nil {
			t.Fatalf("an overflowing journal batch failed the night: %v", err)
		}
		if n := stored(t, db, "wide-"); n != 0 {
			t.Fatalf("%d journal versions were left behind", n)
		}
		assertRetired(t, db, after)
		if res.JournalCandidates != retirementBatchSize || len(res.Oversized) != 0 || len(res.Objects) < 3 {
			t.Fatalf("result %+v: want %d journal candidates retired across at least two objects, plus the run row's", res, retirementBatchSize)
		}
	})

	t.Run("a record too large for one object is kept whole and named, and the rest go", func(t *testing.T) {
		db, i, old, after := setup(t)
		seedJournal(t, db, "small-", 3, 2, old)
		seedJournal(t, db, "oversized", 1, retirementMaxVersions+1, old)

		res, err := i.RetentionSweep(retentionOwner(), false)
		if err != nil {
			t.Fatalf("one oversized journal record failed the night: %v", err)
		}
		if n := stored(t, db, "small-"); n != 0 {
			t.Fatalf("%d versions of the ordinary rows were left behind the oversized one", n)
		}
		if n := stored(t, db, "oversized"); n != retirementMaxVersions+1 {
			t.Fatalf("the oversized row kept %d of its %d versions; it has no archive and must be kept whole", n, retirementMaxVersions+1)
		}
		if want := []string{modelCallConcept + ":oversized1"}; !slices.Equal(res.Oversized, want) {
			t.Fatalf("oversized = %v, want %v", res.Oversized, want)
		}
		assertRetired(t, db, after)
	})
}

// TestRetentionRetiresAPipelineGoalOnceNoRunOfItRemains (ruling R28), with the
// REAL writers: component/workjournal opens the goals, runs and steps exactly
// as the pipelines driver does, through the real engine and the production row
// gate. So the goalId spelling the sweep has to match is the one a run
// actually stores, not one a fixture chose. A goal run twice goes in the same
// sweep as its last run; a goal whose run is still in flight stays with it.
func TestRetentionRetiresAPipelineGoalOnceNoRunOfItRemains(t *testing.T) {
	db, i := sweepDB(t)
	eng := dispatchDBEngine(t, db)
	i.engine, i.admitRow = eng, memqlengine.AdmitSourceRow
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	ctx := context.Background()
	journal := workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
		return eng.Execute(ctx, q)
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), "agent-1")
	open := func(goalKey, attempt string) *workjournal.Run {
		t.Helper()
		run, err := journal.Begin(ctx, workjournal.Work{
			OwnerUserID: "pipeline-goal-owner", Template: "pipeline:acme-app", Statement: "Run acme-app on 3f9c2ab (push)",
			GoalKey: goalKey, RunKey: attempt, RequestedVia: "pipeline", TriggeredBy: pipelines.WorkTriggerPrefix + "full",
			QueueSteps: true,
			Steps: []workjournal.StepDecl{{Key: "checks.vet", Kind: workjournal.KindDeterministic, StepType: "exec",
				Call: map[string]any{"construct": "pipeline", "name": "vet", "stage": "checks"}}},
		})
		if err != nil || run == nil {
			t.Fatalf("the journal could not open the pipeline's run: %v", err)
		}
		return run
	}
	finish := func(run *workjournal.Run) {
		step, err := run.Step(ctx, "checks.vet")
		if err != nil {
			t.Fatal(err)
		}
		if err := step.Done(ctx, map[string]any{"exitCode": 0}); err != nil {
			t.Fatal(err)
		}
		run.Succeeded(ctx, map[string]any{"conclusion": "success"})
	}
	// A goal run twice -- a re-run is a second run of the same goal -- both finished.
	first := open("p|finished", "1")
	finish(first)
	second := open("p|finished", "2")
	finish(second)
	// And a goal whose run is still in flight.
	live := open("p|live", "1")

	family := func(run *workjournal.Run) (goal string, ids []string) {
		t.Helper()
		goal = goalConcept + ":" + run.GoalID()
		runID := runConcept + ":" + run.RunID()
		rows, err := db.QueryContext(ctx, `SELECT DISTINCT id FROM "MemoryNodes" WHERE concept='v1:work:step' AND payload->>'runId'=?`, runID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		ids = []string{runID}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if len(ids) != 2 {
			t.Fatalf("run %s: want the run and its one step stored, found %v", runID, ids)
		}
		var stored string
		if err := db.QueryRowContext(ctx, `SELECT payload->>'goalId' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, runID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		t.Logf("run %s stores goalId %q; its goal's row id is %q", runID, stored, goal)
		return goal, ids
	}
	finishedGoal, firstIDs := family(first)
	_, secondIDs := family(second)
	liveGoal, liveIDs := family(live)
	retired := append(append([]string{finishedGoal}, firstIDs...), secondIDs...)
	kept := append([]string{liveGoal}, liveIDs...)
	if len(storedKeys(t, db, finishedGoal)) == 0 || len(storedKeys(t, db, liveGoal)) == 0 {
		t.Fatal("the goals are not stored under their canonical ids; the assertions below would pass over nothing")
	}
	before := storedKeys(t, db, kept...)

	// The sweep's clock moves past the window: every row above is now old.
	i.SetNow(func() time.Time { return time.Now().UTC().AddDate(0, 0, DefaultPipelinesRunRetentionDays+15) })
	results, err := i.operationalRetention(retentionOwner(), false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)
	if r := resultFor(t, results, goalConcept, EnvPipelinesRunRetentionDays); r.Candidates != 1 || r.DeletedVersions == 0 {
		t.Errorf("the goal policy reported %+v, want the finished goal alone, retired", r)
	}
}

// TestRetentionKeepsAPipelineGoalWhileARunOfItRemains (ruling R28): a
// pipeline's goal goes only once no v1:work:run names it -- in EITHER
// spelling, canonical as the relationship stores it or bare as an older row
// might -- and only a pipeline's goal goes at all.
func TestRetentionKeepsAPipelineGoalWhileARunOfItRemains(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	old := now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays + 10))
	runOf := func(name, goalID, status string) string {
		return seedWorkRun(t, db, name, status, old, map[string]any{
			"ownerUserId": pipelineOwner, "goalId": goalID, "automationName": "pipeline:acme-app",
			"triggeredBy": pipelines.WorkTriggerPrefix + "full",
		})
	}

	// The reachable positives: a goal no run was ever opened for, and one
	// whose only run finished -- that run goes first, in the same sweep.
	retired := []string{
		seedGoal(t, db, "orphan", old, pipelineGoal(pipelineOwner)),
		seedGoal(t, db, "ran-out", old, pipelineGoal(pipelineOwner)),
		runOf("ran-out", goalConcept+":ran-out", runStatusSucceeded),
	}
	notPipeline := pipelineGoal(pipelineOwner)
	notPipeline["requestedVia"], notPipeline["origin"] = "api", "user"
	userOrigin := pipelineGoal(pipelineOwner)
	userOrigin["origin"] = "user"
	kept := []string{
		// A run of it remains, naming it canonically...
		seedGoal(t, db, "canonical", old, pipelineGoal(pipelineOwner)), runOf("canonical", goalConcept+":canonical", runStatusRunning),
		// ...or bare.
		seedGoal(t, db, "bare", old, pipelineGoal(pipelineOwner)), runOf("bare", "bare", runStatusRunning),
		// Not a pipeline's goal: goals are not on the list.
		seedGoal(t, db, "not-a-pipeline", old, notPipeline),
		seedGoal(t, db, "user-origin", old, userOrigin),
		// Its latest version is inside the window.
		seedGoal(t, db, "recent", now.AddDate(0, 0, -2), pipelineGoal(pipelineOwner)),
	}
	before := storedKeys(t, db, kept...)

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	assertRetired(t, db, retired...)
	assertKeptWhole(t, db, before, kept...)
	// The SQL half: a goal a run still names is not even a candidate, in
	// either spelling.
	if r := resultFor(t, results, goalConcept, EnvPipelinesRunRetentionDays); r.Candidates != 2 {
		t.Errorf("the goal policy read %d candidates, want only the orphan and the goal whose run went first", r.Candidates)
	}
}

// TestRetentionKeepsAGoalWhoseRunIsOpenedAfterTheArchive (ruling R28): the
// candidate read found a pipeline goal with no run, and a run of it is opened
// before the delete. The pipelines driver opens a new run of an old goal by
// writing the goal again and then the run; a run may also arrive on its own,
// naming the goal either way. Every shape leaves the goal in place: the
// archive was written, and nothing was deleted.
func TestRetentionKeepsAGoalWhoseRunIsOpenedAfterTheArchive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exact bool // only a run is written, so the goal's versions are exactly as before
		open  func(t *testing.T, db *bun.DB, goal string, at time.Time)
	}{
		{"the driver's order: the goal written again, then its run", false, func(t *testing.T, db *bun.DB, goal string, at time.Time) {
			fields := pipelineGoal(pipelineOwner)
			fields["status"] = "open"
			insertSweepRow(t, db, goal, goalConcept, at, fields)
			seedWorkRun(t, db, "raced", runStatusRunning, at, map[string]any{"ownerUserId": pipelineOwner, "goalId": goal, "triggeredBy": pipelines.WorkTriggerPrefix + "full"})
		}},
		{"a run alone, naming the goal canonically", true, func(t *testing.T, db *bun.DB, goal string, at time.Time) {
			seedWorkRun(t, db, "raced", runStatusRunning, at, map[string]any{"ownerUserId": pipelineOwner, "goalId": goal, "triggeredBy": pipelines.WorkTriggerPrefix + "full"})
		}},
		{"a run alone, naming the goal bare", true, func(t *testing.T, db *bun.DB, goal string, at time.Time) {
			seedWorkRun(t, db, "raced", runStatusRunning, at, map[string]any{"ownerUserId": pipelineOwner, "goalId": strings.TrimPrefix(goal, goalConcept+":"), "triggeredBy": pipelines.WorkTriggerPrefix + "full"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			now := time.Now().UTC().Truncate(time.Second)
			i.SetNow(func() time.Time { return now })
			t.Setenv(EnvArchiveContainer, "test-retention")
			t.Setenv(EnvPipelinesRunRetentionDays, "")
			goal := seedGoal(t, db, "raced", now.AddDate(0, 0, -(DefaultPipelinesRunRetentionDays+10)), pipelineGoal(pipelineOwner))
			before := storedKeys(t, db, goal)
			opened := false
			archive := &verifyingArchive{afterUpload: func() {
				if !opened {
					opened = true
					tc.open(t, db, goal, now)
				}
			}}
			i.SetArchiver(archive)

			results, err := i.operationalRetention(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			r := resultFor(t, results, goalConcept, EnvPipelinesRunRetentionDays)
			// The race was real: the goal WAS a candidate and WAS archived.
			if !opened || r.Candidates != 1 || len(archive.objects) != 1 {
				t.Fatalf("the goal was never archived (%+v, %d objects); the race did not happen", r, len(archive.objects))
			}
			if r.DeletedVersions != 0 {
				t.Fatalf("deleted %d versions of a goal a run was opened for", r.DeletedVersions)
			}
			if tc.exact {
				assertKeptWhole(t, db, before, goal)
			} else {
				assertStillStored(t, db, before)
			}
		})
	}
}
