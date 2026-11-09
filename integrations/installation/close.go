package installation

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// CloseSelected ends the original operator's started plan without completion
// proof, so an update that can no longer be verified or restored stops holding
// the installation. The case it exists for is a saved protected-resource digest
// that can no longer be reproduced: Check progress (completion.go) and Restore
// previous release (rollback_workflow.go) both compare fresh evidence with that
// digest, and Cancel admits only an unstarted plan. Production's 0.31.27 plan of
// 2026-11-09 was saved under the whole-object owner fingerprint that engine
// b74ef4f3a replaced, so neither engine can reproduce it.
//
// It changes no Application. Under the plan's effect lock it reads every intent
// the plan may have written (dispatchedIntents) and refuses while one still has
// its own controller operation waiting or running, because that write would
// finish with no journal owner. It does not require an Application to still sit
// at the plan's intent: a hand deploy may have moved it on, or the write may
// never have landed. A read failure, a deleted Application included, refuses.
// The journal records the operator, the time and their reason, releases the
// head and Application claims, and leaves no rollback eligibility behind.
//
// The lock excludes every live writer. A writer whose process died mid-request
// releases it, and in that narrow window its request could still land after the
// close; the controller's next status write moves the Application's
// resourceVersion, after which that request's compare-and-swap refuses.
// Regression tests: TestInstallationCloseSelected* in close_db_test.go.
//
// A restoration of a completed update (reverting) is closed differently: when
// no reverse write has a durable start, nothing of it reached the cluster, so
// the close returns the attempt to its verified completed state with the
// operator, time and reason recorded (withdrawRestoration). It reads no
// Application, because none was written and the forward operations finished
// before completion. Before this, a restoration left reverting by a refused
// check held the installation with no way to end it. A restoration that has
// recorded any reverse start may have reached the cluster and is not closable.
// Regression tests: TestInstallationRestoration* in restore_lifecycle_db_test.go.
func (p *Preparer) CloseSelected(ctx context.Context, installationID, planID, reason string) (SelectionResult, error) {
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	if !identifier.MatchString(installationID) || !internalDigest.MatchString(planID) {
		return SelectionResult{}, errors.New("exact installation and plan identities are required")
	}
	if err := validCloseReason(reason); err != nil {
		return SelectionResult{}, err
	}
	actor, err := preparationActor(ctx)
	if err != nil {
		return SelectionResult{}, err
	}
	client, err := argocd.New(p.host.api)
	if err != nil {
		return SelectionResult{}, err
	}
	journal := &revisionJournal{db: p.host.journal.db}
	closed, err := journal.withEffectLock(ctx, planID, func(ctx context.Context) (any, error) {
		r, err := journal.get(ctx, installationID, planID)
		if err != nil {
			return nil, err
		}
		if r.Plan.RequestedBy != actor {
			return nil, errors.New("only the operator who started this installation may close it")
		}
		if r.State == "closed" {
			return r, nil
		}
		if r.State == "reverting" || (r.State == "complete" && r.lastWithdrawalClosed()) {
			// The second form recovers a lost reply to this same close.
			return journal.withdrawRestoration(ctx, installationID, planID, withdrawalClosed, reason)
		}
		if r.State != "applying" {
			return nil, errors.New("only an installation that is still being applied, or a restoration that has changed no application, can be closed")
		}
		intents, err := r.dispatchedIntents()
		if err != nil {
			return nil, err
		}
		for _, intent := range intents {
			settled, err := client.OperationSettled(ctx, intent)
			if err != nil {
				return nil, fmt.Errorf("installation close could not read Application %s: %w", intent.Target.Name, err)
			}
			if !settled {
				return nil, fmt.Errorf("this update's own controller operation on Application %s is still running; check progress, and close the update after that operation finishes", intent.Target.Name)
			}
		}
		digests, err := intentDigests(intents)
		if err != nil {
			return nil, err
		}
		return journal.close(ctx, installationID, planID, reason, digests)
	})
	if err != nil {
		return SelectionResult{}, err
	}
	return selectionResult(ctx, closed.(revisionRecord)), nil
}
