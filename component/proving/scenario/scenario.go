// Package scenario is the proving corpus's format: what a scenario says, and
// what the loader refuses.
//
// It is PURE -- standard library only -- for the reason the sibling `figure`
// package is: a corpus reviewable without running anything is a corpus a
// reviewer can actually check, and the epic's standard is that the numbers
// must be honest before they are good.
//
// A scenario is DATA. The common case has no Go behind it at all: a goal
// statement, the variables to run it with, a fake external world, the failures
// to inject, and a verifier written as row and effect assertions. That is
// deliberate -- a corpus whose members are Go functions is a corpus only its
// author can review, and a benchmark nobody reviews is a benchmark nobody
// should believe.
//
// EVERY enumerated field is a CLOSED set, refused at load with the offending
// value and the legal set both named. The escape hatch -- a named Go check --
// resolves against a closed registry, so an unknown check name fails at LOAD
// rather than mid-run: a scenario that cannot be verified must not be able to
// report a pass.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/proving/figure"
)

// Kind is a failure injection's kind. The four are the epic's own list:
// transient, environment, contract and human. `kill` is the fifth and is not
// a symptom -- it is the durability family's instrument, a process death
// rather than an error the classifier reads.
type Kind string

const (
	// KindTransient injects a retryable failure: a timeout, a rate limit, a
	// refused connection. The classifier's rules table must read it as
	// `transient` without a model call.
	KindTransient Kind = "transient"
	// KindEnvironment injects a permission denial, a missing file, or a
	// literal that does not hold here.
	KindEnvironment Kind = "environment"
	// KindContract injects a result that violates the step's postcondition.
	KindContract Kind = "contract"
	// KindHuman injects the case that must reach a person: the same action
	// repeated, which the rules table escalates rather than retrying forever.
	KindHuman Kind = "human"
	// KindKill ends the run mid-step with no receipt -- the crashed-pod state.
	// Not a symptom: nothing classifies it, and what it measures is whether
	// the journal is enough to resume.
	KindKill Kind = "kill"
)

// AllKinds returns the closed set.
func AllKinds() []Kind {
	return []Kind{KindTransient, KindEnvironment, KindContract, KindHuman, KindKill}
}

// Valid reports whether k is one of the five.
func (k Kind) Valid() bool {
	for _, x := range AllKinds() {
		if k == x {
			return true
		}
	}
	return false
}

// Facet is one face of the fake external world. Three, and no network: a
// benchmark that reached the internet would measure the internet.
type Facet string

const (
	// FacetMachine is a worker that accepts a script, returns recorded
	// output, and RECORDS THE HASH of what it was asked to run -- which is
	// what the fleet scenario verifies.
	FacetMachine Facet = "machine"
	// FacetMailbox is deliveries, with duplicate detection. The durability
	// family's whole assertion is a count kept here.
	FacetMailbox Facet = "mailbox"
	// FacetHTTP is a request/reply table keyed by method and path.
	FacetHTTP Facet = "http"
)

// AllFacets returns the closed set.
func AllFacets() []Facet { return []Facet{FacetMachine, FacetMailbox, FacetHTTP} }

// Valid reports whether f is one of the three.
func (f Facet) Valid() bool {
	for _, x := range AllFacets() {
		if f == x {
			return true
		}
	}
	return false
}

// Scenario is one member of the corpus.
type Scenario struct {
	// Id is the file's own name and the row key every sample carries. Dotted,
	// family-first: `durability.resume-elsewhere`.
	Id string `json:"id"`
	// Family is which of the six (or governance) this belongs to.
	Family figure.Family `json:"family"`
	// Title is one line a person reads on the scorecard.
	Title string `json:"title"`
	// Goal is the goal statement, with {{name}} placeholders bound from
	// Variables.
	Goal string `json:"goal"`
	// Variables are the bindings to run the goal with. One entry is one run.
	// The amortized-cost family's whole shape is "the same goal, N sets of
	// variables" -- reason once, replay many.
	Variables []map[string]string `json:"variables"`
	// Steps is the authored plan the platform arm executes and the baseline
	// arm is measured against. Named steps, so an injection can point at one.
	Steps []Step `json:"steps"`
	// World is the fake external world this scenario runs against.
	World World `json:"world"`
	// Inject are the failures to inject. Empty for a scenario that measures a
	// clean run.
	Inject []Injection `json:"inject"`
	// Verify is the deterministic verifier. A scenario with an EMPTY verifier
	// is refused: a run nothing checks cannot report a pass.
	Verify []Check `json:"verify"`
	// Floors are per-metric minimum sample counts. Below the floor the figure
	// is `belowFloor` rather than a median of three.
	Floors map[string]int `json:"floors,omitempty"`
	// NegativeControlFor names the metric whose zero-claim this scenario is
	// the control for. A counter that is never incremented on ANY path reads
	// as zero forever, so every zero-claim scenario is paired with one that
	// must produce a non-zero -- and CorpusControls asserts the pairing.
	NegativeControlFor figure.Metric `json:"negativeControlFor,omitempty"`
	// Claims are the metrics this scenario produces. Declared rather than
	// discovered, so a scenario that silently stops emitting a metric is a
	// load-time failure rather than a quietly shrinking scorecard.
	Claims []figure.Metric `json:"claims"`
}

