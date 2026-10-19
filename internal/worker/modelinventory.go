package worker

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// ModelInventory reports which local models this machine offers, for
// Register (memql-cockpit#361).
//
// An interface rather than a concrete discoverer so the runner's tests can
// drive the wire shape without Ollama on the box, and so a build that
// wants no model reporting at all can pass nil.
type ModelInventory interface {
	Models(ctx context.Context) models.Inventory
	// Invalidate forgets whatever was cached, so the next Models pays
	// for a fresh probe.
	//
	// On the INTERFACE rather than behind a type assertion, because the
	// caller that needs it -- the pull path, through
	// Runner.RequestImmediateReadvertise -- is re-advertising on the
	// strength of what the next Models says. An implementation that
	// silently could not forget would hand it the label set from before
	// the pull, the reconnect would re-register the old labels, and the
	// person watching would see a pull that did nothing. The compiler
	// asking every implementation to answer is the point.
	Invalidate()
}

// DefaultModelInventoryTTL bounds how stale a discovery result may be.
//
// WHY CACHE. Discovery is one HTTP call to list what is installed plus one
// per model to ask what it can do, and it is taken on every registration
// and on every refresh check. Uncached, a machine with ten models pulled
// would issue eleven requests a minute forever to answer a question whose
// answer changes when somebody runs `ollama pull`.
//
// WHY LONGER THAN modelRefreshInterval (60s). The runner's refresh ticker
// is what keeps this warm, and a TTL shorter than its period would leave a
// window in which the cache is cold and the next reader pays for a full
// probe. That reader is usually a model call resolving its model, and the
// probe would land as latency in front of somebody's generation. The
// ticker refreshes at 60s, comfortably inside 90s, so a cold read happens
// once -- at the first registration -- and never again while connected.
const DefaultModelInventoryTTL = 90 * time.Second

// modelDiscoverer is the discovery half, as an interface for the reason
// ModelInventory is one: the cache and its invalidation are testable on a
// machine with no Ollama, and on one that HAS Ollama they cannot pass for
// the wrong reason.
type modelDiscoverer interface {
	Discover(ctx context.Context, req models.Request) models.Inventory
}

// policyModelInventory pairs the discoverer with the policy that gates it.
// The policy is read on every call rather than captured, so a SIGHUP that
// adds a model to models.allow is picked up by the next refresh.
type policyModelInventory struct {
	discoverer modelDiscoverer
	policy     *tools.Policy
	ttl        time.Duration

	mu     sync.Mutex
	cached models.Inventory
	at     time.Time
	now    func() time.Time
}

// newDiscoverer is the ONE place a models.Discoverer is built. Seven
// commands and run paths probe this machine, and every one of them has to
// read the same sources for the same facts -- the worker's environment, the
// unit it manages -- or `memql worker models` prints a ceiling the worker
// does not advertise. TestEveryDiscoveryIsBuiltInOnePlace holds the seven
// to it.
func newDiscoverer() *models.Discoverer {
	return &models.Discoverer{ManagedParallel: inference.ManagedParallel}
}

// logModelCeilings says, for every model this machine serves, how many
// requests the worker takes at once and where that number came from: one Info
// line per model. It is the worker's own record, and the only one that cannot
// differ from what it advertises. `memql worker models` runs in the owner's
// shell and can only read the worker's environment file, as it is now, while
// this line is written by the process that took its environment when it
// started, from the sources discovery actually used.
//
// Only the models the machine serves are logged (Advertised): a blocked model,
// or any model below the hardware floor, is offered no ceiling at all.
func logModelCeilings(logger *slog.Logger, inv models.Inventory) {
	for _, m := range inv.Advertised() {
		logger.Info("model concurrency ceiling",
			"model", m.ID, "model_max", m.MaxConcurrent, "source", m.CeilingSource)
	}
}

// announceModelCeilings logs the ceilings from the shared inventory. It is
// called when the worker starts and after every SIGHUP reload, and by `go`:
// it asks the inventory, which probes Ollama when its cache is cold, and neither
// the worker's start nor the goroutine that also handles SIGTERM may wait on
// that. The inventory is cached for 90 seconds, so the registration that
// follows reuses this probe. After a reload it must be called AFTER
// RequestImmediateReadvertise, which drops the cache: otherwise it would log the
// ceiling the old policy gave.
func announceModelCeilings(ctx context.Context, logger *slog.Logger, inventory ModelInventory) {
	if inventory == nil {
		return
	}
	logModelCeilings(logger, inventory.Models(ctx))
}

// modelsRequest is what this machine's policy asks discovery: which models
// may be offered, which runtimes are declared, and the owner's tuning --
// the per-model ceiling and the models kept loaded. The ONE place that
// reads those out of the policy, for newDiscoverer's reason.
func modelsRequest(policy *tools.Policy) models.Request {
	tuning := policy.ModelsTuning()
	return models.Request{
		Allow:    policy.ModelsAllow(),
		Runtimes: policy.ModelRuntimes(),
		Parallel: tuning.Parallel,
		Resident: tuning.Resident,
	}
}

