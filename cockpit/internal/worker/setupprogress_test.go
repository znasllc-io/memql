package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
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
		{Answer: &SetupAnswer{Stage: "models", Choice: "skip"}},
		{Answer: &SetupAnswer{Stage: "permissions", Choice: "allow"}},
		{Reset: true, Finished: true},
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

	if r, err := callControl(path, controlRequest{Action: "setup", Setup: setupControlFromFlags("runtime, models,workbench", false, false, "", "")}); err != nil || !r.OK {
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
	if r, err := callControl(path, controlRequest{Action: "setup", Setup: setupControlFromFlags("", true, false, "", "")}); err != nil || !r.OK {
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

// THE PERSON'S TURN (memql-cockpit#548): the permissions stage waits on the
// person, and their answer is a report of its own, taken only while the
// stage is waiting and kept through the setup command's keepalives.
func TestSetupTrackerTakesTheAnswerOnlyWhileTheStageWaits(t *testing.T) {
	tr := newSetupTracker(time.Now)
	skip := SetupControl{Answer: &SetupAnswer{Stage: "permissions", Choice: "skip"}}
	if err := tr.Apply(skip); err == nil {
		t.Fatal("an answer with no install waiting must be refused")
	}
	mustApply(t, tr, SetupControl{Plan: []string{"permissions", "runtime", "models"}})
	if err := tr.Apply(skip); err == nil {
		t.Fatal("an answer for a stage still to come must be refused")
	}

	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	mustApply(t, tr, skip)
	// The setup command's keepalive re-sends "running" every thirty seconds.
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	local := tr.Local()
	if local == nil || local.Answers["permissions"].Choice != "skip" {
		t.Fatalf("a keepalive must never overwrite the person's answer: %+v", local)
	}
	if got := stagesOf(tr.Proto()); got != "permissions=running runtime=waiting models=waiting" {
		t.Fatalf("an answer is not a stage report; the setup command reports how the stage ended: %s", got)
	}

	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "skipped", Detail: "Continued without Screen Recording."}})
	if err := tr.Apply(skip); err == nil {
		t.Fatal("an answer for a stage that has ended has nobody left to read it, and must be refused")
	}
	// Run again later, the stage asks again.
	mustApply(t, tr, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	if local := tr.Local(); len(local.Answers) != 0 {
		t.Fatalf("a stage starting again must not inherit the answer it was given before: %+v", local.Answers)
	}
	mustApply(t, tr, skip)
	mustApply(t, tr, SetupControl{Plan: []string{"permissions"}})
	if local := tr.Local(); len(local.Answers) != 0 {
		t.Fatal("a new install's plan starts with no answers")
	}
}

// A STEP IS WHAT THE CLUSTER HEARS AT ONCE: the plan, a stage's state or
// sentence, an answer, the end, a reset. A download's figures and a
// keepalive keep the heartbeat's own cadence.
func TestSetupTrackerSaysWhichReportsChangeAStep(t *testing.T) {
	tr := newSetupTracker(time.Now)
	steps := 0
	tr.onStep = func() { steps++ }
	expect := func(want int, what string, c SetupControl) {
		t.Helper()
		mustApply(t, tr, c)
		if steps != want {
			t.Fatalf("%s: %d steps, want %d", what, steps, want)
		}
	}
	expect(0, "a reset with nothing to forget", SetupControl{Reset: true})
	expect(1, "the plan", SetupControl{Plan: []string{"permissions", "models"}})
	expect(2, "a stage starting", SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	expect(2, "a keepalive", SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	expect(3, "the person's answer", SetupControl{Answer: &SetupAnswer{Stage: "permissions", Choice: "skip"}})
	expect(4, "a stage ending", SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "skipped", Detail: "Skipped for now."}})
	expect(5, "the next stage starting", SetupControl{Stage: &SetupStageReport{Name: "models", State: "running", CompletedBytes: 1, TotalBytes: 9}})
	expect(5, "a download moving", SetupControl{Stage: &SetupStageReport{Name: "models", State: "running", CompletedBytes: 5, TotalBytes: 9}})
	expect(6, "a stage's sentence changing", SetupControl{Stage: &SetupStageReport{Name: "models", State: "running", Detail: "Waiting for the disk."}})
	expect(7, "the end", SetupControl{Finished: true})
	expect(8, "a reset of an earlier install", SetupControl{Reset: true})
	if p := tr.Proto(); p.GetState() != "finished" || len(p.GetStages()) != 0 {
		t.Fatalf("a reset leaves a finished report with nothing in it, which replaces the engine's: %s %s", p.GetState(), stagesOf(p))
	}
	if err := tr.Apply(SetupControl{Answer: &SetupAnswer{Stage: "permissions", Choice: "skip"}}); err == nil || steps != 8 {
		t.Fatal("a refused report is no step")
	}
}

