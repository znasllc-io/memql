// Package cassette is the CI tier's recorded model responses.
//
// PURE -- standard library only.
//
// THE FACT THAT SURPRISES PEOPLE: in the CI tier BOTH ARMS replay. It follows
// from having no provider. A bare-loop baseline that cannot call a model
// cannot run at all, so the baseline's responses come from a cassette exactly
// as the platform's do. What CI therefore measures about either arm is
// STRUCTURAL -- how many recorded responses it consumed, how many steps it
// re-ran, whether it duplicated a side effect -- and never its dollars.
//
// The counter is the point. `Reads` is what the amortized-cost family's
// headline rests on: a run served entirely from the journal consumes NO
// recorded response, and that is a number this package keeps rather than a
// claim the runner makes.
//
// A cassette carries the model id and the capture date it was recorded with,
// and every figure derived from it carries them onward. A recording read back
// months later is still a recording, and the provenance is what stops it being
// read as today's measurement.
package cassette

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// Cassette is one scenario's recorded responses for one arm.
type Cassette struct {
	// Scenario is the scenario id this was recorded for.
	Scenario string `json:"scenario"`
	// Arm is platform or baseline.
	Arm string `json:"arm"`
	// ModelId is the model the recording was made against. It travels onto
	// every figure derived from this cassette.
	ModelId string `json:"modelId"`
	// RecordedAt is the capture date, YYYY-MM-DD. Also travels.
	RecordedAt string `json:"recordedAt"`
	// Turns are the recorded exchanges, keyed by request hash.
	Turns []Turn `json:"turns"`
}

// Turn is one recorded model exchange.
type Turn struct {
	// RequestHash keys the turn. The runner computes the same hash over the
	// same inputs, so a prompt change is a MISS rather than a silently
	// mismatched reply -- which is the whole difference between a replay and
	// a recording played back regardless.
	RequestHash string `json:"requestHash"`
	// Prompt is kept for a human reading the file. It is NOT what the hash is
	// over on its own, and it is not used for matching.
	Prompt string `json:"prompt"`
	// Response is what the model said.
	Response string `json:"response"`
	// InputTokens and OutputTokens are what the recording cost. Carried so
	// the live tier's figures and a recording's are comparable in shape --
	// never so a CI run can publish them as its own measurement.
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	// CostUSD is what this turn cost when it was recorded.
	CostUSD float64 `json:"costUsd"`
}

// Player serves a cassette and counts what was served.
type Player struct {
	mu        sync.Mutex
	c         Cassette
	byHash    map[string]Turn
	reads     int
	misses    []string
	tokensIn  int
	tokensOut int
	cost      float64
}

// NewPlayer builds a player over one cassette.
func NewPlayer(c Cassette) *Player {
	p := &Player{c: c, byHash: make(map[string]Turn, len(c.Turns))}
	for _, t := range c.Turns {
		p.byHash[t.RequestHash] = t
	}
	return p
}

// RequestHash is the key a turn is stored under. Hashing the model id and the
// prompt TOGETHER is what makes a model change a miss: the same prompt against
// a different model is a different request, and serving the old reply for it
// would make a model upgrade invisible to the suite.
func RequestHash(modelId, prompt string) string {
	h := sha256.Sum256([]byte(modelId + "\x00" + prompt))
	return hex.EncodeToString(h[:])[:24]
}

// Serve answers one model request from the recording.
//
// A MISS IS AN ERROR, not a silent empty answer. The CI tier has no provider,
// so a miss means the scenario changed under its cassette; answering "" would
// let the run continue and report a structural figure about a conversation
// that never happened.
func (p *Player) Serve(modelId, prompt string) (Turn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := RequestHash(modelId, prompt)
	t, ok := p.byHash[h]
	if !ok {
		p.misses = append(p.misses, h)
		return Turn{}, fmt.Errorf(
			"proving/cassette: no recorded response for %s/%s at request hash %s.\n"+
				"The scenario or the prompt changed since the cassette was captured on %s against %s. "+
				"Re-record with `memql-bench record --scenario=%s`",
			p.c.Scenario, p.c.Arm, h, p.c.RecordedAt, p.c.ModelId, p.c.Scenario)
	}
	p.reads++
	p.tokensIn += t.InputTokens
	p.tokensOut += t.OutputTokens
	p.cost += t.CostUSD
	return t, nil
}

