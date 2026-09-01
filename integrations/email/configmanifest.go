package email

// configmanifest.go -- a DECLARED configuration surface for an integration
// (memql#4825).
//
// # What this replaces
//
// The email lane's configuration was written down twice. lazy.go knew which
// values to read and which of them a lane cannot start without; status.go
// knew the same list a second time, in a different shape, in order to report
// it. The duplication was DELIBERATE and the reason survives here unchanged:
// a status read must not call the real resolver, because that resolver caches
// its answer behind a sync.Once on the first Send -- so reporting through it
// would either trigger resolution early or answer from a cache that predates
// a credential the operator has since seeded.
//
// But "the reporter must walk the tiers itself" is a statement about the WALK.
// It was never a reason for the two to hold separate copies of the SLOT LIST,
// and while they did, the two could disagree about which values exist, which
// are secret and which a lane requires -- a disagreement that surfaces as a
// console showing four green slots beside a sender that never resolved.
//
// So the list becomes a declaration, and both consume it. The reporter still
// resolves independently; it just no longer decides for itself what there is
// to resolve.
//
// # What a second integration has to supply
//
// Nothing in this file mentions email. To grow the same surface, an
// integration supplies exactly one value -- a ConfigManifest -- and gets the
// resolution ladder, the lane-completeness rule and the machine-readable
// state for free:
//
//	 1. Declare the SLOTS: for each value the integration reads, a stable
//	    machine Name, the EnvVar that supplies it, an optional Legacy alias
//	    for a rename window, operator-facing Purpose prose, whether it is
//	    Secret (sealed; its value must never leave the process) and whether
//	    it is Required for its lane to work at all.
//	 2. Group them into LANES. A lane is one complete way to be configured
//	    -- Graph or SMTP for email, and typically just one for anything
//	    simpler. Lanes are tried in declared order, so declare the preferred
//	    one first.
//	 3. Call Resolve with a ConfigResolver carrying whichever tiers apply
//	    (env alone is fine; the two row resolvers arrive from PluginContext).
//	 4. Build the concrete client from the returned ConfigResolution's Active
//	    lane, and hand the same resolution to the status reporter.
//
// What it must NOT do is fail boot when nothing resolved. An unconfigured
// integration is a normal state of a fresh cluster; the honest behaviour is
// that its FEATURES refuse with the stated reason, so the operator learns it
// from the surface that needed it rather than from a pod that will not start.
// (The email lane's boot refusal is a deliberate and narrow exception -- see
// delivery.go: log-only mail did not refuse, it reported SUCCESS, which is
// the one failure worse than not booting.)

import (
	"context"
	"fmt"
	"strings"
)

// ConfigSlot is one configurable value an integration reads.
type ConfigSlot struct {
	// Name is the stable machine identifier a surface keys on. Not the env
	// var: an operator can rename the variable behind a slot, and a client
	// that keyed on the variable would silently lose the value it was
	// rendering.
	Name string
	// EnvVar is the canonical environment variable, and also the key the
	// row tiers are looked up under -- one name for one value, so an
	// operator moving a setting from the environment into a globalVariable
	// row does not have to learn a second spelling.
	EnvVar string
	// Legacy is a pre-rename alias, consulted after EnvVar in every tier.
	// Empty when the slot has never been renamed.
	Legacy string
	// Purpose is operator-facing prose, rendered beside the slot. It says
	// what the value DOES and what changing it costs -- never how to set it,
	// which the surface already knows.
	Purpose string
	// Secret marks a sealed value. Its resolved value must never appear in
	// anything a client can read: report presence, source and a rotation
	// command instead.
	Secret bool
	// Required marks a slot the lane cannot work without. An optional slot
	// that is unset is a normal state and never a reason.
	Required bool
}

// ConfigLane is one complete way to configure an integration -- a set of
// slots that work together and are useless apart.
//
// A lane must resolve WHOLE, from ONE tier. That rule is not fussiness: a
// resolver that took a tenant id from the environment and a client secret
// from a stored row would produce a credential pair nobody configured, and
// the state that produces it (half a lane migrated) is exactly the state an
// operator is in mid-migration.
type ConfigLane struct {
	// Name is the stable machine identifier for the lane ("graph", "smtp").
	Name string
	// Description is one sentence naming what this lane IS, for a surface
	// offering a choice between lanes.
	Description string
	Slots       []ConfigSlot
}

