package pipelinerun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestTrainRowsAreWrittenAndReadBack(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	ctx := context.Background()
	owner := "v1:identity:user:" + ownerID
	departed := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)

	tr := Train{
		ID: "t1", OwnerUserID: owner, AccountID: "a1", PipelineID: "p1", Repository: "acme/shop",
		Branch: "train/2026-10-28-1", Departure: TrainDepartureScheduled, State: TrainBuilding,
		OnBoard:    []TrainCar{{Number: 7, Title: "Fix the badge", SHA: shaA, URL: "https://github.com/acme/shop/pull/7"}},
		LeftOff:    []pipelines.TrainSkip{{Number: 9, Reason: "is a draft"}},
		BaseSHA:    shaB,
		Replaces:   "t0",
		DepartedAt: departed,
	}
	if err := store.CreateTrain(ctx, owner, tr); err != nil {
		t.Fatalf("CreateTrain: %v", err)
	}
	if err := store.CreateTrain(ctx, " ", tr); err == nil {
		t.Errorf("a train with no owner was written")
	}
	calls := engine.recorded()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want the one create (the ownerless one must not reach the engine)", len(calls))
	}
	create := calls[0]
	if !strings.HasPrefix(create.query, "mutation "+mCreatePipelineTrain+"(") {
		t.Errorf("create rendered %q", create.query)
	}
	if !create.origin.IsInternal() || create.actor == nil || create.actor.UserId != owner || create.actor.Synthetic {
		t.Errorf("a train is written as its owner, stamped internal: %+v", create.actor)
	}
	for _, want := range []string{"trainId:", "onBoard:", "leftOff:", "is a draft", "baseSha:", "departure:", "departedAt:", "replaces:"} {
		if !strings.Contains(create.query, want) {
			t.Errorf("create carries no %s: %s", want, create.query)
		}
	}

	engine.answers[qPipelineTrainsForPipeline] = []any{map[string]any{
		"id":             "v1:pipelines:train:t1",
		"ownerUserId":    owner,
		"accountId":      "v1:accounts:account:a1",
		"pipelineId":     "v1:pipelines:pipeline:p1",
		"repository":     "acme/shop",
		"branch":         "train/2026-10-28-1",
		"departure":      "express",
		"state":          "awaiting_approval",
		"requested":      []any{float64(7)},
		"replaces":       "v1:pipelines:train:t0",
		"onBoard":        []any{map[string]any{"number": float64(7), "title": "Fix the badge", "sha": shaA, "url": "https://github.com/acme/shop/pull/7"}},
		"leftOff":        []any{map[string]any{"number": float64(9), "reason": "is a draft"}},
		"dropped":        []any{map[string]any{"number": float64(8), "sha": shaB, "reason": "broke tests"}},
		"version":        "0.31.5",
		"baseSha":        shaB,
		"headSha":        shaA,
		"pullRequest":    float64(42),
		"pullRequestUrl": "https://github.com/acme/shop/pull/42",
		"runId":          "v1:pipelines:run:r1",
		"rebuilds":       float64(1),
		"retried":        true,
		"notified":       []any{"departed@" + shaA},
		"departedAt":     "2026-10-28T14:00:00Z",
		"updatedAt":      "2026-10-28T14:05:00Z",
	}}
	got, err := store.LatestTrain(ctx, "p1")
	if err != nil || got == nil {
		t.Fatalf("LatestTrain = %v, %v", got, err)
	}
	if got.ID != "t1" || got.PipelineID != "p1" || got.RunID != "r1" || got.AccountID != "a1" {
		t.Errorf("ids are read back bare: %+v", got)
	}
	if got.State != TrainAwaitingApproval || got.Departure != TrainDepartureExpress || got.PullRequest != 42 || got.Rebuilds != 1 || !got.Retried {
		t.Errorf("scalars: %+v", got)
	}
	if len(got.Requested) != 1 || got.Requested[0] != 7 || got.Replaces != "t0" {
		t.Errorf("requested = %v, replaces %q; want [7] and t0", got.Requested, got.Replaces)
	}
	if len(got.OnBoard) != 1 || got.OnBoard[0].Number != 7 || got.OnBoard[0].SHA != shaA || got.OnBoard[0].URL == "" {
		t.Errorf("onBoard = %+v", got.OnBoard)
	}
	if len(got.LeftOff) != 1 || got.LeftOff[0] != (pipelines.TrainSkip{Number: 9, Reason: "is a draft"}) {
		t.Errorf("leftOff = %+v", got.LeftOff)
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Number != 8 || got.Dropped[0].Reason != "broke tests" {
		t.Errorf("dropped = %+v", got.Dropped)
	}
	if !got.DepartedAt.Equal(departed) || got.UpdatedAt.IsZero() || len(got.Notified) != 1 {
		t.Errorf("times and notified: %+v", got)
	}

	if err := store.UpdateTrain(ctx, owner, "t1", TrainPatch{State: ptr(TrainRunning), HeadSHA: ptr(shaA), Rebuilds: ptr(2)}); err != nil {
		t.Fatalf("UpdateTrain: %v", err)
	}
	update := engine.recorded()[len(engine.recorded())-1]
	if !strings.HasPrefix(update.query, "mutation "+mUpdatePipelineTrain+"(") || !strings.Contains(update.query, "rebuilds: 2") {
		t.Errorf("update rendered %q", update.query)
	}
	if strings.Contains(update.query, "onBoard") || strings.Contains(update.query, "leftOff") {
		t.Errorf("an update writes only the named fields: %s", update.query)
	}
}

func TestMemStoreTrainsRoundTrip(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	owner := "v1:identity:user:" + ownerID
	first := Train{ID: "t1", OwnerUserID: owner, PipelineID: "p1", Branch: "train/2026-10-27-1", State: TrainLanded, DepartedAt: time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)}
	second := Train{ID: "t2", OwnerUserID: owner, PipelineID: "p1", Branch: "train/2026-10-28-1", State: TrainBuilding, DepartedAt: time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)}
	for _, tr := range []Train{first, second} {
		if err := s.CreateTrain(ctx, owner, tr); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := s.LatestTrain(ctx, "p1")
	if err != nil || latest == nil || latest.ID != "t2" {
		t.Fatalf("LatestTrain = %+v, %v; want t2", latest, err)
	}
	if err := s.UpdateTrain(ctx, owner, "t2", TrainPatch{State: ptr(TrainRunning)}); err != nil {
		t.Fatal(err)
	}
	if latest, _ := s.LatestTrain(ctx, "p1"); latest.State != TrainRunning {
		t.Errorf("state after update = %q", latest.State)
	}
	since, err := s.TrainsSince(ctx, "p1", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	if err != nil || len(since) != 1 || since[0].ID != "t2" {
		t.Errorf("TrainsSince = %+v, %v; want only t2", since, err)
	}
	if err := s.UpdateTrain(ctx, owner, "nope", TrainPatch{State: ptr(TrainRunning)}); err == nil {
		t.Errorf("an update of a train that does not exist succeeded")
	}
}
