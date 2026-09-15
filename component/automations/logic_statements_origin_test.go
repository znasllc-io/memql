package automations

// A logic's statements run at the caller's origin (#2800 applied to the one
// execution model). Main's logic runner handed each statement the caller's
// context untouched; running a logic through runSequence put executeStep's
// SourceTrusted stamp in between, and a logic's own execution record was never
// trusted -- so accountDeletionSweep's @serverOnly read, called from a trusted
// automation, was refused as a client call.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// originProbe records the origin each step is executed at.
type originProbe struct {
	mu      sync.Mutex
	origins map[string]auth.CallOrigin
}

func (p *originProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	p.mu.Lock()
	p.origins[step.ID] = auth.OriginFromContext(ctx)
	p.mu.Unlock()
	now := time.Now()
	return &StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now, Status: "success"}, nil
}

func TestLogicStatementsRunAtTheCallersOrigin(t *testing.T) {
	name, body := compiledLogic(t, `logic readsServerOnly {
  rows := query serverOnlyRows()
  return rows
}`)
	for _, c := range []struct {
		label    string
		ctx      context.Context
		internal bool
	}{
		{"a trusted automation's call", auth.ContextWithInternalOrigin(context.Background()), true},
		{"a client's call", auth.ContextWithClientOrigin(context.Background()), false},
	} {
		probe := &originProbe{origins: map[string]auth.CallOrigin{}}
		r := NewLogicRunner(nil, probe, nil)
		r.noJournal = true
		if _, err := r.RunLogicBody(c.ctx, name, body, nil); err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		got, ran := probe.origins["rows"]
		if !ran {
			t.Fatalf("%s: the statement never reached the registry: %v", c.label, probe.origins)
		}
		if got.IsInternal() != c.internal {
			t.Errorf("%s: the statement ran at origin %v; want internal=%v", c.label, got, c.internal)
		}
	}
}