// Step is one authored step. It carries only what the corpus needs to name
// and inject at: the runner binds it to a real automation step.
type Step struct {
	// Key is the step's name, unique within the scenario. An injection points
	// at it.
	Key string `json:"key"`
	// Type is the automation step type: query, mutation, function, action.
	Type string `json:"type"`
	// Target is the construct the step calls, by name.
	Target string `json:"target"`
	// DependsOn are the step keys that must complete first.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Effect names the world facet this step touches, if any. A step with an
	// effect is a side-effecting step, and the governance property "every side
	// effect has a receipt" is asked of exactly these.
	Effect Facet `json:"effect,omitempty"`
	// Reasoning marks a step that would call a model. The baseline arm calls
	// one for every step; the platform arm calls one only for these, and only
	// when the catalog misses.
	Reasoning bool `json:"reasoning,omitempty"`
}

// World is the fake external world. Every facet counts what it was asked to
// do, because "zero duplicated side effects" is a claim about a counter
// somebody kept.
type World struct {
	Machine *MachineWorld `json:"machine,omitempty"`
	Mailbox *MailboxWorld `json:"mailbox,omitempty"`
	HTTP    *HTTPWorld    `json:"http,omitempty"`
}

// MachineWorld is a fake worker.
type MachineWorld struct {
	// Labels the fake machine reports, so a routing assertion has something
	// to match on.
	Labels map[string]string `json:"labels,omitempty"`
	// Scripts maps a script name to the stdout it returns.
	Scripts map[string]string `json:"scripts,omitempty"`
}

// MailboxWorld is a fake mailbox.
type MailboxWorld struct {
	// Addresses the mailbox will accept. A delivery to any other address is
	// an error rather than a silent drop.
	Addresses []string `json:"addresses,omitempty"`
}

// HTTPWorld is a fake request/reply table.
type HTTPWorld struct {
	// Routes maps "METHOD /path" to a canned JSON body.
	Routes map[string]string `json:"routes,omitempty"`
}

// Injection is one failure to inject.
type Injection struct {
	// At is the step key to inject at.
	At string `json:"at"`
	// Kind is which failure.
	Kind Kind `json:"kind"`
	// Message is the error text the injected failure carries. It matters:
	// the classifier's rules table matches on it, so a scenario proving
	// "the rules classified this without a model call" has to say exactly
	// what the failure looked like.
	Message string `json:"message,omitempty"`
	// Once injects only on the first attempt, which is what makes a retry
	// observable: a failure that never stops is a failure, not a recovery.
	Once bool `json:"once,omitempty"`
}

// CheckForm is which kind of assertion a Check is.
type CheckForm string

const (
	// FormRows asserts on graph rows: a concept, a field predicate, a count.
	FormRows CheckForm = "rows"
	// FormEffects asserts on the fake world's counters.
	FormEffects CheckForm = "effects"
	// FormNamed resolves against the closed registry of Go checks. It exists
	// for what data cannot express, and the registry being closed is what
	// stops it becoming the place scenarios go to be untestable.
	FormNamed CheckForm = "named"
)

