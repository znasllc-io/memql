package router

// doors.go -- the three-step default chain and what happens when it runs out
// (epic memql#5096, task memql#5101, design D4 / D9).
//
// THE CHAIN CHANGED SHAPE, and this file is where the new shape is decided.
// The local-models epic made the shipped policies local-first with NO
// fallback, so an idle fleet parked the work; that was the right default when
// the only alternative to a laptop was a metered key. There is now a middle
// step -- a subscription the person already pays for -- so the default is
// local-strongest, then a fleet app, then federation, and work parks only when
// EVERY door is shut.
//
// THE FEDERATION HOP IS THE ONE THAT COSTS MONEY, so it is the one that asks
// permission of the guard before it is taken. The check is a READ: it does not
// charge anything, because nothing has been spent yet and a probe that
// incremented a tally would make simply CONSIDERING the cloud count against
// the ceiling.
//
// A CEILING REFUSAL IS NOT A PARK. They look alike from a distance -- both
// stop the work -- and they are different answers: "every door is shut" is a
// condition the world will change (a lid opens, somebody signs in), while "the
// ceiling is reached" is a condition only a person changes. The first is worth
// re-checking on a timer; the second is worth asking about once.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
)

// Door names, matching v1:platform:inferenceStatus.doorsOpen so an operator
// reads one vocabulary across the readiness line, the log and the park card.
const (
	DoorLocal      = "local"
	DoorApp        = "app"
	DoorFederation = "federation"
	// DoorSession is an app door that took the whole STEP (design D7). The
	// value is airoute's: the record, the log and the park card read one
	// vocabulary, and a door spelled two ways is a filter that silently
	// misses half its rows.
	DoorSession = airoute.DoorSession
)

// doorFor classifies a chain entry by the reference an author wrote.
//
// Everything that is not a `fleet:` or an `app:` name is FEDERATION -- the
// explicit `federation:` forms and a bare provider name alike -- which
// over-approximates deliberately. A cluster with a raw API key rather than
// workload-identity federation reaches a vendor through the same chain entry
// and spends money the same way, and the ceiling must gate both; calling the
// door `federation` and being slightly wrong about the credential is better
// than not gating a paid call because it was configured differently.
func doorFor(name string) string {
	if _, ok := memql.IsFleetReference(name); ok {
		return DoorLocal
	}
	if _, ok := memql.IsAppReference(name); ok {
		return DoorApp
	}
	return DoorFederation
}

// InferenceUnavailable is the refusal when a chain could not be served.
//
// It carries EVERY door and why each did not open, because "no provider
// available" is a sentence with no action in it: the four fixes live in four
// places, and which one applies is exactly what the door list says.
type InferenceUnavailable struct {
	// Code is work.RefusalEveryDoorShut or work.RefusalCeilingReached.
	Code string
	// PolicyName names the chain, when it came from a policy.
	PolicyName string
	// Doors is the report, in chain order -- the order the router tried
	// them, which is the order a reader needs to follow the decision.
	Doors []work.DoorReport
	// Considered is the SAME report in the shared vocabulary's shape, carried
	// alongside rather than instead of Doors.
	//
	// Two shapes for one report is not duplication here: work.DoorReport is
	// what work.DoorReporter hands across a module boundary neither side can
	// import through, and airoute.ConsideredEntry is what the decision record
	// stores and what a resolution keeps on SUCCESS -- where there is no
	// refusal to hang a DoorReport on. They are built by the same calls, so
	// they cannot disagree.
	Considered []airoute.ConsideredEntry
	// Decision is the whole resolution that ended here: the level asked for,
	// the level it got down to, the rule and policy that decided.
	//
	// A PARK IS AS LEGIBLE AS A HIT (design D9). Without this, the only
	// decisions a person could read back would be the successful ones, and
	// the interesting question -- why did this call refuse when the identical
	// one yesterday did not -- is a question about a refusal.
	Decision airoute.Decision
	// CeilingReason is the guard's own sentence when Code is
	// ceiling_reached. Carried verbatim: it names the env var to change.
	CeilingReason string
}

