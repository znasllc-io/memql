package automations

import memqldsl "github.com/znasllc-io/memql/dsl"

// engineNamesWin keeps every automation of the engine's own tree, and every
// other automation whose name is not one of the engine's; it answers the ones
// it left out separately, in the order they came.
//
// The order of in does not matter: an engine automation keeps its name whether
// it was read before or after the automation that carries the same one.
// Automations of two other layers that share a name are both kept, as before.
func engineNamesWin(in []*Automation) (kept, leftOut []*Automation) {
	engine := make(map[string]struct{}, len(in))
	for _, a := range in {
		if a != nil && a.Source == memqldsl.SourceEngine {
			engine[a.Name] = struct{}{}
		}
	}
	kept = make([]*Automation, 0, len(in))
	for _, a := range in {
		if a == nil {
			continue
		}
		if _, taken := engine[a.Name]; taken && a.Source != memqldsl.SourceEngine {
			leftOut = append(leftOut, a)
			continue
		}
		kept = append(kept, a)
	}
	return kept, leftOut
}
