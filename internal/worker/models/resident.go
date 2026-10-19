package models

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// models.resident: the models this machine keeps loaded.
//
// Ollama unloads a model five minutes after its last call unless told
// otherwise, and reloads it (about three seconds from a warm disk cache,
// forty from a cold one, on the 27B this fleet serves) the next time
// anybody asks. Two things cause a reload, and a resident model has to
// answer both: the idle timeout, which keep_alive moves, and a change in
// the requested working context, which the working window pins. See
// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

// MaxKeepAlive is the longest finite keep_alive accepted. Past it the
// owner almost certainly meant -1, and a value that is nearly forever but
// not quite is the kind of setting that is found a month later.
const MaxKeepAlive = 30 * 24 * time.Hour

// KeepAlive is a validated keep_alive setting: how long Ollama keeps a
// model loaded after a call.
//
// The zero value is "the owner said nothing", and it is not the same as a
// zero duration: nothing is sent and the runtime's own default
// (OLLAMA_KEEP_ALIVE, five minutes unless somebody changed it) applies.
type KeepAlive struct {
	// Set is false when no keep_alive is configured.
	Set bool
	// Forever keeps the model loaded until Ollama stops or another model
	// needs the space. It is not a pin: an idle model is still evicted
	// when a different one must load and does not fit.
	Forever bool
	// For is the finite duration, when Set and not Forever.
	For time.Duration
}

var errKeepAliveZero = errors.New("keep_alive 0 unloads the model after every call, which is the opposite of resident; delete the setting to get Ollama's default, or use -1 to keep the model loaded")

// ParseKeepAlive reads what an owner wrote under models.resident.
//
// Accepted, as Ollama itself reads them: a Go duration ("30m", "2h"), a
// number of seconds ("3600"), and -1 or any negative value for forever.
// "forever" is also accepted because `-1` reads as a typo to the next
// person. Zero is refused by name, and so is anything unreadable:
// returning the zero KeepAlive for garbage would make a typo
// indistinguishable from an owner who asked for nothing.
func ParseKeepAlive(s string) (KeepAlive, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return KeepAlive{}, nil
	}
	if strings.EqualFold(s, "forever") {
		return KeepAlive{Set: true, Forever: true}, nil
	}
	// A bare integer is seconds, the way Ollama reads a JSON number. Only text
	// that is ENTIRELY digits, after one optional sign, is an integer: ParseInt
	// reports a range error the moment the digits overflow, before it reaches a
	// letter that follows them, so "-99999999999999999999h" would otherwise come
	// back as an integer below zero -- forever, from a typo.
	if integerText(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		// An integer past int64 is still an integer, and it must not reach the
		// duration parser, which would call it "not a duration". ParseInt clamps
		// it to the bound of its own sign, so the sign says which way it
		// overflowed.
		if errors.Is(err, strconv.ErrRange) {
			if n < 0 {
				return KeepAlive{Set: true, Forever: true}, nil
			}
			return KeepAlive{}, errKeepAliveTooLong(s)
		}
		if err == nil {
			switch {
			case n < 0:
				return KeepAlive{Set: true, Forever: true}, nil
			case n == 0:
				return KeepAlive{}, errKeepAliveZero
			case n > int64(MaxKeepAlive/time.Second):
				return KeepAlive{}, errKeepAliveTooLong(s)
			}
			return KeepAlive{Set: true, For: time.Duration(n) * time.Second}, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return KeepAlive{}, fmt.Errorf("keep_alive %q is not a duration like 30m or 2h, a number of seconds, or -1 for forever", s)
	}
	switch {
	case d < 0:
		return KeepAlive{Set: true, Forever: true}, nil
	case d == 0:
		return KeepAlive{}, errKeepAliveZero
	case d > MaxKeepAlive:
		return KeepAlive{}, errKeepAliveTooLong(s)
	}
	return KeepAlive{Set: true, For: d}, nil
}

