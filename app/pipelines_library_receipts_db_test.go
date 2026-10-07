package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func TestPipelineReceiptDBReadIsScopedOrderedAndIndependentOfLibrary(t *testing.T) {
	u := &pipelineStreamFake{}
	writer, _, db := pipelineStreamDBStore(t, u)
	reader, _, _ := pipelineStreamDBStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "artifact")
	first, err := writer.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	f.Path, f.Body = "another.tar", strings.NewReader("artifact")
	second, err := writer.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: f.OwnerUserID, WorkRunID: f.WorkRunID, StepKey: f.StepKey, Attempt: f.Attempt}
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), f.OwnerUserID))
	ids := []string{second.Receipt.IntentID, first.Receipt.IntentID}
	got, err := reader.ReadRunFileReceipts(ctx, scope, ids)
	want := []pipelinesteps.StoredFileReceipt{*second.Receipt, *first.Receipt}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("replacement read: %+v %v", got, err)
	}
	// Ready receipt lookup does not consult the editable Library at all.
	reader.engine = pipelineStreamEngineFunc(func(context.Context, string) (any, error) {
		t.Fatal("receipt read consulted mutable Library metadata")
		return nil, nil
	})
	if got, err := reader.ReadRunFileReceipts(ctx, scope, ids); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("independent receipt: %+v %v", got, err)
	}
	for _, mutate := range []func(*pipelinesteps.RunFileReceiptScope){
		func(s *pipelinesteps.RunFileReceiptScope) { s.OwnerUserID = id.NewShortId() },
		func(s *pipelinesteps.RunFileReceiptScope) { s.WorkRunID = id.NewShortId() },
		func(s *pipelinesteps.RunFileReceiptScope) { s.StepKey = "different" },
		func(s *pipelinesteps.RunFileReceiptScope) { s.Attempt++ },
	} {
		changed := scope
		mutate(&changed)
		// Even a legitimate internal actor for the other owner cannot read
		// these IDs; SQL enforces scope independently of the origin gate.
		caller := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), changed.OwnerUserID))
		if result, err := reader.ReadRunFileReceipts(caller, changed, ids); err == nil || result != nil {
			t.Fatalf("wrong-scope read returned data: %+v %v", result, err)
		}
	}
	if result, err := reader.ReadRunFileReceipts(ctx, scope, []string{first.Receipt.IntentID, strings.Repeat("0", 64)}); err == nil || result != nil {
		t.Fatalf("partial missing read: %+v %v", result, err)
	}
	if _, err := db.Exec("UPDATE pipeline_artifact_uploads SET state='blob_verified' WHERE intent_id=$1", second.Receipt.IntentID); err != nil {
		t.Fatal(err)
	}
	if result, err := reader.ReadRunFileReceipts(ctx, scope, ids); err == nil || result != nil {
		t.Fatalf("not-ready read: %+v %v", result, err)
	}
}
