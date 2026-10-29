package worker

// The router: which of a user's machines gets a piece of work.
//
// WHAT IT REPLACES. Registry.PickWorker returned the first online
// registration in CONNECTION order that had the capability and matched a
// label requirement its single caller passed as nil (dispatch.go's pickWorker).
// Three things were wrong with that and each is fixed here:
//
//   - connection order is per-replica. Two agent replicas holding two streams
//     each, in whatever order the laptops woke up, would order the same fleet
//     differently and route the same work to different machines. Candidate
//     order now comes from the ROW (registration order, `sort "registeredAt"`
//     in myWorkersWithStatus), so every replica agrees.
//   - the registry is one replica's stream table, so "online" meant "connected
//     to ME". A machine held by the other replica was invisible. Online is now
//     derived from the row -- lastSeenAt within component/worker.OnlineWindow
//     and not revoked -- which is the same answer everywhere.
//   - the label matcher was never fed. The dispatch builtins had no way to
//     express a requirement, so MatchesLabels ran on nil forever.
//
// WHAT IT DELIBERATELY DOES NOT DO. It never accepts a machine id from a model.
// The builtins take requireLabels / preferLabels and nothing else (design D4),
// so a model naming a machine is not a failure mode this surface has. The worst
// a wrong label can do is empty the candidate set, which is reported as
// no_worker_available with every machine considered and the reason each was
// rejected.
//
// THE ONE NARROWING BY REGISTRATION ID IS THE ENGINE'S OWN. A pipeline step's
// retry may be pinned to the machine its last attempt ran on, or exclude the
// machines it has already tried (RoutePlan.PinRegistration /
// ExcludeRegistrations). The ids are the ones earlier attempts reported, the
// purpose is the pipeline's alone (purposeRefusal refuses either field on every
// other), and both only subtract from a plan that already enforced every
// requirement, so they choose among machines the owner's consents admit and
// never admit one.
//
// RE-PICK ONLY BEFORE SIDE EFFECTS (design D5). The loop in dispatch.go moves
// to the next candidate on `worker_busy`, on a stream that went away BEFORE
// the call started, and on a machine whose owner switched the feature off
// (refused at admission, before anything was sent). It never re-picks after
// the wire carried the dispatch: an exec that lost its stream mid-run, or was
// cancelled by the owner's switch, may have run, and running it again on
// another machine is a second side effect, not a retry.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
	fleetcatalog "github.com/znasllc-io/memql/component/worker/fleetcatalog"
	"github.com/znasllc-io/memql/core/num"
)

// Strategy values for v1:worker:routingPolicy.strategy.
const (
	StrategyFirstFit    = "firstFit"
	StrategyRoundRobin  = "roundRobin"
	StrategyLeastLoaded = "leastLoaded"
	StrategyLabelMatch  = "labelMatch"
)

// Fallback values for v1:worker:routingPolicy.fallback.
const (
	FallbackNone         = "none"
	FallbackNextMatching = "nextMatching"
)

// selectedBy values for the invocation's routing record.
const (
	SelectedByPolicy        = "policy"
	SelectedByReroute       = "reroute"
	SelectedByOnlyCandidate = "only_candidate"
)

// ReroutedFromWorkbench is the routing record's marker for a call the
// workbench refused with environment_mismatch (memql#4353).
const ReroutedFromWorkbench = "workbench"

// Candidate is the shared registration projection used by reads and dispatch.
type Candidate = fleetcatalog.Candidate

// Policy is the owner's routing policy, or DefaultPolicy when they have none.
type Policy struct {
	Id            string
	Strategy      string
	RequireLabels map[string]string
	PreferLabels  map[string]string
	Fallback      string
	// ModelPreference is an explicit ordered list of model ids, consulted
	// when a policy names `fleet:*` (epic memql#5096, design D5). Empty for
	// most users, and that is why the default size ordering has to be good
	// on its own rather than a fallback nobody exercises.
	//
	// It orders; it does not filter. A model absent from the list is still
	// eligible, it is simply tried after every model the list names --
	// because a preference that silently removed a model would take the
	// fleet's only tool-capable one out of every tool turn.
	ModelPreference []string
}

