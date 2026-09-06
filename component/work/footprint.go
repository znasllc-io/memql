package work

// footprint.go -- effects are declared once, on the capability (design
// record docs/superpowers/specs/2026-09-05-work-spine-design.md, section B
// "Footprint is declared once, on the capability").
//
// A builtin declares its effects; a mutation's effect is its concept; a
// query has none. A STEP's expected footprint is the union over what it
// calls, walked transitively, and the safety gate reads that union before
// a side effect. This is the typed-action claim of the ontology papers:
// what a step may touch is a property of the capability, stated once,
// rather than a guess made at the call site every time.
//
// THE UNION IS SORTED AND DEDUPLICATED, and that is not cosmetic. The
// union is written onto v1:work:step.expectedFootprint, so an
// order-dependent result would make the same step write a different row
// on every run, and a diff of two runs would show changes that are not
// changes.

import "sort"

// Footprint is what a capability may touch.
type Footprint struct {
	// Concepts are the graph concepts written.
	Concepts []string `json:"concepts,omitempty"`
	// Files is set when the capability writes a filesystem.
	Files bool `json:"files,omitempty"`
	// Machine is set when it acts on somebody's machine.
	Machine bool `json:"machine,omitempty"`
	// External is set when it leaves the cluster.
	External bool `json:"external,omitempty"`
	// Spend is set when it costs money.
	Spend bool `json:"spend,omitempty"`
}

// IsSideEffect reports whether this footprint touches anything at all.
// The safety gate runs on exactly this predicate, so a capability that
// declares nothing is passed through -- which is why a builtin that DOES
// touch something and forgets to declare it is a hole, and why @effects
// is validated at load rather than trusted.
func (f Footprint) IsSideEffect() bool {
	return len(f.Concepts) > 0 || f.Files || f.Machine || f.External || f.Spend
}

// merge folds other into f.
func (f *Footprint) merge(other Footprint) {
	f.Concepts = append(f.Concepts, other.Concepts...)
	f.Files = f.Files || other.Files
	f.Machine = f.Machine || other.Machine
	f.External = f.External || other.External
	f.Spend = f.Spend || other.Spend
}

// UnionFootprint walks the call graph from each name and unions every
// declared effect it reaches. Cycle-safe, and a name absent from the
// registry contributes nothing (its resolution is the loader's error to
// raise, not this function's to guess at).
func UnionFootprint(names []string, reg Registry) Footprint {
	seen := map[string]bool{}
	out := Footprint{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		t, ok := reg[n]
		if !ok {
			return
		}
		switch t.ConstructKind {
		case ConstructMutation:
			if t.Concept != "" {
				out.Concepts = append(out.Concepts, t.Concept)
			}
		case ConstructQuery, ConstructShape, ConstructSpec:
			// A read has no effect. Stated rather than defaulted so the
			// reader can see it was decided.
		default:
			out.merge(t.Effects)
		}
		for _, c := range t.Calls {
			walk(c)
		}
	}
	for _, n := range names {
		walk(n)
	}
	out.Concepts = sortedUnique(out.Concepts)
	return out
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
