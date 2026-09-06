package app

import (
	"os"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/server"
	composeint "github.com/znasllc-io/memql/integrations/compose"
)

// integrations_compose.go -- joins the Materializer's plug-in to the three
// things it cannot reach through PluginContext (epic memql#4977).
//
// ===========================================================================
// WITHOUT THIS CALL THE APP IS INSTALLED AND INERT
// ===========================================================================
// The plug-in registers itself from `init()`, so its five capabilities
// resolve at boot and nothing fails loudly. What it has no way to obtain
// from `PluginContext` is object storage, the concept registry and the
// cluster's own domain -- and each absence degrades to a DIFFERENT wrong
// answer rather than to a crash:
//
//	no uploader   every materialize fails "object storage is not
//	              configured on this node", which reads as an operator
//	              problem on a cluster that is configured
//	no registry   the Sources column reports it cannot read the concept
//	              registry, on a node that can
//	no domain     every provenance record omits WHICH MemQL made the file,
//	              which is the question somebody holding it later has
//
// That combination is the shape this repo's CLAUDE.md warns about twice:
// the registration IS the feature, and an app that renders is not an app
// that works. It was caught by re-reading the epic's own bullets against
// the code rather than by any test -- every gate was green, because a
// capability that returns a typed refusal is a capability that resolves.
//
// ===========================================================================
// THE REGISTERED INSTANCE, NOT A SECOND ONE
// ===========================================================================
// `transport_artifacts.go` builds a SECOND *library.Integration because what
// it needs is a pure function of the engine handle. That is not available
// here: capability dispatch goes through the instance the registry built, so
// a second one would be configured and never called.
// `engine.IntegrationByName` is how the work spine's compiler reaches its own
// registered instance (app/safety_llm.go), and it is the same answer.
//
// ===========================================================================
// THE GOAL OPENER IS NOT WIRED HERE, AND THAT IS DELIBERATE
// ===========================================================================
// A materialization IS a goal (design D6), and the obvious wiring is a Go
// seam onto integrations/work. There is none to take: `createGoal` exists as
// a capability handler, not as an exported method, and adding one would make
// `integrations/compose` depend on `integrations/work` in Go for something
// the DSL already exposes. So the integration opens its goal through the
// `createGoal` BUILTIN over its own engine handle, under the caller's own
// actor -- the same path a DSL author would take, with no new coupling and
// no second spelling of `requestedVia`.

// wireComposeIntegration installs the Materializer's collaborators on the
// registered plug-in. Idempotent and non-fatal: every setter degrades to a
// typed refusal the surface renders verbatim, so a node that cannot supply
// one is worse than a node that can and better than a node that lies.
func (a *App) wireComposeIntegration(uploader server.FileUploader, container string) {
	integ := a.lookupComposeIntegration()
	if integ == nil {
		// Not a warning. A node type that did not materialize the plug-in
		// has no Materializer to configure, which is an ordinary state --
		// unlike the work compiler's absence on a planner node, which is a
		// defect.
		return
	}

	// THE CONCEPT REGISTRY, which is what the `@composable` marks live on.
	// Absent, `composableConcepts` reports `registryAvailable: false` and
	// the Sources column says so -- deliberately distinguishable from
	// "nothing is marked", because only one of the two is fixable.
	if a.engine != nil {
		integ.SetConceptSource(engineConceptSource{engine: a.engine})
	}

	// THE INSTANCE'S OWN DOMAIN -- the "which MemQL made this" fact every
	// provenance record carries. Read from MEMQL_DOMAIN directly rather
	// than from a derived host: the derivations compose per-ROLE hostnames
	// (api., identity., portal.), and a provenance record wants the
	// instance rather than one of its front doors.
	if domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")); domain != "" {
		integ.SetInstance(domain)
	}

	// OBJECT STORAGE. The bff's own blob client and container, the same
	// pair the Library's upload route and the site-bundle publisher take,
	// so "where materialized bytes live" and "where uploaded bytes live"
	// cannot drift apart.
	if uploader != nil && strings.TrimSpace(container) != "" {
		if up, ok := uploader.(composeint.Uploader); ok {
			integ.SetUploader(up, container)
		} else {
			// A configured uploader whose shape does not match is worth
			// saying out loud: the surface would otherwise report the
			// not-configured message on a cluster that IS configured.
			a.Logger.Error("materializer: the configured blob uploader does not implement the compose Uploader shape, so every materialization will report storage as unconfigured",
				"component", "compose")
		}
	}
}

// lookupComposeIntegration returns the registered plug-in instance, or nil.
func (a *App) lookupComposeIntegration() *composeint.Integration {
	if a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("compose")
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*composeint.Integration)
	return integ
}

// engineConceptSource adapts the engine's concept registry onto the narrow
// surface integrations/compose declares.
//
// A NARROW INTERFACE ON THE INTEGRATION'S SIDE rather than an engine handle,
// so its tests can hand it a fixture of three concepts instead of standing up
// a registry -- which is why that package's capability tests need no engine
// at all.
type engineConceptSource struct{ engine conceptLister }

type conceptLister interface {
	Concepts() memorynodes.Registry
}

func (s engineConceptSource) ConceptDefinitions() []*memorynodes.Concept {
	if s.engine == nil {
		return nil
	}
	reg := s.engine.Concepts()
	if reg == nil {
		return nil
	}
	return reg.List()
}