// IT SURVIVES A RESTART: Screen Recording needs the worker restarted
// mid-install, and the stages still to come must not vanish with it.
func TestSetupTrackerSurvivesARestart(t *testing.T) {
	clock := &setupClock{at: time.Date(2026, 10, 18, 15, 30, 0, 0, time.UTC)}
	stateDir := filepath.Join(t.TempDir(), "state", "homes", "a")
	path := setupReportPath(stateDir)
	if filepath.Base(path) != setupReportFile || filepath.Dir(path) != filepath.Dir(filepath.Dir(stateDir)) {
		t.Fatalf("the report is the machine's, kept at the state root: %s", path)
	}
	before := loadSetupTracker(clock.now, path)
	if before.Proto() != nil {
		t.Fatal("no file is no report")
	}
	mustApply(t, before, SetupControl{Plan: []string{"permissions", "runtime", "models", "workbench"}})
	mustApply(t, before, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	mustApply(t, before, SetupControl{Answer: &SetupAnswer{Stage: "permissions", Choice: "skip"}})
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the report is saved, readable by its owner alone: %v %v", info, err)
	}

	after := loadSetupTracker(clock.now, path)
	if got := stagesOf(after.Proto()); got != "permissions=running runtime=waiting models=waiting workbench=waiting" {
		t.Fatalf("the next worker must carry the plan the last one held: %s", got)
	}
	if after.Local().Answers["permissions"].Choice != "skip" {
		t.Fatal("the next worker must keep the person's answer for the setup command still waiting on it")
	}
	if got := after.Proto().GetState(); got != "running" {
		t.Fatalf("a report saved moments ago is still running, got %s", got)
	}
	clock.at = clock.at.Add(setupQuietAfter + time.Second)
	if got := loadSetupTracker(clock.now, path).Proto().GetState(); got != "stopped" {
		t.Fatalf("a report read back from an installer that went quiet is stopped, got %s", got)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadSetupTracker(clock.now, path).Proto() != nil {
		t.Fatal("a file that cannot be read is a report this worker does not have")
	}
}

func TestSetupControlFlagsSayOneThing(t *testing.T) {
	if c := setupControlFromFlags("", false, false, "permissions", "skip"); c.Answer == nil || c.Answer.Stage != "permissions" || c.Answer.Choice != "skip" || c.Finished || c.Reset {
		t.Fatalf("--answer: %+v", c)
	}
	if c := setupControlFromFlags("", false, true, "", ""); !c.Reset || c.Answer != nil || len(c.Plan) != 0 {
		t.Fatalf("--reset: %+v", c)
	}
	if c := setupControlFromFlags("permissions,models", true, true, "", ""); len(c.Plan) != 2 || c.Finished || c.Reset {
		t.Fatalf("a plan is the whole report: %+v", c)
	}
}

func TestLocalStatusCarriesTheSetupReport(t *testing.T) {
	fleet, _ := controlFixture(t)
	if fleet.LocalStatus().Setup != nil {
		t.Fatal("no install, no report")
	}
	mustApply(t, fleet.setup, SetupControl{Plan: []string{"permissions", "models"}})
	mustApply(t, fleet.setup, SetupControl{Stage: &SetupStageReport{Name: "permissions", State: "running"}})
	mustApply(t, fleet.setup, SetupControl{Answer: &SetupAnswer{Stage: "permissions", Choice: "skip"}})
	setup := fleet.LocalStatus().Setup
	if setup == nil || setup.State != "running" || len(setup.Stages) != 2 || setup.Stages[0].State != "running" || setup.Answers["permissions"].Choice != "skip" {
		t.Fatalf("the status must say setup is waiting on the person, and what they answered: %+v", setup)
	}
}

// A STEP BEATS AT ONCE. A stream that opens while an install is under way
// beats immediately (Register carries no report); a step reported inside
// setupBeatSpacing of the last beat waits it out; and neither proves the
// stream, which only the cadence does.
func TestAStepOfTheInstallBeatsAtOnce(t *testing.T) {
	tr := newSetupTracker(time.Now)
	mustApply(t, tr, SetupControl{Plan: []string{"permissions", "models"}})
	seam := &seamStream{}
	r := &Runner{heartbeat: time.Hour, setup: tr, beatNow: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	var proved atomic.Bool
	done := make(chan struct{})
	go func() { defer close(done); r.heartbeatLoop(ctx, &Connection{conn: seam}, &proved) }()
	defer func() { cancel(); <-done }()
	beats := func() int {
		return seam.count(func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil })
	}

	waitFor(t, "the beat a stream sends when it opens during an install", func() bool { return beats() == 1 })
	if got := seam.frames[0].GetHeartbeat().GetSetup(); got.GetState() != "running" {
		t.Fatalf("that beat must carry the report: %+v", got)
	}
	r.RequestBeat()
	r.RequestBeat()
	time.Sleep(200 * time.Millisecond)
	if beats() != 1 {
		t.Fatal("a step reported inside the spacing must wait it out")
	}
	waitFor(t, "the held step's beat", func() bool { return beats() == 2 })
	time.Sleep(1200 * time.Millisecond)
	if beats() != 2 {
		t.Fatalf("two steps inside the spacing must leave as one beat, got %d", beats())
	}
	if proved.Load() {
		t.Fatal("a beat sent for a step must not prove the stream; only the cadence does")
	}
}
