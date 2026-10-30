package worker

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql/cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// AppInventory reports which local apps this machine has, for Register
// and for every Heartbeat (memql-cockpit#346).
//
// An interface rather than a concrete detector so the runner's tests can
// drive the wire shape without a claude or codex on PATH, and so a build
// that wants no app reporting at all can pass nil.
type AppInventory interface {
	Apps(ctx context.Context) []apps.Info
}

// AppInventories hands each home its own app inventory, over ONE detector.
//
// PER HOME, because app consent is per cluster (policy.yaml apps.homes):
// the same claude on this machine is allowed for one cluster and blocked
// for another, and a single inventory shared by every home told every
// cluster whatever the machine-wide list said. ONE DETECTOR, because what
// is installed is a fact about the machine: its cached version and harness
// probes are shared, so two homes do not fork two probes of one binary.
//
// ONE TRACKER, for the same reason: an app's login and its plan are facts about
// the machine, whichever cluster asks, so two homes must not ping one login twice.
// What each cluster hears about consent stays per home.
type AppInventories struct {
	detector  *apps.Detector
	policy    *tools.Policy
	readiness *apps.ReadinessTracker

	mu    sync.Mutex
	homes map[string]bool
}

// appReadinessFile is where what the machine measured survives a restart, at the
// machine's state root because it is about the machine and not one cluster.
const appReadinessFile = "app-readiness.json"

// NewAppInventories builds the per-home reporters the worker runs with. A
// nil detector is the real one.
func NewAppInventories(policy *tools.Policy, detector *apps.Detector) *AppInventories {
	if detector == nil {
		detector = &apps.Detector{}
	}
	a := &AppInventories{detector: detector, policy: policy, homes: map[string]bool{}}
	a.readiness = apps.NewReadinessTracker(apps.ReadinessOptions{
		Detector: detector,
		Allowed:  a.allowedAnywhere,
		Launch:   appsession.CheckLauncher(),
	})
	detector.Readiness = a.readiness
	return a
}

// For is the inventory one home's stream reports on Register and every
// Heartbeat.
func (a *AppInventories) For(home string) AppInventory {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.homes[home] = true
	a.mu.Unlock()
	return &policyInventory{detector: a.detector, policy: a.policy, home: home}
}

// allowedAnywhere is whether any cluster this machine serves allows the app, read
// live so a SIGHUP reaches the next check. An app none allows costs nothing: the
// machine's owner has not agreed to MemQL driving it.
func (a *AppInventories) allowedAnywhere(appID string) bool {
	a.mu.Lock()
	homes := make([]string, 0, len(a.homes))
	for home := range a.homes {
		homes = append(homes, home)
	}
	a.mu.Unlock()
	for _, home := range homes {
		if appsAllowedFor(a.policy, home)(appID) {
			return true
		}
	}
	return false
}

// Readiness is the tracker sessions report to.
func (a *AppInventories) Readiness() *apps.ReadinessTracker {
	if a == nil {
		return nil
	}
	return a.readiness
}

// RunReadiness measures the apps on a schedule until ctx ends. stateDir is the
// worker's state directory; what was measured survives a restart under its root.
func (a *AppInventories) RunReadiness(ctx context.Context, stateDir string, logger *slog.Logger) {
	if a == nil {
		return
	}
	a.readiness.UseStatePath(filepath.Join(machineStateRoot(stateDir), appReadinessFile), logger)
	go a.readiness.Run(ctx)
}

// policyInventory pairs the detector with the policy that gates it for ONE
// home. The policy is read on every call rather than captured, so a SIGHUP
// that allows an app for this cluster takes effect on the next beat -- the
// same cadence signing into the app itself gets.
type policyInventory struct {
	detector *apps.Detector
	policy   *tools.Policy
	home     string
}

func (p *policyInventory) Apps(ctx context.Context) []apps.Info {
	if p == nil || p.detector == nil {
		return nil
	}
	return p.detector.Detect(ctx, p.policy.AppsAllowFor(p.home))
}

// appsAllowedFor is one home's app-session gate: whether policy.yaml allows
// an app for THAT cluster. Read on every call, so a SIGHUP reaches the next
// session; a nil policy allows nothing.
func appsAllowedFor(policy *tools.Policy, home string) func(appID string) bool {
	return func(appID string) bool {
		for _, allowed := range policy.AppsAllowFor(home) {
			if strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(appID)) {
				return true
			}
		}
		return false
	}
}

// appsToProto converts the inventory to the wire shape.
//
// Order is preserved from the detector, which sorts by id. The engine
// sorts too, but an unstable order here would rewrite the registration
// row on every beat for no actual change.
func appsToProto(inventory []apps.Info) []*memqlv1.AppInfo {
	if len(inventory) == 0 {
		return nil
	}
	out := make([]*memqlv1.AppInfo, 0, len(inventory))
	for _, a := range inventory {
		entry := &memqlv1.AppInfo{
			Id:           apps.Truncate(a.Id),
			Version:      apps.Truncate(a.Version),
			SignedIn:     a.SignedIn,
			Subscription: apps.NormalizeSubscription(a.Subscription),
			Allowed:      a.Allowed,
			Readiness:    a.Readiness,

			ReadinessDetail: a.ReadinessDetail,
		}
		if !a.ReadinessCheckedAt.IsZero() {
			entry.ReadinessCheckedAt = timestamppb.New(a.ReadinessCheckedAt)
		}
		if !a.UsageResetsAt.IsZero() {
			entry.UsageResetsAt = timestamppb.New(a.UsageResetsAt)
		}
		out = append(out, entry)
	}
	return out
}

// appDescriptorsToProto converts the inventory's harness descriptors to
// Register.app_descriptors (memql-cockpit#444): HOW this machine drives
// each app -- the protocol word, and whether that protocol can return a
// structured answer and take a follow-up.
//
// THE ENGINE READS AN ABSENT DESCRIPTOR AS "ASSUME CAPABLE", because absence
// is how a cockpit that predates the field looks, and refusing on silence
// would take structured answers away from every machine that had not
// upgraded. That makes sending one the only way this machine can say no --
// and the codex-mcp fallback, which cannot constrain an answer, needs to.
//
// They ride REGISTER ONLY, which the engine accepts once per stream, so a
// harness this machine gains mid-connection (a Codex upgraded to one with
// the app-server) is advertised on the next reconnect: the same rollout
// cost the model labels carry, and the one the proto documents.
//
// An entry with no harness word is left out rather than sent empty: the
// engine drops a word it does not know, but an empty one is a claim this
// machine never meant to make. The order is the detector's, for
// appsToProto's reason.
func appDescriptorsToProto(inventory []apps.Info) []*memqlv1.AppDescriptor {
	var out []*memqlv1.AppDescriptor
	for _, a := range inventory {
		if strings.TrimSpace(a.Harness) == "" {
			continue
		}
		out = append(out, &memqlv1.AppDescriptor{
			Id:               apps.Truncate(a.Id),
			Harness:          a.Harness,
			StructuredResult: a.StructuredResult,
			FollowUps:        a.FollowUps,
			InferenceOnly:    a.InferenceOnly,
			SignIn:           a.SignIn,
		})
	}
	return out
}
