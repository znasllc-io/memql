package worker

// Network-aware fleet selection: a pipeline step prefers machines whose
// connection to the cluster holds (design record
// docs/superpowers/specs/2026-11-24-network-aware-fleet-selection-design.md;
// the maintained account is "Fleet steps prefer machines whose connection
// holds" in docs/internal/ops/harness-decisions.md).
//
// WHY. A fleet step dies with its machine's stream (Cockpit stops every tool
// call in flight when the stream is lost) and is answered pipeline_node_lost,
// which is not retried for a step that is not repeatable. On 2026-11-24 a Mac on
// weak Wi-Fi lost its stream eight times in an hour and a half and failed the
// macOS lanes twice, while the router chose it as readily as a wired machine.
// Round trip would not have told them apart -- the dropping Mac read 299 ms and
// the other 360 ms -- so the evidence is DROPS: the rows the replica holding a
// stream writes when it ends in a way Cockpit could not have chosen
// (component/worker connection_drop.go).
//
// WHAT IT DOES, for the pipeline purpose only (D7; an agent's call keeps the
// owner's strategy untouched):
//
//   - ORDER (D4, D5). A machine with a drop inside the run's window is
//     UNSTEADY; steady machines are tried first in the owner's strategy order,
//     then unsteady ones by fewest drops, then oldest last drop.
//   - WAIT (D1, D6), a `placement: fleet` step only (ConnectionPreference.
//     WaitForSteady, set by the fleet executor; a pool step never waits or goes
//     to the cloud over a drop, D10). When a steady machine refused only because
//     it is busy with work the cluster admitted, the walk stops before the
//     first unsteady machine and returns that busy refusal, so the fleet
//     executor's capacity wait asks again; the unsteady machines it passed over
//     ride on the result (Result.PassedOver) so the step's log can say so.
//
// It narrows nothing and admits nothing: every machine it orders is one the
// plan's consents, labels and contracts already admitted, and a failed read
// leaves a machine counted steady -- the preference is never a reason to fail
// or hold a step that today would run.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// connectionDropReadCap is myWorkerConnectionDrops' page: a machine with this
// many drops in a window is unsteady either way, and its count reads "100 or
// more".
const connectionDropReadCap = 100

// ConnectionPreference is a pipeline step's request for network-aware placement.
// Set only by the engine's fleet executor, from the run's captured compute
// placement policy (pipelines.ConnectionPolicy) and the step's placement; the
// builtins never read it, and purposeRefusal refuses it on every purpose but the
// pipeline's, as it refuses a pin.
type ConnectionPreference struct {
	// Window is how long one drop makes a machine unsteady. Zero reads no drops
	// and changes nothing: the pre-existing placement.
	Window time.Duration
	// WaitForSteady is D1: return a steady machine's busy refusal instead of
	// starting on an unsteady machine. A `placement: fleet` step only (D10).
	WaitForSteady bool
}

// set reports whether the preference asks for anything at all.
func (p ConnectionPreference) set() bool {
	return p.Window != 0 || p.WaitForSteady
}

// ConnectionHealth is one machine's connection as placement judged it.
type ConnectionHealth struct {
	// Drops is how many drops fall inside Window; LastDropAt is the newest.
	Drops      int
	LastDropAt time.Time
	Window     time.Duration
	// Capped is a read that hit connectionDropReadCap: Drops is a floor.
	Capped bool
	// Unavailable is a read that failed. It counts as STEADY: a database that
	// cannot answer is not evidence against the machine, and ordering on a
	// guess would move steps the owner's strategy put elsewhere.
	Unavailable bool
}

// Steady is no drop inside the window, or no answer about one.
func (h ConnectionHealth) Steady() bool {
	return h.Unavailable || h.Drops == 0
}

// RecordValue is the health as the invocation's routing record states it:
// "steady", "2 drops, last <RFC3339>", "100 or more drops, last <RFC3339>" or
// "unavailable".
func (h ConnectionHealth) RecordValue() string {
	switch {
	case h.Unavailable:
		return "unavailable"
	case h.Drops == 0:
		return "steady"
	}
	count := fmt.Sprintf("%d drops", h.Drops)
	switch {
	case h.Capped:
		count = fmt.Sprintf("%d or more drops", h.Drops)
	case h.Drops == 1:
		count = "1 drop"
	}
	return count + ", last " + h.LastDropAt.UTC().Format(time.RFC3339)
}

// PassedOver is an unsteady machine a fleet step did not try because a steady
// machine was only busy (D1). MachineName is the owner's chosen display name
// only, never the reported hostname (ownerChosenName): the step's log line that
// names it can reach a public check run.
type PassedOver struct {
	WorkerId    string
	MachineName string
	Health      ConnectionHealth
}

// ConnectionDropReader is the read placement needs; fleetcatalog.EngineStore
// implements it, and the agent EngineStore delegates to it. The dispatcher finds
// it by a type assertion on its Store, as it finds destinationChecker, so a
// store without it -- every test fake but the fleet's -- reads no drops and
// routes exactly as before. The production store asserts it at compile time
// (store.go), because a store that silently lacked it would turn the feature
// off.
type ConnectionDropReader interface {
	ConnectionDrops(ctx context.Context, ownerUserId, registrationId string, since time.Time) ([]workerservice.ConnectionDrop, error)
}