// integerText reports whether s is one optional sign and then digits, and
// nothing else: the only text ParseKeepAlive reads as a whole number.
func integerText(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func errKeepAliveTooLong(s string) error {
	return fmt.Errorf("keep_alive %q is longer than 30 days; use -1 to keep the model loaded until Ollama stops", s)
}

// Wire is the value for the request's keep_alive key, or nil when the key
// must be left out. Forever is the JSON number -1 and a duration is a
// string: Ollama reads "-1" as a string with no unit and refuses it, so
// the two cannot share a spelling.
func (k KeepAlive) Wire() any {
	switch {
	case !k.Set:
		return nil
	case k.Forever:
		return -1
	default:
		return k.For.String()
	}
}

// String is the setting as `memql worker models` prints it.
func (k KeepAlive) String() string {
	switch {
	case !k.Set:
		return ""
	case k.Forever:
		return "forever"
	default:
		return k.For.String()
	}
}

// ResidentModel is one entry of policy.yaml models.resident, as written.
//
//	models:
//	  resident:
//	    - id: qwen3.8:27b
//	      keep_alive: 2h
//	      working_context: 16384
//
// KeepAlive is a string so that `keep_alive: -1`, `keep_alive: 30m` and
// `keep_alive: 3600` all decode: yaml.v3 stores a scalar's own text in a
// string field. Validation is ParseKeepAlive's, and the policy reader
// turns each entry into a Resident.
type ResidentModel struct {
	ID             string `yaml:"id"`
	KeepAlive      string `yaml:"keep_alive"`
	WorkingContext int    `yaml:"working_context"`
}

// Resident is a ResidentModel after validation: the shape discovery
// applies. A field the owner left out, or one that failed validation, is
// its zero value, which means "send nothing for this".
type Resident struct {
	ID        string
	KeepAlive KeepAlive
	// WorkingContext is the num_ctx floor in tokens. A call that needs no
	// more than this runs at exactly this window, so the runner Ollama
	// loaded for one call is the runner the next call finds. Zero leaves
	// the engine's own window alone.
	WorkingContext int
}

// residentByID indexes the validated entries. The first entry for an id
// wins, matching the policy reader, so a hand-built Request cannot make a
// later line quietly override an earlier one.
func residentByID(list []Resident) map[string]Resident {
	out := make(map[string]Resident, len(list))
	for _, r := range list {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			continue
		}
		if _, dup := out[id]; !dup {
			out[id] = r
		}
	}
	return out
}

// applyResident puts an owner's entry on the model it names. Only an
// Ollama model takes one: an OpenAI-compatible runtime has no keep_alive
// to send, and sending its model an Ollama-shaped field would be a request
// the runtime may reject.
//
// The window is capped at the context the model itself reported. Asking a
// runtime for more than a model can hold is a request it cannot honour,
// and the model's own ceiling is the number that was always going to win.
func applyResident(info *Info, r Resident) {
	if info.Kind != KindOllama {
		return
	}
	if !r.KeepAlive.Set && r.WorkingContext <= 0 {
		return
	}
	info.KeepAlive = r.KeepAlive
	w := r.WorkingContext
	if w > 0 && info.ContextWindow > 0 && w > info.ContextWindow {
		w = info.ContextWindow
	}
	// An embedder runs at a fixed window, so there is no working context to
	// pin. residentNotes already says so, and the stored value must agree.
	if info.Embeddings {
		w = 0
	}
	info.WorkingContext = w
}

// residentNotes says why an entry is idle, in the channel `memql worker
// models` already prints (Inventory.ProbeNotes). An entry that names a
// model the machine does not have is the likeliest mistake -- the bake-off
// names a winner before anyone has pulled it -- and silence would read as
// a model that is resident.
func residentNotes(found []Info, resident []Resident) []string {
	byModel := make(map[string]Info, len(found))
	for _, m := range found {
		byModel[m.ID] = m
	}
	var notes []string
	seen := map[string]bool{}
	for _, r := range resident {
		id := strings.TrimSpace(r.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m, ok := byModel[id]
		switch {
		case !ok:
			notes = append(notes, fmt.Sprintf("models.resident: %s is not installed in any runtime this machine probes", id))
		case m.Kind != KindOllama:
			notes = append(notes, fmt.Sprintf("models.resident: %s is served by %s, which has no keep_alive; models.resident is ignored for it", id, m.Runtime))
		case m.Embeddings && r.WorkingContext > 0:
			notes = append(notes, fmt.Sprintf("models.resident: %s is an embedding model; embeddings run at a fixed window, so working_context is ignored", id))
		}
	}
	return notes
}
