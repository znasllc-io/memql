package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The installer's setup report (memql-cockpit#541): what the worker keeps,
// how it reaches the heartbeat, and what the setup commands send.

type setupClock struct{ at time.Time }

func (c *setupClock) now() time.Time { return c.at }

func stagesOf(p *memqlv1.SetupProgress) string {
	var parts []string
	for _, st := range p.GetStages() {
		parts = append(parts, st.GetName()+"="+st.GetState())
	}
	return strings.Join(parts, " ")
}

func mustApply(t *testing.T, tr *setupTracker, c SetupControl) {
	t.Helper()
	if err := tr.Apply(c); err != nil {
		t.Fatalf("apply %+v: %v", c, err)
	}
}

func TestSetupTrackerCarriesThePlanAndEachStage(t *testing.T) {
	clock := &setupClock{at: time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)}
	tr := newSetupTracker(clock.now)
	if tr.Proto() != nil {
		t.Fatal("a worker that heard no installer must report none, so the engine keeps what it has")
	}

	mustApply(t, tr, SetupControl{Plan: []string{"runtime", "models", "workbench"}})
	p := tr.Proto()
	if p.GetState() != "running" || stagesOf(p) != "runtime=waiting models=waiting workbench=waiting" {
		t.Fatalf("the plan must show every stage as still to come: %s %s", p.GetState(), stagesOf(p))
	}

	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "runtime", State: "done"}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "models", State: "running", Model: "qwen3.8:27b", CompletedBytes: 4 << 30, TotalBytes: 18 << 30, ModelsTotal: 2}})
	p = tr.Proto()
	if stagesOf(p) != "runtime=done models=running workbench=waiting" {
		t.Fatalf("a stage report replaces its stage in place: %s", stagesOf(p))
	}
	models := p.GetStages()[1]
	if models.GetModel() != "qwen3.8:27b" || models.GetCompletedBytes() != 4<<30 || models.GetTotalBytes() != 18<<30 || models.GetModelsTotal() != 2 {
		t.Fatalf("the download's figures must reach the wire: %+v", models)
	}
	if !p.GetUpdatedAt().AsTime().Equal(clock.at) {
		t.Fatal("the report must say when the installer last spoke")
	}
}

func TestSetupTrackerFinishedSkipsWhatNeverRan(t *testing.T) {
	tr := newSetupTracker(time.Now)
	mustApply(t, tr, SetupControl{Plan: []string{"runtime", "models", "workbench"}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "runtime", State: "done"}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "models", State: "running"}})
	mustApply(t, tr, SetupControl{Finished: true})

	p := tr.Proto()
	if p.GetState() != "finished" || stagesOf(p) != "runtime=done models=failed workbench=skipped" {
		t.Fatalf("finished: a stage never reached is skipped and one left running ended without a result: %s %s", p.GetState(), stagesOf(p))
	}
	if p.GetStages()[1].GetDetail() != setupEndedWithoutResult {
		t.Fatalf("a stage that ended without a result must say so: %q", p.GetStages()[1].GetDetail())
	}

	// Running a stage by hand afterwards is setup running again.
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "workbench", State: "running"}})
	if got := tr.Proto().GetState(); got != "running" {
		t.Fatalf("a stage started after the installer finished must read as running, got %s", got)
	}
}

func TestSetupTrackerWithNothingLeftToRunIsFinished(t *testing.T) {
	// `worker setup --inference` run by hand sends no plan and no finish.
	tr := newSetupTracker(time.Now)
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "runtime", State: "done"}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "models", State: "done"}})
	if got := tr.Proto().GetState(); got != "finished" {
		t.Fatalf("every stage has an outcome, so the setup is finished, got %s", got)
	}
}

func TestSetupTrackerThatWentQuietIsStopped(t *testing.T) {
	clock := &setupClock{at: time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)}
	tr := newSetupTracker(clock.now)
	mustApply(t, tr, SetupControl{Plan: []string{"models"}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "models", State: "running"}})

	clock.at = clock.at.Add(setupQuietAfter - time.Second)
	if got := tr.Proto().GetState(); got != "running" {
		t.Fatalf("inside the quiet window the setup is still running, got %s", got)
	}
	clock.at = clock.at.Add(2 * time.Second)
	if got := tr.Proto().GetState(); got != "stopped" {
		t.Fatalf("a running report nobody refreshed for %s belongs to an installer that stopped, got %s", setupQuietAfter, got)
	}
}