// DefaultPolicy is what a user who never opened the Fleet page gets: the
// pre-policy behaviour plus a re-pick. firstFit IS what Registry.PickWorker
// did, so an unconfigured fleet routes exactly as it did before this epic --
// which is the property that makes shipping the router safe.
func DefaultPolicy() Policy {
	return Policy{Strategy: StrategyFirstFit, Fallback: FallbackNextMatching}
}

// Normalize fills blanks and rejects values the enum does not carry. An
// unrecognised strategy falls back to firstFit rather than failing the call:
// the policy row is enum-constrained at the concept, so a bad value here means
// the row predates a value or was written by something that should not have,
// and refusing the user's work over it would be the wrong trade.
func (p Policy) Normalize(logger *slog.Logger) Policy {
	switch p.Strategy {
	case StrategyFirstFit, StrategyRoundRobin, StrategyLeastLoaded, StrategyLabelMatch:
	default:
		if logger != nil && strings.TrimSpace(p.Strategy) != "" {
			logger.Warn("worker router: unknown routing strategy, using firstFit",
				"strategy", p.Strategy, "policy_id", p.Id)
		}
		p.Strategy = StrategyFirstFit
	}
	switch p.Fallback {
	case FallbackNone, FallbackNextMatching:
	default:
		p.Fallback = FallbackNextMatching
	}
	return p
}

// RoutingRecord is what lands on v1:worker:invocation.routing. It is the
// answer to "why did this run there", asked after the fact by a person looking
// at a machine's activity list and by whoever tunes a finer policy later.
type RoutingRecord struct {
	PolicyId             string
	Strategy             string
	CandidatesConsidered []string
	Attempts             int
	SelectedBy           string
	ReroutedFrom         string
	RequireLabels        map[string]string
	PreferLabels         map[string]string
	// Rejected explains, per machine, why it was not a candidate. Present
	// even -- especially -- when the candidate list is empty: a
	// no_worker_available with nothing in it says only that the router found
	// nothing, and the question a person actually has is which of their four
	// machines was ruled out for what.
	Rejected map[string]string
}

// AsMap renders the record for the mutation's `routing object` argument.
// Empty and zero members are omitted so a row carries what happened rather
// than a fixed skeleton.
func (r RoutingRecord) AsMap() map[string]any {
	out := map[string]any{}
	if r.PolicyId != "" {
		out["policyId"] = r.PolicyId
	}
	if r.Strategy != "" {
		out["strategy"] = r.Strategy
	}
	if len(r.CandidatesConsidered) > 0 {
		out["candidatesConsidered"] = r.CandidatesConsidered
	}
	if r.Attempts > 0 {
		out["attempts"] = r.Attempts
	}
	if r.SelectedBy != "" {
		out["selectedBy"] = r.SelectedBy
	}
	if r.ReroutedFrom != "" {
		out["reroutedFrom"] = r.ReroutedFrom
	}
	if len(r.RequireLabels) > 0 {
		out["requireLabels"] = r.RequireLabels
	}
	if len(r.PreferLabels) > 0 {
		out["preferLabels"] = r.PreferLabels
	}
	if len(r.Rejected) > 0 {
		out["rejected"] = r.Rejected
	}
	return out
}

// FleetStore is the router's read/write surface on the graph. EngineStore
// implements it; tests supply a fake.
type FleetStore interface {
	// WorkersForOwner returns every machine the owner registered, in
	// REGISTRATION ORDER. The order is part of the contract, not an
	// implementation detail: firstFit is this order and the other strategies
	// break their ties with it.
	WorkersForOwner(ctx context.Context, ownerUserId string) ([]Candidate, error)
	// RoutingPolicyForOwner returns the owner's active policy, or nil when
	// they have none -- which is the common case and not an error.
	RoutingPolicyForOwner(ctx context.Context, ownerUserId string) (*Policy, error)
	// TouchWorkerSelected stamps lastSelectedAt, which is what makes
	// roundRobin a rotation two replicas agree on with no shared counter.
	TouchWorkerSelected(ctx context.Context, registrationId, ownerUserId string) error
}

