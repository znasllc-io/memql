package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (r revisionRecord) rollbackActive() bool { return r.State == "applying" || r.State == "reverting" }

// Eligibility is a fresh review projection, never an effect grant. The same
// predicate is checked under the installation head lock when reclaiming it.
func rollbackEligibility(ctx context.Context, tx *sql.Tx, r revisionRecord, actor string) (bool, error) {
	if actor != r.Plan.RequestedBy || r.Plan.Preparation == nil || r.Plan.HealthBaseline == nil || r.Plan.RollbackWorkflow == nil || r.Plan.CompletionWorkflow == nil {
		return false, nil
	}
	var active, preparation sql.NullString
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT active_plan_id,active_preparation_id,slot_epoch FROM installation_revision_heads WHERE installation_id=$1`, r.Plan.InstallationID).Scan(&active, &preparation, &epoch); err != nil {
		return false, err
	}
	if epoch != r.SlotEpoch || preparation.Valid {
		return false, nil
	}
	if r.rollbackActive() {
		return active.Valid && active.String == r.ID, nil
	}
	if r.State != "complete" || active.Valid || r.Completion == nil || r.Rollback != nil {
		return false, nil
	}
	for _, app := range r.Plan.applicationMembers() {
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM installation_application_claims WHERE namespace=$1 AND name=$2)`, app.Intent.Target.Namespace, app.Intent.Target.Name).Scan(&occupied); err != nil {
			return false, err
		}
		if occupied {
			return false, nil
		}
	}
	return true, nil
}

var errRestoreIneligible = errors.New("rollback requires the original operator's latest unchanged installation attempt")

// reacquireCompleted is the head transfer itself, under the head and attempt
// locks its caller holds: it claims every member Application for the original
// preparation again, points the head at this attempt and marks it reverting.
// A newer preparation (even cancelled), plan or target claim permanently
// prevents a historical attempt taking over; it grants no external effect.
//
// Its one production caller is reserveCompletedRollback, which stores the
// reversal in the same transaction. Engines up to 0.31.28 made this transfer
// on its own before any restoration check (the two-step admission the tests
// keep as reacquireRollback), so a refused check left the installation held;
// withdrawRestoration returns an attempt they left that way to complete.
func reacquireCompleted(ctx context.Context, tx *sql.Tx, installation, key string, r revisionRecord, actor string) error {
	allowed, err := rollbackEligibility(ctx, tx, r, actor)
	if err != nil {
		return err
	}
	if !allowed || r.State != "complete" {
		return errRestoreIneligible
	}
	preparation, err := readPreparation(ctx, tx, installation, r.Plan.Preparation.ID)
	if err != nil {
		return err
	}
	if preparation.State != "promoted" || preparation.PromotedPlanID != key || preparation.SlotEpoch+1 != r.SlotEpoch || preparation.Scope.RequestedBy != actor {
		return errors.New("completed rollback lost its original preparation")
	}
	if _, err := promotedRevision(ctx, tx, preparation); err != nil {
		return err
	}
	if err := claimApplications(ctx, tx, preparation.Scope, preparation.ID); err != nil {
		return err
	}
	if err := requireApplicationClaims(ctx, tx, r.Plan); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=$2 WHERE installation_id=$1`, installation, key); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='reverting',updated_at=clock_timestamp() WHERE plan_id=$1`, key)
	return err
}

// reserveCompletedRollback is the one transaction in which Restore previous
// release takes a completed update back: it rechecks eligibility under the head
// lock, reclaims the head and every member Application, marks the attempt
// reverting and stores the admitted reversal. RollbackSelected calls it only
// after the rollback recipe's every refusable check has passed without the
// head (rollbackWorkflowScope.qualifying): configuration and its invariant,
// signed publications and compatibility, retained sources and renders,
// resources and images, storage and protected-resource digests, unchanged
// migrations and the planned reversal of each member's own finished operation.
// Before this, the head was taken first and any of those refusals left the
// plan reverting, and nothing could end it (close, cancel and completion all
// refuse a reverting attempt with no reversal written).
//
// An attempt already reverting (a lost reply, or another replica's request)
// stores the reversal under the head it holds, replacing one that has not
// started (replaceUnstartedRollback): replacing it, rather than refusing,
// keeps this request from withdrawing the other replica's reservation.
func (j *revisionJournal) reserveCompletedRollback(ctx context.Context, installation, key string, plan rollbackPlan) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if r.State == "complete" {
			if err := reacquireCompleted(ctx, tx, installation, key, *r, actor); err != nil {
				return err
			}
			current, err := readRevision(ctx, tx, installation, key)
			if err != nil {
				return err
			}
			*r = current
		}
		return insertRollback(ctx, tx, key, *r, actor, plan, true)
	})
}

