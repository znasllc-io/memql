package installation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// This binding makes the source consumer survive the handoff to the revision
// journal. Promotion does not release source artifact pins. Their eventual
// retirement must account for the revision, including its rollback lifetime.
type preparationBinding struct {
	ArtifactDigest               string `json:"artifactDigest"`
	ArtifactWorkflowDigest       string `json:"artifactWorkflowDigest"`
	ArtifactExpiresAt            string `json:"artifactExpiresAt"`
	RollbackPublicationDigest    string `json:"rollbackPublicationDigest"`
	ID                           string `json:"id"`
	ConfigurationDigest          string `json:"configurationDigest"`
	ConfigurationInvariantDigest string `json:"configurationInvariantDigest"`
	CandidateSourceDigest        string `json:"candidateSourceDigest"`
	RollbackSourceDigest         string `json:"rollbackSourceDigest"`
	CandidateReceiptDigest       string `json:"candidateReceiptDigest"`
	RollbackReceiptDigest        string `json:"rollbackReceiptDigest"`
	StorageDigest                string `json:"storageDigest"`
	SensitiveDigest              string `json:"sensitiveDigest"`
}

func (b preparationBinding) validate() error {
	if expires, err := time.Parse(time.RFC3339Nano, b.ArtifactExpiresAt); err != nil || expires.IsZero() || !artifactDigest.MatchString(b.RollbackPublicationDigest) {
		return errors.New("installation artifact binding is incomplete")
	}
	for _, value := range []string{b.ID, b.ConfigurationDigest, b.ConfigurationInvariantDigest, b.CandidateSourceDigest, b.RollbackSourceDigest,
		b.CandidateReceiptDigest, b.RollbackReceiptDigest, b.StorageDigest, b.SensitiveDigest, b.ArtifactDigest, b.ArtifactWorkflowDigest} {
		if !internalDigest.MatchString(value) {
			return errors.New("installation preparation binding is incomplete")
		}
	}
	return nil
}

// Native verifier results, never deserialized client/DSL evidence. The scoped
// preparer must reobserve configuration, compatibility and availability before
// exposing an update. This private journal operation only transfers ownership
// and verifies the relationships among the supplied native observations.
type promotionEvidence struct {
	sources                         map[string]verifiedSourceCapture
	renders                         map[string]argocd.RenderedRevision
	configuration                   *receiverSnapshot
	artifacts                       artifactEvidence
	published                       pipelines.VerifiedPublishedRelease
	candidateSource, rollbackSource verifiedSourceCapture
	candidateRender, rollbackRender argocd.RenderInventory
	resources                       resourceEvidence
	storage                         storageEvidence
	sensitive                       sensitiveEvidence
}

