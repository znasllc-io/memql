// Package graph is the install/uninstall STEP GRAPH: the declarative
// description of what a local memQL cluster install does, in what order, what
// each step must prove before the next one runs, and what uninstall has to take
// back afterwards.
//
// # WHY A GRAPH AND NOT A SCRIPT
//
// The ten capability scripts under scripts/install/ each do one thing well.
// What was missing was the thing that says how they compose -- and that is
// exactly the knowledge that, written as an installer script, ends up
// duplicated in three places (a shell driver, an editor extension, an in-engine
// automation) and drifting in all three. Here it is DATA, loaded and validated
// by one loader, consumed by every executor.
//
// Three properties follow from that choice, and they are the point of the
// package:
//
//  1. PARALLELISM IS A PROPERTY OF THE DATA. TopoOrder returns WAVES, not a
//     flat list. An executor runs a wave concurrently and waits for it; it
//     never decides what is independent, so it cannot decide wrong.
//  2. EVERY STEP PROVES ITSELF. `verify` is mandatory. The one predicate that
//     proves nothing about the machine -- scriptOk, which reads only the exit
//     code -- is permitted only on a step that declares readOnly, because on a
//     step that changed something it is a rubber stamp.
//  3. UNINSTALL IS DERIVABLE. Every install step that leaves an artifact
//     behind declares a `receipt` and a `preExistingPath`; the uninstall graph
//     declares which install step each of its steps `reverses`. graph_test.go
//     (#3370) turns that into a hard structural assertion, so uninstall cannot
//     silently fall behind install.
//
// # THE VERIFY PREDICATE VOCABULARY
//
// A predicate reads the capability-script result envelope
// ({ok, capability, changed, result{...}, error}). The vocabulary is CLOSED --
// five kinds, nothing else -- so a graph document can never grow an executor
// requirement that only one executor implements:
//
//	{"kind":"scriptOk"}
//	    The envelope's ok is true. Reads nothing about the machine, so it is
//	    ONLY valid on a step with "readOnly": true.
//
//	{"kind":"resultTrue","field":"result.<name>"}
//	    result.<name> is boolean true (or the string "true").
//
//	{"kind":"resultFalse","field":"result.<name>"}
//	    result.<name> is boolean false (or the string "false").
//
//	{"kind":"resultNonEmpty","field":"result.<name>"}
//	    result.<name> is present and not empty / zero / null.
//
//	{"kind":"resultEquals","field":"result.<name>","value":"<v>"}
//	    result.<name> stringifies to exactly <v>.
//
// Every field is spelled with its `result.` prefix on purpose: the envelope's
// own bookkeeping (ok, changed, capability) describes the RUN, while result{}
// describes the MACHINE, and only the second is evidence that a step did its
// job. Refusing an unprefixed field keeps that distinction visible at the point
// where a reviewer reads the graph.
//
// # THE PRE-EXISTENCE PATH
//
// A receipt is a promise that uninstall can take an artifact back. It is only
// safe to keep that promise when the receipt also records whether the installer
// CREATED the artifact or merely FOUND it: a developer who already had mkcert,
// or a k3d cluster, or a checkout at that path must not lose it because they
// uninstalled memQL. So `receipt` and `preExistingPath` are required together.
//
//	"preExistingPath": "result.preExisting"    // pre-existed when that is truthy
//	"preExistingPath": "!result.cloned"        // pre-existed when cloned is FALSE
//	"preExistingPath": "none"                  // cannot pre-exist as a foreign
//	                                           // artifact (marker-owned), stated
//	                                           // explicitly rather than omitted
//
// The executor copies the resolved boolean onto the receipt; uninstall passes
// it back as remove-artifact.sh's --pre-existing, whose refusal is the last
// wall between the flag and the damage.
//
// # ELEVATION IS DECLARED, NEVER DISCOVERED
//
// Every step states the privilege it needs ("none" / "sudo" / "user-trust").
// A step that quietly needs root is a step whose blast radius nobody reviewed,
// and an executor that discovers the need at run time has already started.
//
// Refs: #3369 #3357
package graph

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// --------------------------------------------------------------------------
// vocabulary
// --------------------------------------------------------------------------

// Kind distinguishes the two shipped graph documents. It is not decoration:
// `receipt` is meaningful only on an install graph and `reverses` only on an
// uninstall one, and the loader enforces that split.
type Kind string