// healthFrom counts the drops inside [now-window, ...). The start is
// INCLUSIVE, as the query's `droppedAt >= since` is, and there is no upper
// bound: a drop is stamped on the clock of the replica that held the stream,
// which may run a few seconds ahead of this one, and such a drop is recent, not
// invalid.
func healthFrom(drops []workerservice.ConnectionDrop, now time.Time, window time.Duration, capped bool) ConnectionHealth {
	h := ConnectionHealth{Window: window, Capped: capped}
	since := now.Add(-window)
	for _, d := range drops {
		if d.DroppedAt.Before(since) {
			continue
		}
		h.Drops++
		if d.DroppedAt.After(h.LastDropAt) {
			h.LastDropAt = d.DroppedAt
		}
	}
	return h
}

// connectionKey is the map key a machine's health is filed under: the bare
// registration id, so a health read under one spelling of the id is found
// under the other (sameRegistration's rule).
func connectionKey(registrationId string) string {
	return lastSegment(strings.TrimSpace(registrationId))
}

// ApplyConnection files each machine's health on the plan and orders the
// candidates: steady first in the order they arrived (the owner's strategy),
// then unsteady by fewest drops, then oldest last drop (D5), every sort stable.
// A plan with no unsteady candidate keeps its order exactly, which is what keeps
// placement unchanged for a fleet that has not dropped. Offline candidates are
// left alone: they are never dispatched to.
func (p *RoutePlan) ApplyConnection(health map[string]ConnectionHealth) {
	if p.Connection == nil {
		p.Connection = make(map[string]ConnectionHealth, len(health))
	}
	for id, h := range health {
		p.Connection[connectionKey(id)] = h
	}
	steady := make([]Candidate, 0, len(p.Candidates))
	var unsteady []Candidate
	for _, c := range p.Candidates {
		if h, ok := p.healthOf(c); ok && !h.Steady() {
			unsteady = append(unsteady, c)
			continue
		}
		steady = append(steady, c)
	}
	if len(unsteady) == 0 {
		return
	}
	sort.SliceStable(unsteady, func(i, j int) bool {
		hi, _ := p.healthOf(unsteady[i])
		hj, _ := p.healthOf(unsteady[j])
		if hi.Drops != hj.Drops {
			return hi.Drops < hj.Drops
		}
		return hi.LastDropAt.Before(hj.LastDropAt)
	})
	p.Candidates = append(steady, unsteady...)
}

// healthOf is the health filed for a machine, if any was read.
func (p RoutePlan) healthOf(c Candidate) (ConnectionHealth, bool) {
	h, ok := p.Connection[connectionKey(c.RegistrationId)]
	return h, ok
}

// readConnectionHealth reads each candidate's drops inside the window, one read
// per machine (myWorkerConnectionDrops) so a machine that drops in a loop
// cannot push another's drops past a shared cap. A failed read marks that
// machine Unavailable -- steady -- and is logged; the other machines are still
// judged.
func (d *Dispatcher) readConnectionHealth(ctx context.Context, ownerUserId string, candidates []Candidate, window time.Duration) map[string]ConnectionHealth {
	now := d.clock()
	out := make(map[string]ConnectionHealth, len(candidates))
	for _, c := range candidates {
		drops, err := d.drops.ConnectionDrops(ctx, ownerUserId, c.RegistrationId, now.Add(-window))
		if err != nil {
			d.logger.Warn("worker router: could not read a machine's connection drops; counting it steady",
				"owner_user_id", ownerUserId, "registration_id", c.RegistrationId, "error", err)
			out[connectionKey(c.RegistrationId)] = ConnectionHealth{Window: window, Unavailable: true}
			continue
		}
		out[connectionKey(c.RegistrationId)] = healthFrom(drops, now, window, len(drops) >= connectionDropReadCap)
	}
	return out
}

// hasWorkInFlight says a machine's busy refusal will clear by itself (D6): the
// cluster has admitted work on it that has not finished -- its registration's
// activeCount, flushed by the replica holding its stream at most a heartbeat
// ago, or this replica's live count when this replica holds it. Cockpit answers
// pipeline_capacity_busy for low disk and memory headroom too, which waiting
// does not fix when nothing of ours is running there.
func (d *Dispatcher) hasWorkInFlight(c Candidate) bool {
	if c.ActiveCount > 0 {
		return true
	}
	if d.registry != nil {
		if w := d.registry.WorkerById(c.RegistrationId); w != nil && w.ActiveCount() > 0 {
			return true
		}
	}
	return false
}

// passedOver is the unsteady candidates a fleet step did not try, with their
// owner-chosen names and health.
func (p RoutePlan) passedOver(rest []Candidate) []PassedOver {
	out := make([]PassedOver, 0, len(rest))
	for _, c := range rest {
		h, _ := p.healthOf(c)
		out = append(out, PassedOver{WorkerId: c.RegistrationId, MachineName: ownerChosenName(c), Health: h})
	}
	return out
}
