package tools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"gopkg.in/yaml.v3"
)

// models.parallel and models.resident, read the way apps.levels is read:
// a pure function of the block as written, run on every read, that never
// fails. A shape it cannot use becomes a sentence about that entry and
// the entry is ignored -- the direction that changes nothing -- so a typo
// costs the one setting and never the machine. Both are walked as YAML
// nodes for that reason: a typed decode fails the whole policy.yaml on one
// wrong shape, and reads 2.5 as 2 without a word.
//
// Design record: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

const (
	// MaxModelsParallel bounds models.parallel. Past it the number is a
	// typo, and a typo here is not harmless: the ceiling is also the sum
	// the engine rations by.
	MaxModelsParallel = 64
	// MaxWorkingContext bounds a resident model's working window, in
	// tokens. No local model serves more than a million.
	MaxWorkingContext = 1 << 20
)

// ModelsTuning is models.parallel and models.resident after validation.
type ModelsTuning struct {
	// Parallel is the owner's per-model ceiling for the native Ollama, or
	// zero when the file does not say (or said something unusable).
	Parallel int
	// Resident is every entry that stands, in file order. An entry for a
	// model the machine does not serve stands: allowing the model later
	// must just work, and ModelsTuningProblems says it is idle meanwhile.
	Resident []models.Resident
}