func promotionPlan(r preparationRecord, plan preparedPlan, configuration string, evidence promotionEvidence) (preparedPlan, error) {
	if plan.Preparation != nil || configuration != r.Scope.ConfigurationDigest ||
		plan.InstallationID != r.Scope.InstallationID || plan.RequestedBy != r.Scope.RequestedBy ||
		plan.WorkflowDigest != r.Scope.WorkflowDigest || plan.ExecutionWorkflowDigest != r.Scope.ExecutionWorkflowDigest || plan.CandidateID != r.Scope.CandidateID || plan.PublicationDigest != r.Scope.PublicationDigest {
		return preparedPlan{}, errors.New("installation plan differs from its reserved preparation")
	}
	artifacts := evidence.artifacts
	if evidence.configuration == nil || evidence.configuration.digest != configuration || evidence.configuration.invariantDigest != r.Scope.ConfigurationInvariantDigest || evidence.configuration.observed.IsZero() ||
		evidence.configuration.observed.After(time.Now()) || time.Since(evidence.configuration.observed) > time.Minute ||
		artifacts.configuration != configuration || !internalDigest.MatchString(artifacts.digest) || !internalDigest.MatchString(artifacts.workflow) ||
		artifacts.operator != r.Scope.RequestedBy || artifacts.candidate != plan.PublicationDigest ||
		!artifactDigest.MatchString(artifacts.rollback) || artifacts.resources != plan.ResourceDiffDigest ||
		artifacts.before != plan.RollbackRenderDigest || artifacts.after != plan.RenderDigest || !artifacts.expires.After(time.Now()) {
		return preparedPlan{}, errors.New("promotion requires fresh native configuration and complete scoped artifact evidence")
	}
	release, err := evidence.published.Release()
	if err != nil || evidence.published.Digest() != plan.PublicationDigest || release.CandidateID != plan.CandidateID || release.ApprovalID != plan.CandidateApprovalID {
		return preparedPlan{}, errors.New("installation plan differs from its signed publication")
	}
	proofs := evidence.sources
	if proofs == nil {
		proofs = map[string]verifiedSourceCapture{"candidate": evidence.candidateSource, "rollback": evidence.rollbackSource}
	}
	for _, member := range r.Scope.applicationMembers() {
		for _, role := range []string{"candidate", "rollback"} {
			key := applicationCaptureKey(member.Key, role)
			proof := proofs[key]
			entry := r.Captures[key]
			capture, err := newSourceCapture(r.Scope.Captures[key])
			if err != nil || !entry.Acknowledged || entry.Receipt == nil || proof.scope != capture.digest || proof.receipt.IntentID != entry.Receipt.IntentID || proof.source.Digest() == "" || proof.source.Digest() != entry.SourceDigest || preparationReceiptDigest(proof.receipt) != entry.ReceiptDigest || !sameJSON(proof.source.Spec(), capture.render) {
				return preparedPlan{}, errors.New("promotion requires exact verified and acknowledged source captures")
			}
			if plan.FormatVersion == 2 {
				render := evidence.renders[key]
				app, memberErr := plan.application(member.Key)
				wantRender, wantSource, wantReceipt := app.RenderDigest, app.CandidateSourceDigest, app.CandidateReceiptDigest
				if role == "rollback" {
					wantRender, wantSource, wantReceipt = app.RollbackRenderDigest, app.RollbackSourceDigest, app.RollbackReceiptDigest
				}
				if memberErr != nil || wantRender != render.Digest() || wantSource != entry.SourceDigest || wantReceipt != entry.ReceiptDigest {
					return preparedPlan{}, errors.New("group plan member differs from native source and render evidence")
				}
				if render.Digest() == "" || !sameJSON(render.Spec(), proof.source.Spec()) {
					return preparedPlan{}, errors.New("group member render differs from closed source")
				}
			}
		}
	}

	before, after := evidence.rollbackRender, evidence.candidateRender
	if !validRenderInventory(before) || !validRenderInventory(after) ||
		(plan.FormatVersion == 1 && (!sameJSON(before.RenderScopes()[0], evidence.rollbackSource.source.Spec()) || !sameJSON(after.RenderScopes()[0], evidence.candidateSource.source.Spec()))) ||
		plan.RenderDigest != after.Digest() || plan.RollbackRenderDigest != before.Digest() ||
		evidence.resources.publication != plan.PublicationDigest || evidence.resources.before != before.Digest() || evidence.resources.after != after.Digest() ||
		plan.ResourceDiffDigest != evidence.resources.digest || !internalDigest.MatchString(evidence.resources.digest) ||
		evidence.storage.before != before.Digest() || evidence.storage.after != after.Digest() ||
		evidence.sensitive.before != before.Digest() || evidence.sensitive.after != after.Digest() {
		return preparedPlan{}, errors.New("promotion evidence does not describe one candidate and rollback render")
	}
	now := time.Now()
	if !freshPreservationObservation(evidence.resources.observed, now) || !freshPreservationObservation(evidence.storage.observed, now) || !freshPreservationObservation(evidence.sensitive.observed, now) {
		return preparedPlan{}, errors.New("promotion requires freshly observed resources, storage and protected resources")
	}
	if err := promotionIntentMatches(r.Scope, plan); err != nil {
		return preparedPlan{}, err
	}
	plan.Preparation = &preparationBinding{ID: r.ID, ConfigurationDigest: configuration, ConfigurationInvariantDigest: evidence.configuration.invariantDigest,
		CandidateSourceDigest: preparationSourcesDigest(r, "candidate", false), RollbackSourceDigest: preparationSourcesDigest(r, "rollback", false),
		CandidateReceiptDigest: preparationSourcesDigest(r, "candidate", true), RollbackReceiptDigest: preparationSourcesDigest(r, "rollback", true),
		StorageDigest: evidence.storage.digest, SensitiveDigest: evidence.sensitive.digest,
		ArtifactDigest: artifacts.digest, ArtifactWorkflowDigest: artifacts.workflow, ArtifactExpiresAt: artifacts.expires.UTC().Format(time.RFC3339Nano), RollbackPublicationDigest: artifacts.rollback}
	if _, _, err := plan.canonical(); err != nil {
		return preparedPlan{}, err
	}
	return plan, nil
}

