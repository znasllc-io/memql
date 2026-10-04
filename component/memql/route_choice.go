package memql

// route_choice.go -- the Ask route picker's choice, from the wire to every
// model call of the run it was made for (design brief "routing in MemQL OS",
// section 6).
//
// # The three halves
//
//   - ParseRouteChoice is the GRAMMAR, closed and pure: the words the picker
//     offers and nothing else. A turn that names anything outside it is
//     refused with a code before any goal exists.
//   - GoalRouteChoice decides what a goal about to be opened carries: the
//     request's own choice or -- for a goal one of a run's steps opened --
//     that run's choice, either for the person who made it only. The work
//     integration writes it onto the goal and run ROWS.
//   - ApplyRunRouting is the MODEL SEAM: the router applies it to every
//     request that reaches it, from the RunContext the executing node built
//     off the run row. That is the only way the choice reaches a planner
//     compile and an agent reply alike: neither shares memory with the node
//     that took the turn.
//
// # What a choice does to one call
//
//   - A PIN (app:, fleet:, federation:) is the owner's explicit provider. It
//     skips every rule, and a pin that cannot serve REFUSES: nothing is
//     substituted for a source a person named. Because the pin is the owner's
//     own (PinnedBy), an app door reached through it passes the app gate on
//     their machine.
//   - A ROUTE (policy:<name>) replaces the rule's chain and FAILS OVER along
//     it like any chain -- which is how a planner that cannot reach an app
//     door yet still answers, from the next entry.
//   - The LEVEL binds the calls the run's STEPS make -- the reply, a compose.
//     Calls made while compiling (triage, the compile pass) and the failure
//     classifier run outside any step and keep the level they declare; they
//     still go where the person said.
//
// A person's override for ONE step (ai_step_override.go) is more specific than
// their choice for the whole run, so its model and its level each win over the
// run's. Embeddings, vision and audio are left alone: an embedding must come
// from the embedder of the index it is written into (D10), and an image or a
// voice has a capability floor that a conversation's source says nothing about.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// RouteSourcePolicyPrefix names a ROUTE in a choice's Source: `policy:<name>`.
const RouteSourcePolicyPrefix = "policy:"

// The refusal codes of a route choice. STABLE STRINGS: the gRPC handler puts
// one in the error's metadata and a client branches on it.
const (
	// RouteSourceInvalid: the source is not one of the picker's words.
	RouteSourceInvalid = "route_source_invalid"
	// RouteLevelInvalid: the level is not fast, strong or reasoning.
	RouteLevelInvalid = "route_level_invalid"
	// RoutePolicyUnknown: the source names a route this cluster does not have.
	RoutePolicyUnknown = "route_policy_unknown"
)

// RouteChoiceError is a refused choice. Error() LEADS WITH THE CODE, the
// contract every refusal in the routing seam keeps.
type RouteChoiceError struct {
	// Code is one of the Route* codes above.
	Code string
	// Field is the half of the choice refused: "source" or "level".
	Field string
	// Value is what was asked for, verbatim.
	Value string
	// Reason says why, in words a person can act on.
	Reason string
}

func (e *RouteChoiceError) Error() string {
	if e == nil {
		return RouteSourceInvalid
	}
	return e.Code + ": " + e.Reason
}

// federationChoices are the vendor selectors the picker offers. They are the
// router's own words (component/router's FederationSelector*), spelled here
// because the router imports this package and not the other way round; the
// router's tests assert the two agree.
var federationChoices = map[string]bool{"cheapest": true, "strongest": true}