const (
	KindInstall   Kind = "install"
	KindUninstall Kind = "uninstall"
)

// VerifyKind is the closed predicate vocabulary documented in the package
// comment.
type VerifyKind string

const (
	// VerifyScriptOk reads the envelope's ok flag and nothing else. Valid only
	// on a readOnly step.
	VerifyScriptOk VerifyKind = "scriptOk"
	// VerifyResultTrue asserts result.<field> is true.
	VerifyResultTrue VerifyKind = "resultTrue"
	// VerifyResultFalse asserts result.<field> is false.
	VerifyResultFalse VerifyKind = "resultFalse"
	// VerifyResultNonEmpty asserts result.<field> is present and non-empty.
	VerifyResultNonEmpty VerifyKind = "resultNonEmpty"
	// VerifyResultEquals asserts result.<field> stringifies to Value.
	VerifyResultEquals VerifyKind = "resultEquals"
)

// Elevation is the privilege a step declares up front.
type Elevation string

const (
	// ElevationNone runs entirely as the invoking user.
	ElevationNone Elevation = "none"
	// ElevationSudo needs root (writing /etc/hosts).
	ElevationSudo Elevation = "sudo"
	// ElevationUserTrust mutates the user's trust store (mkcert -install),
	// which may prompt for a password on its own terms without being sudo.
	ElevationUserTrust Elevation = "user-trust"
)

// PreExistingNone is the explicit declaration that an artifact cannot
// pre-exist as a FOREIGN artifact -- it is delimited by our own marker, so
// finding it means finding our own earlier work. Stated rather than omitted:
// "there is no signal" and "nobody thought about it" must not look alike.
const PreExistingNone = "none"

var (
	// resultFieldRe matches the required `result.<name>` spelling.
	resultFieldRe = regexp.MustCompile(`^result\.([A-Za-z][A-Za-z0-9_]*)$`)
	// preExistingRe matches `[!]result.<name>`.
	preExistingRe = regexp.MustCompile(`^(!?)result\.([A-Za-z][A-Za-z0-9_]*)$`)
	// stepIDRe keeps step ids usable as receipt keys and CLI arguments.
	stepIDRe = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
)

// --------------------------------------------------------------------------
// schema
// --------------------------------------------------------------------------

// Verify is one step's proof obligation. See the package comment for the
// vocabulary.
type Verify struct {
	Kind  VerifyKind `json:"kind"`
	Field string     `json:"field,omitempty"`
	Value string     `json:"value,omitempty"`
}

// Step is one capability-script invocation plus everything an executor needs
// to schedule it, prove it, and (for an install step) undo it later.
type Step struct {
	// ID names the step within its graph; receipts and `reverses` key off it.
	ID string `json:"id"`
	// Script is the capability-script id (the `script` arg of a shell.script
	// action), which must be in the engine's capability allowlist.
	Script string `json:"script"`
	// Description is the operator-facing sentence for this step.
	Description string `json:"description"`
	// Params are the graph-PINNED flags for the script: the values that are
	// policy rather than run input (which tool, which action, the confirmation
	// phrase). Run-specific values (a release tag, a key file path) are
	// supplied by the executor at invocation time, so this map is a SUBSET of
	// the script's declared flags -- loader_test.go asserts every key here is
	// really declared by the script's --print-spec.
	Params map[string]string `json:"params,omitempty"`
	// DependsOn lists step ids that must have verified before this one starts.
	DependsOn []string `json:"dependsOn,omitempty"`
	// ReadOnly declares that the step does not change the machine. It is what
	// licenses the scriptOk predicate.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Elevation is the privilege this step needs, declared not discovered.
	Elevation Elevation `json:"elevation"`
	// Receipt names the artifact class this step leaves behind (the `kind` an
	// uninstall step passes to remove-artifact.sh). Empty means the step
	// creates nothing that needs taking back. Install graphs only.
	Receipt string `json:"receipt,omitempty"`
	// PreExistingPath resolves, against the result envelope, to "did this
	// artifact already exist before we ran". Required exactly when Receipt is
	// set. See the package comment for the grammar.
	PreExistingPath string `json:"preExistingPath,omitempty"`
	// ContainedBy names another install step whose receipt already covers this
	// step's change -- a secret written INTO the cluster goes away when the
	// cluster does, so it needs no receipt of its own. It is the honest
	// alternative to a receipt, not a way around one: graph_test.go (#3370)
	// requires every mutating install step to declare one or the other, and
	// requires the named step to be reversible and to be a dependency of this
	// one. Mutually exclusive with Receipt. Install graphs only.
	ContainedBy string `json:"containedBy,omitempty"`
	// Reverses names the install-graph step id this step undoes. Uninstall
	// graphs only; it is what makes the drift assertion in #3370 possible.
	Reverses string `json:"reverses,omitempty"`
	// Verify is mandatory: what this step must prove before the graph moves on.
	Verify Verify `json:"verify"`
}

