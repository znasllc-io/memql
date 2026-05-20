// Package provenance defines the engine-stamped "where did this row
// come from" intrinsic that every v1:* row carries. Sits alongside
// createdBy (who) and createdAt (when) as load-bearing metadata the
// caller cannot set directly: the engine sets it from the writing
// goroutine's Go context at insert time.
//
// The intrinsic is per-version. memql is time-series; every insert
// stamps fresh provenance, so a row updated by a UI mutation at v2
// after being created by a seed at v1 has both attributions preserved
// in history.
//
// Six kinds cover every observable write origin (closed enum --
// adding a new category is a deliberate engine change, not a free-
// text decision at every call site):
//
//	seed       — the SeedMaterializer's startup or runtime materialization
//	mutation   — a DSL mutation invoked from a caller (cockpit, web UI,
//	             CLI) without higher-level intent
//	automation — an automation step issuing a mutation in response to
//	             an event
//	direct     — engine-internal / framework-level inserts that don't
//	             go through a named DSL mutation
//	system     — engine startup / bootstrap (default partition, etc.)
//	migration  — explicit data migrations from `memqlmigrate` / tools
//
// If you hit a write path that doesn't fit one of those, that's a
// signal we need to consciously add a category -- not silently smear
// it across an existing one.
//
// Threading: every originating context wraps its ctx with
// ContextWithProvenance(ctx, ...) before issuing the write. The
// mutation executor reads FromContext at row-insert time and rejects
// writes whose context is empty (genesis-mode: no silent defaults,
// every write path must explicitly declare its intent).
package provenance

import (
	"context"
	"fmt"
	"strings"
)

// Kind is the closed-enum category of write origins. Engine-stamped
// at insert time from the Go context's Provenance value.
type Kind string

const (
	KindSeed       Kind = "seed"
	KindMutation   Kind = "mutation"
	KindAutomation Kind = "automation"
	KindDirect     Kind = "direct"
	KindSystem     Kind = "system"
	KindMigration  Kind = "migration"
)

// validKinds is the exhaustive set; Provenance.Validate uses it.
var validKinds = map[Kind]bool{
	KindSeed:       true,
	KindMutation:   true,
	KindAutomation: true,
	KindDirect:     true,
	KindSystem:     true,
	KindMigration:  true,
}

// Provenance is the engine-stamped intrinsic. Mirrors the proto
// message + the row column shape; one canonical Go type all layers
// use.
type Provenance struct {
	// Kind is the originating category. Required; rejected at insert
	// if empty or not in the closed enum.
	Kind Kind `json:"kind"`

	// Name is the originating-thing's identifier (seed name, mutation
	// name, automation name, bootstrap routine name, migration tool
	// name). Required; the cockpit's source-link lookup keys off of
	// this.
	Name string `json:"name"`

	// Trigger is the event topic that fired the originating
	// automation (set when Kind=automation). Empty otherwise.
	Trigger string `json:"trigger,omitempty"`

	// Via is the mutation actually executed by the engine. Set by
	// the engine itself, NOT by the caller -- if an automation calls
	// `mutationCreateAgent`, Via is `mutationCreateAgent` and the
	// automation's intent is captured in {Kind, Name, Trigger}.
	Via string `json:"via,omitempty"`
}

// Validate returns an error if the provenance is missing required
// fields or carries an unknown kind. Called by the mutation executor
// before stamping the value onto a row.
func (p Provenance) Validate() error {
	if p.Kind == "" {
		return fmt.Errorf("provenance: kind is required")
	}
	if !validKinds[p.Kind] {
		return fmt.Errorf("provenance: unknown kind %q (must be seed|mutation|automation|direct|system|migration)", p.Kind)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("provenance: name is required")
	}
	return nil
}

// IsZero reports whether no provenance was attached.
func (p Provenance) IsZero() bool {
	return p.Kind == "" && p.Name == "" && p.Trigger == "" && p.Via == ""
}