// restorationUnwritten reports whether a restoration of a completed update has
// recorded no start that could have reached the cluster. Every reverse Apply
// commits a durable start first, under the plan's effect lock and after every
// apply check: a member's own start (beginRollbackApplication), for a group
// and for a single-Application reversal that follows the per-apply protocol
// (perApplyStarts). A single-Application reversal an engine up to 0.31.28 may
// have written has only the reversal's start (beginRollback), which then
// counts as a possible write. Without one of those markers no process sent a
// reverse write, so nothing of the reversal can still land.
func (r revisionRecord) restorationUnwritten() bool {
	if r.State != "reverting" || r.Completion == nil {
		return false
	}
	if r.Rollback == nil || !r.Rollback.Started {
		return true
	}
	if r.Rollback.Plan.FormatVersion != 2 {
		reverse, _ := r.Rollback.application("")
		return r.Rollback.perApplyStarts() && !reverse.Started
	}
	for _, member := range r.Rollback.Applications {
		if member.Started {
			return false
		}
	}
	return true
}

const (
	withdrawalRefused = "refused"
	withdrawalClosed  = "closed"
	// maxWithdrawnRestorations bounds the history kept on one attempt; the
	// oldest entries are dropped beyond it.
	maxWithdrawnRestorations = 32
)

// withdrawnRestoration is the record of one restoration returned to its
// completed attempt before it wrote anything: automatically after one of its
// checks refused it ("refused", no reason), or by its operator's close
// ("closed", with their one-line reason). RollbackID names the reversal that
// had been reserved, if any; that row itself is deleted, because a completed
// attempt carries no reversal.
type withdrawnRestoration struct {
	Kind       string `json:"kind"`
	By         string `json:"by"`
	At         string `json:"at"`
	Reason     string `json:"reason,omitempty"`
	RollbackID string `json:"rollbackId,omitempty"`
}

func decodeWithdrawnRestorations(body []byte, r revisionRecord) ([]withdrawnRestoration, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var out []withdrawnRestoration
	if receiverJSON(body, &out) != nil || len(out) == 0 || len(out) > maxWithdrawnRestorations || r.Completion == nil {
		return nil, errors.New("installation withdrawn restorations are malformed")
	}
	for _, entry := range out {
		at, err := time.Parse(time.RFC3339Nano, entry.At)
		reasonOK := (entry.Kind == withdrawalRefused && entry.Reason == "") || (entry.Kind == withdrawalClosed && validCloseReason(entry.Reason) == nil)
		if err != nil || at.IsZero() || !reasonOK || entry.By != r.Plan.RequestedBy || (entry.RollbackID != "" && !internalDigest.MatchString(entry.RollbackID)) {
			return nil, errors.New("installation withdrawn restoration is inconsistent")
		}
	}
	return out, nil
}

// lastWithdrawalClosed reports whether this attempt's newest withdrawal was its
// operator's close, which a lost close reply recovers.
func (r revisionRecord) lastWithdrawalClosed() bool {
	n := len(r.WithdrawnRestorations)
	return n > 0 && r.WithdrawnRestorations[n-1].Kind == withdrawalClosed
}

