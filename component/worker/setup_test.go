package worker

import (
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The installer's report (memql#6229): what one may carry, and how it reaches
// the row.

func installerReport(stages ...*memqlv1.SetupStage) *memqlv1.SetupProgress {
	return &memqlv1.SetupProgress{State: "running", Stages: stages}
}

func TestSetupFromProtoKeepsOnlyWhatItCanSay(t *testing.T) {
	received := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	if s := SetupFromProto(nil, received); s.Present() {
		t.Fatal("no report must read as no report")
	}
	if s := SetupFromProto(&memqlv1.SetupProgress{State: "installing"}, received); s.Present() {
		t.Fatal("a state outside the closed set must read as no report, not as a guess at one")
	}

	s := SetupFromProto(&memqlv1.SetupProgress{
		State:     "running",
		UpdatedAt: timestamppb.New(received.Add(-time.Hour)),
		Stages: []*memqlv1.SetupStage{
			{Name: "runtime", State: "done"},
			{Name: "models", State: "running", Model: " qwen3.8:27b ", CompletedBytes: 30, TotalBytes: 20, ModelsDone: 9, ModelsTotal: 2},
			{Name: "models", State: "failed"},
			{Name: "menu", State: "running"},
			{Name: "workbench", State: "pending"},
		},
	}, received)
	if !s.ReportedAt.Equal(received) || !s.UpdatedAt.Equal(received.Add(-time.Hour)) {
		t.Fatalf("freshness is the cluster's clock and the machine's is kept apart: %+v", s)
	}
	if len(s.Stages) != 2 || s.Stages[0].Name != "runtime" || s.Stages[1].Name != "models" {
		t.Fatalf("an unknown stage name or state, and a repeated stage, must be dropped: %+v", s.Stages)
	}
	models := s.Stages[1]
	if models.State != "running" || models.Model != "qwen3.8:27b" {
		t.Fatalf("the first models stage is the one kept: %+v", models)
	}
	if models.CompletedBytes != models.TotalBytes {
		t.Fatalf("bytes past the whole must be clamped to it, got %d of %d", models.CompletedBytes, models.TotalBytes)
	}
	if models.ModelsDone != 2 || models.ModelsTotal != 2 {
		t.Fatalf("more models done than fetched must be clamped, got %d of %d", models.ModelsDone, models.ModelsTotal)
	}
}

func TestSetupFromProtoBoundsTheInstallersWords(t *testing.T) {
	long := strings.Repeat("é", maxSetupDetailLen)
	s := SetupFromProto(installerReport(
		&memqlv1.SetupStage{Name: "workbench", State: "failed", Detail: long},
		&memqlv1.SetupStage{Name: "models", State: "running", Model: strings.Repeat("m", maxSetupModelLen+1)},
	), time.Now())
	detail := s.Stages[0].Detail
	if len(detail) > maxSetupDetailLen || !strings.HasPrefix(long, detail) || strings.ContainsRune(detail, '�') {
		t.Fatalf("the detail must be cut at a character boundary within the bound, got %d bytes", len(detail))
	}
	if s.Stages[1].Model != "" {
		t.Fatal("a model id too long to keep must be dropped, never cut into a different name")
	}
}

func TestSetupRowCarriesModelFiguresOnTheModelsStageOnly(t *testing.T) {
	received := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	if (Setup{}).Row() != nil {
		t.Fatal("no report must render as nil, which the store reads as leave it alone")
	}
	row := SetupFromProto(installerReport(
		&memqlv1.SetupStage{Name: "runtime", State: "done"},
		&memqlv1.SetupStage{Name: "models", State: "running", Model: "qwen3.8:27b", CompletedBytes: 4 << 30, TotalBytes: 18 << 30, ModelsTotal: 2},
		&memqlv1.SetupStage{Name: "workbench", State: "waiting"},
	), received).Row()
	if row["state"] != "running" || row["reportedAt"] != received.Format(time.RFC3339Nano) {
		t.Fatalf("row: %#v", row)
	}
	if _, ok := row["updatedAt"]; ok {
		t.Fatal("a report that did not say when must not be given a time")
	}
	stages := row["stages"].([]any)
	runtime, models := stages[0].(map[string]any), stages[1].(map[string]any)
	if _, ok := runtime["completedBytes"]; ok {
		t.Fatal("only the models stage carries download figures")
	}
	if _, ok := runtime["detail"]; ok {
		t.Fatal("an empty detail is omitted rather than stored as a sentence")
	}
	if models["model"] != "qwen3.8:27b" || models["completedBytes"] != uint64(4<<30) || models["totalBytes"] != uint64(18<<30) || models["modelsTotal"] != 2 {
		t.Fatalf("models stage: %#v", models)
	}
}

func setupBeat(s *streamSession, at time.Time, setup *memqlv1.SetupProgress) {
	s.server.clock = func() time.Time { return at }
	s.handleHeartbeat(&memqlv1.Heartbeat{Ts: timestamppb.New(at), Setup: setup}, "10.0.0.1:1234")
}

func TestTheInstallersReportRidesTheHeartbeatFlush(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	setupBeat(session, t0, installerReport(&memqlv1.SetupStage{Name: "models", State: "running", Model: "qwen3.8:27b"}))
	first := store.lastSeenFlushes[len(store.lastSeenFlushes)-1]
	if first.Setup == nil || first.Setup["state"] != "running" {
		t.Fatalf("the beat's report must land on its flush: %#v", first.Setup)
	}

	// A beat with nothing, and one with a report this engine cannot read,
	// both leave the last report in place: silence is not "no install".
	beatAt(session, t0.Add(HeartbeatBatchInterval+time.Second))
	setupBeat(session, t0.Add(2*HeartbeatBatchInterval+2*time.Second), &memqlv1.SetupProgress{State: "installing"})
	last := store.lastSeenFlushes[len(store.lastSeenFlushes)-1]
	if len(store.lastSeenFlushes) < 3 || last.Setup == nil || last.Setup["state"] != "running" {
		t.Fatalf("the stored report must survive a silent or unreadable beat: %d flushes, last %#v", len(store.lastSeenFlushes), last.Setup)
	}

	setupBeat(session, t0.Add(3*HeartbeatBatchInterval+3*time.Second), &memqlv1.SetupProgress{State: "finished"})
	if got := store.lastSeenFlushes[len(store.lastSeenFlushes)-1].Setup["state"]; got != "finished" {
		t.Fatalf("a later report replaces the earlier one, got %v", got)
	}
}

func TestAMachineWithNoInstallerReportFlushesNone(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	beatAt(session, t0)
	if got := store.lastSeenFlushes[len(store.lastSeenFlushes)-1].Setup; got != nil {
		t.Fatalf("a cockpit that reports no install must send nothing to the row, got %#v", got)
	}
}