// ParseRouteChoice accepts exactly the picker's vocabulary:
//
//	source: "" | app:claude-code | app:codex | app:* | fleet:strongest |
//	        fleet:fastest | fleet:<modelId> | federation:cheapest |
//	        federation:strongest | policy:<name>
//	level:  "" | fast | strong | reasoning
//
// Anything else is a *RouteChoiceError. It does not know which routes a
// cluster has -- that is CheckRouteChoice's -- and it never stamps By: who
// chose is the server's to say.
func ParseRouteChoice(source, level string) (common.RouteChoice, error) {
	source, level = strings.TrimSpace(source), strings.TrimSpace(level)
	if err := routeSourceRefusal(source); err != nil {
		return common.RouteChoice{}, err
	}
	if level != "" {
		parsed, err := airoute.ParseLevel(level)
		if err != nil || parsed == airoute.LevelEmbeddings {
			return common.RouteChoice{}, &RouteChoiceError{
				Code: RouteLevelInvalid, Field: "level", Value: level,
				Reason: fmt.Sprintf("level %q is not one of fast, strong or reasoning (or empty for each call's own)", level),
			}
		}
	}
	return common.RouteChoice{Source: source, Level: level}, nil
}

func routeSourceRefusal(source string) error {
	refuse := func(why string) error {
		return &RouteChoiceError{Code: RouteSourceInvalid, Field: "source", Value: source,
			Reason: fmt.Sprintf("source %q %s", source, why)}
	}
	switch {
	case source == "":
		return nil
	case strings.ContainsAny(source, " \t\r\n"):
		return refuse("contains whitespace")
	case strings.HasPrefix(source, AppReferencePrefix):
		appId, model, ok := SplitAppReference(source)
		if !ok || model != "" || (appId != AppWildcardId && !airoute.IsRunnableApp(appId)) {
			return refuse(fmt.Sprintf("is not an app this engine drives: choose app:*, or one of app:%s",
				strings.Join(airoute.RunnableApps(), ", app:")))
		}
		return nil
	case strings.HasPrefix(source, FleetReferencePrefix):
		if _, ok := IsFleetReference(source); !ok || IsFleetWildcard(source) {
			return refuse("names no model: choose fleet:strongest, fleet:fastest or fleet:<modelId>")
		}
		return nil
	case strings.HasPrefix(source, "federation:"):
		if !federationChoices[strings.TrimPrefix(source, "federation:")] {
			return refuse("is not a vendor choice: choose federation:cheapest or federation:strongest")
		}
		return nil
	case strings.HasPrefix(source, RouteSourcePolicyPrefix):
		if !policyNamePattern.MatchString(strings.TrimPrefix(source, RouteSourcePolicyPrefix)) {
			return refuse("does not name a route: policy:<name>, the name a letter followed by letters and digits")
		}
		return nil
	}
	return refuse("is not a source: leave it empty for Auto, or choose an app:, fleet:, federation: or policy: source")
}

// routePolicyName returns the route a source names, when it names one.
func routePolicyName(source string) (string, bool) {
	name, ok := strings.CutPrefix(strings.TrimSpace(source), RouteSourcePolicyPrefix)
	return strings.TrimSpace(name), ok
}

// CheckRouteChoice refuses a route this cluster does not have, reading the
// routing configuration the router itself reads (the shipped policies plus the
// owner's saved ones). Every other source is a question about machines and
// keys that changes minute to minute, and is answered when a call is made --
// by a refusal naming the door, rather than by refusing the turn now.
func (e *MemQLEngine) CheckRouteChoice(ctx context.Context, choice common.RouteChoice) error {
	name, isRoute := routePolicyName(choice.Source)
	if !isRoute {
		return nil
	}
	unknown := &RouteChoiceError{Code: RoutePolicyUnknown, Field: "source", Value: choice.Source,
		Reason: fmt.Sprintf("route %q is not one of this cluster's routes", name)}
	if e == nil || e.Policies() == nil {
		return unknown
	}
	policies, _, err := e.Policies().SnapshotRouting(ctx, e.Rules())
	if err != nil {
		// Storage, not the person's choice: said as what it is.
		return fmt.Errorf("the cluster's routes could not be read to check %q: %w", name, err)
	}
	if policies == nil {
		return unknown
	}
	if _, ok := policies.Lookup(name); !ok {
		return unknown
	}
	return nil
}

// routedByChoice reports whether a call of this modality and level is one a
// conversation's choice governs: text calls -- chat, tools, structured output.
func routedByChoice(req airoute.ResolveRequest) bool {
	if req.Level == airoute.LevelEmbeddings {
		return false
	}
	switch req.Modality {
	case airoute.ModalityChat, airoute.ModalityStreamingChat, airoute.ModalityTools,
		airoute.ModalityStreamingTools, airoute.ModalityStructured:
		return true
	}
	return false
}