func TestSetupTrackerRefusesWhatTheEngineWouldDrop(t *testing.T) {
	tr := newSetupTracker(time.Now)
	for _, bad := range []SetupControl{
		{},
		{Plan: []string{"models"}, Finished: true},
		{Plan: []string{"menu"}},
		{Plan: []string{"models", "models"}},
		{Stage: &SetupStageReport{Name: "menu", State: "running"}},
		{Stage: &SetupStageReport{Name: "models", State: "pending"}},
	} {
		if err := tr.Apply(bad); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if tr.Proto() != nil {
		t.Fatal("a refused report must leave no report behind")
	}
}

func TestSetupTrackerHoldsAStageToTheEnginesBounds(t *testing.T) {
	tr := newSetupTracker(time.Now)
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{
		Name: "workbench", State: "failed", Detail: strings.Repeat("é", maxSetupDetailBytes),
		Model: "stray", CompletedBytes: 9, TotalBytes: 9, ModelsDone: 1, ModelsTotal: 1,
	}})
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{
		Name: "models", State: "running", Model: strings.Repeat("m", maxSetupModelBytes+1),
		CompletedBytes: 30, TotalBytes: 20, ModelsDone: 5, ModelsTotal: 2,
	}})
	stages := tr.Proto().GetStages()
	wb, models := stages[0], stages[1]
	if len(wb.GetDetail()) > maxSetupDetailBytes || strings.ContainsRune(wb.GetDetail(), '�') {
		t.Fatalf("the detail must be cut at a character boundary within the bound, got %d bytes", len(wb.GetDetail()))
	}
	if wb.GetModel() != "" || wb.GetTotalBytes() != 0 || wb.GetModelsTotal() != 0 {
		t.Fatalf("only the models stage carries download figures: %+v", wb)
	}
	if models.GetModel() != "" {
		t.Fatal("a model id too long to keep is dropped, never cut into a different name")
	}
	if models.GetCompletedBytes() != 20 || models.GetModelsDone() != 2 {
		t.Fatalf("figures past their whole must be clamped: %+v", models)
	}
}

func TestHeartbeatCarriesTheSetupReport(t *testing.T) {
	seam := &seamStream{}
	conn := &Connection{conn: seam}
	report := &memqlv1.SetupProgress{State: "running", Stages: []*memqlv1.SetupStage{{Name: "models", State: "running", Model: "qwen3.8:27b"}}}
	if err := conn.SendHeartbeat(0, nil, nil, nil, nil, report); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendHeartbeat(0, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := seam.frames[0].GetHeartbeat().GetSetup(); got.GetState() != "running" || got.GetStages()[0].GetModel() != "qwen3.8:27b" {
		t.Fatalf("setup on the wire: %+v", got)
	}
	if seam.frames[1].GetHeartbeat().GetSetup() != nil {
		t.Fatal("a beat with no report carried one")
	}
}

func TestLocalControlAcceptsTheInstallersReport(t *testing.T) {
	fleet, _ := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "mq-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "private", "worker.sock")
	l, err := listenControl(ctx, path, fleet)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if r, err := callControl(path, controlRequest{Action: "setup", Setup: setupControlFromFlags("runtime, models,workbench", false)}); err != nil || !r.OK {
		t.Fatalf("plan: %+v %v", r, err)
	}
	if r, err := callControl(path, controlRequest{Action: "setup", Setup: &SetupControl{Stage: &SetupStageReport{Name: "models", State: "running"}}}); err != nil || !r.OK {
		t.Fatalf("stage: %+v %v", r, err)
	}
	if got := stagesOf(fleet.setup.Proto()); got != "runtime=waiting models=running workbench=waiting" {
		t.Fatalf("the fleet's report: %s", got)
	}
	if r, err := callControl(path, controlRequest{Action: "setup", Setup: &SetupControl{Plan: []string{"menu"}}}); err != nil || r.OK || !strings.Contains(r.Error, "menu") {
		t.Fatalf("an unknown stage must be refused in words: %+v %v", r, err)
	}
	if r, err := callControl(path, controlRequest{Action: "setup", Setup: setupControlFromFlags("", true)}); err != nil || !r.OK {
		t.Fatalf("finished: %+v %v", r, err)
	}
	if got := fleet.setup.Proto().GetState(); got != "finished" {
		t.Fatalf("after the installer's end the report is finished, got %s", got)
	}
}

// recordedReports is a reporter's send that keeps what it was given.
type recordedReports struct {
	mu   sync.Mutex
	sent []SetupStageReport
}

func (r *recordedReports) send(c SetupControl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, *c.Stage)
	return nil
}

func (r *recordedReports) states() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	for _, st := range r.sent {
		parts = append(parts, st.Name+"="+st.State)
	}
	return strings.Join(parts, " ")
}

func (r *recordedReports) last(name string) SetupStageReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sent) - 1; i >= 0; i-- {
		if r.sent[i].Name == name {
			return r.sent[i]
		}
	}
	return SetupStageReport{}
}

func TestSetupReporterSendsStatesAtOnceAndCoalescesADownload(t *testing.T) {
	rec := &recordedReports{}
	r := newSetupReporter(rec.send)
	r.Stage("models", "running", "")
	for i := uint64(1); i <= 500; i++ {
		r.Pull("qwen3.8:27b", i<<20, 18<<30, 0, 2)
	}
	r.Close()

	if rec.states() != "models=running models=running" {
		t.Fatalf("a state is sent at once, and five hundred progress events leave as one report: %s", rec.states())
	}
	if got := rec.last("models"); got.CompletedBytes != 500<<20 || got.TotalBytes != 18<<30 || got.ModelsTotal != 2 {
		t.Fatalf("the report sent is the LATEST figure: %+v", got)
	}
}