// ConfigManifest is an integration's whole configuration surface.
type ConfigManifest struct {
	// Integration is the plug-in name this describes.
	Integration string
	// Lanes in PREFERENCE ORDER. The first lane that resolves whole wins.
	Lanes []ConfigLane
}

// Slots flattens every lane's slots in declared order.
func (m ConfigManifest) Slots() []ConfigSlot {
	out := make([]ConfigSlot, 0, 8)
	for _, lane := range m.Lanes {
		out = append(out, lane.Slots...)
	}
	return out
}

// Lane returns the named lane.
func (m ConfigManifest) Lane(name string) (ConfigLane, bool) {
	for _, lane := range m.Lanes {
		if lane.Name == name {
			return lane, true
		}
	}
	return ConfigLane{}, false
}

// Required returns the names of the lane's slots that must resolve.
func (l ConfigLane) Required() []string {
	out := make([]string, 0, len(l.Slots))
	for _, slot := range l.Slots {
		if slot.Required {
			out = append(out, slot.Name)
		}
	}
	return out
}

// ConfigResolver supplies the tiers a slot is looked up in, in order:
// environment, then the two global row stores. Any of them may be nil; a nil
// tier is simply skipped, which is what makes the same machinery usable from
// a plug-in factory (all three) and from a bare process (env alone).
type ConfigResolver struct {
	// Env reads an OS environment variable. Nil means "do not consult the
	// environment", which is a legitimate configuration and not a default.
	Env func(name string) (string, bool)
	// Vars resolves a v1:platform:globalVariable row.
	Vars VariableResolver
	// Secrets resolves a v1:platform:globalSecret row.
	Secrets SecretResolver
}

// SlotResolution is one slot's resolved value and where it came from.
//
// It CARRIES the value, including a secret one, because the resolver's own
// caller needs it to build a client. Nothing here decides what a report may
// contain -- that is the reporter's job, and it is why Secret travels on the
// slot rather than being inferred downstream.
type SlotResolution struct {
	Slot   ConfigSlot
	Value  string
	Source string
}

// LaneResolution is one lane's verdict.
type LaneResolution struct {
	Lane  ConfigLane
	Slots []SlotResolution
	// Complete reports that every required slot resolved AND did so from one
	// tier. False with an empty Missing means the lane resolved but was
	// SPLIT -- a distinction a surface has to render, because "you are
	// missing a client secret" and "your client secret is in the wrong
	// place" have different fixes.
	Complete bool
	// Source is the tier a complete lane resolved from.
	Source string
	// Missing names the required slots that resolved nowhere.
	Missing []string
	// Split is true when every required slot resolved but not from one tier.
	Split bool
	// Partial is true when SOMETHING resolved and something required did
	// not -- somebody started configuring this lane and stopped. Distinct
	// from an untouched lane, which is silence and never a complaint: on a
	// Graph install the SMTP lane is empty on purpose, and reporting it
	// would put a permanent amber warning on every correctly configured
	// deployment.
	Partial bool
}

// Configured reports whether anything at all resolved in this lane.
func (l LaneResolution) Configured() bool {
	for _, slot := range l.Slots {
		if slot.Source != SourceUnset {
			return true
		}
	}
	return false
}

// ConfigResolution is the whole answer.
type ConfigResolution struct {
	Integration string
	Lanes       []LaneResolution
	// Active names the winning lane, or "" when none resolved whole.
	Active string
	// ActiveSource is the tier the winning lane came from.
	ActiveSource string
}

// Slot returns a resolved slot by name, across every lane.
func (r ConfigResolution) Slot(name string) (SlotResolution, bool) {
	for _, lane := range r.Lanes {
		for _, slot := range lane.Slots {
			if slot.Slot.Name == name {
				return slot, true
			}
		}
	}
	return SlotResolution{}, false
}

// Value returns a resolved slot's value, or "".
func (r ConfigResolution) Value(name string) string {
	slot, _ := r.Slot(name)
	return slot.Value
}

// ActiveLane returns the winning lane's resolution.
func (r ConfigResolution) ActiveLane() (LaneResolution, bool) {
	for _, lane := range r.Lanes {
		if lane.Lane.Name == r.Active && r.Active != "" {
			return lane, true
		}
	}
	return LaneResolution{}, false
}

