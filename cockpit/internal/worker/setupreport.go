package worker

import (
	"errors"
	"sync"
	"time"
)

// setupReporter is a setup command's side of the setup report
// (setupprogress.go): it tells the running worker what this command is
// doing, so the worker can carry it to the cluster.
//
// IT NEVER STANDS IN THE WAY OF THE SETUP. Every send is best-effort: no
// worker running, an older one that does not know the action, a socket that
// refuses -- the command carries on exactly as it would have, and says
// nothing about it. A setup that failed because its progress could not be
// reported would be the report causing the failure it exists to explain.
//
// A download's progress arrives many times a second on the goroutine reading
// the runtime's stream, so Pull only records it; a loop sends the latest one
// at most every setupReportEvery. A change of state is sent at once.
type setupReporter struct {
	send  func(SetupControl) error
	every time.Duration

	mu       sync.Mutex
	stages   map[string]SetupStageReport
	order    []string
	pending  bool
	lastSent time.Time
	now      func() time.Time

	quit chan struct{}
	done chan struct{}
}

// setupReportEvery bounds how often a download's progress is sent.
const setupReportEvery = 2 * time.Second

// newSetupReporter starts a reporter. A nil send makes every method a no-op.
func newSetupReporter(send func(SetupControl) error) *setupReporter {
	return startSetupReporter(send, setupReportEvery)
}

func startSetupReporter(send func(SetupControl) error, every time.Duration) *setupReporter {
	r := &setupReporter{send: send, every: every, stages: map[string]SetupStageReport{}, now: time.Now}
	if send == nil {
		return r
	}
	r.quit = make(chan struct{})
	r.done = make(chan struct{})
	go r.loop()
	return r
}

// Stage records a stage's state and sends it now. Model figures already
// recorded for the stage are kept, so a finished models stage still says how
// many it fetched.
func (r *setupReporter) Stage(name, state, detail string) {
	if r == nil || r.send == nil {
		return
	}
	r.mu.Lock()
	st, ok := r.stages[name]
	if !ok {
		r.order = append(r.order, name)
	}
	st.Name, st.State, st.Detail = name, state, detail
	if name == "models" && state == "done" && st.ModelsTotal > 0 {
		st.ModelsDone = st.ModelsTotal
	}
	r.stages[name] = st
	r.pending = false
	r.mu.Unlock()
	r.sendStage(st)
}

// Pull records a download's progress on the models stage. It never blocks:
// the loop sends the latest figures.
func (r *setupReporter) Pull(model string, completed, total uint64, done, of int) {
	if r == nil || r.send == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stages["models"]
	if !ok {
		r.order = append(r.order, "models")
	}
	st.Name, st.State, st.Detail = "models", "running", ""
	st.Model, st.CompletedBytes, st.TotalBytes = model, completed, total
	st.ModelsDone, st.ModelsTotal = done, of
	r.stages["models"] = st
	r.pending = true
}

// Close sends what is still unsent and stops the loop.
func (r *setupReporter) Close() {
	if r == nil || r.send == nil {
		return
	}
	close(r.quit)
	<-r.done
}

func (r *setupReporter) loop() {
	defer close(r.done)
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		select {
		case <-r.quit:
			r.flush(true)
			return
		case <-tick.C:
			r.flush(false)
		}
	}
}

// flush sends the models stage when its figures moved, and re-sends every
// running stage once setupKeepalive has passed without a send -- which is
// what tells the worker this command is still alive.
func (r *setupReporter) flush(final bool) {
	r.mu.Lock()
	var out []SetupStageReport
	keepalive := r.now().Sub(r.lastSent) >= setupKeepalive
	for _, name := range r.order {
		st := r.stages[name]
		switch {
		case name == "models" && r.pending:
			out = append(out, st)
		case st.State == "running" && keepalive && !final:
			out = append(out, st)
		}
	}
	r.pending = false
	r.mu.Unlock()
	for _, st := range out {
		r.sendStage(st)
	}
}

func (r *setupReporter) sendStage(st SetupStageReport) {
	stage := st
	_ = r.send(SetupControl{Stage: &stage})
	r.mu.Lock()
	r.lastSent = r.now()
	r.mu.Unlock()
}

// sendSetupControl hands one report to the running worker over its control
// socket.
func sendSetupControl(c SetupControl) error {
	response, err := callControl(defaultControlPath(), controlRequest{Action: "setup", Setup: &c})
	if err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Error)
	}
	return nil
}
