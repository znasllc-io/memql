package events

// cause.go -- an event's causal lineage (D19 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md,
// epic memql#5380).
//
// Every event an automation run publishes or causes carries the run that
// caused it, the chain that run belongs to and how deep that chain is. The
// executor reads the triggering event's cause when a run starts, refuses a run
// whose chain has grown past the cap, and puts the run's own cause into the
// context its steps run under; every publisher that has the context stamps it
// on what it publishes. That is how a loop that escapes the load-time graph is
// still stopped at run time, on every replica: the mesh carries the cause on
// the hop (component/node's EventForward), because a depth that reset to zero
// at every node boundary would let a two-node ping-pong run forever.

import "context"

// Cause is an event's causal lineage: the automation run whose work published
// it, the chain it belongs to, and how deep that chain is. The zero value is a
// ROOT event -- a person's write, a cron tick, anything no automation run
// caused.
type Cause struct {
	// CausationId is the run id of the automation run that caused the event.
	CausationId string `json:"causationId,omitempty"`
	// CorrelationId is the chain's id, shared by every event and run one root
	// cause led to.
	CorrelationId string `json:"correlationId,omitempty"`
	// Depth is how many automation runs stand between the root cause and this
	// event. 0 for a root event.
	Depth int `json:"depth,omitempty"`
	// Chain names those runs, oldest first. It is never longer than the depth
	// cap, because a run past the cap is refused before it can extend it.
	Chain []Link `json:"chain,omitempty"`
}

// Link is one run in a chain.
type Link struct {
	Automation string `json:"automation"`
	RunId      string `json:"runId"`
}

// IsZero reports whether no run caused the event.
func (c Cause) IsZero() bool {
	return c.CausationId == "" && c.CorrelationId == "" && c.Depth == 0 && len(c.Chain) == 0
}

// Clone returns a copy whose Chain shares no backing array with c, so a
// subscriber that appends to its copy cannot reach into another's.
func (c Cause) Clone() Cause {
	if c.Chain != nil {
		c.Chain = append([]Link(nil), c.Chain...)
	}
	return c
}

// Next is the cause a run carries: the parent's chain plus this run, one
// deeper, with this run as the causation of everything it publishes.
func (c Cause) Next(automation, runId, correlationId string) Cause {
	chain := make([]Link, 0, len(c.Chain)+1)
	chain = append(chain, c.Chain...)
	chain = append(chain, Link{Automation: automation, RunId: runId})
	return Cause{CausationId: runId, CorrelationId: correlationId, Depth: c.Depth + 1, Chain: chain}
}

type causeKey struct{}

// ContextWithCause returns ctx carrying c. The executor stamps the run's cause
// here so that every write and publish the run makes -- through the engine,
// a step, a sub-automation -- carries it without a signature having to.
func ContextWithCause(ctx context.Context, c Cause) context.Context {
	return context.WithValue(ctx, causeKey{}, c.Clone())
}

// CauseFromContext returns the cause ctx carries. ok is false when there is
// none, or when it is the zero cause: a zero cause stamped on an event would
// read as a root anyway, so a caller never needs to tell the two apart.
func CauseFromContext(ctx context.Context) (Cause, bool) {
	if ctx == nil {
		return Cause{}, false
	}
	c, ok := ctx.Value(causeKey{}).(Cause)
	if !ok || c.IsZero() {
		return Cause{}, false
	}
	return c.Clone(), true
}
