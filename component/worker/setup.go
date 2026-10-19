package worker

import (
	"strings"
	"time"
	"unicode/utf8"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// What a machine's installer is doing (memql#6229).
//
// ===========================================================================
// THE INSTALLER KEEPS GOING AFTER THE MACHINE CONNECTS
// ===========================================================================
// The add-machine wizard's command connects the worker first and only then
// installs the model runtime, downloads the models and sets up workbenches.
// Those stages run in a terminal on the machine, and the model pull goes
// straight to the runtime rather than through ModelPullStart, so nothing about
// them reached the cluster: an install still downloading 18 GB read, from
// here, exactly like a machine with no models, and the wizard offered to
// download them again.
//
// The cockpit now relays the installer's own account on the heartbeat. It is
// stored as the machine said it and decides nothing here: no routing, no
// admission. It exists so the OS can say what the machine is doing.

// Setup is a machine's latest setup report. The zero value is NO REPORT,
// which is what every cockpit that predates the field sends.
type Setup struct {
	// State is "running", "finished" or "stopped".
	State  string
	Stages []SetupStage
	// UpdatedAt is when the installer last spoke, on the MACHINE's clock;
	// zero when it did not say.
	UpdatedAt time.Time
	// ReportedAt is when this replica received the report, on its own clock.
	// A reader judges whether a "running" report is still being refreshed by
	// this, never by the machine's clock.
	ReportedAt time.Time
}

// SetupStage is one stage of the installer.
type SetupStage struct {
	// Name is "permissions", "runtime", "models" or "workbench".
	// "permissions" is the person's turn on a computer-use Mac: the installer
	// waits for them to allow MemQL, or to go on without it, before anything
	// downloads (memql#6235).
	Name string
	// State is "waiting", "running", "done", "failed" or "skipped".
	State string
	// Detail is the installer's own sentence for the stage, bounded.
	Detail string
	// The models stage only: the model downloading now, its bytes so far and
	// its whole size (zero while unknown), and how many of the stage's models
	// are on the machine of how many it is fetching.
	Model          string
	CompletedBytes uint64
	TotalBytes     uint64
	ModelsDone     int
	ModelsTotal    int
}

// The closed vocabularies. A word outside them is a cockpit this engine does
// not understand, and guessing what it meant would put a wrong sentence in
// front of somebody watching an install.
var (
	setupStates      = map[string]bool{"running": true, "finished": true, "stopped": true}
	setupStageNames  = map[string]bool{"permissions": true, "runtime": true, "models": true, "workbench": true}
	setupStageStates = map[string]bool{"waiting": true, "running": true, "done": true, "failed": true, "skipped": true}
)

// Bounds on what one report may carry. It is a cockpit's word about itself,
// so nothing in it is trusted to be small.
const (
	maxSetupDetailLen = 512
	maxSetupModelLen  = 256
	// A model count past this is a corrupt report, not a recommendation.
	maxSetupModels = 64
)

// SetupFromProto reads a cockpit's setup report, stamped with when this
// replica received it.
//
// A malformed report is NO REPORT rather than a refusal: it explains an
// install to a person and steers nothing, so it is not worth a live stream.
// Within a well-formed report a stage with an unknown name or state is
// dropped, and a repeated stage keeps its first appearance.
func SetupFromProto(p *memqlv1.SetupProgress, receivedAt time.Time) Setup {
	if p == nil {
		return Setup{}
	}
	state := strings.TrimSpace(p.GetState())
	if !setupStates[state] {
		return Setup{}
	}
	s := Setup{State: state, ReportedAt: receivedAt.UTC()}
	if ts := p.GetUpdatedAt(); ts != nil && ts.IsValid() {
		s.UpdatedAt = ts.AsTime().UTC()
	}
	seen := map[string]bool{}
	for _, st := range p.GetStages() {
		name := strings.TrimSpace(st.GetName())
		stageState := strings.TrimSpace(st.GetState())
		if !setupStageNames[name] || !setupStageStates[stageState] || seen[name] {
			continue
		}
		seen[name] = true
		stage := SetupStage{
			Name:   name,
			State:  stageState,
			Detail: clipSetupText(st.GetDetail(), maxSetupDetailLen),
		}
		if name == "models" {
			// A model id is an exact name the router selects on, so one too
			// long to keep is dropped rather than cut into a different name.
			if model := strings.TrimSpace(st.GetModel()); len(model) <= maxSetupModelLen {
				stage.Model = model
			}
			stage.CompletedBytes, stage.TotalBytes = st.GetCompletedBytes(), st.GetTotalBytes()
			// More fetched than the whole is two observations of a moving
			// download, not progress past the end.
			if stage.TotalBytes > 0 && stage.CompletedBytes > stage.TotalBytes {
				stage.CompletedBytes = stage.TotalBytes
			}
			stage.ModelsTotal = min(int(st.GetModelsTotal()), maxSetupModels)
			stage.ModelsDone = min(int(st.GetModelsDone()), stage.ModelsTotal)
		}
		s.Stages = append(s.Stages, stage)
	}
	return s
}

// SameSteps reports whether two reports say the same thing about every step:
// the same state, and each stage in the same state with the same sentence. A
// download's figures and the timestamps are progress within a step, not a
// new one.
func (s Setup) SameSteps(o Setup) bool {
	if s.State != o.State || len(s.Stages) != len(o.Stages) {
		return false
	}
	for i := range s.Stages {
		a, b := s.Stages[i], o.Stages[i]
		if a.Name != b.Name || a.State != b.State || a.Detail != b.Detail {
			return false
		}
	}
	return true
}

// setupStepFlushFloor is how soon after one write a changed step may write
// again ahead of the batch interval (memql#6235). Under the cockpit's own
// one-second floor between the beats it sends for a step, so jitter on the
// wire never holds a step back for a whole interval.
const setupStepFlushFloor = 500 * time.Millisecond

// Present reports whether the machine sent a setup report.
func (s Setup) Present() bool { return s.State != "" }

// Row is the report as `registration.setup` stores it, nil when there is
// none -- which the store reads as "leave the stored report alone".
func (s Setup) Row() map[string]any {
	if !s.Present() {
		return nil
	}
	stages := make([]any, 0, len(s.Stages))
	for _, st := range s.Stages {
		entry := map[string]any{"name": st.Name, "state": st.State}
		if st.Detail != "" {
			entry["detail"] = st.Detail
		}
		if st.Name == "models" {
			entry["model"] = st.Model
			entry["completedBytes"] = st.CompletedBytes
			entry["totalBytes"] = st.TotalBytes
			entry["modelsDone"] = st.ModelsDone
			entry["modelsTotal"] = st.ModelsTotal
		}
		stages = append(stages, entry)
	}
	row := map[string]any{
		"state":      s.State,
		"stages":     stages,
		"reportedAt": s.ReportedAt.UTC().Format(time.RFC3339Nano),
	}
	if !s.UpdatedAt.IsZero() {
		row["updatedAt"] = s.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return row
}

// SetSetup replaces what the machine's installer last reported.
func (w *Worker) SetSetup(s Setup) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setup = s
}

// Setup returns what the machine's installer last reported.
func (w *Worker) Setup() Setup {
	if w == nil {
		return Setup{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.setup
}

// clipSetupText trims s and bounds it to n bytes without splitting a
// character: the detail is somebody's sentence, and half a rune at its end
// would render as a replacement glyph on the page that quotes it.
func clipSetupText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
