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

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/core/num"
)

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
	chain := append([]Link(nil), c.Chain...)
	chain = append(chain, Link{Automation: automation, RunId: runId})
	return Cause{CausationId: runId, CorrelationId: correlationId, Depth: c.Depth + 1, Chain: chain}
}

// WithCause returns a copy of the event with c as its cause. A run's
// executor stamps this on every event it publishes -- through the engine, a
// step, a sub-automation -- so the chain that led here survives to whatever
// the event triggers next. Value receiver / return, like WithMetadata and
// WithPartition: the caller reassigns (event = event.WithCause(c)) rather
// than mutating a value another caller might still hold.
func (e Event) WithCause(c Cause) Event {
	e.Cause = c
	return e
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

// CauseFromMap reads a Cause back from its JSON shape: the object a run's
// journal records as triggerEvent.cause (component/automations' openRun), so
// that a resumed run, or a recovery that adopts the row, runs at the depth its
// first attempt did rather than as a new root. A nil or empty map is the zero
// cause -- a run that recorded none was triggered by a root event.
//
// The fields arrive as whatever decoded them: a depth is a float64 from
// encoding/json, a json.Number from a decoder that keeps numbers, an int from a
// Go caller. A chain entry that is not an object names no run, and is skipped
// rather than invented.
func CauseFromMap(m map[string]any) Cause {
	if len(m) == 0 {
		return Cause{}
	}
	c := Cause{Depth: causeDepth(m["depth"])}
	c.CausationId, _ = m["causationId"].(string)
	c.CorrelationId, _ = m["correlationId"].(string)
	switch chain := m["chain"].(type) {
	case []any:
		for _, entry := range chain {
			if link, ok := entry.(map[string]any); ok {
				c.Chain = append(c.Chain, linkFromMap(link))
			}
		}
	case []map[string]any:
		for _, link := range chain {
			c.Chain = append(c.Chain, linkFromMap(link))
		}
	}
	return c
}

func linkFromMap(m map[string]any) Link {
	var l Link
	l.Automation, _ = m["automation"].(string)
	l.RunId, _ = m["runId"].(string)
	return l
}

// causeDepth narrows a decoded depth to an int.
//
// narrowing: SATURATE -- a depth orders a chain against the cap, so a value too
// large for an int must stay larger than every cap: saturated, it is refused;
// wrapped, it would be admitted as a small one. A negative depth is no depth,
// and reads as the root's 0.
func causeDepth(v any) int {
	d := 0
	switch n := v.(type) {
	case float64:
		d = num.ClampFloat64(n)
	case int:
		d = n
	case int64:
		d = num.ClampInt64(n)
	case int32:
		d = int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			d = num.ClampInt64(i)
		} else if f, err := n.Float64(); err == nil {
			d = num.ClampFloat64(f)
		}
	}
	if d < 0 {
		return 0
	}
	return d
}