// ApplyRunRouting applies the run's route choice to one router request. The
// router calls it for EVERY request that reaches it (router.ResolveFor), so a
// call site cannot forget it; a context that is not a run's, or a run nobody
// chose for, changes nothing.
//
// It is idempotent, and it leaves alone whatever the executing step's own
// override names (see the file comment for why that one wins).
func ApplyRunRouting(ctx context.Context, req airoute.ResolveRequest) (airoute.ResolveRequest, error) {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.Routing.IsZero() || !routedByChoice(req) {
		return req, nil
	}
	// Re-parsed, not trusted: the value came off a row, and a row that no
	// longer parses is refused rather than served under a choice nobody made.
	choice, err := ParseRouteChoice(rc.Routing.Source, rc.Routing.Level)
	if err != nil {
		var refusal *RouteChoiceError
		if errors.As(err, &refusal) {
			refusal.Reason = "run " + rc.RunId + " carries a routing choice that no longer parses: " + refusal.Reason
		}
		return req, err
	}
	ov := rc.Override
	if choice.Source != "" && (ov == nil || strings.TrimSpace(ov.Model) == "") {
		if name, isRoute := routePolicyName(choice.Source); isRoute {
			req.Route = name
			// The person's route overrules an author's pin, and a cleared pin
			// is nobody's.
			req.ExplicitProvider, req.PinnedBy = "", ""
		} else {
			req.ExplicitProvider = choice.Source
			req.PinnedBy = strings.TrimSpace(rc.Routing.By)
			req.Route = ""
		}
	}
	if choice.Level != "" && strings.TrimSpace(rc.StepKey) != "" && (ov == nil || strings.TrimSpace(ov.Level) == "") {
		req.Level = airoute.Level(choice.Level)
	}
	return req, nil
}

type goalRouteChoiceKey struct{}

// ContextWithGoalRouteChoice stamps the choice the goal a request is about to
// open should carry. It is how Ask hands the turn's choice to createGoal, which
// runs in-process on the same call: the choice is deliberately NOT a goal
// input (the model reads the input), and createGoal's argument list is the
// SDK's contract rather than Ask's.
//
// choice.By must name the person who made it -- the server's authenticated
// caller -- because the stamp applies ONLY to that person's goals: a context
// that travels on into other work cannot lend one person's choice to
// somebody else's goal. A zero choice, or one naming nobody, returns ctx
// unchanged.
func ContextWithGoalRouteChoice(ctx context.Context, choice common.RouteChoice) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if choice.IsZero() || strings.TrimSpace(choice.By) == "" {
		return ctx
	}
	return context.WithValue(ctx, goalRouteChoiceKey{}, choice)
}

// GoalRouteChoice is the choice a goal owned by owner, opened on ctx, carries.
//
// The request's own choice wins, for the goals of the person who made it.
// Otherwise a goal opened by one of a run's steps -- a compose, an agent() --
// carries that run's choice, again only when it is the same person's. A choice
// made for one person's conversation is never lent to somebody else's goal,
// where their pin would not be theirs. Otherwise Auto.
func GoalRouteChoice(ctx context.Context, owner string) common.RouteChoice {
	if ctx == nil {
		return common.RouteChoice{}
	}
	owner = strings.TrimSpace(owner)
	if requested, ok := ctx.Value(goalRouteChoiceKey{}).(common.RouteChoice); ok && !requested.IsZero() && sameUserId(requested.By, owner) {
		return requested
	}
	if rc, ok := common.RunFromContext(ctx); ok && !rc.Routing.IsZero() && sameUserId(rc.Routing.By, owner) {
		return rc.Routing
	}
	return common.RouteChoice{}
}

// sameUserId compares two user ids that may differ only in being canonical or
// bare -- "v1:identity:user:alice" and "alice" name one person.
func sameUserId(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || BareShortId(a) == BareShortId(b)
}