// Graph is a loaded, validated step graph.
type Graph struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description"`
	Steps       []Step `json:"steps"`

	index map[string]*Step
}

// --------------------------------------------------------------------------
// loading + validation
// --------------------------------------------------------------------------

//go:embed install.json
var installJSON []byte

//go:embed uninstall.json
var uninstallJSON []byte

var (
	installOnce sync.Once
	installG    *Graph
	installErr  error

	uninstallOnce sync.Once
	uninstallG    *Graph
	uninstallErr  error
)

// Install returns the shipped install graph.
func Install() (*Graph, error) {
	installOnce.Do(func() { installG, installErr = Load(installJSON, "install.json") })
	return installG, installErr
}

// Uninstall returns the shipped uninstall graph.
func Uninstall() (*Graph, error) {
	uninstallOnce.Do(func() { uninstallG, uninstallErr = Load(uninstallJSON, "uninstall.json") })
	return uninstallG, uninstallErr
}

// Load parses and fully validates a graph document. It refuses anything an
// executor would otherwise have to guess about: an unknown field, a step with
// no verify, a scriptOk stamp on a mutating step, a dangling or duplicated or
// self-referential dependency, an undeclared elevation, a receipt with no
// pre-existence signal, and any dependency cycle. A graph that loads is a graph
// an executor can run without re-deriving anything.
func Load(data []byte, source string) (*Graph, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// An invented or misspelled key must be an error, not a silently ignored
	// intention: "retries": 3 that nothing reads is worse than no retries.
	dec.DisallowUnknownFields()

	var g Graph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: trailing content after the graph document", source)
	}

	if strings.TrimSpace(g.Name) == "" {
		return nil, fmt.Errorf("%s: graph has no name", source)
	}
	switch g.Kind {
	case KindInstall, KindUninstall:
	default:
		return nil, fmt.Errorf("%s: unknown graph kind %q (want %q or %q)", source, g.Kind, KindInstall, KindUninstall)
	}
	if len(g.Steps) == 0 {
		return nil, fmt.Errorf("%s: graph has no steps", source)
	}

	g.index = make(map[string]*Step, len(g.Steps))
	for i := range g.Steps {
		s := &g.Steps[i]
		if !stepIDRe.MatchString(s.ID) {
			return nil, fmt.Errorf("%s: step %d has an invalid id %q (want lowerCamelCase)", source, i, s.ID)
		}
		if _, dup := g.index[s.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate step id %q", source, s.ID)
		}
		g.index[s.ID] = s
	}

	for i := range g.Steps {
		if err := g.validateStep(&g.Steps[i], source); err != nil {
			return nil, err
		}
	}

	// Cycles are found here rather than left for the executor to discover as a
	// hang: TopoOrder is the same walk, so a graph that loads always orders.
	if _, err := g.TopoOrder(); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &g, nil
}

