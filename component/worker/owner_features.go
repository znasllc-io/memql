package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type OwnerFeature string

const (
	FeatureHeadless    OwnerFeature = "headless"
	FeatureApps        OwnerFeature = "apps"
	FeatureComputerUse OwnerFeature = "computerUse"
)

type OwnerFeatures struct{ Headless, Apps, ComputerUse bool }

// Missing fields retain the existing owner's access. Explicit malformed
// values never enable a feature. These settings grant no borrowed access.
func OwnerFeaturesFromRow(value any) OwnerFeatures {
	row, ok := value.(map[string]any)
	if value != nil && !ok {
		return OwnerFeatures{}
	}
	enabled := func(key string) bool { v, exists := row[key]; return !exists || v == true }
	return OwnerFeatures{Headless: enabled("headless"), Apps: enabled("apps"), ComputerUse: enabled("computerUse")}
}

func (f OwnerFeatures) Allows(feature OwnerFeature) bool {
	switch feature {
	case FeatureHeadless:
		return f.Headless
	case FeatureApps:
		return f.Apps
	case FeatureComputerUse:
		return f.ComputerUse
	default:
		return false
	}
}

type OwnerFeaturePolicy struct {
	Features OwnerFeatures
	// Registration row version fences reordered events and late reads.
	Version time.Time
}

type OwnerFeatureStore interface {
	ReadOwnerFeaturePolicy(context.Context, string, string) (OwnerFeaturePolicy, error)
}

type ownerFeatureLease struct {
	feature OwnerFeature
	cancel  context.CancelCauseFunc
}

func featureDisabled(feature OwnerFeature) error {
	return fmt.Errorf("worker_feature_disabled: %s is disabled for this machine in Fleet settings", feature)
}

// ApplyOwnerFeaturePolicy is called on the holding replica. Only affected
// contexts are canceled; model calls, workbenches and other features continue.
// Enabling a feature never revives the contexts canceled by a previous write.
func (w *Worker) ApplyOwnerFeaturePolicy(policy OwnerFeaturePolicy) {
	if w == nil || policy.Version.IsZero() {
		return
	}
	w.mu.Lock()
	if !policy.Version.After(w.ownerFeaturePolicy.Version) {
		w.mu.Unlock()
		return
	}
	w.ownerFeaturePolicy = policy
	var stopped []ownerFeatureLease
	for _, lease := range w.ownerFeatureLeases {
		if !policy.Features.Allows(lease.feature) {
			stopped = append(stopped, lease)
		}
	}
	w.mu.Unlock()
	for _, lease := range stopped {
		lease.cancel(featureDisabled(lease.feature))
	}
}

func (w *Worker) enterOwnerFeature(ctx context.Context, feature OwnerFeature, policy OwnerFeaturePolicy) (context.Context, func(), error) {
	if policy.Version.IsZero() {
		return nil, nil, fmt.Errorf("worker_feature_unavailable: machine settings have no version")
	}
	w.ApplyOwnerFeaturePolicy(policy)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ownerFeaturePolicy.Features.Allows(feature) {
		return nil, nil, featureDisabled(feature)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	w.nextOwnerFeatureLease++
	key := w.nextOwnerFeatureLease
	if w.ownerFeatureLeases == nil {
		w.ownerFeatureLeases = make(map[uint64]ownerFeatureLease)
	}
	w.ownerFeatureLeases[key] = ownerFeatureLease{feature: feature, cancel: cancel}
	var once sync.Once
	return callCtx, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.ownerFeatureLeases, key)
			w.mu.Unlock()
			cancel(nil)
		})
	}, nil
}

func ownerFeatureForTool(dispatch *memqlv1.ToolDispatch) OwnerFeature {
	switch dispatch.GetTool() {
	case "workerHost":
		return FeatureHeadless
	case "workerComputer":
		// Passive capability inspection does not take control of the desktop.
		if dispatch.GetAction() != "capabilities" && dispatch.GetAction() != "wait" {
			return FeatureComputerUse
		}
	}
	return ""
}

func (s *streamSession) admitOwnerFeature(ctx context.Context, feature OwnerFeature) (context.Context, func(), error) {
	if feature == "" {
		return ctx, func() {}, nil
	}
	store, ok := s.server.store.(OwnerFeatureStore)
	if !ok {
		return nil, nil, fmt.Errorf("worker_feature_unavailable: machine feature settings cannot be read")
	}
	policy, err := store.ReadOwnerFeaturePolicy(ctx, s.worker.RegistrationId, s.worker.OwnerUserId)
	if err != nil {
		return nil, nil, fmt.Errorf("worker_feature_unavailable: %w", err)
	}
	return s.worker.enterOwnerFeature(ctx, feature, policy)
}

// The event path is prompt; this fresh read repairs a missed event. Failure
// preserves existing calls, while every new admission independently fails
// closed if it cannot read current consent.
func (s *streamSession) recheckOwnerFeatures(now time.Time) {
	if !s.lastOwnerFeatureCheck.IsZero() && now.Sub(s.lastOwnerFeatureCheck) < 15*time.Second {
		return
	}
	s.lastOwnerFeatureCheck = now
	store, ok := s.server.store.(OwnerFeatureStore)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if policy, err := store.ReadOwnerFeaturePolicy(ctx, s.worker.RegistrationId, s.worker.OwnerUserId); err == nil {
		s.worker.ApplyOwnerFeaturePolicy(policy)
	}
}

func (s *EngineStore) ReadOwnerFeaturePolicy(ctx context.Context, registrationID, ownerID string) (OwnerFeaturePolicy, error) {
	if s == nil || s.Engine == nil {
		return OwnerFeaturePolicy{}, fmt.Errorf("worker store unavailable")
	}
	ctx, err := ownerActor(ctx, ownerID)
	if err != nil {
		return OwnerFeaturePolicy{}, err
	}
	ctx = memqlengine.ContextWithFreshRead(ctx)
	call, err := langparser.RenderCall("workersForUser", map[string]any{"ownerUserId": ownerID})
	if err != nil {
		return OwnerFeaturePolicy{}, err
	}
	nodes, err := s.executeAndExtract(ctx, "query "+call)
	if err != nil {
		return OwnerFeaturePolicy{}, err
	}
	for _, node := range nodes {
		g := newWorkerFieldGetter(node)
		if !SameSubjectId(firstNonEmpty(g.str("id"), node.GetId()), registrationID) {
			continue
		}
		if !SameSubjectId(g.str("ownerUserId"), ownerID) || g.str("revokedAt") != "" || node.GetCreatedAt() == nil {
			break
		}
		var features any
		if value := node.GetPayload().GetFields()["ownerFeatures"]; value != nil {
			features = value.AsInterface()
		}
		return OwnerFeaturePolicy{Features: OwnerFeaturesFromRow(features), Version: node.GetCreatedAt().AsTime()}, nil
	}
	return OwnerFeaturePolicy{}, fmt.Errorf("machine is unavailable to its enrolled owner")
}