// ResolveSlot walks the tiers for one slot and reports (value, source).
//
// Order is environment, then rows, and within each tier the canonical name
// then the legacy alias. Env first because it is the bootstrap envelope and
// a deployment that sets a value there has decided; a stored row that
// silently overrode it would make the deployment's own configuration a
// suggestion.
func (r ConfigResolver) ResolveSlot(ctx context.Context, slot ConfigSlot) (string, string) {
	names := []string{slot.EnvVar, slot.Legacy}
	if r.Env != nil {
		for _, name := range names {
			if name == "" {
				continue
			}
			if v, ok := r.Env(name); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), SourceEnv
			}
		}
	}
	// Declared as the bare function type rather than as VariableResolver:
	// the two named types are structurally identical and deliberately
	// distinct at their declaration sites, so the tier that answers a slot is
	// chosen HERE, once, off slot.Secret. A secret slot must never be looked
	// up in the plaintext store -- that is what would put a client secret in
	// a globalVariable row a client can read.
	var lookup func(context.Context, string) (string, error)
	source := SourceGlobalVariable
	if slot.Secret {
		if r.Secrets != nil {
			lookup = r.Secrets
		}
		source = SourceGlobalSecret
	} else if r.Vars != nil {
		lookup = r.Vars
	}
	if lookup == nil {
		return "", SourceUnset
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if v, err := lookup(ctx, name); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), source
		}
	}
	return "", SourceUnset
}

// Resolve walks every lane in the manifest and picks the winner.
//
// # The two-pass shape, and why it is not a simplification waiting to happen
//
// It resolves every lane's slots first, then chooses -- rather than stopping
// at the first complete lane. A surface has to render the lanes that did NOT
// win: an operator whose Graph credentials are half-seeded needs to see which
// half, and a first-match loop would report the SMTP lane's silence as the
// whole story.
//
// Lane choice is by TIER first and declaration order second: every lane is
// tried against the environment before any lane is tried against the stored
// rows. That mirrors what the sender resolver does and it is the behaviour
// that matters -- an operator who exports SMTP_HOST for an afternoon expects
// it to win over a Graph credential seeded into rows months ago, because the
// environment is the thing they just changed.
func (m ConfigManifest) Resolve(ctx context.Context, r ConfigResolver) ConfigResolution {
	out := ConfigResolution{Integration: m.Integration}

	for _, lane := range m.Lanes {
		resolved := LaneResolution{Lane: lane, Slots: make([]SlotResolution, 0, len(lane.Slots))}
		sources := map[string]string{}
		for _, slot := range lane.Slots {
			value, source := r.ResolveSlot(ctx, slot)
			resolved.Slots = append(resolved.Slots, SlotResolution{Slot: slot, Value: value, Source: source})
			sources[slot.Name] = source
		}
		for _, name := range lane.Required() {
			if sources[name] == SourceUnset {
				resolved.Missing = append(resolved.Missing, name)
			}
		}
		switch {
		case len(resolved.Missing) > 0:
			// Neither complete nor split. It is PARTIAL only if somebody had
			// started -- an untouched lane is silence.
			resolved.Partial = resolved.Configured()
		case laneSourced(sources, lane.Required(), SourceEnv):
			resolved.Complete, resolved.Source = true, SourceEnv
		case laneSourced(sources, lane.Required(), SourceGlobalVariable, SourceGlobalSecret):
			resolved.Complete, resolved.Source = true, SourceGlobalVariable
		default:
			resolved.Split = true
		}
		out.Lanes = append(out.Lanes, resolved)
	}

	for _, tier := range []string{SourceEnv, SourceGlobalVariable} {
		for _, lane := range out.Lanes {
			if lane.Complete && lane.Source == tier {
				out.Active, out.ActiveSource = lane.Lane.Name, lane.Source
				return out
			}
		}
	}
	return out
}

