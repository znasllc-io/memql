package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineLegacyLogFake struct {
	pipelineRetirementFake
	afterVerify func()
}

func (u *pipelineLegacyLogFake) Upload(ctx context.Context, bucket, object string, body []byte, mime string) (string, error) {
	h := sha256.Sum256(body)
	r, err := u.CreateVerifiedStream(ctx, bucket, object, strings.NewReader(string(body)), int64(len(body)), hex.EncodeToString(h[:]), mime)
	return r.URL, err
}
func (u *pipelineLegacyLogFake) VerifyStream(_ context.Context, _, object string, size int64, digest string) (azureblob.VerifiedBlob, error) {
	u.mu.Lock()
	r, ok := u.objects[object]
	u.mu.Unlock()
	if !ok || r.Size != size || r.SHA256 != digest {
		return azureblob.VerifiedBlob{}, errors.New("legacy object mismatch")
	}
	if u.afterVerify != nil {
		u.afterVerify()
	}
	return r, nil
}

// The enrollment list (migration 20261108150000) is seeded from the rows the
// real store writes: a receipt-less pipeline log is listed once, bare, with the
// owner, run and step its row names, and a receipted file is not. The seed is
// the migration itself, run again, which adds nothing twice.
func testPipelineLegacyLogEnrollmentListDB(t *testing.T, replicas pipelineDBReplicas) {
	writer, _, db := replicas.caseFactory()(t, &pipelineLegacyLogFake{})
	owner := id.NewShortId()
	legacy := pipelinesteps.RunFile{OwnerUserID: owner, WorkRunID: id.NewShortId(), StepKey: "tests.unit", Name: "tests.unit.log", MimeType: "text/plain", Bytes: []byte("pre-receipt step output")}
	stored, err := writer.StoreRunFile(t.Context(), legacy)
	if err != nil || stored.FileID == "" {
		t.Fatalf("legacy writer %+v %v", stored, err)
	}
	receipted, err := writer.StoreRunFileStream(t.Context(), pipelineStreamFile(owner, "receipted artifact bytes"))
	if err != nil || receipted.FileID == "" {
		t.Fatalf("receipted writer %+v %v", receipted, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM pipeline_legacy_log_enrollment WHERE owner_user_id=$1`, owner); err != nil {
			t.Error(err)
		}
	})
	migration, err := os.ReadFile("../component/database/memory-nodes/migrations/20261108150000_pipeline_legacy_log_enrollment.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.ExecContext(t.Context(), string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.QueryContext(t.Context(), `SELECT file_id,work_run_id,step_key,state,tries FROM pipeline_legacy_log_enrollment WHERE owner_user_id=$1`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var listed []string
	for rows.Next() {
		var file, work, step, state string
		var tries int
		if err := rows.Scan(&file, &work, &step, &state, &tries); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, strings.Join([]string{file, work, step, state, strconv.Itoa(tries)}, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{stored.FileID, legacy.WorkRunID, "tests.unit", "pending", "0"}, " ")
	if len(listed) != 1 || listed[0] != want {
		t.Fatalf("the list holds %q, want only the receipt-less log %q", listed, want)
	}
}

func testPipelineLegacyLogDB(t *testing.T, replicas pipelineDBReplicas) {
	for _, tc := range []struct {
		name         string
		keep, fenced bool
	}{
		{name: "adopt and retire"},
		{name: "user edit during verification", keep: true},
		// The step's other artifacts were retired first, which fenced its
		// scope; the log's adoption still enters it (20261108150100).
		{name: "adopt into a scope retention already fenced", fenced: true},
	} {
		keep := tc.keep
		t.Run(tc.name, func(t *testing.T) {
			factory := replicas.caseFactory()
			u := &pipelineLegacyLogFake{}
			writer, engine, db := factory(t, u)
			peer, _, _ := factory(t, u)
			file := pipelinesteps.RunFile{OwnerUserID: id.NewShortId(), WorkRunID: id.NewShortId(), StepKey: "tests.unit", Name: "tests.unit.log", MimeType: "text/plain", Bytes: []byte("retained legacy step output")}
			stored, err := writer.StoreRunFile(t.Context(), file)
			if err != nil || stored.FileID == "" {
				t.Fatalf("legacy writer %+v %v", stored, err)
			}
			ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(t.Context(), file.OwnerUserID))
			templateMutation(t, engine, ctx, "createArtifact", map[string]any{"sourceConceptRef": "v1:library:file:" + stored.FileID, "ownerUserId": file.OwnerUserID, "lens": "artifact", "kind": "file", "source": "pipeline", "title": file.Name})
			scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: file.OwnerUserID, WorkRunID: file.WorkRunID, StepKey: file.StepKey, Attempt: 1}
			if tc.fenced {
				if err := writer.FenceRunFileScope(ctx, scope); err != nil {
					t.Fatal(err)
				}
			}
			if keep {
				u.afterVerify = func() {
					if err := appendPipelineLibraryTestVersion(ctx, db, pipelineLibraryArtifactID(stored.FileID), map[string]any{"labels": []string{"keep"}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			r, err := peer.AdoptLegacyRunLog(ctx, scope, stored.FileID)
			if keep {
				if !errors.Is(err, pipelinesteps.ErrArtifactRetained) {
					t.Fatal("verification race ignored user edit", err)
				}
				return
			}
			if err != nil || r.FileID != stored.FileID || r.ETag == "" {
				t.Fatalf("adoption %+v %v", r, err)
			}
			if err := writer.FenceRunFileScope(ctx, scope); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.RetireRunFile(ctx, scope, r.IntentID); err != nil {
				t.Fatal(err)
			}
			if err := writer.PurgeRetiredRunFile(ctx, scope, r.IntentID); err != nil {
				t.Fatal(err)
			}
			again, err := peer.AdoptLegacyRunLog(ctx, scope, stored.FileID)
			if err != nil || again.IntentID != r.IntentID || u.writes != 1 {
				t.Fatalf("adoption resurrected bytes %+v %v writes=%d", again, err, u.writes)
			}
		})
	}
}
