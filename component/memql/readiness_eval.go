package memql

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// Slot sources, spelled the way email's ConfigResolver spells them so the
// Set up group reads one vocabulary.
const (
	readinessSourceEnv      = "env"
	readinessSourceVariable = "globalVariable"
	readinessSourceSecret   = "globalSecret"
	readinessSourceUnset    = "unset"
)

// readinessResolvers is everything the evaluator needs from the node, as
// functions, so the decision is testable with no engine.
type readinessResolvers struct {
	Env      func(name string) (string, bool)
	Variable func(ctx context.Context, name string) (string, error)
	Secret   func(ctx context.Context, name string) (string, error)
	IsSecret func(name string) bool
	// Hosted reports whether this node hosts the module.
	Hosted func(mod envregistry.Module) bool
	// Registrations reads EVERY v1:worker:registration row in the cluster.
	//
	// A door is CONFIGURED when a row says it exists (design record
	// 2026-09-07-core-gate-and-honest-install, D3), so this is what the `ai`
	// arm reads. It replaced InferenceOpen, which asked the fleet and app
	// seams -- a different question ("is a door open here, for this caller"),
	// answerable only on an agent node, which is why every other node type
	// reported no door and the fold pinned `ai` at partial forever.
	Registrations func(ctx context.Context) ([]readiness.RegistrationFacts, error)
	// FederationConfigured reports structural presence of a federated
	// provider, which every node type can answer from its own registry.
	FederationConfigured func() bool
	// IntegrationState asks integration.<name>.status in-process:
	// state is the report's own word, touched is "any slot present",
	// registered=false means the integration is not on this node.
	IntegrationState func(ctx context.Context, name string) (state string, touched bool, registered bool, err error)
	// Logger records the reads that FAILED. A failed read answers Unknown with
	// a closed-vocabulary reason (design record
	// 2026-09-14-readiness-convergence, D1); the log line is where the error
	// itself survives, because a row is broadcast to every signed-in reader
	// and an error string may carry an address or a DSN fragment.
	//
	// Optional: nil is a valid resolver set, and the pure tests pass one.
	Logger *slog.Logger
}

// resolveSlot walks the ladder -- environment, then the row tier the slot's
// own kind names -- and answers presence and source, NEVER the value.
//
// A secret slot stops at the secret tier rather than falling through to
// globalVariable. The two tiers are different stores with different readers:
// a plaintext row written under a secret's name would satisfy a
// falling-through check while the decrypting reader still finds nothing, so
// the report would say configured about a lane that cannot work.
func resolveSlot(ctx context.Context, r readinessResolvers, name string, optional bool) readiness.SlotReport {
	out := readiness.SlotReport{Name: name, Optional: optional, Source: readinessSourceUnset}
	if r.Env != nil {
		if v, ok := r.Env(name); ok && strings.TrimSpace(v) != "" {
			out.Present, out.Source = true, readinessSourceEnv
			return out
		}
	}
	if r.IsSecret != nil && r.IsSecret(name) {
		if r.Secret != nil {
			if v, err := r.Secret(ctx, name); err == nil && strings.TrimSpace(v) != "" {
				out.Present, out.Source = true, readinessSourceSecret
			}
		}
		return out
	}
	if r.Variable != nil {
		if v, err := r.Variable(ctx, name); err == nil && strings.TrimSpace(v) != "" {
			out.Present, out.Source = true, readinessSourceVariable
		}
	}
	return out
}

func evaluateLane(ctx context.Context, r readinessResolvers, lane envregistry.Lane) readiness.LaneReport {
	out := readiness.LaneReport{Name: lane.Name, ConfigurableFrom: lane.ConfigurableFrom, Complete: true, Slots: []readiness.SlotReport{}}
	for _, name := range lane.Slots {
		s := resolveSlot(ctx, r, name, false)
		if !s.Present {
			out.Complete = false
		}
		out.Slots = append(out.Slots, s)
	}
	// An optional slot is reported but never decides completeness: it is
	// there so a person can see the whole lane, not so a lane can be
	// half-satisfied by the part that did not matter.
	for _, name := range lane.OptionalSlots {
		out.Slots = append(out.Slots, resolveSlot(ctx, r, name, true))
	}
	return out
}