// Error leads with the CODE, and that is a contract rather than a style
// choice: work.InferenceRefusalCode matches on this string to decide whether a
// failed run parks or fails, and the executor that reads it is in a different
// module and sees only what was recorded.
func (e *InferenceUnavailable) Error() string {
	if e == nil {
		return work.RefusalEveryDoorShut
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Code == work.RefusalCeilingReached {
		b.WriteString(": a paid provider could have served this call and the cost ceiling is reached")
		if e.CeilingReason != "" {
			fmt.Fprintf(&b, " (%s)", e.CeilingReason)
		}
	} else {
		b.WriteString(": no door to a model is open for this call")
	}
	if e.PolicyName != "" {
		fmt.Fprintf(&b, " [policy %s]", e.PolicyName)
	}
	for _, d := range e.Doors {
		fmt.Fprintf(&b, "; %s (%s): %s", d.Name, d.Door, d.Reason)
		for _, k := range sortedKeys(d.Considered) {
			fmt.Fprintf(&b, " [%s: %s]", k, d.Considered[k])
		}
	}
	return b.String()
}

// AsMap renders the refusal for the approval subject and the park card. The
// doors stay a SLICE in chain order, so every reader sees the order the router
// actually tried.
func (e *InferenceUnavailable) AsMap() map[string]any {
	if e == nil {
		return nil
	}
	doors := make([]map[string]any, 0, len(e.Doors))
	for _, d := range e.Doors {
		doors = append(doors, d.AsMap())
	}
	out := map[string]any{
		"code":  e.Code,
		"doors": doors,
	}
	if e.PolicyName != "" {
		out["policyName"] = e.PolicyName
	}
	if e.CeilingReason != "" {
		out["ceilingReason"] = e.CeilingReason
	}
	return out
}

// RefusalCode implements work.DoorReporter.
func (e *InferenceUnavailable) RefusalCode() string {
	if e == nil {
		return work.RefusalEveryDoorShut
	}
	return e.Code
}

// ReportedDoors implements work.DoorReporter, handing the structured report
// across a module boundary neither side can import through.
func (e *InferenceUnavailable) ReportedDoors() []work.DoorReport {
	if e == nil {
		return nil
	}
	return e.Doors
}

var _ work.DoorReporter = (*InferenceUnavailable)(nil)

// DoorsShut reports which doors were tried and found closed, deduplicated and
// sorted, for a caller that wants the set rather than the sequence.
func (e *InferenceUnavailable) DoorsShut() []string {
	if e == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	for _, d := range e.Doors {
		if seen[d.Door] {
			continue
		}
		seen[d.Door] = true
		out = append(out, d.Door)
	}
	sort.Strings(out)
	return out
}

// doorReporter accumulates the report as the chain walk rejects entries.
//
// It exists so the walk records a reason at every `continue`. A door with no
// reason reads as a bug in the report, and the previous version of this walk
// simply dropped unavailable entries -- which is why an exhausted chain could
// only say "no provider in chain [...] is available", a sentence that names
// the chain and explains nothing.
type doorReporter struct {
	doors      []work.DoorReport
	considered []airoute.ConsideredEntry
}

func (r *doorReporter) note(name, reason string) {
	r.doors = append(r.doors, work.DoorReport{Door: doorFor(name), Name: name, Reason: reason})
	r.considered = append(r.considered, airoute.ConsideredEntry{Entry: name, Door: doorFor(name), Reason: reason})
}

// noteConsidered records a line that belongs on the DECISION but not in a
// refusal's door list: the entry that won, and the notes a selector makes
// about candidates it ordered rather than rejected.
//
// The split is what keeps the refusal honest. work.DoorReport answers "why did
// nothing serve this call", and a line saying "selected" in that list would
// contradict the refusal it appears in.
func (r *doorReporter) noteConsidered(name, door, reason string) {
	r.considered = append(r.considered, airoute.ConsideredEntry{Entry: name, Door: door, Reason: reason})
}

// entries returns the decision's considered list, in walk order.
func (r *doorReporter) entries() []airoute.ConsideredEntry {
	if r == nil {
		return nil
	}
	return r.considered
}

// noteLocal records a local door with the DETAIL that door can give: which
// machines or apps were considered and why each was ruled out.
//
// The machine-level report is the part somebody can act on. "Your fleet is
// unavailable" sends a person nowhere; "laptop: offline; desktop: does not
// offer llama3.1:8b" names the laptop to open and the model to pull. It was
// the whole point of the typed fleet refusal (memql#4682) and it survives the
// move to a door-shaped report rather than being replaced by it.
func (r *doorReporter) noteLocal(name, reason string, considered map[string]string) {
	r.doors = append(r.doors, work.DoorReport{
		Door:       doorFor(name),
		Name:       name,
		Reason:     reason,
		Considered: considered,
	})
	// The decision's line carries the machine detail INLINE, because
	// airoute.ConsideredEntry is three strings by design -- a record a person
	// reads, not a nested structure a client has to walk.
	full := reason
	for _, k := range sortedKeys(considered) {
		full += fmt.Sprintf(" [%s: %s]", k, considered[k])
	}
	r.considered = append(r.considered, airoute.ConsideredEntry{Entry: name, Door: doorFor(name), Reason: full})
}

// refusal builds the typed refusal from what the walk recorded.
func (r *doorReporter) refusal(code, policyName, ceilingReason string) *InferenceUnavailable {
	return &InferenceUnavailable{
		Code:          code,
		PolicyName:    policyName,
		Doors:         r.doors,
		Considered:    r.considered,
		CeilingReason: ceilingReason,
	}
}

// sortedKeys renders a considered map in a stable order, so two readers of the
// same refusal see the same sentence.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