// laneSourced reports whether every named slot resolved from one of the
// allowed tiers.
func laneSourced(sources map[string]string, required []string, allowed ...string) bool {
	for _, name := range required {
		match := false
		for _, a := range allowed {
			if sources[name] == a {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// --- the machine-readable verdict ---------------------------------------

// Configuration states. A surface renders from these rather than from prose,
// so they are a CLOSED set and an unknown value must be treated as unknown
// rather than as any of them.
const (
	// StateNeedsConfiguration -- no lane resolved whole. Not an error: it is
	// the normal state of a fresh cluster, and the reasons say exactly what
	// is missing and where it would go.
	StateNeedsConfiguration = "needs_configuration"
	// StateConfigured -- a lane resolved whole. Says nothing about whether
	// the provider accepts the credentials, which only a probe can answer.
	StateConfigured = "configured"
	// StateUnhealthy -- configured AND known not to work: a probe failed, or
	// the integration is in a mode this install refuses.
	StateUnhealthy = "unhealthy"
)

// Reason codes. Also closed, and also rendered rather than parsed.
const (
	// ReasonMissingSlot -- a required slot resolved nowhere.
	ReasonMissingSlot = "missing_slot"
	// ReasonSplitLane -- every required slot resolved, but across tiers, so
	// the resolver takes neither. The fix is to move values, not to add one,
	// which is why it is not a missing-slot reason.
	ReasonSplitLane = "split_lane"
	// ReasonProbeFailed -- a live check reached the provider and was refused.
	ReasonProbeFailed = "probe_failed"
	// ReasonRefused -- the integration is configured in a way this install
	// refuses to run. Email's log-only-on-a-cloud-domain is the case.
	ReasonRefused = "refused"
)

// ConfigReason is one machine-readable explanation.
//
// Code is what a surface branches on; Detail is the sentence it renders;
// Slot / EnvVar / Lane are what let it highlight the field responsible. It
// NEVER carries a resolved value -- a reason about a secret that quoted the
// secret would put it on the wire, which is the one thing the status surface
// may not do.
type ConfigReason struct {
	Code   string `json:"code"`
	Lane   string `json:"lane,omitempty"`
	Slot   string `json:"slot,omitempty"`
	EnvVar string `json:"envVar,omitempty"`
	Detail string `json:"detail"`
}

// Reasons explains a resolution: why no lane won, or nothing when one did.
//
// Built per LANE rather than as one overall sentence, because an operator
// configuring Graph does not want to read about SMTP -- but a surface cannot
// know which lane they intended, so it gets both, each labelled, and decides
// what to show.
func (r ConfigResolution) Reasons() []ConfigReason {
	if r.Active != "" {
		return nil
	}
	out := []ConfigReason{}
	for _, lane := range r.Lanes {
		// An UNTOUCHED lane produces no reasons. On a Graph install the SMTP
		// lane is empty deliberately, and listing its six absent values as
		// six things to fix would bury the one that matters.
		if !lane.Configured() {
			continue
		}
		for _, name := range lane.Missing {
			slot, _ := laneSlot(lane, name)
			out = append(out, ConfigReason{
				Code:   ReasonMissingSlot,
				Lane:   lane.Lane.Name,
				Slot:   name,
				EnvVar: slot.EnvVar,
				Detail: fmt.Sprintf("%s is not set anywhere, and the %s lane cannot work without it. %s",
					slot.EnvVar, lane.Lane.Name, slot.Purpose),
			})
		}
		if lane.Partial && len(lane.Missing) == 0 {
			// Unreachable by construction (Partial implies Missing), stated
			// so the invariant is visible rather than inferred.
			continue
		}
		if lane.Split {
			out = append(out, ConfigReason{
				Code: ReasonSplitLane,
				Lane: lane.Lane.Name,
				Detail: fmt.Sprintf("Every value the %s lane needs is present, but they are not all in the same place. "+
					"A lane is taken WHOLE from the environment or WHOLE from stored settings, never mixed -- "+
					"half a migration would build a credential nobody configured. Move them together.", lane.Lane.Name),
			})
		}
	}
	return out
}

func laneSlot(lane LaneResolution, name string) (ConfigSlot, bool) {
	for _, slot := range lane.Slots {
		if slot.Slot.Name == name {
			return slot.Slot, true
		}
	}
	return ConfigSlot{}, false
}

// SlotByName finds a slot by its stable machine name.
//
// The NAME, never the env var: a surface keys on the name precisely so an
// operator can rename the variable behind a slot without every client losing
// the value it was rendering, and a lookup by variable would undo that.
func (m ConfigManifest) SlotByName(name string) (ConfigSlot, bool) {
	want := strings.TrimSpace(name)
	for _, s := range m.Slots() {
		if s.Name == want {
			return s, true
		}
	}
	return ConfigSlot{}, false
}

// SlotNames lists every configurable slot, in declared order. Used to make a
// refusal actionable -- "that is not a setting, these are" beats "unknown
// setting", and the list is one a caller cannot get wrong twice.
func (m ConfigManifest) SlotNames() []string {
	slots := m.Slots()
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.Name)
	}
	return out
}
