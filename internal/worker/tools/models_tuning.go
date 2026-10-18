package tools

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// models.parallel and models.resident, read the way apps.levels is read:
// a pure function of the block as written, run on every read, that never
// fails. A shape it cannot use becomes a sentence about that entry and
// the entry is ignored -- the direction that changes nothing -- so a typo
// costs the one setting and never the machine.
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
// models.resident, as sentences an owner can act on. The worker logs them
// when the file is read and `memql worker models` prints them, because a
// setting that silently did nothing is indistinguishable from one that
// worked.
func (p *Policy) ModelsTuningProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, problems := readModelsTuning(p.models)
	return problems
}

func readModelsTuning(m ModelsPolicy) (ModelsTuning, []string) {
	var (
		out      ModelsTuning
		problems []string
	)
	switch {
	case m.Parallel == 0:
		// Not said.
	case m.Parallel < 1 || m.Parallel > MaxModelsParallel:
		problems = append(problems, fmt.Sprintf(
			"models.parallel: %d is outside 1 to %d and is ignored; OLLAMA_NUM_PARALLEL, the managed Ollama unit, or 1 applies",
			m.Parallel, MaxModelsParallel))
	default:
		out.Parallel = m.Parallel
	}

	allowed := make(map[string]bool, len(m.Allow))
	for _, id := range m.Allow {
		allowed[strings.TrimSpace(id)] = true
	}

	seen := map[string]bool{}
	for i, raw := range m.Resident {
		n := i + 1
		id := strings.TrimSpace(raw.ID)
		if id == "" {
			problems = append(problems, fmt.Sprintf("models.resident entry %d has no id and is ignored", n))
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
			if strings.TrimSpace(raw.KeepAlive) == "" && raw.WorkingContext == 0 {
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