// The intent must equal the native protocol baseline reserved before source
// dispatch: Application namespace/name/UID, generation, complete spec,
// destination cluster, request identity and sync options. Receiving admission
// must still authenticate that baseline and renderer; this comparison does not
// discover a live Application from a name or a claimed configuration digest.
func promotionIntentMatches(scope preparationScope, plan preparedPlan) error {
	if scope.FormatVersion == 2 {
		if plan.FormatVersion != 2 || len(plan.Applications) != len(scope.Applications) {
			return errors.New("group plan membership differs from preparation")
		}
		for i, member := range scope.Applications {
			app := plan.Applications[i]
			if app.Key != member.Key {
				return errors.New("group plan application order differs")
			}
			one := preparationScope{Intent: member.Intent, Captures: map[string]sourceCaptureSpec{"candidate": scope.Captures[applicationCaptureKey(member.Key, "candidate")], "rollback": scope.Captures[applicationCaptureKey(member.Key, "rollback")]}}
			if err := promotionIntentMatches(one, preparedPlan{Intent: app.Intent}); err != nil {
				return err
			}
		}
		return nil
	}

	expectedIntent, expectedErr := scope.Intent.Digest()
	actualIntent, actualErr := plan.Intent.Digest()
	if expectedErr != nil || actualErr != nil || expectedIntent != actualIntent {
		return errors.New("installation intent differs from the reserved native Application intent")
	}
	before, after := scope.Captures["rollback"].Render, scope.Captures["candidate"].Render
	var spec struct {
		Source      json.RawMessage `json:"source"`
		Project     string          `json:"project"`
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
	}
	var target struct {
		Revision string `json:"targetRevision"`
	}
	if json.Unmarshal(plan.Intent.BeforeSpec, &spec) != nil || json.Unmarshal(after.Source, &target) != nil {
		return errors.New("installation intent differs from the captured Application source")
	}
	// JSONB may reorder the persisted source fields. Compare canonical values,
	// never the raw encoding from the original host versus the replacement.
	actual, actualErr := canonicalJSON(spec.Source)
	expected, expectedErr := canonicalJSON(before.Source)
	if actualErr != nil || expectedErr != nil || !bytes.Equal(actual, expected) || spec.Project != before.ProjectName || spec.Destination.Namespace != before.Namespace ||
		plan.Intent.Target.Name != before.AppName || plan.Intent.Revision != target.Revision {
		return errors.New("installation intent differs from the captured Application source")
	}
	return nil
}

// All callers lock the shared head before either journal row. This check also
// permits historical reads after a revision is cancelled or superseded; it
// never restores that historical revision as the active head.
func promotedRevision(ctx context.Context, tx *sql.Tx, r preparationRecord) (revisionRecord, error) {
	revision, err := readRevision(ctx, tx, r.Scope.InstallationID, r.PromotedPlanID)
	if err != nil {
		return revisionRecord{}, err
	}
	b := revision.Plan.Preparation
	if r.State != "promoted" || b == nil || b.ID != r.ID || b.ConfigurationDigest != r.Scope.ConfigurationDigest || b.ConfigurationInvariantDigest != r.Scope.ConfigurationInvariantDigest ||
		revision.SlotEpoch != r.SlotEpoch+1 || revision.Plan.RequestedBy != r.Scope.RequestedBy ||
		revision.Plan.WorkflowDigest != r.Scope.WorkflowDigest || revision.Plan.CandidateID != r.Scope.CandidateID || revision.Plan.PublicationDigest != r.Scope.PublicationDigest ||
		b.CandidateSourceDigest != preparationSourcesDigest(r, "candidate", false) || b.RollbackSourceDigest != preparationSourcesDigest(r, "rollback", false) ||
		b.CandidateReceiptDigest != preparationSourcesDigest(r, "candidate", true) || b.RollbackReceiptDigest != preparationSourcesDigest(r, "rollback", true) {
		return revisionRecord{}, errors.New("installation preparation and promoted revision disagree")
	}
	if err := promotionIntentMatches(r.Scope, revision.Plan); err != nil {
		return revisionRecord{}, err
	}
	return revision, nil
}

