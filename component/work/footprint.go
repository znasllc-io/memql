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

// FieldValue is what a mutation template writes into one field, as far as
// its source says at load.
type FieldValue struct {
	Literal    any    `json:"literal,omitempty"`
	HasLiteral bool   `json:"hasLiteral,omitempty"`
	Arg        string `json:"arg,omitempty"` // the value is exactly args.<Arg>
}

// WriteSpec is one mutation's write.
//
// It is what the static loop graph reads to decide whether a write can fire
// an automation (component/automations/loop_graph.go): which topics a write
// publishes depends on Kind, and whether a trigger filter holds depends on
// what the row is known to carry. A field absent from Fields is not written
// by the template; one present with neither Literal nor Arg is written a
// value only the run knows.
type WriteSpec struct {
	Kind string `json:"kind"` // insert | update
	// NewRow reports that the write creates the row with exactly the fields
	// Fields names: an insert whose template names no id and spells its
	// payload out field by field. Such a write is the row's first version,
	// and a field it does not set is absent. (A `payload:` splat can write
	// any field, so a splatting insert is not one.)
	NewRow bool                  `json:"newRow,omitempty"`
	Fields map[string]FieldValue `json:"fields,omitempty"`
}

// clone copies the spec, so a caller refining one Write's fields does not
// rewrite the registry the walk read it from.
func (s WriteSpec) clone() WriteSpec {
	if s.Fields != nil {
		fields := make(map[string]FieldValue, len(s.Fields))
		for k, v := range s.Fields {
			fields[k] = v
		}
		s.Fields = fields
	}
	return s
}

// Write is one mutation a set of names reaches, with the path that reaches it.
type Write struct {
	Concept  string
	Mutation string
	// Path is the call path from the name the walk started at to the
	// mutation, both included: [m] for a mutation named directly, [l, m] for
	// one a logic l calls.
	Path []string
	Spec WriteSpec
}

// UnionWrites walks the call graph from each name, as UnionFootprint does, and
// returns every mutation it reaches, sorted by (Concept, Mutation, Path).
//
// The walk from each name is its own: a mutation two names reach is reported
// once for each, with the first path the walk finds from that name, because
// the path is what an edge of the loop graph prints and what decides whether
// the call site's arguments reach the write (only a mutation named directly,
// a path of one, is written with them). Within one name's walk each construct
// is entered once, so a logic cycle terminates. As in UnionFootprint, a name
// absent from the registry contributes nothing, and a mutation with no
// concept writes nothing the caller can name.
func UnionWrites(names []string, reg Registry) []Write {
	var out []Write
	for _, root := range names {
		seen := map[string]bool{}
		var walk func(n string, path []string)
		walk = func(n string, path []string) {
			if seen[n] {
				return
			}
			seen[n] = true
			t, ok := reg[n]
			if !ok {
				return
			}
			here := append(append(make([]string, 0, len(path)+1), path...), n)
			if t.ConstructKind == ConstructMutation && t.Concept != "" {
				w := Write{Concept: t.Concept, Mutation: n, Path: here}
				if t.Write != nil {
					w.Spec = t.Write.clone()
				}
				out = append(out, w)
			}
			for _, c := range t.Calls {
				walk(c, here)
			}
		}
		walk(root, nil)
	}
	sort.SliceStable(out, func(i, j int) bool { return writeLess(out[i], out[j]) })
	deduped := out[:0]
	for i, w := range out {
		if i > 0 && !writeLess(out[i-1], w) {
			continue // the same mutation by the same path, from a repeated name
		}
		deduped = append(deduped, w)
	}
	if len(deduped) == 0 {
		return nil
	}
	return deduped
}

// writeLess orders writes by concept, then mutation, then path element by
// element (a path that is a prefix of another sorts first).
func writeLess(a, b Write) bool {
	if a.Concept != b.Concept {
		return a.Concept < b.Concept
	}
	if a.Mutation != b.Mutation {
		return a.Mutation < b.Mutation
	}
	for i := 0; i < len(a.Path) && i < len(b.Path); i++ {
		if a.Path[i] != b.Path[i] {
			return a.Path[i] < b.Path[i]
		}
	}
	return len(a.Path) < len(b.Path)
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
