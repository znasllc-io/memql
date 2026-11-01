package worker

import (
	"context"
	"errors"
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

// The codes a refusal at admission carries in a RefusedBeforeSendError. The
// cancellation of a call already admitted does not: see that type.
const (
	codeFeatureDisabled    = "worker_feature_disabled"
	codeFeatureUnavailable = "worker_feature_unavailable"
)

// RefusedBeforeSendError says this replica refused a dispatch at ADMISSION --
// the owner has the feature switched off, or the setting could not be read --
// and that nothing was sent to the machine.
//
// WHY A TYPE. A dispatcher may move a call on to another machine only when it
// is certain nothing ran (design D5): anywhere else the command would run a
// second time. A plain error from the dispatch hook cannot say that, because a
// refusal here and a stream lost mid-call look alike to the caller, so the
// dispatcher had to read every error as "the connection ended, it may have
// run". A machine whose owner had switched Host commands off was therefore
// never routed around: it answered worker_disconnected to every attempt. This
// type is the one claim a re-pick can rest on. It crosses the replica hop as
// WorkerForwardResponse.refused_before_start.
//
// THE INVARIANT: an error of this type comes only from a decision made BEFORE
// the dispatch envelope is handed to the stream's send, and never from what
// happens afterwards. When the owner switches a feature off while a call is in
// flight, ApplyOwnerFeaturePolicy cancels that call with the plain
// featureDisabled error as its context's cause, and streamSession.dispatch
// returns that cause after the send. The machine has the command and may have
// run it; reading that as a refusal would run it again on another machine.
// TestAdmissionRefusalIsTypedButMidCallWithdrawalIsNot pins both sides, and
// integrations/agent/worker's host_commands_hop_test.go what the dispatcher
// does with each.
//
// Code is the stable word callers match on. Err is the detail and the cause (a
// failed settings read stays reachable through errors.Is and errors.As), and
// Error() reads "Code: detail", the sentence these refusals always had.
type RefusedBeforeSendError struct {
	Code string
	Err  error
}

func (e *RefusedBeforeSendError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *RefusedBeforeSendError) Unwrap() error { return e.Err }

// RefusedBeforeSend reports whether err, or an error it wraps, is a refusal at
// admission, and its code. False is not "it failed": it means the call may have
// reached the machine, which is what a caller must assume of every other error.
func RefusedBeforeSend(err error) (code string, ok bool) {
	var refused *RefusedBeforeSendError
	if errors.As(err, &refused) && refused != nil {
		return refused.Code, true
	}
	return "", false
}

type ownerFeatureLease struct {
	feature OwnerFeature
	cancel  context.CancelCauseFunc
}

// featureDisabledReason is the one sentence the owner reads, whether the call
// was refused at admission or cancelled after it. It names the feature as the
// engine's own refusals and MemQL OS do, and the switch by the label Fleet
// settings draws (clients/os/src/apps/fleet/machines/OwnerFeaturesGroup.tsx),
// so the owner finds what to turn back on. The code travels beside it
// (RefusedBeforeSendError.Code), so the sentence does not repeat it.
func featureDisabledReason(feature OwnerFeature) string {
	switch feature {
	case FeatureHeadless:
		return "Host commands are off for this machine in Fleet settings (Commands and files)"
	case FeatureApps:
		return "CLIs and apps are off for this machine in Fleet settings"
	case FeatureComputerUse:
		return "Computer use is off for this machine in Fleet settings"
	}
	return fmt.Sprintf("%s is off for this machine in Fleet settings", feature)
}

// featureDisabled is the error an admitted call is cancelled with when the
// owner switches its feature off (ApplyOwnerFeaturePolicy). It is PLAIN on
// purpose, never a RefusedBeforeSendError: the call was admitted, and a command
// among those cancelled has already been sent to the machine.
func featureDisabled(feature OwnerFeature) error {
	return errors.New(codeFeatureDisabled + ": " + featureDisabledReason(feature))
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
		// The cause is the plain error, whatever the call's state: these calls
		// were admitted, and one may be on the machine already. A typed refusal
		// would send it to a second machine (RefusedBeforeSendError).
		lease.cancel(featureDisabled(lease.feature))
	}
}

// enterOwnerFeature admits one call under the policy just read, and returns the
// context the owner's later withdrawal cancels. Both refusals are decided here,
// before the caller sends anything, so both are typed (RefusedBeforeSendError).
// A cancelled caller is neither the owner's word nor the machine's, and stays
// the plain context error.
func (w *Worker) enterOwnerFeature(ctx context.Context, feature OwnerFeature, policy OwnerFeaturePolicy) (context.Context, func(), error) {
	if policy.Version.IsZero() {
		return nil, nil, &RefusedBeforeSendError{Code: codeFeatureUnavailable, Err: errors.New("machine settings have no version")}
	}
	w.ApplyOwnerFeaturePolicy(policy)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ownerFeaturePolicy.Features.Allows(feature) {
		return nil, nil, &RefusedBeforeSendError{Code: codeFeatureDisabled, Err: errors.New(featureDisabledReason(feature))}
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

// admitOwnerFeature is the owner's consent, read fresh at the moment a call is
// admitted and before anything is sent to the machine. A setting that cannot be
// read refuses, as the stricter reading, rather than passing as "on". Every
// refusal is a RefusedBeforeSendError, because the caller has sent nothing yet:
// the dispatcher may try another machine, and the owner's later withdrawal
// (which cancels the admitted call) is deliberately a different, plain error.
func (s *streamSession) admitOwnerFeature(ctx context.Context, feature OwnerFeature) (context.Context, func(), error) {
	if feature == "" {
		return ctx, func() {}, nil
	}
	store, ok := s.server.store.(OwnerFeatureStore)
	if !ok {
		return nil, nil, &RefusedBeforeSendError{Code: codeFeatureUnavailable, Err: errors.New("machine feature settings cannot be read")}
	}
	policy, err := store.ReadOwnerFeaturePolicy(ctx, s.worker.RegistrationId, s.worker.OwnerUserId)
	if err != nil {
		return nil, nil, &RefusedBeforeSendError{Code: codeFeatureUnavailable, Err: err}
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
