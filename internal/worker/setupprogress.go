package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// What this machine's installer is doing, as the worker carries it to the
// cluster (memql-cockpit#541; engine half znasllc-io/memql#6229).
//
// ===========================================================================
// THE INSTALLER OUTLIVES THE MOMENT THE MACHINE CONNECTS
// ===========================================================================
// The installer starts this worker and only then installs the model runtime,
// pulls the models and sets up workbenches -- each a `worker setup` process
// in a terminal, and the pull goes straight to the runtime, not through
// ModelPullStart. None of it reached the cluster, so MemQL OS read a machine
// still downloading 18 GB as a machine with no models and offered to
// download them again. The installer now tells this worker what it is doing
// over the owner-only control socket, and every heartbeat carries it.
//
// THE WORKER HOLDS IT; THE INSTALLER WRITES IT. Three kinds of report arrive:
// the PLAN (the stages the installer will run, sent once the worker is up),
// one STAGE at a time from the setup commands, and FINISHED from the
// installer at its end. A stage it never reached is then skipped, and one
// still running ended without a result -- so a person is never told a stage
// is coming that will not.
//
// SILENCE IS NOT PROGRESS. A setup command re-sends its running stage every
// thirty seconds, so a report that has gone quiet for two minutes belongs to
// an installer that is no longer running -- the terminal was closed, the
// process killed -- and is carried as "stopped" rather than as work in
// flight.
//
// THE PERSON'S TURN IS A STAGE (memql-cockpit#548). On a computer-use Mac
// the installer's first stage is `permissions`: it opens the Permissions
// window and waits until both permissions are allowed or the person chooses
// to go on without them -- in that window, or at the terminal -- before
// anything downloads. The choice arrives as an ANSWER, a report of its own
// that the waiting setup command reads back; the setup command then reports
// how the stage ended. An answer never rides a stage report, so the
// command's keepalive, re-sending "running", can never overwrite it.
//
// IT SURVIVES A RESTART. Turning Screen Recording on needs the worker to
// restart, mid-install, and the report used to live in memory only: the
// plan was lost, and the stages still to come vanished from the page until
// each one started. It is now kept in setup.json at the machine's state
// root, written on every report and read when the worker starts. A report
// read back from an installer that has since gone quiet is carried as
// "stopped" by the same rule as one that went quiet while this worker ran.
//
// THE CLUSTER HEARS A STEP AT ONCE. A report that changes a step -- the
// plan, a stage's state or sentence, an answer, the end -- asks every home
// for a heartbeat now (onStep) instead of at its next tick: fifteen seconds
// of the old step was long enough for a person to press Download models on
// a download already under way. A download's figures moving within a step
// keep the ordinary cadence.

// SetupControl is one report on the control socket. Exactly one of its
// parts is set.
type SetupControl struct {
	// Plan is the stages the installer will run, in its order. It starts a
	// new report: whatever an earlier install said is replaced.
	Plan []string `json:"plan,omitempty"`
	// Stage is one stage's latest state.
	Stage *SetupStageReport `json:"stage,omitempty"`
	// Finished is the installer saying it is done.
	Finished bool `json:"finished,omitempty"`
	// Answer is the person's choice for a stage waiting on them.
	Answer *SetupAnswer `json:"answer,omitempty"`
	// Reset is an install with no stage to run: what an earlier install
	// reported is no longer this machine's setup.
	Reset bool `json:"reset,omitempty"`
}

// SetupAnswer is the person's choice for a stage that waits on them. Only the
// permissions stage does, and its one choice is "skip": go on without what
// is not allowed yet. Allowing everything needs no answer -- the setup
// command sees the grant itself.
type SetupAnswer struct {
	Stage  string `json:"stage"`
	Choice string `json:"choice"`
}