// withdrawRestoration returns a restoration of a completed update that never
// wrote an Argo intent (restorationUnwritten) to the completed attempt it
// started from: the reserved reversal, if any, is deleted, the attempt is
// complete again with its original forward receipt, and the installation head
// and every member's claim are released, exactly as they stood before Restore
// previous release was pressed, so the operator may restore again or prepare
// another update. The record keeps who, when and why (kind, and for a close
// the operator's reason).
//
// Its two callers: RollbackSelected after any of the restoration's checks
// refused it, and CloseSelected, the operator's explicit end of a restoration
// that has written nothing. A restoration that recorded any reverse start may
// have reached the cluster and is refused here, as it always was.
//
// The caller holds the plan's effect lock, so no reverse Apply runs on any
// host meanwhile; the markers are read again here under the head lock.
func (j *revisionJournal) withdrawRestoration(ctx context.Context, installation, key, kind, reason string) (revisionRecord, error) {
	if kind == withdrawalClosed {
		if err := validCloseReason(reason); err != nil {
			return revisionRecord{}, err
		}
	} else if kind != withdrawalRefused || reason != "" {
		return revisionRecord{}, errors.New("installation restoration withdrawal is invalid")
	}
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if actor != r.Plan.RequestedBy {
			return errors.New("only the operator who started this installation may end its restoration")
		}
		if r.State == "complete" && kind == withdrawalClosed && r.lastWithdrawalClosed() {
			return nil // A lost reply recovers the stored close; its reason stands.
		}
		if r.State != "reverting" || r.Completion == nil {
			return errors.New("only a restoration of a completed update can be withdrawn")
		}
		if !r.restorationUnwritten() {
			return errors.New("this restoration has started a reverse sync, so it can no longer be withdrawn; resume it or check its progress")
		}
		var at time.Time
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
			return err
		}
		entry := withdrawnRestoration{Kind: kind, By: actor, At: at.UTC().Format(time.RFC3339Nano), Reason: reason}
		if r.Rollback != nil {
			entry.RollbackID = r.Rollback.ID
		}
		entries := append(append([]withdrawnRestoration(nil), r.WithdrawnRestorations...), entry)
		if len(entries) > maxWithdrawnRestorations {
			entries = entries[len(entries)-maxWithdrawnRestorations:]
		}
		body, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM installation_revision_rollbacks WHERE parent_plan_id=$1`, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='complete',withdrawn_restorations=$2::jsonb,updated_at=clock_timestamp() WHERE plan_id=$1`, key, string(body)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=NULL WHERE installation_id=$1 AND active_plan_id=$2 AND slot_epoch=$3`, installation, key, r.SlotEpoch); err != nil {
			return err
		}
		return releaseApplicationClaims(ctx, tx, installation, r.Plan.Preparation.ID)
	})
}

// withdrawRefusedRestoration is RollbackSelected's answer to a refusal: under
// the plan's effect lock it returns the restoration to its completed attempt
// when, and only when, nothing of it was written. It reports nil when there was
// nothing to withdraw (the attempt was not reverting, or its reversal has a
// recorded start).
func (j *revisionJournal) withdrawRefusedRestoration(ctx context.Context, installation, key string) (*revisionRecord, error) {
	out, err := j.withEffectLock(ctx, key, func(ctx context.Context) (any, error) {
		r, err := j.get(ctx, installation, key)
		if err != nil {
			return nil, err
		}
		if !r.restorationUnwritten() {
			return nil, nil
		}
		withdrawn, err := j.withdrawRestoration(ctx, installation, key, withdrawalRefused, "")
		if err != nil {
			return nil, err
		}
		return &withdrawn, nil
	})
	if err != nil || out == nil {
		return nil, err
	}
	return out.(*revisionRecord), nil
}

// reopenCompleted reopens one retained source of a completed update while
// Restore previous release qualifies it, before the installation head is
// taken back. It makes the same checks revisionWorkflowScope.reopen makes of
// the promoted preparation and its capture entry, then reads through
// reopenCompletedCapture, which fences the read by the released head instead of
// an active one.
func (s *rollbackWorkflowScope) reopenCompleted(ctx context.Context, r revisionRecord, args map[string]any) (any, error) {
	scope := s.revision
	role, sourceRole, member, err := scope.sourceKey(ctx, args)
	if err != nil {
		return nil, err
	}
	binding := r.Plan.Preparation
	if scope.configuration == nil || binding == nil {
		return nil, errors.New("installation retained source requires fresh admission")
	}
	if scope.preparation.ID == "" {
		record, err := scope.host.journal.get(ctx, r.Plan.InstallationID, binding.ID)
		if err != nil {
			return nil, err
		}
		if record.State != "promoted" || record.PromotedPlanID != r.ID || record.Scope.InstallationID != r.Plan.InstallationID || record.Scope.RequestedBy != scope.operator ||
			record.Scope.ExecutionWorkflowDigest != scope.workflow.digest || record.Scope.ConfigurationDigest != binding.ConfigurationDigest || record.Scope.ConfigurationInvariantDigest != binding.ConfigurationInvariantDigest {
			return nil, errors.New("installation retained sources are outside the exact promoted preparation")
		}
		scope.preparation = record
	}
	entry, exists := scope.preparation.Captures[role]
	wantSource, wantReceipt := binding.CandidateSourceDigest, binding.CandidateReceiptDigest
	if sourceRole == "rollback" {
		wantSource, wantReceipt = binding.RollbackSourceDigest, binding.RollbackReceiptDigest
	}
	if r.Plan.FormatVersion == 2 {
		wantSource, wantReceipt = member.CandidateSourceDigest, member.CandidateReceiptDigest
		if sourceRole == "rollback" {
			wantSource, wantReceipt = member.RollbackSourceDigest, member.RollbackReceiptDigest
		}
	}
	if !exists || !entry.Acknowledged || entry.Receipt == nil || entry.SourceDigest != wantSource || entry.ReceiptDigest != wantReceipt {
		return nil, errors.New("installation retained source differs from its promoted digest and receipt")
	}
	proof, err := scope.host.journal.reopenCompletedCapture(ctx, r.Plan.InstallationID, binding.ID, r.ID, role, scope.host.files)
	if err != nil {
		return nil, err
	}
	scope.sources[role] = proof
	return nil, nil
}

// reopenCompletedCapture is reopenPromotedCapture for a completed update under
// restore admission: the same native closure proof of the exact retained
// receipt, fenced on both sides of the external read by the head having no
// active plan or preparation, no successor epoch, and the completed attempt
// still eligible for restoration. A preparation or plan that takes the head
// meanwhile discards the proof, and the reservation's own transaction refuses
// it again (reserveCompletedRollback).
func (j *preparationJournal) reopenCompletedCapture(ctx context.Context, installation, preparation, plan, role string, files sourceCaptureFiles) (verifiedSourceCapture, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	record, capture, err := j.completedCapture(ctx, installation, preparation, plan, role)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	entry := record.Captures[role]
	proof, err := reopenSourceGraph(ctx, record, role, files)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if proof.scope != capture.digest || proof.source.Digest() != entry.SourceDigest ||
		preparationReceiptDigest(proof.receipt) != entry.ReceiptDigest ||
		!sameJSON(proof.source.Spec(), capture.render) {
		return verifiedSourceCapture{}, errors.New("retained source differs from its promoted native binding")
	}
	fresh, _, err := j.completedCapture(ctx, installation, preparation, plan, role)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if !sameJSON(record, fresh) {
		return verifiedSourceCapture{}, errors.New("promoted source journal changed during verification")
	}
	if err := ctx.Err(); err != nil {
		return verifiedSourceCapture{}, err
	}
	return proof, nil
}

func (j *preparationJournal) completedCapture(ctx context.Context, installation, preparation, plan, role string) (preparationRecord, sourceCapture, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if !identifier.MatchString(installation) || !internalDigest.MatchString(preparation) || !internalDigest.MatchString(plan) || !captureStepKey.MatchString(role) {
		return preparationRecord{}, sourceCapture{}, errors.New("promoted source requires exact native installation identities and role")
	}
	db, err := j.database()
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	defer tx.Rollback()
	active, preparing, epoch, err := lockPreparationHead(ctx, tx, installation)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if active != "" || preparing != "" {
		return preparationRecord{}, sourceCapture{}, errChanged
	}
	r, err := readPreparation(ctx, tx, installation, preparation)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if r.State != "promoted" || r.PromotedPlanID != plan || actor != r.Scope.RequestedBy {
		return preparationRecord{}, sourceCapture{}, errors.New("retained source is outside the original requester's promoted preparation")
	}
	parent, err := promotedRevision(ctx, tx, r)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if parent.ID != plan || parent.SlotEpoch != epoch || parent.State != "complete" {
		return preparationRecord{}, sourceCapture{}, errChanged
	}
	eligible, err := rollbackEligibility(ctx, tx, parent, actor)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if !eligible {
		return preparationRecord{}, sourceCapture{}, errRestoreIneligible
	}
	_, root := r.Scope.Captures[role]
	entry, found := r.Captures[role]
	if !root || !found || !entry.Acknowledged || entry.Receipt == nil || !internalDigest.MatchString(entry.SourceDigest) || !internalDigest.MatchString(entry.ReceiptDigest) {
		return preparationRecord{}, sourceCapture{}, errors.New("promoted source has no exact retained receipt")
	}
	capture, err := newSourceCapture(r.Scope.Captures[role])
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if err := tx.Commit(); err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	return r, capture, nil
}