// Reads is how many recorded responses were served. This is the CI tier's
// honest stand-in for "provider calls": a run that consumed none made none.
func (p *Player) Reads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// RecordedTokens and RecordedCost are what the SERVED turns cost WHEN THEY
// WERE RECORDED. They are named "Recorded" rather than "Tokens" and "Cost"
// because the naming is the only thing standing between them and a scorecard
// column claiming CI measured a dollar figure. Design P1: the CI tier
// publishes only what a replay can honestly measure, and these are not it.
func (p *Player) RecordedTokens() (in, out int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokensIn, p.tokensOut
}

// RecordedCost is the recorded dollar cost of the served turns. See
// RecordedTokens for why the name matters.
func (p *Player) RecordedCost() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cost
}

// ModelId is the model this cassette was recorded against.
func (p *Player) ModelId() string { return p.c.ModelId }

// RecordedAt is the capture date.
func (p *Player) RecordedAt() string { return p.c.RecordedAt }

// Misses returns the request hashes that were not found.
func (p *Player) Misses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.misses))
	copy(out, p.misses)
	return out
}

// Set is a loaded cassette library, keyed by (scenario, arm).
type Set struct {
	byKey map[string]Cassette
}

func key(scenario, arm string) string { return scenario + "/" + arm }

// Load reads every `*.json` directly under dir. The filename is
// `<scenario>.<arm>.json`, and the content must agree with it -- a cassette
// whose name and content disagree is one that can be served for the wrong arm.
func Load(fsys fs.FS, dir string) (Set, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Set{}, fmt.Errorf("proving/cassette: reading %s: %w", dir, err)
	}
	s := Set{byKey: map[string]Cassette{}}
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		var c Cassette
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		scenario, arm, ok := cutLast(stem, ".")
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: the name must be <scenario>.<arm>.json", e.Name()))
			continue
		}
		if c.Scenario != scenario || c.Arm != arm {
			problems = append(problems, fmt.Sprintf("%s: content says %s/%s; the name and the content must agree or a cassette can be served for the wrong arm", e.Name(), c.Scenario, c.Arm))
			continue
		}
		if c.ModelId == "" || c.RecordedAt == "" {
			problems = append(problems, fmt.Sprintf("%s: a cassette must carry the model it was recorded against and the date; a recording read back months later is still a recording", e.Name()))
			continue
		}
		s.byKey[key(scenario, arm)] = c
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Set{}, fmt.Errorf("proving/cassette: %d problem(s) in %s:\n  %s", len(problems), dir, strings.Join(problems, "\n  "))
	}
	return s, nil
}

// For returns the cassette for one scenario and arm. ok is false when there is
// none, and the caller must treat that as "this scenario cannot run in the CI
// tier" rather than as an empty recording.
func (s Set) For(scenario, arm string) (Cassette, bool) {
	c, ok := s.byKey[key(scenario, arm)]
	return c, ok
}

// Len is how many cassettes were loaded.
func (s Set) Len() int { return len(s.byKey) }

// ModelIds returns the distinct models the loaded cassettes were recorded
// against, sorted.
//
// It exists so the scorecard can SAY what it replayed. A CI figure derived
// from placeholder responses is honest about structure and silent about
// anything model-dependent, and the only thing keeping that distinction alive
// for a reader is the page naming what was on the tape.
func (s Set) ModelIds() []string {
	seen := map[string]bool{}
	for _, c := range s.byKey {
		seen[c.ModelId] = true
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