// NewModelInventory builds the reporter the worker runs with.
//
// The discoverer is the CALLER's, not built here, because the runner's
// pull arm needs the base URL this same discoverer resolved -- a second
// one would read OLLAMA_HOST again, and two readings drift.
func NewModelInventory(policy *tools.Policy, discoverer *models.Discoverer) ModelInventory {
	if discoverer == nil {
		discoverer = newDiscoverer()
	}
	return &policyModelInventory{
		discoverer: discoverer,
		policy:     policy,
		ttl:        DefaultModelInventoryTTL,
	}
}

func (p *policyModelInventory) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *policyModelInventory) Models(ctx context.Context) models.Inventory {
	if p == nil || p.discoverer == nil {
		return models.Inventory{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && p.clock().Sub(p.at) < p.ttl {
		return p.cached
	}
	p.cached = p.discoverer.Discover(ctx, modelsRequest(p.policy))
	p.at = p.clock()
	return p.cached
}

// Invalidate drops the cached inventory so the next Models re-probes.
//
// WHY IT EXISTS. The cache is 90 seconds wide, and a model pulled a
// moment ago is not in one taken 30 seconds before it. A re-advertise
// triggered right after a pull -- which is the whole point of the pull
// path: somebody pressed Pull and is watching -- would otherwise spend a
// RECONNECT to re-register the label set from BEFORE the pull. The
// reconnect happens, the labels do not change, and the pull reads to the
// person watching as one that did nothing, with every log line saying it
// succeeded.
//
// It is a one-shot, not a switch: the fresh result is cached in its turn.
// Nothing here re-probes on the spot, because the caller is usually
// holding a pull that just finished and the next reader is a moment away.
func (p *policyModelInventory) Invalidate() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = models.Inventory{}
	p.at = time.Time{}
}

// -----------------------------------------------------------------------------
// The registration shape
// -----------------------------------------------------------------------------

// modelRegistration is everything an inventory contributes to Register.
type modelRegistration struct {
	// Labels are the `model:<id>` and `runtime:<kind>` entries. Nil when
	// this machine offers nothing.
	Labels map[string]string
	// Capability is models.Capability when anything is offered, empty
	// otherwise. A machine that advertises MODEL with no model labels
	// would be selected by the capability-level plan and then ruled out
	// by every narrowing, which reads in the refusal report as a machine
	// that ALMOST worked.
	Capability string
	// Concurrency is the machine-wide MODEL ceiling: the sum of the
	// advertised per-model caps, which is the most this machine could be
	// running if every model ran at its own limit. The serving side
	// enforces this same number, so the advertisement and the enforcement
	// agree by construction rather than by two people remembering to
	// update both.
	Concurrency uint32
}

// modelRegistrationFor derives the registration contribution.
//
// It NEVER emits `sharedInference`. That label is the owner's grant, read
// by the engine from operatorLabels alone -- deliberately not from the
// merged map, because `labels` is overwritten from Register on every
// reconnect, so an opt-in stored there would be granted by the machine
// rather than by its owner and revoked roughly whenever the lid closed.
// A cockpit that derived one would be claiming a permission it has no
// standing to give itself.
func modelRegistrationFor(inv models.Inventory) modelRegistration {
	labels := inv.Labels()
	if len(labels) == 0 {
		return modelRegistration{}
	}
	var total int
	for _, m := range inv.Advertised() {
		total += m.MaxConcurrent
	}
	if total <= 0 {
		total = 1
	}
	return modelRegistration{
		Labels:      labels,
		Capability:  models.Capability,
		Concurrency: uint32(total),
	}
}

// mergeModelLabels folds the derived model labels onto the operator's own
// label map without mutating it.
//
// The operator's labels win on a collision. That direction matters
// exactly once -- if somebody hand-wrote a `model:` label in worker.yaml,
// discovery must not silently overrule it, because the machine they were
// describing is the one they are standing next to.
func mergeModelLabels(operator map[string]string, derived map[string]string) map[string]string {
	if len(derived) == 0 {
		return operator
	}
	out := make(map[string]string, len(operator)+len(derived))
	for k, v := range derived {
		out[k] = v
	}
	for k, v := range operator {
		out[k] = v
	}
	return out
}

// withModelCapability returns capabilities plus MODEL, without mutating
// the caller's slice and without a duplicate.
func withModelCapability(capabilities []string, capability string) []string {
	if capability == "" {
		return capabilities
	}
	for _, c := range capabilities {
		if c == capability {
			return capabilities
		}
	}
	out := make([]string, len(capabilities), len(capabilities)+1)
	copy(out, capabilities)
	return append(out, capability)
}

// withModelConcurrency returns concurrency plus the MODEL entry, without
// mutating the caller's map.
func withModelConcurrency(concurrency map[string]uint32, capability string, n uint32) map[string]uint32 {
	if capability == "" || n == 0 {
		return concurrency
	}
	out := make(map[string]uint32, len(concurrency)+1)
	for k, v := range concurrency {
		out[k] = v
	}
	out[capability] = n
	return out
}

// advertisedFingerprint renders the advertised label set as a stable
// string, so the runner can tell "the model set changed" from "discovery
// ran again" without comparing maps by hand.
func advertisedFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, '\n')
	}
	return string(b)
}