// SetupStageReport is one stage's latest state.
type SetupStageReport struct {
	// Name is "permissions", "runtime", "models" or "workbench".
	Name string `json:"name"`
	// State is "waiting", "running", "done", "failed" or "skipped".
	State string `json:"state"`
	// Detail is the setup command's own sentence: what it is doing, or why
	// it stopped.
	Detail string `json:"detail,omitempty"`
	// The models stage only: the model downloading now, its bytes so far
	// and its whole size across every part (zero while unknown), and how
	// many of the stage's models are on the machine of how many it is
	// fetching.
	Model          string `json:"model,omitempty"`
	CompletedBytes uint64 `json:"completed_bytes,omitempty"`
	TotalBytes     uint64 `json:"total_bytes,omitempty"`
	ModelsDone     int    `json:"models_done,omitempty"`
	ModelsTotal    int    `json:"models_total,omitempty"`
}

// The closed vocabularies, the engine's own (component/worker/setup.go). A
// word outside them is dropped there, so it is refused here, where the
// command that sent it can still be told.
var (
	setupStageNames  = map[string]bool{"permissions": true, "runtime": true, "models": true, "workbench": true}
	setupStageStates = map[string]bool{"waiting": true, "running": true, "done": true, "failed": true, "skipped": true}
	// setupAnswers is the stages that wait on the person, and the choices
	// each one takes. This machine's own vocabulary: answers never leave it.
	setupAnswers = map[string]map[string]bool{"permissions": {"skip": true}}
)

// setupReportFile is the report's file at the machine's state root.
const setupReportFile = "setup.json"

const (
	// setupQuietAfter is how long a running report may go unrefreshed
	// before it is carried as stopped. Four keepalives' worth.
	setupQuietAfter = 2 * time.Minute
	// setupKeepalive is how often a setup command re-sends a running stage.
	setupKeepalive = 30 * time.Second
	// The engine's own bounds, applied here so the report it stores is the
	// one this worker sent.
	maxSetupDetailBytes = 512
	maxSetupModelBytes  = 256
)

// setupEndedWithoutResult is a running stage's detail when the installer
// finished without it saying how it ended.
const setupEndedWithoutResult = "This stage ended without reporting a result. The installer's terminal output says why."

func (c SetupControl) validate() error {
	parts := 0
	if len(c.Plan) > 0 {
		parts++
	}
	if c.Stage != nil {
		parts++
	}
	if c.Finished {
		parts++
	}
	if c.Answer != nil {
		parts++
	}
	if c.Reset {
		parts++
	}
	if parts != 1 {
		return errors.New("a setup report carries exactly one of plan, stage, finished, answer or reset")
	}
	seen := map[string]bool{}
	for _, name := range c.Plan {
		if !setupStageNames[name] {
			return fmt.Errorf("unknown setup stage %q", name)
		}
		if seen[name] {
			return fmt.Errorf("setup stage %q is planned twice", name)
		}
		seen[name] = true
	}
	if c.Stage != nil {
		if !setupStageNames[c.Stage.Name] {
			return fmt.Errorf("unknown setup stage %q", c.Stage.Name)
		}
		if !setupStageStates[c.Stage.State] {
			return fmt.Errorf("unknown setup stage state %q", c.Stage.State)
		}
	}
	if c.Answer != nil {
		choices, ok := setupAnswers[c.Answer.Stage]
		if !ok {
			return fmt.Errorf("setup stage %q does not wait for an answer", c.Answer.Stage)
		}
		if !choices[c.Answer.Choice] {
			return fmt.Errorf("unknown answer %q for setup stage %q", c.Answer.Choice, c.Answer.Stage)
		}
	}
	return nil
}

// setupTracker is the machine's one setup report. Every home's heartbeat
// carries the same one: an install sets up the machine, not a cluster.
type setupTracker struct {
	now func() time.Time
	// path is where the report is kept across restarts; "" keeps it in
	// memory only.
	path string
	// onStep is told, outside the lock, when a report changes a step.
	onStep func()

	mu       sync.Mutex
	present  bool
	finished bool
	stages   []SetupStageReport
	answers  map[string]SetupAnswer
	updated  time.Time
}

func newSetupTracker(now func() time.Time) *setupTracker {
	if now == nil {
		now = time.Now
	}
	return &setupTracker{now: now}
}

