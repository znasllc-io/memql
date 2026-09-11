package readiness

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// WHETHER THIS CLUSTER HAS AN INFERENCE DOOR, decided from rows (design record
// docs/superpowers/specs/2026-09-07-core-gate-and-honest-install-design.md, D3).
//
// ===========================================================================
// CONFIGURATION, NOT PRESENCE -- AND BOTH, ON ONE LANE
// ===========================================================================
// The shipped `ai` evaluator asked "is a door OPEN RIGHT NOW", through the
// fleet and app seams that only an agent binary holds. Two things followed and
// both were wrong. Every other node type answered "no door", so on any
// multi-node cluster the fold saw disagreement and pinned `ai` at `partial`
// forever. And a laptop closing changed a CLUSTER'S CONFIGURATION, which is
// not what configuration means.
//
// So a door is CONFIGURED when the rows say it exists, and whether it is OPEN
// is a second fact on the same lane: `Complete` is configured, and the single
// slot `live` is presence. The gate and the wizard read `Complete`; the park
// card and the Readiness signpost keep reading `inferenceStatus`, which stays
// the live, per-caller answer.
//
// ===========================================================================
// PURE, AND IN THIS PACKAGE, BECAUSE OF THE MODULE GRAPH
// ===========================================================================
// component/worker owns the worker vocabulary and REQUIRES component/memql, so
// component/memql cannot import it -- the edge would be a cycle and the
// module-boundaries lane exists to hold that direction. The three constants
// below are therefore a second Go home for values component/worker owns,
// pinned to it by component/worker/readiness_contract_parity_test.go (which
// may import this package, the direction that is legal), and the two label
// attribute keys are pinned to the router by worker_contract_parity_test.go
// beside this file.
//
// Two copies with a gate, rather than one copy with a cycle. The alternative
// -- moving the worker vocabulary here -- would put "what a cockpit
// advertises" inside a package about configuration readiness, and would drag
// HeartbeatBatchInterval along with it.

// OnlineWindow is how stale a registration's lastSeenAt may be before its
// machine reads as offline.
//
// It MUST equal component/worker.OnlineWindow, which is 2 x
// HeartbeatBatchInterval; the gate named above fails the build if it stops
// doing so.
const OnlineWindow = 30 * time.Second

// ModelLabelPrefix and AppLabelPrefix are how a machine advertises what it
// serves. Both MUST equal component/worker's, held by the same gate.
//
// THE APP DOOR IS READ OFF THE LABEL rather than off the apps inventory, and
// that is the point: component/worker derives an `app:<id>` label only from an
// app that is known to the engine AND allowed by the machine's own policy.yaml
// AND signed in. Reading the label therefore needs no copy of the engine's
// closed runnable app set here, and a cockpit that learns a new app cannot
// make this file wrong. The inventory is still read, as the fallback for a
// registration written before labels carried apps.
const (
	ModelLabelPrefix = "model:"
	AppLabelPrefix   = "app:"
)

// MinContextWindow is the context floor a model must advertise to count as a
// door. It MUST equal memql.MinimumContextWindow; readiness_inference_test.go
// pins that, because this package cannot import its parent.
const MinContextWindow = 8192

// The lane names, in the order the default chain tries them: a model on the
// person's own hardware, then a subscription they already pay for, then
// federation. A client renders this list rather than re-deriving one, so the
// order is part of the answer.
const (
	InferenceLaneLocal      = "local"
	InferenceLaneApp        = "app"
	InferenceLaneFederation = "federation"
)

// InferenceLiveSlot is the one slot every inference lane carries.
const InferenceLiveSlot = "live"

// The two attributes the FLOOR turns on, as the `model:<id>` label value
// spells them. Pinned to integrations/agent/worker's attrContext and
// attrStructured by worker_contract_parity_test.go.
const (
	attrContextKey    = "ctx"
	attrStructuredKey = "structured"
)