// Check is one verifier assertion.
type Check struct {
	// Rows names a concept, for FormRows.
	Rows string `json:"rows,omitempty"`
	// Where is the field predicate, for FormRows. All keys must match.
	Where map[string]string `json:"where,omitempty"`
	// Effects names "<facet>.<counter>", for FormEffects.
	Effects string `json:"effects,omitempty"`
	// Named is a check name, for FormNamed.
	Named string `json:"check,omitempty"`
	// Count is the expected count. A pointer so that "expect zero" is
	// expressible and distinguishable from "count not asserted" -- which is
	// the same absent-versus-zero rule the figure package enforces, and it
	// bites hardest here: `duplicated side effects: 0` is the durability
	// family's headline and it must not be the default.
	Count *int `json:"count,omitempty"`
	// AtLeast is the alternative to Count for an assertion with no exact
	// number. Exactly one of Count and AtLeast may be set.
	AtLeast *int `json:"atLeast,omitempty"`
}

// Form reports which assertion form c is, and whether it is well-formed.
func (c Check) Form() (CheckForm, error) {
	set := 0
	var form CheckForm
	if c.Rows != "" {
		set++
		form = FormRows
	}
	if c.Effects != "" {
		set++
		form = FormEffects
	}
	if c.Named != "" {
		set++
		form = FormNamed
	}
	switch {
	case set == 0:
		return "", errors.New("a check must name one of `rows`, `effects` or `check`")
	case set > 1:
		return "", errors.New("a check names more than one of `rows`, `effects` and `check`; they are alternatives, not a conjunction")
	}
	if c.Count != nil && c.AtLeast != nil {
		return "", errors.New("a check sets both `count` and `atLeast`; they are alternatives")
	}
	if c.Count == nil && c.AtLeast == nil {
		return "", errors.New("a check asserts no count; write `count: 0` for an assertion that nothing happened, which is not the same as asserting nothing")
	}
	return form, nil
}

// namedChecks is the CLOSED registry of Go checks. A name absent from it is
// refused at LOAD, so a scenario that cannot be verified can never report a
// pass. Each entry carries the sentence explaining why data could not express
// it; an entry with no such reason is one that should have been data.
var namedChecks = map[string]string{
	"scriptHashMatchesWhatWasAsked":   "the fleet scenario compares the hash the fake machine recorded against the hash the step computed; a row assertion cannot reach the machine's own record",
	"approvalRefusesAChangedArtifact": "the governance property needs a SECOND resume attempt against a mutated artifact, which is a sequence rather than a state",
	"resumedRunKeepsItsRunId":         "the assertion is that two executions share one id, which is a relation between rows rather than a predicate over one",
	"journalIsCompleteForEveryStep":   "the assertion quantifies over the step list, which the row form has no syntax for",
}