// Router turns "this owner, this capability, these needs" into an ordered list
// of machines to try.
type Router struct {
	store  FleetStore
	logger *slog.Logger
	clock  func() time.Time
	// groups resolves an acting person's ACTIVE groups, for a machine lent to
	// a group (epic memql#5344). Nil is workerservice.InstalledGroups.
	groups workerservice.GroupResolver
}

// SetGroupResolver replaces how an acting person's groups are read. Tests use
// it to say who is in which group; nil restores the installed membership
// source. Not safe to call while the router is planning.
func (r *Router) SetGroupResolver(fn workerservice.GroupResolver) {
	if r != nil {
		r.groups = fn
	}
}

// NewRouter constructs a Router. A nil store makes Plan return an empty plan
// with a stated reason rather than panicking -- the dispatcher then reports
// no_worker_available, which is the truthful answer for a node with no read
// path to the fleet.
func NewRouter(store FleetStore, logger *slog.Logger, clock func() time.Time) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Router{store: store, logger: logger, clock: clock}
}

// RoutePlan is the router's answer.
type RoutePlan struct {
	Policy     Policy
	Candidates []Candidate
	// OfflineCandidates satisfy capability and label requirements but have no
	// stream. They are never dispatched; pipeline admission may wait for their
	// connection after applying the same repository, contract and platform gates.
	OfflineCandidates []Candidate
	Require           map[string]string
	Prefer            map[string]string
	Rejected          map[string]string
	// Total is how many machines the owner has registered at all, before any
	// filtering. It separates "you have no machines" from "none of your four
	// matched", which are different problems with different fixes.
	Total int
}

// Record seeds a RoutingRecord from the plan. The dispatcher fills in
// Attempts / SelectedBy / ReroutedFrom as it walks the candidates.
func (p RoutePlan) Record() RoutingRecord {
	ids := make([]string, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		ids = append(ids, c.RegistrationId)
	}
	return RoutingRecord{
		PolicyId:             p.Policy.Id,
		Strategy:             p.Policy.Strategy,
		CandidatesConsidered: ids,
		RequireLabels:        p.Require,
		PreferLabels:         p.Prefer,
		Rejected:             p.Rejected,
	}
}

// Plan resolves the policy, filters the owner's machines to those that can
// take this call, and orders them by the strategy.
func (r *Router) Plan(
	ctx context.Context,
	ownerUserId string,
	capability string,
	require map[string]string,
	prefer map[string]string,
) (RoutePlan, error) {
	if r == nil || r.store == nil {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: no fleet store configured")
	}
	if strings.TrimSpace(ownerUserId) == "" {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: ownerUserId is required")
	}

	policy := DefaultPolicy()
	if stored, err := r.store.RoutingPolicyForOwner(ctx, ownerUserId); err != nil {
		// A policy read that failed must not silently become "the default".
		// It is logged and the default IS applied, because refusing the
		// user's work over an unreadable preference is worse than routing it
		// the way it was routed before policies existed -- but the log line
		// is what stops that being invisible.
		r.logger.Warn("worker router: routing policy read failed, applying the default",
			"owner_user_id", ownerUserId, "error", err)
	} else if stored != nil {
		policy = *stored
	}
	policy = policy.Normalize(r.logger)

	// The requirement is the UNION, and the union is an AND: the policy's
	// labels narrow, the agent's labels narrow, and neither can widen the
	// other. A policy cannot make a machine eligible for work the agent did
	// not ask to run there, and an agent cannot escape a requirement the
	// owner set.
	merged, conflicts := unionLabels(policy.RequireLabels, require)
	// preferLabels only ORDER, so a disagreement between the policy's and the
	// agent's is not a conflict -- it is two hints, and both are honoured by
	// counting hits. Only the requirement can be unsatisfiable.
	preferred, _ := unionLabels(policy.PreferLabels, prefer)

	if len(conflicts) > 0 {
		// No candidate set to compute: the requirement cannot be met by any
		// machine, present or future. Returned as an empty plan with the reason
		// on it rather than as an error, so the dispatcher records the same
		// no_worker_available it records for every other empty candidate set --
		// with a message that says which two demands disagree.
		return RoutePlan{
			Policy:   policy,
			Require:  merged,
			Prefer:   preferred,
			Rejected: map[string]string{"(requirement)": "unsatisfiable: " + strings.Join(conflicts, "; ")},
		}, nil
	}

	all, err := r.store.WorkersForOwner(ctx, ownerUserId)
	if err != nil {
		return RoutePlan{Policy: policy, Require: merged, Prefer: preferred},
			fmt.Errorf("worker router: read fleet: %w", err)
	}

	kept := make([]Candidate, 0, len(all))
	var offline []Candidate
	rejected := map[string]string{}
	for _, c := range all {
		switch {
		case !c.RevokedAt.IsZero():
			rejected[c.RegistrationId] = "revoked"
		case !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt):
			// No holding replica: not eligible for Ask/dispatch. A fresh
			// lastSeenAt without connectedNodeId is exactly the false-ready
			// shape StreamHeld exists to refuse.
			rejected[c.RegistrationId] = "offline"
			if c.SupportsCapability(capability) && satisfiesMachineRequirements(c, merged) {
				offline = append(offline, c)
			}
		case !c.SupportsCapability(capability):
			rejected[c.RegistrationId] = "missing capability " + capability
		case !satisfiesMachineRequirements(c, merged):
			rejected[c.RegistrationId] = "labels do not satisfy " + formatLabels(merged)
		default:
			kept = append(kept, c)
		}
	}

	orderCandidates(kept, policy, preferred, capability)

	return RoutePlan{
		Policy:            policy,
		Candidates:        kept,
		OfflineCandidates: offline,
		Require:           merged,
		Prefer:            preferred,
		Rejected:          rejected,
		Total:             len(all),
	}, nil
}

