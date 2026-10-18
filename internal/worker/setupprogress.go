package worker

import (
	"errors"
	"fmt"
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
// It lives in memory only. A worker restart mid-install loses the plan, and
// the next stage report rebuilds what is left; by then the earlier stages'
// results are on the registration anyway (an installed runtime, pulled
// models), which is what the OS reads them from.

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
}

// SetupStageReport is one stage's latest state.
type SetupStageReport struct {
	// Name is "runtime", "models" or "workbench".
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
	setupStageNames  = map[string]bool{"runtime": true, "models": true, "workbench": true}
	setupStageStates = map[string]bool{"waiting": true, "running": true, "done": true, "failed": true, "skipped": true}
)

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
	if parts != 1 {
		return errors.New("a setup report carries exactly one of plan, stage or finished")
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
	return nil
}

// setupTracker is the machine's one setup report. Every home's heartbeat
// carries the same one: an install sets up the machine, not a cluster.
type setupTracker struct {
	now func() time.Time

	mu       sync.Mutex
	present  bool
	finished bool
	stages   []SetupStageReport
	updated  time.Time
}

func newSetupTracker(now func() time.Time) *setupTracker {
	if now == nil {
		now = time.Now
	}
	return &setupTracker{now: now}
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
	defer t.mu.Unlock()
	t.present = true
	t.updated = t.now()
	switch {
	case len(c.Plan) > 0:
		t.finished = false
		t.stages = make([]SetupStageReport, 0, len(c.Plan))
		for _, name := range c.Plan {
			t.stages = append(t.stages, SetupStageReport{Name: name, State: "waiting"})
		}
	case c.Stage != nil:
		stage := boundedStage(*c.Stage)
		// A stage starting again after the installer finished -- somebody
		// running `worker setup --workbench` by hand later -- is setup
		// running again, and the page should say so.
		if stage.State == "running" {
			t.finished = false
		}
		for i := range t.stages {
			if t.stages[i].Name == stage.Name {
				t.stages[i] = stage
				return nil
			}
		}
		t.stages = append(t.stages, stage)
	case c.Finished:
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
	}
	return nil
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