func (g *Graph) validateStep(s *Step, source string) error {
	where := fmt.Sprintf("%s: step %q", source, s.ID)

	if strings.TrimSpace(s.Script) == "" {
		return fmt.Errorf("%s names no script", where)
	}
	if strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("%s has no description -- an operator has to be told what is about to happen", where)
	}

	switch s.Elevation {
	case ElevationNone, ElevationSudo, ElevationUserTrust:
	case "":
		return fmt.Errorf("%s declares no elevation -- privilege is declared, never discovered (want %q, %q or %q)",
			where, ElevationNone, ElevationSudo, ElevationUserTrust)
	default:
		return fmt.Errorf("%s declares an unknown elevation %q (want %q, %q or %q)",
			where, s.Elevation, ElevationNone, ElevationSudo, ElevationUserTrust)
	}

	for _, d := range s.DependsOn {
		if d == s.ID {
			return fmt.Errorf("%s depends on itself", where)
		}
		if _, ok := g.index[d]; !ok {
			return fmt.Errorf("%s depends on %q, which is not a step in this graph", where, d)
		}
	}

	// -- receipt / pre-existence ------------------------------------------
	switch g.Kind {
	case KindInstall:
		if s.Reverses != "" {
			return fmt.Errorf("%s sets reverses -- that belongs on an uninstall graph, an install step undoes nothing", where)
		}
	case KindUninstall:
		if s.Receipt != "" {
			return fmt.Errorf("%s sets receipt -- an uninstall step removes artifacts, it does not leave them behind", where)
		}
		if s.ContainedBy != "" {
			return fmt.Errorf("%s sets containedBy -- that records where an INSTALL step's change gets taken back", where)
		}
	}
	if s.ContainedBy != "" {
		if s.Receipt != "" {
			return fmt.Errorf("%s sets both receipt and containedBy -- a change is taken back by its own artifact or by another's, not both", where)
		}
		if s.ContainedBy == s.ID {
			return fmt.Errorf("%s is containedBy itself", where)
		}
		if _, ok := g.index[s.ContainedBy]; !ok {
			return fmt.Errorf("%s is containedBy %q, which is not a step in this graph", where, s.ContainedBy)
		}
		if s.ReadOnly {
			return fmt.Errorf("%s is readOnly and containedBy %q -- a step that changes nothing needs nothing taken back", where, s.ContainedBy)
		}
	}
	if s.Receipt != "" && s.PreExistingPath == "" {
		return fmt.Errorf("%s writes the %q receipt but declares no preExistingPath -- "+
			"uninstall could not tell an artifact we created from one that was already here", where, s.Receipt)
	}
	if s.Receipt == "" && s.PreExistingPath != "" {
		return fmt.Errorf("%s declares a preExistingPath but writes no receipt -- nothing would ever read it", where)
	}
	if s.PreExistingPath != "" && s.PreExistingPath != PreExistingNone && !preExistingRe.MatchString(s.PreExistingPath) {
		return fmt.Errorf("%s has a malformed preExistingPath %q (want %q or [!]result.<field>)",
			where, s.PreExistingPath, PreExistingNone)
	}

	// -- verify ------------------------------------------------------------
	v := s.Verify
	if v.Kind == "" {
		return fmt.Errorf("%s declares no verify -- a step that proves nothing reports success on an exit code, and the next step builds on that", where)
	}
	switch v.Kind {
	case VerifyScriptOk:
		if !s.ReadOnly {
			return fmt.Errorf("%s verifies with scriptOk but is not readOnly -- scriptOk reads only the exit code, "+
				"which is a rubber stamp on a step that changed the machine; verify a result field instead", where)
		}
		if v.Field != "" || v.Value != "" {
			return fmt.Errorf("%s: scriptOk takes no field and no value", where)
		}
	case VerifyResultTrue, VerifyResultFalse, VerifyResultNonEmpty, VerifyResultEquals:
		if v.Field == "" {
			return fmt.Errorf("%s: %s requires a field", where, v.Kind)
		}
		if !resultFieldRe.MatchString(v.Field) {
			return fmt.Errorf("%s: verify field %q must be spelled result.<name> -- "+
				"the envelope's own bookkeeping (ok/changed/capability) describes the run, result{} describes the machine", where, v.Field)
		}
		if v.Kind == VerifyResultEquals {
			if v.Value == "" {
				return fmt.Errorf("%s: resultEquals requires a value", where)
			}
		} else if v.Value != "" {
			return fmt.Errorf("%s: %s takes no value (got %q)", where, v.Kind, v.Value)
		}
	default:
		return fmt.Errorf("%s: unknown verify kind %q (want %s)", where, v.Kind,
			strings.Join([]string{
				string(VerifyScriptOk), string(VerifyResultTrue), string(VerifyResultFalse),
				string(VerifyResultNonEmpty), string(VerifyResultEquals),
			}, ", "))
	}
	return nil
}

// --------------------------------------------------------------------------
// accessors
// --------------------------------------------------------------------------

// Step returns the step with the given id, or nil.
func (g *Graph) Step(id string) *Step {
	if g == nil {
		return nil
	}
	return g.index[id]
}