// orderCandidates sorts in place. Every sort is STABLE over the input, which
// arrives in registration order -- that is what makes firstFit "registration
// order" and gives every other strategy a deterministic tie-break.
func orderCandidates(in []Candidate, policy Policy, prefer map[string]string, capability string) {
	switch policy.Strategy {
	case StrategyRoundRobin:
		// Oldest lastSelectedAt first. A machine never selected has the zero
		// time and therefore sorts first, so a newly paired machine gets work
		// before the rotation settles. This is the whole reason roundRobin is
		// expressed as a timestamp rather than a counter: two agent replicas
		// reading the same row reach the same answer, and no shared state has
		// to exist for them to agree.
		sort.SliceStable(in, func(i, j int) bool {
			return in[i].LastSelectedAt.Before(in[j].LastSelectedAt)
		})
	case StrategyLeastLoaded:
		sort.SliceStable(in, func(i, j int) bool {
			li, lj := loadRatio(in[i], capability), loadRatio(in[j], capability)
			if li != lj {
				return li < lj
			}
			// Tie-break on absolute load. Without it every machine that
			// declared no concurrency cap sits at ratio 0 forever and the
			// first one takes everything -- "least loaded" that never moves.
			return in[i].ActiveCount < in[j].ActiveCount
		})
	case StrategyLabelMatch:
		sort.SliceStable(in, func(i, j int) bool {
			return preferHits(in[i].Labels, prefer) > preferHits(in[j].Labels, prefer)
		})
	default: // firstFit -- registration order, already the input order.
	}
}

// loadRatio is activeCount against the capability's cap. A cap of 0 means the
// machine declared no limit, and an unlimited machine is reported as unloaded:
// it asked for no ceiling, so the ceiling is not the thing to ration it by.
// The absolute-count tie-break in orderCandidates is what keeps that from
// pinning all work to one uncapped machine.
func loadRatio(c Candidate, capability string) float64 {
	cap := uint32(0)
	if c.Concurrency != nil {
		cap = c.Concurrency[capability]
	}
	if cap == 0 {
		return 0
	}
	return float64(c.ActiveCount) / float64(cap)
}

// preferHits counts how many preferred labels a machine carries.
func preferHits(have, prefer map[string]string) int {
	n := 0
	for k, v := range prefer {
		if got, ok := have[k]; ok && got == v {
			n++
		}
	}
	return n
}

