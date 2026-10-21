package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func testPipelineLegacyLogDB(t *testing.T, replicas pipelineDBReplicas) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "adopt and retire", true: "user edit during verification"}[keep], func(t *testing.T) {
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
