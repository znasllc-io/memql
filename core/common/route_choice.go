package common

// route_choice.go -- a PERSON'S routing choice for a whole run: the Ask route
// picker's Source and Level (design brief "routing in MemQL OS", section 6).
//
// WHY IT RIDES THE RUN ROW AND NOT THE CONTEXT OF THE REQUEST. An Ask turn is
// a goal: the bff takes the turn, a planner compiles it, an agent runs its
// steps -- three nodes, none of which shares memory with another. A value put
// on the handler's context (the old engine.WithProviderOverride) reached none
// of the calls that mattered. So the choice is written onto v1:work:run.routing
// when the run opens, and every node that executes the run stamps it onto the
// RunContext it builds FROM THAT ROW: integrations/work's compile claim on the
// planner, app/'s workExecutionContext on the agent. The model seam reads it
// here and nowhere else.
//
// It is NOT in the goal's input. The input is what the model sees; which model
// reads it is not the model's business.
//
// The shape is plain strings, declared in this package for the reason
// StepOverride is: the model seam (component/memql) and the router read it off
// the context, and neither may import the work integration that writes it.
// component/memql.ParseRouteChoice is the grammar; this is only the carrier.

import "strings"

// RouteChoice is the choice itself. The zero value is AUTO: the rules decide
// every call, at every call's own level.
type RouteChoice struct {
	// Source is where the run's model calls are served: "" (the rules
	// decide), a single source the person PINNED -- app:<id>, app:*,
	// fleet:strongest, fleet:fastest, fleet:<modelId>, federation:cheapest,
	// federation:strongest -- or a ROUTE, policy:<name>, whose chain is walked
	// instead of the one a rule would pick.
	Source string
	// Level is "" (each call keeps its own) or fast, strong or reasoning: the
	// level the run's STEPS ask for. Compile-time calls keep their own.
	Level string
	// By is the person who made the choice -- the goal's owner, stamped by
	// the server from the caller's identity and never taken from an argument.
	// It is what makes a pinned app door the OWNER'S pin at the app gate.
	By string
}

// IsZero reports whether the choice changes nothing: Auto source, Auto level.
func (c RouteChoice) IsZero() bool {
	return strings.TrimSpace(c.Source) == "" && strings.TrimSpace(c.Level) == ""
}

// Map is the row form written to v1:work:goal.routing and v1:work:run.routing.
// Auto writes nothing (nil), so a run nobody chose for reads exactly like
// every run written before the field existed.
func (c RouteChoice) Map() map[string]any {
	if c.IsZero() {
		return nil
	}
	out := map[string]any{}
	if v := strings.TrimSpace(c.Source); v != "" {
		out["source"] = v
	}
	if v := strings.TrimSpace(c.Level); v != "" {
		out["level"] = v
	}
	if v := strings.TrimSpace(c.By); v != "" {
		out["by"] = v
	}
	return out
}

// RouteChoiceFrom reads the row form back. Anything that is not an object --
// an absent field, an older row, a value somebody wrote by hand -- reads as
// Auto, and so does a field of the wrong type: a choice that cannot be read is
// no choice, never a guess at one.
func RouteChoiceFrom(v any) RouteChoice {
	m, ok := v.(map[string]any)
	if !ok {
		return RouteChoice{}
	}
	text := func(key string) string {
		s, _ := m[key].(string)
		return strings.TrimSpace(s)
	}
	c := RouteChoice{Source: text("source"), Level: text("level"), By: text("by")}
	if c.IsZero() {
		return RouteChoice{}
	}
	return c
}
