package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineRetirementFake struct {
	pipelineStreamFake
	retireMu         sync.Mutex
	tokens           map[string]string
	err              error
	entered, proceed chan struct{}
}

func (u *pipelineRetirementFake) RetireVerifiedStream(ctx context.Context, _, object string, _ azureblob.VerifiedBlob, token string) (azureblob.RetiredBlob, error) {
	u.retireMu.Lock()
	if u.tokens == nil {
		u.tokens = map[string]string{}
	}
	if old := u.tokens[object]; old != "" && old != token {
		u.retireMu.Unlock()
		return azureblob.RetiredBlob{}, errors.New("retirement recovery changed its token")
	}
	u.tokens[object] = token
	entered, proceed, err := u.entered, u.proceed, u.err
	u.entered, u.proceed = nil, nil
	u.retireMu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-proceed:
		case <-ctx.Done():
			return azureblob.RetiredBlob{}, ctx.Err()
		}
	}
	return azureblob.RetiredBlob{ETag: `"retired-1"`}, err
}

func pipelineLifecycleScope(f pipelinesteps.StreamRunFile) (context.Context, pipelinesteps.RunFileReceiptScope) {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), f.OwnerUserID)),
		pipelinesteps.RunFileReceiptScope{OwnerUserID: f.OwnerUserID, WorkRunID: f.WorkRunID, StepKey: f.StepKey, Attempt: f.Attempt}
}

func testPipelineLifecycleDBPinsAndPermanentReleaseAcrossReplicas(t *testing.T, newStore pipelineStoreFactory) {
	u := &pipelineRetirementFake{}
	writer, _, _ := newStore(t, u)
	other, _, _ := newStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "artifact")
	stored, err := writer.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	ctx, scope := pipelineLifecycleScope(f)
	ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "candidate-1", IntentIDs: []string{stored.Receipt.IntentID}}
	if got, err := other.PinRunFileReceipts(ctx, ref); err != nil || len(got) != 1 || got[0] != *stored.Receipt {
		t.Fatalf("pin: %+v %v", got, err)
	}
	if _, err := writer.RetireRunFile(ctx, scope, stored.Receipt.IntentID); err == nil {
		t.Fatal("retired an active publication reference")
	}
	if len(u.tokens) != 0 {
		t.Fatal("pinned refusal contacted storage")
	}
	changed := ref
	changed.IntentIDs = []string{strings.Repeat("0", 64)}
	if err := writer.ReleaseRunFileReference(ctx, changed); err == nil {
		t.Fatal("changed immutable reference released artifacts")
	}
	if err := writer.ReleaseRunFileReference(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := other.ReleaseRunFileReference(ctx, ref); err != nil {
		t.Fatal("replacement release", err)
	}
	if _, err := writer.PinRunFileReceipts(ctx, ref); err == nil {
		t.Fatal("delayed pin resurrected a released consumer")
	}
	retired, err := other.RetireRunFile(ctx, scope, stored.Receipt.IntentID)
	if err != nil || retired.FileID != stored.FileID || retired.TombstoneETag == "" {
		t.Fatalf("retire: %+v %v", retired, err)
	}
	if same, err := writer.RetireRunFile(ctx, scope, stored.Receipt.IntentID); err != nil || same != retired {
		t.Fatalf("retirement retry: %+v %v", same, err)
	}
	f.Body = strings.NewReader("artifact")
	if _, err := writer.StoreRunFileStream(context.Background(), f); err == nil {
		t.Fatal("retired artifact was reserved again")
	}
	ref.ReferenceID = "new-consumer"
	if _, err := writer.PinRunFileReceipts(ctx, ref); err == nil {
		t.Fatal("new consumer pinned retired bytes")
	}
	if _, err := writer.ReadRunFileReceipts(ctx, scope, ref.IntentIDs); err == nil {
		t.Fatal("retired receipt appeared ready")
	}
}

func testPipelineLifecycleDBReleaseBeforeDelayedPinAndNoPartialPin(t *testing.T, newStore pipelineStoreFactory) {
	u := &pipelineRetirementFake{}
	s, _, db := newStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "data")
	stored, err := s.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	ctx, scope := pipelineLifecycleScope(f)
	ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "consumer", IntentIDs: []string{stored.Receipt.IntentID}}
	if err := s.ReleaseRunFileReference(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinRunFileReceipts(ctx, ref); err == nil {
		t.Fatal("late pin crossed earlier release")
	}
	ref.ReferenceID = "partial"
	ref.IntentIDs = append(ref.IntentIDs, strings.Repeat("0", 64))
	if got, err := s.PinRunFileReceipts(ctx, ref); err == nil || got != nil {
		t.Fatal("partial pin returned receipts", got, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM pipeline_artifact_references WHERE owner_user_id=$1 AND state='active'", f.OwnerUserID).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial pin survived", count, err)
	}
}