func TestSetupReporterKeepsARunningStageAlive(t *testing.T) {
	rec := &recordedReports{}
	clock := &setupClock{at: time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)}
	r := &setupReporter{send: rec.send, stages: map[string]SetupStageReport{}, now: clock.now}
	r.Stage("workbench", "running", "")
	r.flush(false)
	if rec.states() != "workbench=running" {
		t.Fatalf("nothing is re-sent inside the keepalive: %s", rec.states())
	}
	clock.at = clock.at.Add(setupKeepalive)
	r.flush(false)
	if rec.states() != "workbench=running workbench=running" {
		t.Fatalf("a running stage is re-sent once the keepalive passes, or the worker reads the installer as gone: %s", rec.states())
	}
}

func TestSetupReporterWithoutAWorkerIsHarmless(t *testing.T) {
	var r *setupReporter
	r.Stage("models", "running", "")
	r.Pull("m", 1, 2, 0, 1)
	r.Close()
	quiet := newSetupReporter(nil)
	quiet.Stage("models", "done", "")
	quiet.Close()
}

// The setup commands' reports, end to end through runInferenceSetup.

func reportedSetup(t *testing.T, host inference.Host) (*fakeSetup, *recordedReports) {
	t.Helper()
	f := newFakeSetup(t, host)
	rec := &recordedReports{}
	f.setup.progress = newSetupReporter(rec.send)
	return f, rec
}

func TestSetupInference_ReportsEachStageAndTheDownload(t *testing.T) {
	f, rec := reportedSetup(t, macServing())
	f.progress["qwen3.8:27b"] = onePull(18 << 30)
	f.progress["qwen3-embedding:0.6b"] = onePull(600 << 20)
	if code := runInferenceSetup(context.Background(), f.setup); code != SetupExitOK {
		t.Fatalf("exit %d\n%s", code, f.out.String())
	}
	f.setup.progress.Close()

	states := rec.states()
	if !strings.HasPrefix(states, "runtime=running runtime=done models=running") || !strings.HasSuffix(states, "models=done") {
		t.Fatalf("the stages must be reported in the order they ran: %s", states)
	}
	done := rec.last("models")
	if done.ModelsTotal != 2 || done.ModelsDone != 2 {
		t.Fatalf("a finished models stage says it fetched them all: %+v", done)
	}
	// The figures are the WHOLE model's, summed across its parts by the
	// display, not the part in flight.
	if done.Model != "qwen3-embedding:0.6b" || done.TotalBytes != 600<<20 || done.CompletedBytes != 600<<20 {
		t.Fatalf("the last download's whole-model figures: %+v", done)
	}
}

func TestSetupReporterSendsADownloadWhileItRuns(t *testing.T) {
	rec := &recordedReports{}
	r := startSetupReporter(rec.send, 5*time.Millisecond)
	defer r.Close()
	r.Pull("qwen3.8:27b", 4<<30, 18<<30, 0, 2)
	deadline := time.Now().Add(5 * time.Second)
	for rec.last("models").CompletedBytes != 4<<30 {
		if time.Now().After(deadline) {
			t.Fatal("a download's progress must reach the worker while the download runs, not only at its end")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSetupInference_ReportsAFailedPullAgainstTheModelsStage(t *testing.T) {
	f, rec := reportedSetup(t, macServing())
	f.pullErr = inference.ErrPullFailed
	runInferenceSetup(context.Background(), f.setup)
	f.setup.progress.Close()
	got := rec.last("models")
	if got.State != "failed" || got.Detail == "" {
		t.Fatalf("a failed pull fails the models stage with the installer's own sentence: %+v", got)
	}
	if rec.last("runtime").State != "done" {
		t.Fatal("the runtime was ready; its stage must stay done")
	}
}

func TestSetupInference_ReportsAMachineBelowTheFloorAsSkipped(t *testing.T) {
	f, rec := reportedSetup(t, belowFloor())
	if code := runInferenceSetup(context.Background(), f.setup); code != SetupExitPrereq {
		t.Fatalf("exit %d", code)
	}
	f.setup.progress.Close()
	runtime, models := rec.last("runtime"), rec.last("models")
	if runtime.State != "skipped" || !strings.Contains(runtime.Detail, "8 GB") {
		t.Fatalf("a machine the floor rules out is skipped, with the reason: %+v", runtime)
	}
	if models.State != "skipped" {
		t.Fatalf("models never ran on a machine with no runtime, so they are skipped: %+v", models)
	}
}

func TestSetupInference_ReportsPolicyThatForbidsPullsAsSkipped(t *testing.T) {
	f, rec := reportedSetup(t, macServing())
	f.setup.pullAllowed = func() bool { return false }
	runInferenceSetup(context.Background(), f.setup)
	f.setup.progress.Close()
	if got := rec.last("models"); got.State != "skipped" || !strings.Contains(got.Detail, "models.pull is false") {
		t.Fatalf("a policy that forbids pulls skips the models stage and names the switch: %+v", got)
	}
}