// String renders the provenance compactly for log output.
//
//	"seed:assistant via=mutationCreateAgent"
//	"automation:reRouteNeedsAgentOnAgentCreate trigger=graph.node.created.*.v1:agents:agent via=mutationUpdatePlanStatus"
//	"direct:mutationCreateAgent"
//	"system:conceptSeeder:v1:cluster:nodeType"
func (p Provenance) String() string {
	if p.IsZero() {
		return "<none>"
	}
	var b strings.Builder
	b.WriteString(string(p.Kind))
	b.WriteByte(':')
	b.WriteString(p.Name)
	if p.Trigger != "" {
		b.WriteString(" trigger=")
		b.WriteString(p.Trigger)
	}
	if p.Via != "" {
		b.WriteString(" via=")
		b.WriteString(p.Via)
	}
	return b.String()
}

// -----------------------------------------------------------------
// Context plumbing -- the only legitimate way to set provenance.
// -----------------------------------------------------------------

type contextKey struct{}

// ContextWithProvenance returns ctx with the given Provenance value
// attached. Every originating write context must call this before
// issuing the mutation, or the engine will reject the insert with
// "no provenance in context".
//
// nil ctx returns nil; zero-value Provenance returns ctx unchanged
// (callers shouldn't pass zero -- if you can't construct a meaningful
// Provenance, you shouldn't be writing).
func ContextWithProvenance(ctx context.Context, p Provenance) context.Context {
	if ctx == nil {
		return nil
	}
	if p.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, p)
}

// WithVia returns a copy of p with Via set to the given mutation
// name. The engine calls this internally at insert time -- callers
// supply Kind+Name+Trigger, the engine fills Via from "what mutation
// am I actually executing right now."
func (p Provenance) WithVia(mutationName string) Provenance {
	out := p
	out.Via = mutationName
	return out
}

// FromContext returns the Provenance attached to ctx, or the zero
// value if none was set. The mutation executor inspects this and
// rejects writes with a zero-value result.
func FromContext(ctx context.Context) Provenance {
	if ctx == nil {
		return Provenance{}
	}
	v, _ := ctx.Value(contextKey{}).(Provenance)
	return v
}

// -----------------------------------------------------------------
// Convenience constructors -- shorthand for the common cases so
// callers don't keep re-spelling the shape.
// -----------------------------------------------------------------

// Seed stamps a {Kind: seed, Name: seedName} provenance. Used by the
// SeedMaterializer for every row it produces.
func Seed(seedName string) Provenance {
	return Provenance{Kind: KindSeed, Name: seedName}
}

// Mutation stamps a {Kind: mutation, Name: mutationName} provenance.
// Used by direct DSL-mutation callers that don't have a higher-level
// intent (cockpit / web UI / CLI).
func Mutation(mutationName string) Provenance {
	return Provenance{Kind: KindMutation, Name: mutationName}
}

// Automation stamps {Kind: automation, Name: autoName, Trigger: evt}.
// Used by the automation step runner when an automation issues a
// mutation in response to an event.
func Automation(autoName, trigger string) Provenance {
	return Provenance{Kind: KindAutomation, Name: autoName, Trigger: trigger}
}

// Direct stamps {Kind: direct, Name: name}. Used by engine-internal /
// framework-level inserts that don't go through a named DSL mutation
// (cache rows, concept bootstrap writes, etc.).
func Direct(name string) Provenance {
	return Provenance{Kind: KindDirect, Name: name}
}

// System stamps {Kind: system, Name: bootstrapName}. Used by engine
// startup routines (default partition seed, system secrets, etc.).
func System(bootstrapName string) Provenance {
	return Provenance{Kind: KindSystem, Name: bootstrapName}
}

// Migration stamps {Kind: migration, Name: toolName}. Used by
// memqlmigrate and sibling tools.
func Migration(toolName string) Provenance {
	return Provenance{Kind: KindMigration, Name: toolName}
}