// satisfiesLabels is an exact-match AND over the requirement. An empty
// requirement matches everything.
func satisfiesLabels(have, require map[string]string) bool {
	if len(require) == 0 {
		return true
	}
	for k, v := range require {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// MergeLabels overlays the owner's operatorLabels on the cockpit's labels.
// OPERATOR WINS, and the direction matters: `labels` is rewritten from the
// Register message on every reconnect, so the machine gets the last word on
// facts about itself (os, arch, hostname) while the owner gets the last word
// on anything they deliberately set.
func MergeLabels(cockpit, operator map[string]string) map[string]string {
	return fleetcatalog.MergeLabels(cockpit, operator)
}

// unionLabels ANDs two requirements, and reports whether the result is
// satisfiable at all.
//
// On a key both sides name with DIFFERENT values there is no machine that is
// both: the owner's policy said "only where k=a" and the agent said "only where
// k=b". Silently preferring either side would run the work somewhere the other
// excluded, so the conflict is reported rather than resolved.
//
// It returns a flag rather than writing an unmatchable sentinel value into the
// map. A sentinel has to be a string no real label can equal, which is a
// property nothing enforces -- and it would then be RENDERED, into the
// no_worker_available message a person reads and into the invocation's routing
// record. The conflicting keys come back so both can be named.
func unionLabels(a, b map[string]string) (merged map[string]string, conflicts []string) {
	if len(a) == 0 && len(b) == 0 {
		return nil, nil
	}
	out := make(map[string]string, max(len(a), len(b)))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if existing, ok := out[k]; ok && existing != v {
			conflicts = append(conflicts, k+"="+existing+" vs "+k+"="+v)
			continue
		}
		out[k] = v
	}
	sort.Strings(conflicts)
	return out, conflicts
}

// formatLabels renders a requirement for an error message or a consent card,
// sorted so the text is stable across calls.
func formatLabels(m map[string]string) string {
	if len(m) == 0 {
		return "(no requirements)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// LabelsFromArgs reads a requireLabels / preferLabels builtin argument. Values
// are stringified rather than type-checked away: a model that writes
// has-gpu: true means the same thing as has-gpu: "true", and refusing one
// spelling of the same requirement would be a failure the user cannot see the
// cause of.
func LabelsFromArgs(v any) map[string]string {
	// Internal approval subjects carry typed maps before their JSON round trip.
	// Preserve the same selectors before and after persistence.
	if typed, ok := v.(map[string]string); ok {
		out := make(map[string]string, len(typed))
		for key, value := range typed {
			if key = strings.TrimSpace(key); key != "" {
				out[key] = value
			}
		}
		return out
	}
	raw, ok := v.(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		switch t := val.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = fmt.Sprintf("%t", t)
		case float64:
			// JSON numbers land as float64. Render 1 as "1", not "1.000000".
			//
			// narrowing: GUARDED -- num.WholeInt64 IS the guard (memql#4779).
			if whole, ok := num.WholeInt64(t); ok {
				out[k] = fmt.Sprintf("%d", whole)
			} else {
				out[k] = fmt.Sprintf("%g", t)
			}
		case nil:
			continue
		default:
			out[k] = fmt.Sprintf("%v", t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RequireRepository narrows a plan using the machine-reported action scope.
// Missing metadata is unknown consent, and cannot be repaired by labels.
//
// THE REASONS THIS FILE GIVES FOR A PIPELINE STEP'S NARROWING (this, the contract,
// the native platform) NAME THE MACHINE BY checkRunName, not Label(). They become
// the plan's Rejected text, which noCandidateMessage puts into the dispatch's
// error message, which the fleet executor quotes into a failed step's message --
// printed in a check run that is public for a public repository. Label() falls
// back to the name the machine reported, its hostname (machine_words.go).
func (p *RoutePlan) RequireRepository(action, repository string) {
	p.filterCandidates(func(candidate Candidate) string {
		if candidate.RepositoryScopes.Accepts(action, repository) {
			return ""
		}
		return fmt.Sprintf("%s's reported repository policy does not accept %s for %s", checkRunName(candidate), repository, action)
	})
}

// RequireActionContract excludes older implementations that might silently
// ignore newer request fields. Operator labels cannot override this metadata.
//
// An agent's computer-use calls narrow by it too, and the same wording serves
// them: a machine its owner never named is "machine 3f9c2ab0" in their
// rejection as well, with the full registration id beside it in the message.
func (p *RoutePlan) RequireActionContract(action string, version int) {
	p.filterCandidates(func(candidate Candidate) string {
		if candidate.ActionContracts.Supports(action, version) {
			return ""
		}
		return fmt.Sprintf("%s has not reported %s contract %d", checkRunName(candidate), action, version)
	})
}

// RequireHostCommands drops machines whose owner switched "Host commands" off in
// Fleet settings, naming the setting as the reason so the routing record and
// no_worker_available say what to turn back on.
//
// The row can be up to a heartbeat behind the owner's click, so this is the
// router's early skip and not the guarantee: the replica holding the machine's
// stream reads the setting again at admission and refuses before anything is
// sent (component/worker RefusedBeforeSendError), which the dispatcher treats as
// a reason to move on. It narrows the machines waiting for a connection as well:
// a pipeline must not keep waiting for a machine its owner has told not to run
// commands.
func (p *RoutePlan) RequireHostCommands() {
	p.filterCandidates(func(candidate Candidate) string {
		if candidate.HostCommandsOff {
			return "Host commands are off in Fleet settings"
		}
		return ""
	})
}

// RequireNativePlatform uses build metadata, never an operator's placement label.
// Containers use the Docker daemon's live platform check instead: a Mac may
// serve Linux containers, and the daemon may differ from the worker host.
func (p *RoutePlan) RequireNativePlatform(platform string) {
	p.filterCandidates(func(candidate Candidate) string {
		if platform != "" && candidate.NativePlatform == platform {
			return ""
		}
		return fmt.Sprintf("%s has not reported native platform %s", checkRunName(candidate), platform)
	})
}

// PinRegistration keeps only the machine a same-machine retry is pinned to and
// turns every other away as "not the machine this retry is pinned to".
//
// It subtracts from the plan and never looks a machine up, which is what keeps
// a pin from being a way round a consent. A pinned machine the requirements
// ruled out (revoked, labels, contract, Host commands off) is in neither set and
// keeps its own reason in Rejected, so the dispatch reports THAT; one that is
// eligible but disconnected stays in OfflineCandidates alone, which is what lets
// the dispatcher tell a pipeline to wait for it (Result.WaitForConnection) and
// for no other machine.
//
// The id is matched as sameRegistration does, because a registration id is
// spelled canonically on one side of a seam and bare on the other. A blank id
// matches no machine: a pin that lost its value must not become "any machine".
func (p *RoutePlan) PinRegistration(id string) {
	p.filterCandidates(func(candidate Candidate) string {
		if sameRegistration(candidate.RegistrationId, id) {
			return ""
		}
		return "not the machine this retry is pinned to"
	})
}

// ExcludeRegistrations drops the machines a step has already been tried on,
// naming "already tried in this step" as the reason, so an attempt that must
// move does not come back to a machine it left. It narrows the machines waiting
// for a connection as well: a pipeline must not wait for a machine it has
// already tried.
//
// Ids match as in PinRegistration, since an exclusion that failed to match would
// send the retry back to the machine it meant to avoid. A blank id names no
// machine and excludes none.
func (p *RoutePlan) ExcludeRegistrations(ids []string) {
	if len(ids) == 0 {
		return
	}
	p.filterCandidates(func(candidate Candidate) string {
		for _, id := range ids {
			if sameRegistration(candidate.RegistrationId, id) {
				return "already tried in this step"
			}
		}
		return ""
	})
}

// A disconnected worker cannot keep a pipeline waiting after consent or its
// binary contract is withdrawn. Apply every narrowing to both sets.
func (p *RoutePlan) filterCandidates(reason func(Candidate) string) {
	if p.Rejected == nil {
		p.Rejected = map[string]string{}
	}
	filter := func(candidates []Candidate) []Candidate {
		kept := make([]Candidate, 0, len(candidates))
		for _, candidate := range candidates {
			if why := reason(candidate); why != "" {
				p.Rejected[candidate.RegistrationId] = why
			} else {
				kept = append(kept, candidate)
			}
		}
		return kept
	}
	p.Candidates = filter(p.Candidates)
	p.OfflineCandidates = filter(p.OfflineCandidates)
}
