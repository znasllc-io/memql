package pipelinerun

import (
	"context"
	"strings"
	"testing"
)

func TestArtifactExpiryRequiresConfirmedCleanupAndExactOwnerRun(t *testing.T) {
	h, _, ids := artifactRetentionHarness(t, 5)
	files := make([]string, len(ids))
	for n, intent := range ids {
		if err := h.db.QueryRow(`SELECT file_id FROM pipeline_artifact_uploads WHERE intent_id=$1`, intent).Scan(&files[n]); err != nil {
			t.Fatal(err)
		}
	}
	// One complete receipt, two interrupted cleanups, one ready (kept) file,
	// and a valid receipt belonging to a different work run.
	for _, index := range []int{0, 1, 2, 4} {
		if _, err := h.db.Exec(`UPDATE pipeline_artifact_uploads SET state='retired',
retired_etag='confirmed',retirement_lease_id='permanent-fence',library_purged_at=clock_timestamp(),
identity=jsonb_set(identity,'{Name}','"tests.unit.log"'::jsonb) WHERE intent_id=$1`, ids[index]); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []struct {
		index int
		sql   string
	}{
		{1, `UPDATE pipeline_artifact_uploads SET library_purged_at=NULL WHERE intent_id=$1`},
		{2, `UPDATE pipeline_artifact_uploads SET retired_etag='' WHERE intent_id=$1`},
		{4, `UPDATE pipeline_artifact_uploads SET identity=jsonb_set(identity,'{WorkRunID}','"another-run"'::jsonb) WHERE intent_id=$1`},
	} {
		if _, err := h.db.Exec(change.sql, ids[change.index]); err != nil {
			t.Fatal(err)
		}
	}
	// A second replica has no producer-local memory; only the journal counts.
	got, err := h.peer.ExpiredArtifacts(personCtx(h.owner), h.run.ID, append(files, "unknown-file"))
	if err != nil || len(got) != 1 || got[0].FileID != files[0] || got[0].Name != "tests.unit.log" {
		t.Fatalf("confirmed expiry %+v, %v", got, err)
	}
	if _, err := h.peer.ExpiredArtifacts(personCtx(otherID), h.run.ID, files); err == nil {
		t.Fatal("another owner read retirement metadata")
	}
	if _, err := h.peer.ExpiredArtifacts(context.Background(), h.run.ID, files); err == nil {
		t.Fatal("anonymous caller read retirement metadata")
	}
	got, err = h.i.ExpiredArtifacts(personCtx(h.owner), h.run.ID, files[1:])
	if err != nil || len(got) != 0 {
		t.Fatalf("unselected file or incomplete receipt reported %+v, %v", got, err)
	}
	for _, invalid := range [][]string{nil, make([]string, 129), {""}, {" " + files[0]}, {strings.Repeat("x", 257)}, {"v1:library:file:" + files[0]}} {
		if _, err := h.peer.ExpiredArtifacts(personCtx(h.owner), h.run.ID, invalid); err == nil {
			t.Fatal("invalid selection accepted", len(invalid))
		}
	}
}