// promote performs no external effects. Its one transaction binds the complete
// prepared plan, marks its preparation consumed, and transfers the installation
// head without an unowned interval. A lost commit reply is recovered by the
// immutable preparation/plan relationship, never by reserving a second plan.
func (j *preparationJournal) promote(ctx context.Context, installation, key, workflow, configuration string, plan preparedPlan, evidence promotionEvidence) (revisionRecord, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return revisionRecord{}, err
	}
	if !identifier.MatchString(installation) || !internalDigest.MatchString(key) || !internalDigest.MatchString(workflow) || !internalDigest.MatchString(configuration) {
		return revisionRecord{}, errors.New("exact preparation and configuration identities required")
	}
	db, err := j.database()
	if err != nil {
		return revisionRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return revisionRecord{}, err
	}
	defer tx.Rollback()
	activePlan, activePreparation, epoch, err := lockPreparationHead(ctx, tx, installation)
	if err != nil {
		return revisionRecord{}, err
	}
	r, err := readPreparation(ctx, tx, installation, key)
	if err != nil {
		return revisionRecord{}, err
	}
	if actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest {
		return revisionRecord{}, errors.New("preparation promotion authority or workflow changed")
	}
	bound, err := promotionPlan(r, plan, configuration, evidence)
	if err != nil {
		return revisionRecord{}, err
	}
	body, planID, err := bound.canonical()
	if err != nil {
		return revisionRecord{}, err
	}
	if r.State == "promoted" {
		if r.PromotedPlanID != planID {
			return revisionRecord{}, errChanged
		}
		revision, err := promotedRevision(ctx, tx, r)
		if err != nil {
			return revisionRecord{}, err
		}
		if err = tx.Commit(); err != nil {
			return revisionRecord{}, err
		}
		return revision, nil
	}
	if r.State != "preparing" || activePlan != "" || activePreparation != key || epoch != r.SlotEpoch {
		return revisionRecord{}, errChanged
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO installation_revision_attempts(plan_id,installation_id,requested_by,slot_epoch,plan,state) VALUES($1,$2,$3,$4,$5::jsonb,'prepared')`, planID, installation, actor, epoch+1, string(body))
	if err != nil {
		return revisionRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE installation_preparations SET state='promoted',promoted_plan_id=$2,updated_at=clock_timestamp() WHERE preparation_id=$1`, key, planID)
	if err != nil {
		return revisionRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_preparation_id=NULL,active_plan_id=$2,slot_epoch=$3 WHERE installation_id=$1`, installation, planID, epoch+1)
	if err != nil {
		return revisionRecord{}, err
	}
	r, err = readPreparation(ctx, tx, installation, key)
	if err != nil {
		return revisionRecord{}, err
	}
	revision, err := promotedRevision(ctx, tx, r)
	if err != nil {
		return revisionRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return revisionRecord{}, err
	}
	return revision, nil
}

func preparationSourcesDigest(r preparationRecord, role string, receipt bool) string {
	if r.Scope.FormatVersion != 2 {
		c := r.Captures[role]
		if receipt {
			return c.ReceiptDigest
		}
		return c.SourceDigest
	}
	var bindings []any
	for _, app := range r.Scope.Applications {
		key := applicationCaptureKey(app.Key, role)
		c := r.Captures[key]
		digest := c.SourceDigest
		if receipt {
			digest = c.ReceiptDigest
		}
		bindings = append(bindings, []string{key, digest})
	}
	return artifactHash("installation-group-source-v1", bindings)
}