// ModelsTuning returns the validated tuning. The slice is the caller's
// own, built fresh on every call: a SIGHUP reload can land underneath a
// caller that is still deciding.
func (p *Policy) ModelsTuning() ModelsTuning {
	if p == nil {
		return ModelsTuning{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, _ := readModelsTuning(p.models)
	return t
}

// ModelsTuningProblems is every problem with models.parallel and
// models.resident, and every key under models: that nothing reads (the usual
// way either of them is mistyped), as sentences an owner can act on. The
// worker logs them when the file is read and `memql worker models` prints
// them, because a setting that silently did nothing is indistinguishable
// from one that worked.
func (p *Policy) ModelsTuningProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, problems := readModelsTuning(p.models)
	return problems
}

// readModelsTuning validates models.parallel and models.resident: a key under
// models: that nothing reads first, then parallel, then the entries in file
// order.
func readModelsTuning(m ModelsPolicy) (ModelsTuning, []string) {
	var (
		out      ModelsTuning
		problems []string
	)
	problems = append(problems, unknownModelsKeys(m.Unknown)...)
	parallel, problem := readParallel(m.Parallel)
	out.Parallel = parallel
	if problem != "" {
		problems = append(problems, problem)
	}

	allowed := make(map[string]bool, len(m.Allow))
	for _, id := range m.Allow {
		allowed[strings.TrimSpace(id)] = true
	}

	entries, walkProblems := readResident(m.Resident)
	problems = append(problems, walkProblems...)
	seen := map[string]bool{}
	for _, e := range entries {
		raw := e.model
		id := strings.TrimSpace(raw.ID)
		if id == "" {
			problems = append(problems, fmt.Sprintf("models.resident entry %d has no id and is ignored", e.n))
			continue
		}
		if seen[id] {
			problems = append(problems, fmt.Sprintf("models.resident lists %s more than once; the first entry stands", id))
			continue
		}
		seen[id] = true

		r := models.Resident{ID: id}
		ka, err := models.ParseKeepAlive(raw.KeepAlive)
		if err != nil {
			problems = append(problems, fmt.Sprintf("models.resident %s: %v; keep_alive is ignored", id, err))
		} else {
			r.KeepAlive = ka
		}
		switch {
		case raw.WorkingContext == 0:
			// Not said.
		case raw.WorkingContext < 1 || raw.WorkingContext > MaxWorkingContext:
			problems = append(problems, fmt.Sprintf(
				"models.resident %s: working_context %d is outside 1 to %d and is ignored",
				id, raw.WorkingContext, MaxWorkingContext))
		default:
			r.WorkingContext = raw.WorkingContext
		}
		if !r.KeepAlive.Set && r.WorkingContext == 0 {
			// Nothing stands. Say so only when the entry never tried to say
			// anything: an entry whose fields were refused already has its
			// sentences.
			if !e.refused && strings.TrimSpace(raw.KeepAlive) == "" && raw.WorkingContext == 0 {
				problems = append(problems, fmt.Sprintf(
					"models.resident %s sets neither keep_alive nor working_context, so it does nothing", id))
			}
			continue
		}
		if !allowed[id] {
			problems = append(problems, fmt.Sprintf(
				"models.resident names %s, which models.allow does not list; this machine will not serve it, so nothing keeps it loaded", id))
		}
		out.Resident = append(out.Resident, r)
	}
	return out, problems
}

// unknownModelsKeys names each key directly under models: that the block does
// not take, sorted so the sentences come out in the same order every time (a
// map keeps no file order). The block takes five keys, and a mistyped one is
// the likeliest way an owner's tuning does nothing.
func unknownModelsKeys(unknown map[string]yaml.Node) []string {
	if len(unknown) == 0 {
		return nil
	}
	keys := make([]string, 0, len(unknown))
	for k := range unknown {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	problems := make([]string, 0, len(keys))
	for _, k := range keys {
		problems = append(problems, fmt.Sprintf(
			"models: takes allow, runtimes, pull, parallel and resident, and %q is none of them, so that key is ignored", k))
	}
	return problems
}

// readParallel reads models.parallel. Absent and null are not said. A value
// that is not a whole number is refused by its shape, so "2" and 2.5 cannot
// pass for 2, and a whole number outside 1 to MaxModelsParallel is refused
// by its size. A refusal returns zero and its sentence.
func readParallel(node yaml.Node) (int, string) {
	n := resolveYAML(&node)
	if yamlAbsent(n) {
		return 0, ""
	}
	var v int
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" || n.Decode(&v) != nil {
		return 0, fmt.Sprintf(
			"models.parallel belongs here as a whole number from 1 to %d, not %s; it is ignored",
			MaxModelsParallel, describeYAML(n))
	}
	if v < 1 || v > MaxModelsParallel {
		return 0, fmt.Sprintf(
			"models.parallel: %d is outside 1 to %d and is ignored; OLLAMA_NUM_PARALLEL, the managed Ollama unit, or 1 applies",
			v, MaxModelsParallel)
	}
	return v, ""
}

// residentWalk is one models.resident entry as the node walk read it. A
// field whose shape was refused is left at its zero value and has its own
// sentence; refused records that, so the "sets neither" sentence is not
// said on top of it.
type residentWalk struct {
	// n is the entry's place in the list, from 1, for its sentences.
	n     int
	model models.ResidentModel
	// refused is set when keep_alive or working_context was refused by its
	// shape.
	refused bool
}

// readResident walks models.resident. A block that is not a list is one
// sentence and no entries; an item that is not a mapping is one sentence
// about that item. The entries that remain come back in file order.
func readResident(node yaml.Node) ([]residentWalk, []string) {
	list := resolveYAML(&node)
	if yamlAbsent(list) {
		return nil, nil
	}
	if list.Kind != yaml.SequenceNode {
		return nil, []string{fmt.Sprintf(
			"models.resident: a list of entries belongs here (- id: qwen3.8:27b), not %s, so the block is ignored",
			describeYAML(list))}
	}
	var (
		entries  []residentWalk
		problems []string
	)
	for i, item := range list.Content {
		n := i + 1
		entry := resolveYAML(item)
		switch {
		case yamlAbsent(entry):
			// An empty item is an entry with no id, which the caller names.
			entries = append(entries, residentWalk{n: n})
		case entry.Kind != yaml.MappingNode:
			problems = append(problems, fmt.Sprintf(
				"models.resident entry %d: a mapping with an id belongs here (id: qwen3.8:27b, keep_alive: 2h), not %s, so the entry is ignored",
				n, describeYAML(entry)))
		default:
			w, walkProblems, ok := walkResidentEntry(entry, n)
			problems = append(problems, walkProblems...)
			if ok {
				entries = append(entries, w)
			}
		}
	}
	return entries, problems
}

// walkResidentEntry reads one entry's mapping. The id is found first,
// because it names the entry in every sentence after it. A key the entry
// does not take is named and ignored. A key written twice keeps its first
// value: yaml.v3 refuses a duplicate key when it decodes a typed struct but
// not inside a node, so the walk has to check for it. ok is false when the
// id is not a name, which leaves the entry nothing to keep.
func walkResidentEntry(entry *yaml.Node, n int) (residentWalk, []string, bool) {
	w := residentWalk{n: n}
	label := fmt.Sprintf("entry %d", n)
	for i := 0; i+1 < len(entry.Content); i += 2 {
		if entry.Content[i].Value != "id" {
			continue
		}
		id := resolveYAML(entry.Content[i+1])
		if yamlAbsent(id) {
			break
		}
		if id.Kind != yaml.ScalarNode {
			return w, []string{fmt.Sprintf(
				"models.resident entry %d: id is the name of a model, and %s is not one, so the entry is ignored",
				n, describeYAML(id))}, false
		}
		w.model.ID = strings.TrimSpace(id.Value)
		if w.model.ID != "" {
			label = w.model.ID
		}
		break
	}
	var problems []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(entry.Content); i += 2 {
		key, val := entry.Content[i].Value, resolveYAML(entry.Content[i+1])
		if key != "id" && key != "keep_alive" && key != "working_context" {
			problems = append(problems, fmt.Sprintf(
				"models.resident %s: takes id, keep_alive and working_context, and %q is none of them, so that key is ignored",
				label, key))
			continue
		}
		if seen[key] {
			problems = append(problems, fmt.Sprintf(
				"models.resident %s: %s is written twice, and the first one stands", label, key))
			continue
		}
		seen[key] = true
		switch key {
		case "keep_alive":
			if yamlAbsent(val) {
				continue
			}
			if val.Kind != yaml.ScalarNode {
				problems = append(problems, fmt.Sprintf(
					"models.resident %s: keep_alive belongs here as a duration like 2h, or -1 for forever, not %s; keep_alive is ignored",
					label, describeYAML(val)))
				w.refused = true
				continue
			}
			w.model.KeepAlive = val.Value
		case "working_context":
			if yamlAbsent(val) {
				continue
			}
			var v int
			if val.Kind != yaml.ScalarNode || val.ShortTag() != "!!int" || val.Decode(&v) != nil {
				problems = append(problems, fmt.Sprintf(
					"models.resident %s: working_context belongs here as a whole number of tokens from 1 to %d, not %s; working_context is ignored",
					label, MaxWorkingContext, describeYAML(val)))
				w.refused = true
				continue
			}
			w.model.WorkingContext = v
		}
	}
	return w, problems, true
}