// NamedChecks returns the registered check names, sorted.
func NamedChecks() []string {
	out := make([]string, 0, len(namedChecks))
	for k := range namedChecks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NamedCheckReason returns why a named check is not data. ok is false for an
// unregistered name.
func NamedCheckReason(name string) (string, bool) {
	r, ok := namedChecks[name]
	return r, ok
}

// Validate refuses everything the corpus must not contain. Its errors are the
// format's documentation: each names the offending value and the legal set.
func (s Scenario) Validate() error {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if strings.TrimSpace(s.Id) == "" {
		bad("id is empty")
	}
	if !s.Family.Valid() {
		bad("family %q is not one of %v", s.Family, figure.AllFamilies())
	} else if s.Id != "" && !strings.HasPrefix(s.Id, string(s.Family)+".") {
		bad("id %q does not begin with its family %q; the corpus is grouped by the prefix", s.Id, s.Family)
	}
	if strings.TrimSpace(s.Title) == "" {
		bad("title is empty; it is the line a person reads on the scorecard")
	}
	if strings.TrimSpace(s.Goal) == "" {
		bad("goal is empty")
	}
	if len(s.Steps) == 0 {
		bad("no steps")
	}
	if len(s.Verify) == 0 {
		bad("no verifier; a run nothing checks cannot report a pass")
	}
	if len(s.Claims) == 0 {
		bad("claims is empty; a scenario declares the metrics it produces so that one silently ceasing to emit is a load failure rather than a quietly shrinking scorecard")
	}

	keys := map[string]bool{}
	for i, st := range s.Steps {
		if strings.TrimSpace(st.Key) == "" {
			bad("step %d has no key", i)
			continue
		}
		if keys[st.Key] {
			bad("two steps share the key %q", st.Key)
		}
		keys[st.Key] = true
		if strings.TrimSpace(st.Type) == "" {
			bad("step %q has no type", st.Key)
		}
		if st.Effect != "" && !st.Effect.Valid() {
			bad("step %q names effect facet %q, which is not one of %v", st.Key, st.Effect, AllFacets())
		}
		if st.Effect != "" && !s.World.has(st.Effect) {
			bad("step %q has an effect on the %s facet, but the scenario declares no %s world", st.Key, st.Effect, st.Effect)
		}
	}
	for _, st := range s.Steps {
		for _, dep := range st.DependsOn {
			if !keys[dep] {
				bad("step %q depends on %q, which is not a step in this scenario", st.Key, dep)
			}
			if dep == st.Key {
				bad("step %q depends on itself", st.Key)
			}
		}
	}

	for i, in := range s.Inject {
		if !in.Kind.Valid() {
			bad("injection %d has kind %q, which is not one of %v", i, in.Kind, AllKinds())
		}
		if !keys[in.At] {
			bad("injection %d fires at %q, which is not a step in this scenario", i, in.At)
		}
		// A transient failure that never stops is a failure, not a
		// recovery. The corpus is allowed to express that, but not by
		// accident: it has to be a scenario whose verifier expects failure.
		if in.Kind == KindTransient && !in.Once && !s.expectsFailure() {
			bad("injection %d is a transient failure that fires on every attempt, but the verifier expects success; set `once: true` so the retry can succeed, or write a verifier that expects the failure", i)
		}
		if in.Kind != KindKill && strings.TrimSpace(in.Message) == "" {
			bad("injection %d has no message; the symptom classifier matches on the error text, so a scenario proving the rules classified something must say exactly what it looked like", i)
		}
	}

	for i, c := range s.Verify {
		form, err := c.Form()
		if err != nil {
			bad("verifier %d: %v", i, err)
			continue
		}
		switch form {
		case FormNamed:
			if _, ok := NamedCheckReason(c.Named); !ok {
				bad("verifier %d names the check %q, which is not registered; the registry is closed (%v) so that a scenario which cannot be verified fails at LOAD rather than reporting a pass", i, c.Named, NamedChecks())
			}
		case FormEffects:
			facet, _, ok := strings.Cut(c.Effects, ".")
			if !ok {
				bad("verifier %d: effects %q must be `<facet>.<counter>`", i, c.Effects)
			} else if !Facet(facet).Valid() {
				bad("verifier %d: effects names facet %q, which is not one of %v", i, facet, AllFacets())
			} else if !s.World.has(Facet(facet)) {
				bad("verifier %d asserts on the %s facet, but the scenario declares no %s world", i, facet, facet)
			}
		case FormRows:
			if !strings.HasPrefix(c.Rows, "v1:") {
				bad("verifier %d: rows %q is not a canonical concept id", i, c.Rows)
			}
		}
	}

	for _, m := range s.Claims {
		if _, ok := figure.MetricSpec(m); !ok {
			bad("claims %q, which is not a registered metric", m)
		} else if spec, _ := figure.MetricSpec(m); spec.Family != s.Family {
			bad("claims %q, which belongs to family %q, not this scenario's %q", m, spec.Family, s.Family)
		}
	}
	if s.NegativeControlFor != "" {
		if _, ok := figure.MetricSpec(s.NegativeControlFor); !ok {
			bad("negativeControlFor names %q, which is not a registered metric", s.NegativeControlFor)
		}
	}
	for m, floor := range s.Floors {
		if _, ok := figure.MetricSpec(figure.Metric(m)); !ok {
			bad("floors names %q, which is not a registered metric", m)
		}
		if floor < 1 {
			bad("floors[%s] = %d; a floor below one is not a floor", m, floor)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("scenario %q:\n  - %s", s.Id, strings.Join(problems, "\n  - "))
	}
	return nil
}

// expectsFailure reports whether any verifier expects a failed run. Used to
// let a scenario deliberately assert that an unrecoverable failure stays
// failed.
func (s Scenario) expectsFailure() bool {
	for _, c := range s.Verify {
		if c.Where["status"] == "failed" {
			return true
		}
	}
	return false
}

// has reports whether the world declares the named facet.
func (w World) has(f Facet) bool {
	switch f {
	case FacetMachine:
		return w.Machine != nil
	case FacetMailbox:
		return w.Mailbox != nil
	case FacetHTTP:
		return w.HTTP != nil
	}
	return false
}

// Corpus is a loaded scenario set, indexed and ordered.
type Corpus struct {
	Scenarios []Scenario
	// Fingerprint is a stable digest over the corpus's content, stamped on
	// every bench run row. Two runs with different fingerprints measured
	// different things and their figures are not a trend.
	Fingerprint string
}

// ByFamily returns the scenarios in one family, in corpus order.
func (c Corpus) ByFamily(f figure.Family) []Scenario {
	var out []Scenario
	for _, s := range c.Scenarios {
		if s.Family == f {
			out = append(out, s)
		}
	}
	return out
}

// Load reads every `*.json` directly under dir and validates it. It returns
// the FIRST failure as an error carrying every problem it found, rather than
// stopping at the first bad field: a corpus author fixing one error at a time
// against a CI round trip is the reason nobody keeps a corpus current.
func Load(fsys fs.FS, dir string) (Corpus, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Corpus{}, fmt.Errorf("proving/scenario: reading %s: %w", dir, err)
	}
	var (
		out      []Scenario
		problems []string
		names    = map[string]string{}
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, full)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", full, err))
			continue
		}
		var s Scenario
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		// An unknown key is a typo that would otherwise be silently ignored,
		// and a silently ignored `injcet` is a scenario that measures a clean
		// run while its author believes it injects a failure.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", full, err))
			continue
		}
		wantId := strings.TrimSuffix(e.Name(), ".json")
		if s.Id != wantId {
			problems = append(problems, fmt.Sprintf("%s: id is %q but the file is named %q; they must agree so a sample row's scenarioId names a file", full, s.Id, wantId))
			continue
		}
		if prev, dup := names[s.Id]; dup {
			problems = append(problems, fmt.Sprintf("%s: id %q is already used by %s", full, s.Id, prev))
			continue
		}
		names[s.Id] = full
		if err := s.Validate(); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, s)
	}
	if len(problems) > 0 {
		return Corpus{}, fmt.Errorf("proving/scenario: %d problem(s) in %s:\n%s", len(problems), dir, strings.Join(problems, "\n"))
	}
	if len(out) == 0 {
		return Corpus{}, fmt.Errorf("proving/scenario: %s holds no scenarios; an empty corpus passes every gate and proves nothing", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return Corpus{Scenarios: out, Fingerprint: fingerprint(out)}, nil
}

// fingerprint digests the corpus's content. It is over the marshalled
// scenarios rather than the file bytes, so reformatting the JSON does not
// read as a different corpus while changing a value does.
func fingerprint(ss []Scenario) string {
	h := newDigest()
	for _, s := range ss {
		b, err := json.Marshal(s)
		if err != nil {
			// Unreachable for a validated scenario; a digest that silently
			// skipped a member would make two different corpora agree.
			panic("proving/scenario: marshalling a validated scenario: " + err.Error())
		}
		h.Write([]byte(s.Id))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return h.Hex()
}

// CorpusControls checks the negative-control pairing across the whole corpus:
// every metric whose registered direction makes zero the good answer must have
// at least one scenario that produces a NON-zero, or the counter behind it
// could be dead and nothing would notice.
//
// It is a corpus-level rule rather than a per-scenario one because the pairing
// is between two scenarios, and it is here rather than in the runner because
// it is a property of the DATA.
func (c Corpus) CorpusControls() error {
	claimed := map[figure.Metric]bool{}
	controlled := map[figure.Metric]bool{}
	for _, s := range c.Scenarios {
		for _, m := range s.Claims {
			claimed[m] = true
		}
		if s.NegativeControlFor != "" {
			controlled[s.NegativeControlFor] = true
		}
	}
	var missing []string
	for m := range claimed {
		spec, ok := figure.MetricSpec(m)
		if !ok || spec.Direction != figure.LowerIsBetter || !spec.Blocking {
			continue
		}
		if !controlled[m] {
			missing = append(missing, string(m))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"proving/scenario: %d blocking metric(s) whose good answer is zero have no negative control: %s.\n"+
			"A counter that is never incremented on ANY path reads as zero forever, so a green suite with a dead counter is indistinguishable from a working one. "+
			"Add a scenario with `negativeControlFor` set to each",
		len(missing), strings.Join(missing, ", "))
}
