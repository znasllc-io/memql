package router

// session_door.go -- the ONE decision that turns an app door into a session
// (epic memql#5391, task memql#5392, design D7).
//
// IT IS ONE FUNCTION BECAUSE IT IS ASKED TWICE. The chain walk asks it when it
// picks a winner; providerLookup asks it again when the fallback wrapper
// re-resolves that winner BY NAME. Two copies would disagree exactly once -- on
// the retry -- and the retry is where the disagreement would be invisible: the
// wrapper would skip the app entry as "does not serve tool-calling turns" and
// advance to whatever sits behind it, which on the shipped chains is a metered
// vendor. That is the silent paid call this door exists to prevent.
//
// A SESSION WINNER IS NOT A PROVIDER THAT HAPPENS TO RUN ELSEWHERE. The step
// goes over whole and the app drives its own loop; what comes back is one
// assistant turn with no tool calls. So the client is built HERE rather than
// taken off the registry entry: the entry's client is an appProvider, which
// deliberately does not serve tool turns, and making it do so would put two
// agents in one conversation -- the thing app_provider.go's absent tool
// surfaces are there to prevent.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// sessionDoorFor reports whether this chain entry resolves to an APP SESSION
// for this modality, and hands back the client that carries the step.
//
// SKIP is set when the entry is an app door that cannot take a step ON THIS
// NODE: its machine is held by another agent. The entry the walk read is the
// chat door's, which is open wherever one call can be forwarded to the holding
// agent (AppCallForward); a step handover does not cross -- it runs as a
// session subrun on the replica whose delegate opens it -- so here the door is
// passed over with this reason, exactly as it was before forwarding existed,
// rather than won as a session that cannot run and has no chain behind it
// (the planner/app-source design, section 3a, precondition 3). It is asked
// BEFORE the step check: a door no step could open is not a call-site fault.
//
// It returns an ERROR for a tool-needing call that carries no step. That is a
// refusal AT RESOLUTION, before anything is spent, and it names both the entry
// and the modality: a bare Go model call with tools has no step to hand over,
// and reporting that as an unavailable door would send somebody to look at
// their laptop for a fact about the call site.
func (r *Router) sessionDoorFor(ctx context.Context, req ResolveRequest, name string, mod providerModality) (client any, ok bool, skip string, err error) {
	if !toolNeeding(mod) {
		return nil, false, "", nil
	}
	appId, model, isApp := memql.SplitAppReference(name)
	if !isApp {
		return nil, false, "", nil
	}
	if r != nil && r.providers != nil {
		if why := r.providers.AppSessionRefusalHere(ctx, req.UserId, appId); why != "" {
			return nil, false, why, nil
		}
	}
	if strings.TrimSpace(req.StepId) == "" {
		return nil, false, "", fmt.Errorf(
			"router: %q resolved for %s and this call carries no work step to hand over: "+
				"an app door serves a tool-needing call by taking the whole step as a session, and a "+
				"session is opened from a step. A bare model call with tools cannot be delegated",
			name, modalityName(mod))
	}
	if r == nil || r.providers == nil {
		return nil, false, "", fmt.Errorf(
			"router: %q resolved for %s with no provider registry to open a session through",
			name, modalityName(mod))
	}
	return r.providers.SessionProvider(appId, model, memql.SessionRequest{
		ActingUserId: req.UserId,
		AgentId:      req.AgentId,
		Level:        string(req.Level),
		Effort:       strings.TrimSpace(req.Effort),
		RunId:        req.RunId,
		StepId:       req.StepId,
		Pin:          appDoorPin(req, name),
	}), true, "", nil
}

// appDoorPin says how the app door `name` was reached on this request, for the
// app gate on the agent node (memql.AppDoorPin).
//
// A PIN IS THE ONLY WAY TO AN APP DOOR THAT NO RULE NAMED. resolveChain walks
// the pin ALONE when there is one, and an `app:` entry never expands into
// other names (expandEntry), so a door whose name is the request's pin was
// reached through it. Any other door -- a rule's chain, or the consented cloud
// chain a person said yes to -- is not a pin, whatever PinnedBy the request
// happens to carry. Who made the pin is the request's to say; the router
// cannot know it.
func appDoorPin(req ResolveRequest, name string) memql.AppDoorPin {
	pinned := strings.TrimSpace(req.ExplicitProvider)
	if pinned == "" || pinned != strings.TrimSpace(name) {
		return memql.AppDoorPin{}
	}
	return memql.AppDoorPin{Pinned: true, By: strings.TrimSpace(req.PinnedBy)}
}

// toolNeeding maps the router's internal modality onto the shared vocabulary's
// predicate, so the two cannot drift about which turns MemQL drives.
//
// EMBEDDINGS ARE ABSENT AND MUST STAY ABSENT (design D10): an embedding must
// come from the embedder of the index it is written into, and no app exposes
// one. Admitting it here would send a vector into an index it cannot be
// compared against, and every later similarity read would come back plausible
// and wrong.
func toolNeeding(mod providerModality) bool {
	switch mod {
	case modalityTools, modalityStreamTools:
		return true
	}
	return false
}