// loadSetupTracker is a tracker kept at path, starting from what an earlier
// worker saved there. A file it cannot read is a report it does not have:
// the next one from the installer replaces it.
func loadSetupTracker(now func() time.Time, path string) *setupTracker {
	t := newSetupTracker(now)
	t.path = path
	body, err := os.ReadFile(path)
	if err != nil {
		return t
	}
	var saved setupFile
	if json.Unmarshal(body, &saved) != nil {
		return t
	}
	for _, st := range saved.Stages {
		if setupStageNames[st.Name] && setupStageStates[st.State] {
			t.stages = append(t.stages, boundedStage(st))
		}
	}
	for stage, answer := range saved.Answers {
		if setupAnswers[stage][answer.Choice] && answer.Stage == stage {
			if t.answers == nil {
				t.answers = map[string]SetupAnswer{}
			}
			t.answers[stage] = answer
		}
	}
	t.present, t.finished, t.updated = true, saved.Finished, saved.Updated
	return t
}

// setupReportPath is the report's file for a worker whose state lives at
// stateDir, or "" when it has none.
func setupReportPath(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	return filepath.Join(machineStateRoot(stateDir), setupReportFile)
}

// setupFile is the report as setup.json keeps it.
type setupFile struct {
	Finished bool                   `json:"finished"`
	Stages   []SetupStageReport     `json:"stages"`
	Answers  map[string]SetupAnswer `json:"answers,omitempty"`
	Updated  time.Time              `json:"updated"`
}

// Apply records one report from the control socket.
func (t *setupTracker) Apply(c SetupControl) error {
	if t == nil {
		return errors.New("this worker keeps no setup report")
	}
	if err := c.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	step, err := t.applyLocked(c)
	if err == nil {
		t.saveLocked()
	}
	onStep := t.onStep
	t.mu.Unlock()
	if err == nil && step && onStep != nil {
		onStep()
	}
	return err
}

// applyLocked records the report and says whether it changed a step: the
// plan, a stage's state or sentence, an answer, or the end. A download's
// figures moving, and a running stage re-sent to say its command is alive,
// change none.
func (t *setupTracker) applyLocked(c SetupControl) (bool, error) {
	switch {
	case c.Answer != nil:
		// An answer is for a stage that is waiting on the person NOW. One
		// for a stage that has since ended -- allowed in the moment the
		// person chose to skip -- has nobody left to read it.
		waiting := false
		for _, st := range t.stages {
			if st.Name == c.Answer.Stage && st.State == "running" {
				waiting = true
			}
		}
		if !t.present || t.finished || !waiting {
			return false, errors.New("setup is not waiting for that answer")
		}
		if t.answers == nil {
			t.answers = map[string]SetupAnswer{}
		}
		t.answers[c.Answer.Stage] = *c.Answer
		return true, nil
	case c.Reset:
		if !t.present {
			return false, nil
		}
		// Finished with nothing in it, rather than no report at all: the
		// engine keeps the last report it stored until a beat replaces it,
		// and only a report replaces a report.
		t.finished, t.stages, t.answers, t.updated = true, nil, nil, t.now()
		return true, nil
	}
	t.present = true
	t.updated = t.now()
	switch {
	case len(c.Plan) > 0:
		t.finished = false
		t.answers = nil
		t.stages = make([]SetupStageReport, 0, len(c.Plan))
		for _, name := range c.Plan {
			t.stages = append(t.stages, SetupStageReport{Name: name, State: "waiting"})
		}
		return true, nil
	case c.Stage != nil:
		stage := boundedStage(*c.Stage)
		// A stage starting again after the installer finished -- somebody
		// running `worker setup --workbench` by hand later -- is setup
		// running again, and the page should say so.
		if stage.State == "running" {
			t.finished = false
		}
		for i := range t.stages {
			if t.stages[i].Name != stage.Name {
				continue
			}
			before := t.stages[i]
			// A stage STARTING asks its question again; the keepalive of
			// one already running keeps the answer it was given.
			if stage.State == "running" && before.State != "running" {
				delete(t.answers, stage.Name)
			}
			t.stages[i] = stage
			return before.State != stage.State || before.Detail != stage.Detail, nil
		}
		if stage.State == "running" {
			delete(t.answers, stage.Name)
		}
		t.stages = append(t.stages, stage)
		return true, nil
	default: // finished
		t.finished = true
		for i := range t.stages {
			switch t.stages[i].State {
			case "waiting":
				t.stages[i].State = "skipped"
			case "running":
				t.stages[i].State = "failed"
				t.stages[i].Detail = setupEndedWithoutResult
			}
		}
		return true, nil
	}
}

