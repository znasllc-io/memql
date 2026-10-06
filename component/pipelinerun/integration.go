// Package pipelinerun is the `pipelines` plug-in (epic memql#5477): it
// connects a source's pipeline, turns a GitHub delivery or the poll into a
// queued v1:pipelines:run with a check run beside it, and answers the person
// acts on a run -- re-run and cancel -- and the readiness self-report.
//
// The pure half -- the manifest block, validation, compilation, the event
// table, the delivery shapes, the check run's text -- is component/pipelines,
// a standard-library leaf. This package is the half that touches the world:
// rows, GitHub, and the gate that serializes replicas.
//
// ===========================================================================
// WHO DOES WHAT, AND ON WHICH NODE
// ===========================================================================
// A run is OPENED where its cause arrives -- the bff that staged a webhook,
// the agent replica the poll is placed on -- as a `queued` row and a
// `queued` check run. It is DRIVEN by an agent node that claims it under a
// row lease (Task 10b's driver, in this same package): compiled, executed
// over the work spine, concluded. Nothing about a run lives in one node's
// memory; it crosses nodes as the row and its broadcast events.
//
// ===========================================================================
// IT LIVES IN component/, NOT integrations/
// ===========================================================================
// For component/packages' reason one layer up: a pipeline borrows Deployables'
// GitHub App client and its verified grant path (packages.Integration.GitHub
// and InstallationToken), and component/packages is in the root module, which
// integrations/ cannot import back.
package pipelinerun

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// Deps are the integration's ports. New takes them, and Configure sets the
// ones only app/ can build after every plug-in has materialized.
type Deps struct {
	// LoadWorkflow resolves installed definitions. Nil uses the boot-time DSL tree.
	LoadWorkflow func(string) (*automations.Automation, error)
	Store        Store
	GitHub       GitHub
	Gate         Gate
	// RunnerReadiness reads remote workbench evidence on front-door nodes too.
	// Reporting does not register a step executor or make this node a driver.
	RunnerReadiness pipelines.ReadinessReporter
	// Secrets resolves a globalSecret value for the driver (Task 10b).
	Secrets Secrets
	// Journal writes a pipeline run's v1:work goal, run and steps (Task 10b).
	Journal *workjournal.Journal
	// OSOrigin is MemQL OS's origin ("https://os.<domain>"), the base of a
	// check run's details link. "" omits the link rather than sending a
	// relative one GitHub would refuse.
	OSOrigin func() string
	// WebhookReachable reports whether GitHub could plausibly deliver a
	// webhook to this cluster -- githubconnect's own rule for whether the
	// app it registers subscribes to events. The connect preview suggests
	// webhook delivery where it does and the poll where it does not; nil is
	// "no", which suggests the poll.
	WebhookReachable func() bool
	// Domain is the cluster's MEMQL_DOMAIN, read at each call: the front-door
	// domain the driver puts on every step's request, which a step exports as
	// MEMQL_DOMAIN to reach the cluster's public hosts from outside. Nil or ""
	// sends no MEMQL_DOMAIN.
	Domain func() string
	// EngineRevision is the immutable source revision of this driver binary.
	// Defaults to the build stamp; tests supply the revision of their fake engine.
	EngineRevision func() string
	// NodeID is this replica's MEMQL_NODE_ID: what a driver writes as its
	// lease, and what RequestCancel compares a run's driverNodeId with.
	NodeID string
	Now    func() time.Time
	Logger *slog.Logger

	// Recover is the hook the poll calls after polling: Task 10b's recovery
	// (claim and drive queued runs nobody claimed and runs whose driver went
	// silent). Nil -- every node that does not drive -- is a no-op.
	Recover func(ctx context.Context) error
	// SignalCancel is the hook RequestCancel calls after it records a
	// cancel on a run THIS node drives, so the driver stops what is
	// executing now rather than at its next heartbeat (Task 10b). Nil is a
	// no-op: the heartbeat still reads the flag.
	SignalCancel func(ctx context.Context, runID string)

	// Drive makes this node a DRIVER: it claims queued runs from their
	// events, drives them over the work spine and recovers stranded ones
	// (driver.go). Only an agent node sets it (EnableDriver, from
	// app/integrations_pipelines_agent.go); everywhere else HandleRunEvent
	// and RecoverRuns return at once, having done nothing.
	Drive bool
	// HeartbeatEvery is how often a driver renews its lease; zero is
	// leaseRenewEvery (30 s). A test shortens it to watch a lease be lost.
	HeartbeatEvery time.Duration

	// publishBackoff is the wait before each retry of a concluded run's
	// check-run write; nil is finalPublishBackoff. A test shortens it.
	publishBackoff []time.Duration
	// notifyPoll, when set, is the wait before EVERY read of a notification's
	// delivery rows, in place of the production pace (notifyPollAfter); and
	// notifyTimeout, when set, is how long the notify stage waits in all, in
	// place of the step's own timeout. A test shortens both: it cannot wait
	// two seconds a read, or twenty minutes for a delivery that never comes.
	notifyPoll    time.Duration
	notifyTimeout time.Duration
}