// IDs returns every step id in document order.
func (g *Graph) IDs() []string {
	out := make([]string, 0, len(g.Steps))
	for i := range g.Steps {
		out = append(out, g.Steps[i].ID)
	}
	return out
}

// PreExisting decodes the step's pre-existence declaration into the result
// field to read and whether its meaning is inverted. ok is false when the step
// declares "none" or nothing at all -- there is no field to consult.
func (s *Step) PreExisting() (field string, negate bool, ok bool) {
	if s == nil || s.PreExistingPath == "" || s.PreExistingPath == PreExistingNone {
		return "", false, false
	}
	m := preExistingRe.FindStringSubmatch(s.PreExistingPath)
	if m == nil {
		return "", false, false
	}
	return m[2], m[1] == "!", true
}

// VerifyField returns the bare result-envelope field this step's predicate
// reads, or "" for scriptOk.
func (s *Step) VerifyField() string {
	m := resultFieldRe.FindStringSubmatch(s.Verify.Field)
	if m == nil {
		return ""
	}
	return m[1]
}

// --------------------------------------------------------------------------
// evaluation
// --------------------------------------------------------------------------

// Evaluate applies the predicate to a parsed capability-script result envelope
// ({"ok":..., "result":{...}}). Keeping the vocabulary executable HERE is what
// stops each executor from inventing its own reading of the same graph.
func (v Verify) Evaluate(envelope map[string]any) (bool, error) {
	if !truthy(envelope["ok"]) {
		return false, nil
	}
	if v.Kind == VerifyScriptOk {
		return true, nil
	}
	m := resultFieldRe.FindStringSubmatch(v.Field)
	if m == nil {
		return false, fmt.Errorf("verify field %q is not spelled result.<name>", v.Field)
	}
	result, _ := envelope["result"].(map[string]any)
	val, present := result[m[1]]

	switch v.Kind {
	case VerifyResultTrue:
		return present && truthy(val), nil
	case VerifyResultFalse:
		return present && !truthy(val), nil
	case VerifyResultNonEmpty:
		return present && nonEmpty(val), nil
	case VerifyResultEquals:
		return present && stringify(val) == v.Value, nil
	}
	return false, fmt.Errorf("unknown verify kind %q", v.Kind)
}

// truthy reads a JSON value as a boolean the way the capability scripts write
// them: real booleans, and the strings a shell emits for them.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && b
	case float64:
		return t != 0
	default:
		return false
	}
}

// nonEmpty reports whether a value carries content: a non-empty string, a
// non-zero number, true, or a non-empty array/object.
func nonEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case float64:
		return t != 0
	case bool:
		return t
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// stringify renders a JSON scalar the way resultEquals compares it.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// --------------------------------------------------------------------------
// ordering
// --------------------------------------------------------------------------

// TopoOrder returns the graph's steps grouped into dependency WAVES: every id
// in waves[0] has no dependencies, and every id in waves[i] depends only on
// steps in earlier waves. An executor runs a wave concurrently, waits for all
// of it to verify, and moves on.
//
// Returning waves rather than a flat topological list is the whole design
// choice: it makes "these three tool installs are independent" a fact stated by
// the graph document instead of a judgement each executor re-derives (and one
// of them gets wrong, serialising a two-minute install into six).
//
// Ids within a wave are sorted so the order is deterministic across runs.
func (g *Graph) TopoOrder() ([][]string, error) {
	remaining := make(map[string]map[string]bool, len(g.Steps))
	for i := range g.Steps {
		s := &g.Steps[i]
		deps := make(map[string]bool, len(s.DependsOn))
		for _, d := range s.DependsOn {
			deps[d] = true
		}
		remaining[s.ID] = deps
	}

	var waves [][]string
	done := map[string]bool{}
	for len(remaining) > 0 {
		var wave []string
		for id, deps := range remaining {
			ready := true
			for d := range deps {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			stuck := make([]string, 0, len(remaining))
			for id := range remaining {
				stuck = append(stuck, id)
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("dependency cycle among steps: %s", strings.Join(stuck, ", "))
		}
		sort.Strings(wave)
		for _, id := range wave {
			delete(remaining, id)
		}
		for _, id := range wave {
			done[id] = true
		}
		waves = append(waves, wave)
	}
	return waves, nil
}
