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

// THE PERSON'S TURN IS A STAGE (memql#6235). On a computer-use Mac the
// installer waits for the person to allow MemQL, or to go on without it,
// before anything downloads; the stage arrives first and must survive the
// closed vocabulary like the others.
func TestSetupFromProtoKeepsThePermissionsStageInTheInstallersOrder(t *testing.T) {
	s := SetupFromProto(installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "skipped", Detail: "Continued without Screen Recording.", Model: "not a model", TotalBytes: 9},
		&memqlv1.SetupStage{Name: "runtime", State: "waiting"},
		&memqlv1.SetupStage{Name: "models", State: "waiting"},
	), time.Now())
	if len(s.Stages) != 3 || s.Stages[0].Name != "permissions" || s.Stages[1].Name != "runtime" {
		t.Fatalf("the permissions stage must be kept, first, in the installer's order: %+v", s.Stages)
	}
	permissions := s.Stages[0]
	if permissions.State != "skipped" || permissions.Detail != "Continued without Screen Recording." {
		t.Fatalf("the person's choice and the cockpit's sentence for it must be kept: %+v", permissions)
	}
	if permissions.Model != "" || permissions.TotalBytes != 0 {
		t.Fatalf("download figures belong to the models stage alone: %+v", permissions)
	}
	row := s.Row()["stages"].([]any)[0].(map[string]any)
	if _, ok := row["totalBytes"]; ok || row["name"] != "permissions" {
		t.Fatalf("the stored permissions stage carries its name, state and sentence only: %#v", row)
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

// A STEP OF THE INSTALL LANDS NOW (memql#6235). The batch interval held a
// changed step back for up to fifteen seconds -- long enough for the
// add-machine wizard to read a machine that was already downloading as one
// with nothing on it. A step that changes is written at once; the figures of
// a download in flight stay on the batch; and a cockpit changing steps faster
// than the floor waits for it rather than writing every beat.
func TestAChangedInstallStepPersistsInsideTheBatchInterval(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()
	stageState := func(i int, name string) string {
		t.Helper()
		for _, st := range store.lastSeenFlushes[len(store.lastSeenFlushes)-1].Setup["stages"].([]any) {
			if entry := st.(map[string]any); entry["name"] == name {
				return entry["state"].(string)
			}
		}
		t.Fatalf("flush %d carries no %s stage", i, name)
		return ""
	}

	setupBeat(session, t0, installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "running"},
		&memqlv1.SetupStage{Name: "models", State: "waiting"}))
	written := len(store.lastSeenFlushes)

	setupBeat(session, t0.Add(time.Second), installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "done"},
		&memqlv1.SetupStage{Name: "models", State: "running", Model: "qwen3.8:27b", CompletedBytes: 1, TotalBytes: 9}))
	if len(store.lastSeenFlushes) != written+1 || stageState(1, "permissions") != "done" {
		t.Fatalf("a step that changed must be written inside the batch interval: %d writes", len(store.lastSeenFlushes)-written)
	}

	setupBeat(session, t0.Add(3*time.Second), installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "done"},
		&memqlv1.SetupStage{Name: "models", State: "running", Model: "qwen3.8:27b", CompletedBytes: 5, TotalBytes: 9}))
	if len(store.lastSeenFlushes) != written+1 {
		t.Fatal("a download's figures moving within the same step must stay on the batch")
	}

	setupBeat(session, t0.Add(3200*time.Millisecond), installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "done"},
		&memqlv1.SetupStage{Name: "models", State: "failed", Detail: "The disk is full."}))
	if len(store.lastSeenFlushes) != written+2 || stageState(2, "models") != "failed" {
		t.Fatal("a stage that ended must be written at once")
	}

	steps := installerReport(
		&memqlv1.SetupStage{Name: "permissions", State: "done"},
		&memqlv1.SetupStage{Name: "models", State: "failed", Detail: "The disk is full."},
		&memqlv1.SetupStage{Name: "workbench", State: "running"})
	setupBeat(session, t0.Add(3400*time.Millisecond), steps)
	if len(store.lastSeenFlushes) != written+2 {
		t.Fatal("a step changed inside the floor must wait for it, not write every beat")
	}
	setupBeat(session, t0.Add(4*time.Second), steps)
	if len(store.lastSeenFlushes) != written+3 || stageState(3, "workbench") != "running" {
		t.Fatal("a held step must land with the first beat past the floor, though that beat changed nothing")
	}
}