// Integration is the `pipelines` plug-in.
type Integration struct {
	mu   sync.RWMutex
	deps Deps

	// drives is what this node is driving now, by bare run id: the one
	// piece of a run's state that lives in memory, and only for as long as
	// this replica holds the run's lease (driver.go).
	drives driveRegistry
}

// New builds the integration over deps. The plug-in factory (capabilities.go)
// gives it the DSL store and the secret resolver; app/ adds the rest through
// Configure; a test hands it fakes.
func New(d Deps) *Integration { return &Integration{deps: d} }

// Configure changes the integration's ports. app/ calls it once while wiring
// (the GitHub port, the gate, the node id, the OS origin, the journal), and
// the agent's wiring calls it again to install the driver's hooks.
func (i *Integration) Configure(apply func(d *Deps)) {
	if i == nil || apply == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	apply(&i.deps)
}

// EnableDriver makes this node a driver (Deps.Drive) and installs the two
// hooks the opening half calls a driver through: the poll's recovery
// (Deps.Recover) and a cancel's signal (Deps.SignalCancel). app/ calls it on
// an agent node, beside subscribing HandleRunEvent to the run's events; a
// test calls it on an integration over fakes.
func (i *Integration) EnableDriver() {
	i.Configure(func(d *Deps) {
		d.Drive = true
		d.Recover = i.RecoverRuns
		d.SignalCancel = i.SignalCancel
	})
}

// snapshot is the ports as configured now, with the defaults a zero Deps
// leaves out. Every operation reads it once, at its start, so a Configure
// that lands mid-operation cannot change a port half-way through one.
func (i *Integration) snapshot() Deps {
	i.mu.RLock()
	d := i.deps
	i.mu.RUnlock()
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.OSOrigin == nil {
		d.OSOrigin = func() string { return "" }
	}
	if d.Domain == nil {
		d.Domain = func() string { return "" }
	}
	if d.HeartbeatEvery <= 0 {
		d.HeartbeatEvery = leaseRenewEvery
	}
	return d
}

// now is the clock, in UTC and to the second: every time this package writes
// is RFC3339, and a sub-second part that a row cannot carry would make a
// value read back differ from the one written.
func (d Deps) now() time.Time { return d.Now().UTC().Truncate(time.Second) }

// gate runs fn under key, or refuses when no gate is wired: a missing gate
// fails CLOSED, like githubconnect.WithGate with no database.
//
// A gate that could not be TAKEN -- the production one gives up after five
// seconds when the direct pool is busy, and its error names GitHub's
// lifecycle lock -- is said to be the pipelines' gate, with its key, so the
// failure reads as what it is wherever it surfaces: a delivery's automation
// step, which retries it, or a person's re-run. What fn itself returns passes
// through untouched, for its caller to read.
func (d Deps) gate(ctx context.Context, key string, fn func(context.Context) error) error {
	if d.Gate == nil {
		return errNoGate
	}
	ran := false
	err := d.Gate(ctx, key, func(gctx context.Context) error {
		ran = true
		return fn(gctx)
	})
	if err != nil && !ran {
		return fmt.Errorf("pipelines: the cross-replica gate %s was not taken: %w", key, err)
	}
	return err
}