// knownAppIds is the engine's closed runnable set, for the INVENTORY FALLBACK
// ONLY -- a registration whose labels predate app labels. The label path above
// needs no such list, which is why this one can afford to be a fallback rather
// than the authority.
//
// It is still a COPY of component/worker's set, and a copy with nothing holding
// it equal drifts. The authority is component/worker.KnownAppIds(), which this
// package may not import (the edge is a cycle -- see the parity test that runs
// the other way). KnownAppIds below is what lets that test compare them.
var knownAppIds = map[string]bool{"claude-code": true, "codex": true}

// KnownAppIds returns this package's copy of the closed runnable app set,
// sorted, so component/worker's parity test can hold it equal to the original.
//
// Exported FOR THE GATE, and used by nothing else here. A copy nobody can check
// is the one shape of restatement that fails silently: a third app added to
// component/worker would be routable, advertised and dispatchable while a
// registration old enough to need this fallback still reported no app door.
func KnownAppIds() []string {
	out := make([]string, 0, len(knownAppIds))
	for id := range knownAppIds {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RegistrationApp is one local app a cockpit reported.
type RegistrationApp struct {
	Id       string `json:"id"`
	SignedIn bool   `json:"signedIn"`
	Allowed  bool   `json:"allowed"`
}

// RegistrationFacts is the slice of a v1:worker:registration row this decision
// reads. Rows in, lanes out: nothing here knows about an engine.
type RegistrationFacts struct {
	OwnerUserId     string
	Labels          map[string]string
	OperatorLabels  map[string]string
	Apps            []RegistrationApp
	ConnectedNodeId string
	LastSeenAt      time.Time
	RevokedAt       time.Time
}

// online is stream-affinity liveness for Setup / readiness: a replica must
// currently hold the worker stream (connectedNodeId). A recent lastSeenAt
// alone is "saw a heartbeat somewhere" and must not mark the door live; nor
// may activeCount stubs.
//
// LastSeenAt remains a soft corroboration when present: a future timestamp
// (clock skew) is treated as inside the window, matching IsOnline.
func (r RegistrationFacts) online(now time.Time) bool {
	if !r.RevokedAt.IsZero() {
		return false
	}
	if strings.TrimSpace(r.ConnectedNodeId) == "" {
		return false
	}
	if r.LastSeenAt.IsZero() {
		// Stream held, beat not yet flushed -- still live for Setup.
		return true
	}
	return now.Sub(r.LastSeenAt) <= OnlineWindow
}

// labelMerge is the routing merge the fleet router already applies: the
// operator's side wins. An operator who pinned a `model:` label on a machine
// has said it serves that model, and routing would honour it.
func (r RegistrationFacts) labelMerge() map[string]string {
	out := make(map[string]string, len(r.Labels)+len(r.OperatorLabels))
	for k, v := range r.Labels {
		out[k] = v
	}
	for k, v := range r.OperatorLabels {
		out[k] = v
	}
	return out
}

// InferenceInput is everything the decision reads.
type InferenceInput struct {
	Registrations []RegistrationFacts
	// FederationConfigured is structural presence of a federated provider --
	// not a live token exchange. A cluster that has been told how to reach a
	// vendor is configured for it; whether the exchange succeeds right now is
	// a call's problem, not a setup one.
	//
	// "STRUCTURAL" IS SLIGHTLY MORE THAN THE IDENTITY IDS, and the difference
	// is worth stating because this comment used to claim only the ids. The
	// engine's answer (ProviderRegistry.federationConfigured) requires a
	// registered provider that is BOTH `Available` and resolves to the
	// federation auth source -- so a provider whose auth could not be resolved
	// at boot is not counted, which is stricter than the sentence above and in
	// the safe direction: it can report a configured cluster as unconfigured,
	// never the reverse.
	FederationConfigured bool
	Now                  time.Time
}

// InferenceLanes returns the three doors, ALWAYS all three, in chain order.
//
// All three always, because a client renders the list: an absent lane and an
// incomplete lane are different answers, and only the second one is true of a
// fresh cluster.
func InferenceLanes(in InferenceInput) []LaneReport {
	localConfigured, localLive := false, false
	appConfigured, appLive := false, false

	for _, reg := range in.Registrations {
		// A REVOKED REGISTRATION IS NO DOOR AT ALL. Revocation is a decision,
		// and a revoked machine still beating is the case that most needs to
		// read as gone -- not as a configured door that happens to be asleep.
		if !reg.RevokedAt.IsZero() {
			continue
		}
		live := reg.online(in.Now)
		for key, value := range reg.labelMerge() {
			if id, ok := modelIdFromLabel(key); ok && id != "" && meetsFloor(value) {
				localConfigured = true
				if live {
					localLive = true
				}
			}
			if id, ok := appIdFromLabel(key); ok && id != "" {
				appConfigured = true
				if live {
					appLive = true
				}
			}
		}
		// The inventory fallback: a registration written before app labels
		// existed. The same three conditions the label derivation applies.
		for _, app := range reg.Apps {
			if knownAppIds[strings.TrimSpace(app.Id)] && app.Allowed && app.SignedIn {
				appConfigured = true
				if live {
					appLive = true
				}
			}
		}
	}

	return []LaneReport{
		inferenceLane(InferenceLaneLocal, "fleet", localConfigured, localLive),
		inferenceLane(InferenceLaneApp, "fleet", appConfigured, appLive),
		// Federation is set in the DEPLOYMENT (the identity ids), which is
		// what `configurableFrom` names for every other env-driven lane. It is
		// configured and live together: there is no machine to be asleep and
		// no stream to be held.
		inferenceLane(InferenceLaneFederation, "deployment", in.FederationConfigured, in.FederationConfigured),
	}
}

// inferenceLane builds one lane. The live slot carries NO SOURCE, and that is
// deliberate rather than missed: `source` says which tier a VALUE was resolved
// from -- env, a stored variable, a sealed secret -- and this slot carries no
// value. Spelling it "unset" would say a person had not set something, which
// is not what a sleeping laptop means.
func inferenceLane(name, from string, configured, live bool) LaneReport {
	return LaneReport{
		Name:             name,
		ConfigurableFrom: from,
		Complete:         configured,
		Slots:            []SlotReport{{Name: InferenceLiveSlot, Present: live}},
	}
}

// InferenceConfigured is the module's verdict: ONE complete lane configures
// it, the same rule evaluateModule applies to every lane-driven module.
func InferenceConfigured(lanes []LaneReport) bool {
	for _, l := range lanes {
		if l.Complete {
			return true
		}
	}
	return false
}

// InferenceLive reports whether any configured door is open right now. Read by
// nothing that gates; carried so a surface can say "configured, and asleep"
// without re-walking the lanes.
func InferenceLive(lanes []LaneReport) bool {
	for _, l := range lanes {
		if !l.Complete {
			continue
		}
		for _, s := range l.Slots {
			if s.Name == InferenceLiveSlot && s.Present {
				return true
			}
		}
	}
	return false
}

func modelIdFromLabel(key string) (string, bool) {
	if !strings.HasPrefix(key, ModelLabelPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(key, ModelLabelPrefix)), true
}

func appIdFromLabel(key string) (string, bool) {
	if !strings.HasPrefix(key, AppLabelPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(key, AppLabelPrefix)), true
}

// meetsFloor reads the two attributes the FLOOR turns on out of a
// `model:<id>` label value, which is a flat `k=v,k=v` list.
//
// EVERY CAPABILITY DEFAULTS TO FALSE and an unparseable value yields no
// capability at all -- the same direction ParseModelAttributes takes, and for
// the same reason: a model that silently answers prose to a structured prompt
// produces a parse failure three layers away, naming nothing. A garbled value
// costs a machine eligibility; it can never grant it.
func meetsFloor(value string) bool {
	contextWindow, structured := 0, false
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case attrContextKey:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				contextWindow = n
			}
		case attrStructuredKey:
			// The spellings a machine might send, permissive in exactly one
			// direction: anything unrecognised is false, so a novel spelling
			// costs eligibility rather than granting it.
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "y":
				structured = true
			}
		}
	}
	return structured && contextWindow >= MinContextWindow
}