// evaluateModule decides one node's verdict on one module. Presence and
// source only: no branch here ever keeps a resolved value.
//
// THE EVALUATION ACTOR IS APPLIED HERE, at the one place a context reaches a
// resolver, and NOT at the caller (memql#5118, D3).
//
// The reason is what the resolvers do with it. `Registrations` reads every
// `v1:worker:registration` row in the cluster, and that concept declares
// `@rowAuthz(owner="ownerUserId", clusterOwner)` -- so under a caller's own
// actor the read returns ZERO ROWS AND NO ERROR, every door reads shut, and
// the core gate holds a cluster that is configured. Readiness is a fact about
// the CLUSTER; who asked for it changes nothing about what is true.
//
// It used to be a line in `WriteModuleReadiness` (`ectx :=
// readinessEvaluateContext(ctx)`), which meant one caller carried the whole
// property and reverting one assignment left every test green. Here it cannot
// be bypassed by a caller at all, and the test below observes it through a
// probe resolver rather than reading the source.
func evaluateModule(ctx context.Context, r readinessResolvers, mod envregistry.Module, nodeId, nodeType string, now time.Time) readiness.NodeReport {
	ctx = readinessEvaluateContext(ctx)
	out := readiness.NodeReport{Module: mod.Name, NodeId: nodeId, NodeType: nodeType, Core: mod.Core, Lanes: []readiness.LaneReport{}, ReportedAt: now}
	if r.Hosted != nil && !r.Hosted(mod) {
		out.State = readiness.NotApplicable
		return out
	}
	switch {
	case mod.Evaluator == envregistry.EvaluatorInferenceStatus:
		// A READ THAT FAILS IS UNKNOWN, NOT A SHUT DOOR (D1 of the 2026-09-14
		// readiness-convergence record).
		//
		// The arm used to leave every door shut, on the argument that "we
		// could not ask" reported as an open door sends somebody into a
		// console whose every feature then refuses. That is the right
		// direction for the GATE and the wrong one for a PERSISTED ROW: the
		// row outlives the failure, is folded against other nodes' correct
		// rows, and read "Partly set up" on the owner's cluster for a whole
		// deploy (2026-09-13). The fold sets Unknown aside and the writer
		// never persists it over a known row, so the gate still cannot OPEN
		// on a read that broke -- it only stops CLOSING on one.
		var regs []readiness.RegistrationFacts
		if r.Registrations != nil {
			got, err := r.Registrations(ctx)
			if err != nil {
				// SAID OUT LOUD, with the error, because the row carries only
				// the closed-vocabulary reason: a cluster whose fleet could not
				// be read is an operator problem with a completely different
				// repair from a cluster with no door.
				if r.Logger != nil {
					r.Logger.Warn("readiness: could not read the fleet; the inference verdict is unknown, not unconfigured",
						"module", mod.Name, "nodeId", nodeId, "nodeType", nodeType, "error", err)
				}
				out.State = readiness.Unknown
				out.Reason = readiness.ReasonFleetReadFailed
				return out
			}
			regs = got
		}
		out.Lanes = readiness.InferenceLanes(readiness.InferenceInput{
			Registrations:        regs,
			FederationConfigured: r.FederationConfigured != nil && r.FederationConfigured(),
			Now:                  now,
		})
		// ONE COMPLETE LANE CONFIGURES THE MODULE, the same rule the default
		// arm below applies to every lane-driven module. There is deliberately
		// no `partial` here: a door is configured or it is not, and a lane
		// that is complete but not live is a machine asleep -- which the
		// `live` slot says, and which is not a half-finished setup.
		if readiness.InferenceConfigured(out.Lanes) {
			out.State = readiness.Configured
		} else {
			out.State = readiness.Unconfigured
		}
	case strings.HasPrefix(mod.Evaluator, envregistry.EvaluatorIntegrationPrefix):
		name := strings.TrimPrefix(mod.Evaluator, envregistry.EvaluatorIntegrationPrefix)
		state, touched, registered, err := "", false, false, error(nil)
		if r.IntegrationState != nil {
			state, touched, registered, err = r.IntegrationState(ctx, name)
		}
		switch {
		// A probe that FAILED -- or answered with a report this evaluator
		// cannot read -- says nothing about whether a person did the setup,
		// and it is not "not hosted here" either: unknown, with the reason,
		// so a fresh node can say "could not evaluate" and a node with a
		// known row keeps it (readiness_write.go). It used to share
		// notApplicable with the case below, and a node that DOES carry the
		// integration hid behind the word for one that does not.
		case err != nil:
			if r.Logger != nil {
				r.Logger.Warn("readiness: integration probe failed; the verdict is unknown",
					"module", mod.Name, "integration", name, "nodeId", nodeId, "nodeType", nodeType, "error", err)
			}
			out.State = readiness.Unknown
			out.Reason = readiness.ReasonIntegrationProbeFailed
		// An integration this node does not carry is not applicable here.
		// Unconfigured would send a person to a form they may not need.
		case !registered:
			out.State = readiness.NotApplicable
		// unhealthy is CONFIGURED: the setup was done and the send is
		// failing for some other reason, which is a different repair.
		case state == "configured" || state == "unhealthy":
			out.State = readiness.Configured
		case touched:
			out.State = readiness.Partial
		default:
			out.State = readiness.Unconfigured
		}
	default:
		// Lanes are alternatives, not requirements: ONE complete lane
		// configures the module. "Touched" is any required slot present in
		// any lane, which is what separates a half-finished setup from one
		// nobody has started.
		touched, complete := false, false
		for _, lane := range mod.Lanes {
			lr := evaluateLane(ctx, r, lane)
			out.Lanes = append(out.Lanes, lr)
			if lr.Complete {
				complete = true
			}
			for _, s := range lr.Slots {
				if s.Present && !s.Optional {
					touched = true
				}
			}
		}
		switch {
		case complete:
			out.State = readiness.Configured
		case touched:
			out.State = readiness.Partial
		default:
			out.State = readiness.Unconfigured
		}
	}
	return out
}