// saveLocked keeps the report for the next worker. Best-effort, like every
// other part of the report: a disk that refuses costs the report its
// survival of a restart, never the report.
func (t *setupTracker) saveLocked() {
	if t.path == "" || !t.present {
		return
	}
	body, err := json.Marshal(setupFile{Finished: t.finished, Stages: t.stages, Answers: t.answers, Updated: t.updated})
	if err != nil {
		return
	}
	dir := filepath.Dir(t.path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".setup-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return
	}
	if tmp.Close() != nil {
		return
	}
	_ = os.Rename(tmp.Name(), t.path)
}

// LocalSetup is the report as the control socket shows it: what the cluster
// is told, and the person's answers, which only this machine needs.
type LocalSetup struct {
	State   string                 `json:"state"`
	Stages  []SetupStageReport     `json:"stages"`
	Answers map[string]SetupAnswer `json:"answers,omitempty"`
}

// Local is the report for the control socket, or nil when there is none.
func (t *setupTracker) Local() *LocalSetup {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.present {
		return nil
	}
	local := &LocalSetup{State: t.stateLocked(), Stages: append([]SetupStageReport{}, t.stages...)}
	if len(t.answers) > 0 {
		local.Answers = make(map[string]SetupAnswer, len(t.answers))
		for stage, answer := range t.answers {
			local.Answers[stage] = answer
		}
	}
	return local
}

// Proto is the report for the next heartbeat, or nil when there is none.
func (t *setupTracker) Proto() *memqlv1.SetupProgress {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.present {
		return nil
	}
	p := &memqlv1.SetupProgress{State: t.stateLocked(), UpdatedAt: timestamppb.New(t.updated)}
	for _, st := range t.stages {
		p.Stages = append(p.Stages, &memqlv1.SetupStage{
			Name:           st.Name,
			State:          st.State,
			Detail:         st.Detail,
			Model:          st.Model,
			CompletedBytes: st.CompletedBytes,
			TotalBytes:     st.TotalBytes,
			ModelsDone:     uint32(st.ModelsDone),
			ModelsTotal:    uint32(st.ModelsTotal),
		})
	}
	return p
}

// stateLocked derives the report's state. Finished when the installer said
// so or when no stage is left to run; stopped when one is and nothing has
// been heard for setupQuietAfter.
func (t *setupTracker) stateLocked() string {
	if t.finished {
		return "finished"
	}
	pending := false
	for _, st := range t.stages {
		if st.State == "waiting" || st.State == "running" {
			pending = true
			break
		}
	}
	if !pending {
		return "finished"
	}
	if t.now().Sub(t.updated) > setupQuietAfter {
		return "stopped"
	}
	return "running"
}

// boundedStage holds a stage to the engine's bounds. The model figures are
// the models stage's alone; a model id too long to keep is dropped rather
// than cut into a different name.
func boundedStage(st SetupStageReport) SetupStageReport {
	st.Detail = clipText(strings.TrimSpace(st.Detail), maxSetupDetailBytes)
	if st.Name != "models" {
		st.Model, st.CompletedBytes, st.TotalBytes, st.ModelsDone, st.ModelsTotal = "", 0, 0, 0, 0
		return st
	}
	st.Model = strings.TrimSpace(st.Model)
	if len(st.Model) > maxSetupModelBytes {
		st.Model = ""
	}
	if st.TotalBytes > 0 && st.CompletedBytes > st.TotalBytes {
		st.CompletedBytes = st.TotalBytes
	}
	if st.ModelsTotal < 0 {
		st.ModelsTotal = 0
	}
	st.ModelsDone = max(0, min(st.ModelsDone, st.ModelsTotal))
	return st
}

// clipText bounds s to n bytes without splitting a character.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
