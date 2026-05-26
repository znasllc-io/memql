package safety

import "context"

// FanoutRecorder composes multiple DecisionRecorders into one that
// calls each in order. The recorder contract is "best-effort; never
// block the dispatch path", so a panic from a misconfigured sink
// (nil pointer, bad backing store) gets recovered + swallowed
// per-sink and the remaining recorders still run.
//
// Production use case: app boot fans out to both
// SlogRecorder (operator logs) AND a PersistingRecorder
// (v1:safety:classification rows). Tests can use the fanout to
// observe what a Gate would record without touching real backing
// stores -- pass a tiny in-memory recorder alongside the production
// ones.
//
// Order matters only for log readability -- the slog sink lands the
// line BEFORE the persistence attempt, so an operator grep sees the
// decision before any persistence error.
type FanoutRecorder struct {
	recorders []DecisionRecorder
}

// NewFanoutRecorder returns a FanoutRecorder over `recorders` in the
// given order. Nil entries are dropped (so callers can pass an
// optional recorder without checking). A zero-recorder fanout is a
// no-op, which matches the "best-effort" contract.
func NewFanoutRecorder(recorders ...DecisionRecorder) FanoutRecorder {
	out := make([]DecisionRecorder, 0, len(recorders))
	for _, r := range recorders {
		if r != nil {
			out = append(out, r)
		}
	}
	return FanoutRecorder{recorders: out}
}

// Record dispatches to every contained recorder. Each call is
// individually recovered from panic so a misconfigured sink can't
// crash the dispatch path or starve later sinks. Panic information
// is intentionally not logged here -- a panicky recorder is broken
// at the test bench, and the slog sink (which is panic-safe today)
// already captures the data the operator needs.
func (f FanoutRecorder) Record(ctx context.Context, desc ActionDescriptor, cls Classification, decision Decision, mode Mode) {
	for _, r := range f.recorders {
		callOneRecorder(r, ctx, desc, cls, decision, mode)
	}
}

// callOneRecorder runs one recorder with a per-call recover. Split
// out so the inline recover() captures only that one call's frame,
// not the whole loop.
func callOneRecorder(r DecisionRecorder, ctx context.Context, desc ActionDescriptor, cls Classification, decision Decision, mode Mode) {
	defer func() { _ = recover() }()
	r.Record(ctx, desc, cls, decision, mode)
}
