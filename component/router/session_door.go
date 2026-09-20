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
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// sessionDoorFor reports whether this chain entry resolves to an APP SESSION
// for this modality, and hands back the client that carries the step.
//
// It returns an ERROR for a tool-needing call that carries no step. That is a
// refusal AT RESOLUTION, before anything is spent, and it names both the entry
// and the modality: a bare Go model call with tools has no step to hand over,
// and reporting that as an unavailable door would send somebody to look at
// their laptop for a fact about the call site.
func (r *Router) sessionDoorFor(req ResolveRequest, name string, mod providerModality) (client any, ok bool, err error) {
	if !toolNeeding(mod) {
		return nil, false, nil
	}
	appId, model, isApp := memql.SplitAppReference(name)
	if !isApp {
		return nil, false, nil
	}
	if strings.TrimSpace(req.StepId) == "" {
		return nil, false, fmt.Errorf(
			"router: %q resolved for %s and this call carries no work step to hand over: "+
				"an app door serves a tool-needing call by taking the whole step as a session, and a "+
				"session is opened from a step. A bare model call with tools cannot be delegated",
			name, modalityName(mod))
	}
	if r == nil || r.providers == nil {
		return nil, false, fmt.Errorf(
			"router: %q resolved for %s with no provider registry to open a session through",
			name, modalityName(mod))
	}
	return r.providers.SessionProvider(appId, model, memql.SessionRequest{
		ActingUserId: req.UserId,
		AgentId:      req.AgentId,
		Level:        string(req.Level),
		RunId:        req.RunId,
		StepId:       req.StepId,
	}), true, nil
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