func testPipelineLifecycleDBPinAndRetireAreMutuallyExclusive(t *testing.T, newStore pipelineStoreFactory) {
	u := &pipelineRetirementFake{}
	one, _, _ := newStore(t, u)
	two, _, _ := newStore(t, u)
	for range 6 {
		f := pipelineStreamFile(id.NewShortId(), "data")
		stored, err := one.StoreRunFileStream(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		ctx, scope := pipelineLifecycleScope(f)
		ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "racing-consumer", IntentIDs: []string{stored.Receipt.IntentID}}
		start := make(chan struct{})
		pinned, retired := make(chan error, 1), make(chan error, 1)
		go func() { <-start; _, err := one.PinRunFileReceipts(ctx, ref); pinned <- err }()
		go func() { <-start; _, err := two.RetireRunFile(ctx, scope, stored.Receipt.IntentID); retired <- err }()
		close(start)
		pinErr, retireErr := <-pinned, <-retired
		if (pinErr == nil) == (retireErr == nil) {
			t.Fatalf("exactly one operation must win: pin=%v retirement=%v", pinErr, retireErr)
		}
	}
}

func testPipelineLifecycleDBUnknownRetirementRetainsQuotaAndRejectsLateWriters(t *testing.T, newStore pipelineStoreFactory) {
	u := &pipelineRetirementFake{entered: make(chan struct{}), proceed: make(chan struct{}), err: errors.New("lost provider reply")}
	one, _, db := newStore(t, u)
	two, _, _ := newStore(t, u)
	one.quotaBytes = 4
	two.quotaBytes = 4
	f := pipelineStreamFile(id.NewShortId(), "data")
	stored, err := one.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	ctx, scope := pipelineLifecycleScope(f)
	entered, proceed := u.entered, u.proceed
	retired := make(chan error, 1)
	go func() { _, err := one.RetireRunFile(ctx, scope, stored.Receipt.IntentID); retired <- err }()
	<-entered
	ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "late-consumer", IntentIDs: []string{stored.Receipt.IntentID}}
	if _, err := two.PinRunFileReceipts(ctx, ref); err == nil {
		t.Fatal("pin crossed retiring transition")
	}
	f.Body = strings.NewReader("data")
	if _, err := two.StoreRunFileStream(context.Background(), f); err == nil {
		t.Fatal("writer crossed retiring transition")
	}
	close(proceed)
	if err := <-retired; err == nil {
		t.Fatal("unknown provider result reported retirement success")
	}
	f.Path = "replacement"
	f.Body = strings.NewReader("data")
	if _, err := two.StoreRunFileStream(context.Background(), f); err == nil {
		t.Fatal("unknown retirement freed capacity")
	}
	var state string
	if err := db.QueryRow("SELECT state FROM pipeline_artifact_uploads WHERE intent_id=$1", stored.Receipt.IntentID).Scan(&state); err != nil || state != "retiring" {
		t.Fatal(state, err)
	}
	u.retireMu.Lock()
	u.err = nil
	u.retireMu.Unlock()
	if _, err := two.RetireRunFile(ctx, scope, stored.Receipt.IntentID); err != nil {
		t.Fatal("replacement recovery", err)
	}
	f.Body = strings.NewReader("data")
	if _, err := two.StoreRunFileStream(context.Background(), f); err != nil {
		t.Fatal("confirmed retirement did not release capacity", err)
	}
}

func TestPipelineLifecycleRequiresAuthorityBeforeJournal(t *testing.T) {
	s := &pipelinesLibraryStore{uploadDB: func() *sql.DB { t.Fatal("unauthorized call touched journal"); return nil }}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: "owner", WorkRunID: "run", StepKey: "step", Attempt: 1}
	ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "consumer", IntentIDs: []string{strings.Repeat("a", 64)}}
	for _, ctx := range []context.Context{context.Background(), auth.ContextWithUserActor(context.Background(), "owner"), auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "other"))} {
		if _, err := s.PinRunFileReceipts(ctx, ref); err == nil {
			t.Fatal("untrusted pin")
		}
		if err := s.ReleaseRunFileReference(ctx, ref); err == nil {
			t.Fatal("untrusted release")
		}
		if _, err := s.RetireRunFile(ctx, scope, ref.IntentIDs[0]); err == nil {
			t.Fatal("untrusted retirement")
		}
	}
}

// The cases use unique owners and fresh stores, with two independent real
// engines/connections owned by this parent. Reusing only their immutable DSL
// loading preserves replica behavior without paying a full boot per case.
func TestPipelineLifecycleDB(t *testing.T) {
	replicas := pipelineDBTestReplicas(t)
	t.Run("TestPipelineLifecycleDBPinsAndPermanentReleaseAcrossReplicas", func(t *testing.T) {
		testPipelineLifecycleDBPinsAndPermanentReleaseAcrossReplicas(t, replicas.caseFactory())
	})
	t.Run("TestPipelineLifecycleDBReleaseBeforeDelayedPinAndNoPartialPin", func(t *testing.T) {
		testPipelineLifecycleDBReleaseBeforeDelayedPinAndNoPartialPin(t, replicas.caseFactory())
	})
	t.Run("TestPipelineLifecycleDBPinAndRetireAreMutuallyExclusive", func(t *testing.T) { testPipelineLifecycleDBPinAndRetireAreMutuallyExclusive(t, replicas.caseFactory()) })
	t.Run("TestPipelineLifecycleDBUnknownRetirementRetainsQuotaAndRejectsLateWriters", func(t *testing.T) {
		testPipelineLifecycleDBUnknownRetirementRetainsQuotaAndRejectsLateWriters(t, replicas.caseFactory())
	})
}