func evaluateModules(ctx context.Context, r readinessResolvers, mods []envregistry.Module, nodeId, nodeType string, now time.Time) []readiness.NodeReport {
	out := make([]readiness.NodeReport, 0, len(mods))
	for _, mod := range mods {
		out = append(out, evaluateModule(ctx, r, mod, nodeId, nodeType, now))
	}
	return out
}

// readinessResolvers wires the evaluator to this node: the environment, the
// two row tiers, the plug-in registry, the provider registry and the
// in-process capability map.
func (e *MemQLEngine) readinessResolvers() readinessResolvers {
	manifest, _ := envregistry.LoadManifest("")
	nodeType := envregistry.ResolveNodeType()
	return readinessResolvers{
		Env:      os.LookupEnv,
		Variable: e.ResolveSystemVariable,
		Secret:   e.ResolveSystemSecret,
		IsSecret: func(name string) bool { return manifest != nil && manifest.IsSecret(name) },
		Hosted: func(mod envregistry.Module) bool {
			if mod.HostedBy.Everywhere() {
				return true
			}
			for _, name := range mod.HostedBy.Integrations {
				if e.IntegrationByName(name) != nil {
					return true
				}
			}
			for _, nt := range mod.HostedBy.NodeTypes {
				if nt == nodeType {
					return true
				}
			}
			return false
		},
		Registrations:        e.readInferenceRegistrations,
		Logger:               e.safeLogger(),
		FederationConfigured: func() bool { return e.providers != nil && e.providers.federationConfigured() },
		IntegrationState: func(ctx context.Context, name string) (string, bool, bool, error) {
			handler, ok := e.builtinExecutorHandlers["integration."+name+".status"]
			if !ok {
				return "", false, false, nil
			}
			nodes, err := handler(ctx, map[string]any{"probe": false}, 0)
			if err != nil {
				return "", false, true, err
			}
			for _, n := range nodes {
				state, touched, err := readiness.IntegrationStatus(n.Payload, name)
				if err == nil {
					return state, touched, true, nil
				}
			}
			// Malformed or missing reports cannot establish whether setup
			// is complete. Keep them on the existing failed-probe path and
			// never include the value-bearing payload in an error.
			return "", false, true, errors.New("readiness: integration status has no matching report")
		},
	}
}
