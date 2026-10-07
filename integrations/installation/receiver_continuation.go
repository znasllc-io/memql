package installation

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// This value can only be obtained from actual authenticated receiving reads.
// It proves configuration continuity and the exact owned current intent, not
// operator authorization, artifact availability or storage preservation. Those
// independent native proofs remain mandatory before any rollback write.
type receiverContinuationEvidence struct {
	snapshot                                  *receiverSnapshot
	planDigest, invariantDigest, intentDigest string
	target                                    argocd.Target
	observed                                  time.Time
}

func (*receiverContinuationEvidence) String() string {
	return "[private installation continuation evidence]"
}
func (e *receiverContinuationEvidence) GoString() string { return e.String() }

func readReceiverContinuation(ctx context.Context, api argocd.API, namespace, name string, factory CatalogFactory, plan preparedPlan, active argocd.Intent) (*receiverContinuationEvidence, error) {
	if _, err := preparationActor(ctx); err != nil {
		return nil, err
	}
	_, planDigest, err := plan.canonical()
	if err != nil || plan.Preparation == nil || active.Target != plan.Intent.Target {
		return nil, errors.New("installation continuation requires an admitted plan and its exact native target")
	}
	intentDigest, err := active.Digest()
	if err != nil {
		return nil, errors.New("installation continuation intent is invalid")
	}
	snapshot, err := readReceiverInputs(ctx, api, namespace, name, factory, &active)
	if err != nil {
		return nil, err
	}
	evidence := &receiverContinuationEvidence{snapshot: snapshot, planDigest: planDigest,
		invariantDigest: snapshot.invariantDigest, intentDigest: intentDigest,
		target: snapshot.applicationTarget, observed: snapshot.observed}
	if err := evidence.require(plan, active); err != nil {
		return nil, err
	}
	return evidence, nil
}

// The host supplies only its journal-bound parent or validated reversal. Every
// use recomputes both identities; reusing this observation with another plan,
// substituted target, changed intent or an expired read cannot grant a write.
func (e *receiverContinuationEvidence) require(plan preparedPlan, active argocd.Intent) error {
	_, planDigest, planErr := plan.canonical()
	intentDigest, intentErr := active.Digest()
	if e == nil || e.snapshot == nil || planErr != nil || intentErr != nil || plan.Preparation == nil ||
		e.planDigest != planDigest || e.intentDigest != intentDigest || e.target != active.Target || active.Target != plan.Intent.Target ||
		e.snapshot.applicationTarget != e.target || e.snapshot.configuration.InstallationID != plan.InstallationID ||
		!internalDigest.MatchString(e.invariantDigest) || e.snapshot.invariantDigest != e.invariantDigest ||
		plan.Preparation.ConfigurationInvariantDigest != e.invariantDigest ||
		e.observed.IsZero() || e.observed.After(time.Now()) || time.Since(e.observed) > time.Minute || e.snapshot.observed != e.observed {
		return errors.New("installation continuation evidence is absent, expired or bound to different authority")
	}
	return nil
}
