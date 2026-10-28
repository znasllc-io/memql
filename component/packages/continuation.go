package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

var continuationSourceFields = []string{"id", "ownerUserId", "accountId", "sourceKind", "repoUrl", "repoRef", "artifactId", "credentialId", "sourceConnectionId", "sourceScope"}

func continuationSource(row map[string]any) map[string]string {
	out := map[string]string{}
	for _, key := range continuationSourceFields {
		raw, _ := json.Marshal(row[key])
		out[key] = string(raw)
	}
	return out
}

// checkpointSelfRoll is entered only after builds finish and before any active
// pointer or restart. A no-change source continues on the existing fast path.
func (d *Deps) checkpointSelfRoll(ctx context.Context, req DeployRequest, pkg map[string]any, snapshot *SourceSnapshot, rep *Report, out *DeployOutcome, bundles map[string]edge.Bundle) (bool, error) {
	if d.Stager == nil {
		return false, fmt.Errorf("DSL package has no staging support")
	}
	current, err := d.Stager.ReadActiveSet(ctx)
	if err != nil {
		return false, err
	}
	prefixes, selectedDomains := []string{}, map[string]string{}
	changed := false
	for _, domain := range rep.DslDomains {
		tree, err := fs.Sub(snapshot.Tree, path.Join(DslRoot, domain.Domain))
		if err != nil {
			return false, err
		}
		prefix, err := d.Stager.PrefixFor(domain.Domain, tree)
		if err != nil {
			return false, err
		}
		prefixes = append(prefixes, prefix)
		selectedDomains[domain.Domain] = prefix
		if current[domain.Domain] != prefix {
			changed = true
		}
	}
	sort.Strings(prefixes)
	out.DslVersion = strings.Join(prefixes, " ")
	if !changed {
		return false, nil
	}
	stager, ok := d.Stager.(conditionalStager)
	if !ok {
		return false, fmt.Errorf("DSL package continuation requires conditional active-set support")
	}
	before, err := stager.ReadActiveSetVersion(ctx)
	if err != nil {
		return false, err
	}
	next := maps.Clone(before.Domains)
	changed = false
	for domain, prefix := range selectedDomains {
		if next[domain] != prefix {
			changed = true
		}
		next[domain] = prefix
	}
	if !changed {
		return false, nil
	}
	if auth.IsRoleTestContext(ctx) {
		return false, fmt.Errorf("a test persona cannot authorize a durable package continuation")
	}
	if _, delegated := auth.DelegationFromContext(ctx); delegated {
		return false, fmt.Errorf("a delegated session cannot authorize a durable package continuation")
	}
	roller, ok := d.Roller.(stablePackageRoller)
	if !ok {
		return false, fmt.Errorf("DSL package continuation requires stable rollout support")
	}
	if _, err := d.Store.continuationDB(); err != nil {
		return false, err
	}
	plans, err := roller.PrepareRoll(ctx, memql.BareShortId(out.DeploymentId))
	if err != nil {
		return false, err
	}
	if err := d.Store.advance(ctx, out.DeploymentId, StatusStagingDsl); err != nil {
		return false, err
	}
	// Content-addressed domain creation and site placement have no serving-pointer
	// effect. If either fails before the checkpoint, the original attempt closes.
	for _, domain := range rep.DslDomains {
		tree, err := fs.Sub(snapshot.Tree, path.Join(DslRoot, domain.Domain))
		if err != nil {
			return false, err
		}
		prefix, err := d.Stager.StageDomain(ctx, domain.Domain, tree)
		if err != nil {
			return false, err
		}
		if prefix != next[domain.Domain] {
			return false, fmt.Errorf("staged domain differs from selected source")
		}
	}
	outcomes, prepared, intents, domains, err := d.preparePublications(ctx, req, pkg, rep, bundles)
	if err != nil {
		return false, err
	}
	deadline := d.now().Add(30 * time.Minute)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	selected := continuationSelection{DeploymentID: memql.BareShortId(out.DeploymentId), PackageID: memql.BareShortId(req.PackageId), OwnerUserID: memql.BareShortId(rowString(pkg, "ownerUserId")), RequestedBy: memql.BareShortId(req.Actor.UserId), Source: continuationSource(pkg), SourceVersion: snapshot.Version, Report: rep, Request: req, Outcome: *out, Publications: intents, Bundles: prepared, Domains: domains, ActiveBefore: before, ActiveAfter: next, Rollouts: plans, Deadline: deadline}
	selected.Outcome.Deployables = outcomes
	var record *continuationRecord
	err = d.Store.withDeploymentOpeningGate(ctx, req.PackageId, func(ctx context.Context) error {
		ctx = memql.ContextWithFreshRead(ctx)
		row, err := d.Store.deploymentById(ctx, out.DeploymentId)
		if err != nil {
			return err
		}
		if row == nil || IsTerminal(rowString(row, "status")) || rowBool(row, "cancelRequested") {
			return refuse(CodeDeploymentCancelled, "This deployment stopped before its rollout checkpoint.")
		}
		if !sameShortId(rowString(row, "packageId"), req.PackageId) || rowString(row, "sourceVersion") != snapshot.Version {
			return fmt.Errorf("deployment source differs before checkpoint")
		}
		selected.SnapshotID = rowString(row, "snapshotArtifactId")
		if selected.SnapshotID == "" {
			return fmt.Errorf("durable continuation requires the saved source snapshot")
		}
		record, err = d.Store.createContinuation(ctx, selected)
		if err != nil {
			return err
		}
		return d.Store.advance(ctx, out.DeploymentId, StatusRolling)
	})
	if record != nil {
		defer d.Store.releaseContinuation(ctx, record)
		if err != nil {
			return true, errContinuationPending
		}
		err = d.driveContinuation(ctx, record)
		if err == nil {
			*out = record.Selection.Outcome
			return true, nil
		}
		out.Status = StatusRolling
		if record.Phase == "publishing" {
			out.Status = StatusPublishing
		}
		return true, errContinuationPending
	}
	return false, err
}

// Recreate only the exact selected requester's identity, resolve its CURRENT
// role, and read through ordinary owner/account gates. An owner stamp in an
// editable package row never substitutes for the selected requester.
func (d *Deps) continuationAuthority(ctx context.Context, r *continuationRecord) (context.Context, error) {
	v := r.Selection
	if d.Roles == nil {
		return nil, fmt.Errorf("continuation cannot resolve current requester authority")
	}
	actorCtx := auth.ContextWithUserActor(memql.ContextWithFreshAuthorization(ctx), v.RequestedBy)
	role, err := d.Roles(actorCtx, v.RequestedBy)
	if err != nil {
		return nil, err
	}
	if !auth.CanAuthor(auth.UserContext{Role: role}) {
		return nil, refuse(CodeDslRequiresAuthoring, "The original requester can no longer deploy this DSL.")
	}
	actorCtx = auth.ContextWithAccess(auth.ContextWithClientOrigin(actorCtx), &auth.AccessContext{UserId: v.RequestedBy, Role: role})
	pkg, err := d.Store.packageById(actorCtx, v.PackageID)
	if err != nil {
		return nil, err
	}
	if pkg == nil || rowString(pkg, "status") != "active" || rowBool(pkg, "sourceRemoved") || !reflect.DeepEqual(continuationSource(pkg), v.Source) {
		return nil, fmt.Errorf("package source or owner changed during continuation")
	}
	row, err := d.Store.deploymentById(actorCtx, v.DeploymentID)
	if err != nil {
		return nil, err
	}
	if row == nil || IsTerminal(rowString(row, "status")) || rowBool(row, "cancelRequested") || !sameShortId(rowString(row, "packageId"), v.PackageID) || rowString(row, "snapshotArtifactId") != v.SnapshotID || rowString(row, "sourceVersion") != v.SourceVersion {
		return nil, fmt.Errorf("saved deployment identity or cancellation changed")
	}
	if credential := rowString(pkg, "credentialId"); credential != "" {
		readCtx := actorCtx
		if v.OwnerUserID != "" {
			readCtx = auth.ContextWithUserActor(actorCtx, v.OwnerUserID)
		}
		grant, err := d.Store.sourceCredentialSealedById(readCtx, credential)
		if err != nil {
			return nil, err
		}
		if grant == nil || rowString(grant, "status") != credentialStatusActive {
			return nil, fmt.Errorf("source credential is no longer active")
		}
	}
	return actorCtx, nil
}

func (d *Deps) driveContinuation(ctx context.Context, r *continuationRecord) error {
	if !d.now().Before(r.Selection.Deadline) {
		return fmt.Errorf("package continuation deadline elapsed; effects require reconciliation")
	}
	ctx, cancel := context.WithDeadline(ctx, r.Selection.Deadline)
	defer cancel()
	stopped, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(continuationLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if d.Store.renewContinuation(ctx, r) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(stopped); <-joined }()
	stager, ok := d.Stager.(conditionalStager)
	if !ok {
		return fmt.Errorf("conditional active set unavailable")
	}
	roller, ok := d.Roller.(stablePackageRoller)
	if !ok {
		return fmt.Errorf("stable package rollout unavailable")
	}
	publisher, ok := d.Publisher.(durableSitePublisher)
	if !ok {
		return fmt.Errorf("durable package publisher unavailable")
	}
	fresh := func() (context.Context, error) {
		if err := d.Store.saveContinuation(ctx, r, r.Phase); err != nil {
			return nil, err
		}
		return d.continuationAuthority(ctx, r)
	}
	actorCtx, err := fresh()
	if err != nil {
		return err
	}
	active, err := stager.ReadActiveSetVersion(actorCtx)
	if err != nil {
		return err
	}
	if r.State.PointerConfirmed == nil {
		if r.State.PointerAttempted {
			if !reflect.DeepEqual(active.Domains, r.Selection.ActiveAfter) || !active.Exists {
				return errContinuationPending
			}
			r.State.PointerConfirmed = &active
			if err := d.Store.saveContinuation(ctx, r, continuationRollingPhase(r)); err != nil {
				return err
			}
		} else {
			if active.Exists != r.Selection.ActiveBefore.Exists || active.ETag != r.Selection.ActiveBefore.ETag || !reflect.DeepEqual(active.Domains, r.Selection.ActiveBefore.Domains) {
				return fmt.Errorf("active DSL changed before the selected write")
			}
			r.State.PointerAttempted = true
			if err := d.Store.saveContinuation(ctx, r, continuationRollingPhase(r)); err != nil {
				return err
			}
			actorCtx, err = fresh()
			if err != nil {
				return err
			}
			selected, err := stager.CompareAndSwapActiveSet(actorCtx, r.Selection.ActiveBefore, r.Selection.ActiveAfter)
			if err != nil {
				return errContinuationPending
			}
			r.State.PointerConfirmed = &selected
			if err := d.Store.saveContinuation(ctx, r, continuationRollingPhase(r)); err != nil {
				return err
			}
		}
	}
	// Exact provider version stays stable across every rollout and publication.
	active, err = stager.ReadActiveSetVersion(actorCtx)
	if err != nil {
		return err
	}
	if !sameActiveVersion(active, *r.State.PointerConfirmed) {
		return fmt.Errorf("active DSL pointer changed after selected staging")
	}
	allReady := true
	for index, plan := range r.Selection.Rollouts {
		actorCtx, err = fresh()
		if err != nil {
			return err
		}
		active, err = stager.ReadActiveSetVersion(actorCtx)
		if err != nil {
			return err
		}
		if !sameActiveVersion(active, *r.State.PointerConfirmed) {
			return fmt.Errorf("active DSL pointer changed before rollout")
		}
		observation, err := roller.ObserveRoll(actorCtx, plan)
		if err != nil {
			return err
		}
		if observation.Applied {
			if !r.State.RollAttempted[index] {
				return fmt.Errorf("unissued package rollout marker appeared")
			}
			r.State.RollConfirmed[index] = observation.Ready
		} else if !r.State.RollAttempted[index] {
			r.State.RollAttempted[index] = true
			if err := d.Store.saveContinuation(ctx, r, continuationRollingPhase(r)); err != nil {
				return err
			}
			actorCtx, err = fresh()
			if err != nil {
				return err
			}
			// One submission, including ambiguous failure. Recovery only observes.
			if err := roller.ApplyRoll(actorCtx, plan); err != nil {
				return errContinuationPending
			}
		}
		if !r.State.RollConfirmed[index] {
			allReady = false
		}
	}
	if err := d.Store.saveContinuation(ctx, r, continuationRollingPhase(r)); err != nil {
		return err
	}
	if !allReady {
		return errContinuationPending
	}
	if err := d.Store.saveContinuation(ctx, r, "publishing"); err != nil {
		return err
	}
	if err := d.Store.advance(actorCtx, r.Selection.DeploymentID, StatusPublishing); err != nil {
		return err
	}
	for index, p := range r.Selection.Publications {
		current, err := d.Store.readContinuation(ctx, r.Selection.DeploymentID)
		if err != nil {
			return err
		}
		if current == nil || current.Lease != r.Lease {
			return errContinuationClaim
		}
		if receipt, ok := current.Receipts[p.Key]; ok {
			if receipt.SiteID != p.SiteID || receipt.Target != p.Target || receipt.BundleRef != p.BundleRef || receipt.SiteVersion.IsZero() || !reflect.DeepEqual(receipt.Binding, p.Binding) || !reflect.DeepEqual(receipt.PreviewBinding, p.PreviewBinding) {
				return fmt.Errorf("publication receipt identity differs")
			}
			continue
		}
		actorCtx, err = fresh()
		if err != nil {
			return err
		}
		active, err = stager.ReadActiveSetVersion(actorCtx)
		if err != nil {
			return err
		}
		if !sameActiveVersion(active, *r.State.PointerConfirmed) {
			return fmt.Errorf("active DSL pointer changed before publication")
		}
		fenced := memql.ContextWithRowVersionFence(actorCtx, "v1:platform:site", p.SiteID, p.SiteVersion)
		fenced = memql.ContextWithPackagePublicationFence(fenced, r.Selection.DeploymentID, r.SHA, r.Lease, p.Key)
		if _, err := publisher.PublishPrepared(fenced, r.Selection.Bundles[index], *r.State.PointerConfirmed); err != nil {
			return errContinuationPending
		}
	}
	// Domain creation retains its own stable identity and native receipt. A lost
	// reply only advances after observing that exact receipt; it never mints a
	// second challenge or guesses that an unrelated existing hostname is ours.
	for _, domain := range r.Selection.Domains {
		current, err := d.Store.readContinuation(ctx, r.Selection.DeploymentID)
		if err != nil {
			return err
		}
		if current == nil || current.Lease != r.Lease {
			return errContinuationClaim
		}
		if receipt, ok := current.DomainReceipts[domain.Key]; ok {
			if !memql.PackageDomainReceiptMatches(domain, receipt) {
				return fmt.Errorf("domain receipt identity differs")
			}
			continue
		}
		publication, ok := current.Receipts[domain.PublicationKey]
		if !ok || publication.SiteID != domain.SiteID || publication.SiteVersion.IsZero() {
			return fmt.Errorf("domain has no exact site publication receipt")
		}
		actorCtx, err = fresh()
		if err != nil {
			return err
		}
		active, err = stager.ReadActiveSetVersion(actorCtx)
		if err != nil {
			return err
		}
		if !sameActiveVersion(active, *r.State.PointerConfirmed) {
			return fmt.Errorf("active DSL pointer changed before domain binding")
		}
		fenced := memql.ContextWithRowVersionFence(actorCtx, "v1:platform:site", domain.SiteID, publication.SiteVersion)
		fenced = memql.ContextWithPackageDomainFence(fenced, r.Selection.DeploymentID, r.SHA, r.Lease, domain)
		// The builtin still resolves the target and checks the caller. Its native
		// fence spans the internal system stamp used to mint the verification token.
		_, callErr := d.Store.engine.Execute(fenced, fmt.Sprintf("builtin customDomainAdd(siteId: %s, hostname: %s)", langparser.QuoteString(domain.SiteID), langparser.QuoteString(domain.Hostname)))
		observed, readErr := d.Store.readContinuation(ctx, r.Selection.DeploymentID)
		if readErr != nil {
			return readErr
		}
		if observed == nil || observed.Lease != r.Lease {
			return errContinuationClaim
		}
		receipt, ok := observed.DomainReceipts[domain.Key]
		if !ok {
			_ = callErr
			return errContinuationPending
		}
		if !memql.PackageDomainReceiptMatches(domain, receipt) {
			return fmt.Errorf("domain receipt identity differs after submission")
		}
	}

	current, err := d.Store.readContinuation(ctx, r.Selection.DeploymentID)
	if err != nil {
		return err
	}
	if current == nil || !continuationReceiptsComplete(current) {
		return errContinuationPending
	}
	if err := d.Store.saveContinuation(ctx, r, "completed"); err != nil {
		return err
	}
	r.Selection.Outcome = continuationOutcome(current)
	r.Selection.Outcome.Status = StatusSucceeded
	d.recordContinuationSource(ctx, r)
	return nil
}
func continuationReceiptsComplete(r *continuationRecord) bool {
	if len(r.Receipts) != len(r.Selection.Publications) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range r.Selection.Publications {
		got, ok := r.Receipts[p.Key]
		if !ok || seen[p.Key] || got.SiteID != p.SiteID || got.Target != p.Target || got.BundleRef != p.BundleRef || got.SiteVersion.IsZero() || !reflect.DeepEqual(got.Binding, p.Binding) || !reflect.DeepEqual(got.PreviewBinding, p.PreviewBinding) {
			return false
		}
		seen[p.Key] = true
	}
	if len(r.DomainReceipts) != len(r.Selection.Domains) {
		return false
	}
	for _, domain := range r.Selection.Domains {
		receipt, ok := r.DomainReceipts[domain.Key]
		if !ok || seen["domain/"+domain.Key] || !memql.PackageDomainReceiptMatches(domain, receipt) {
			return false
		}
		seen["domain/"+domain.Key] = true
	}

	return true
}
func sameActiveVersion(a, b ActiveSetVersion) bool {
	return a.Exists && b.Exists && a.ETag == b.ETag && reflect.DeepEqual(a.Domains, b.Domains)
}

// Used only by the existing maintenance sweep. It cannot start a new selection,
// rebuild, reopen a terminal graph row, or replay an attempted external effect.
func (d *Deps) continueDeployment(ctx context.Context, id, packageID string) (bool, error) {
	if d.Store.directDB == nil {
		return false, nil
	}
	var r *continuationRecord
	owned := false
	err := d.Store.withDeploymentOpeningGate(ctx, packageID, func(ctx context.Context) error {
		latest, err := d.Store.deploymentById(memql.ContextWithFreshRead(ctx), id)
		if err != nil {
			return err
		}
		if latest == nil || IsTerminal(rowString(latest, "status")) {
			return nil
		}
		r, err = d.Store.readContinuation(ctx, id)
		if err != nil || r == nil {
			return err
		}
		if !sameShortId(r.Selection.PackageID, packageID) || !sameShortId(rowString(latest, "packageId"), r.Selection.PackageID) {
			return fmt.Errorf("continuation package identity differs")
		}
		owned = true
		if r.Phase == "completed" {
			return nil
		}
		if r.Phase == "refused" {
			owned = false
			return nil
		}
		r, err = d.Store.claimContinuation(ctx, id)
		return err
	})
	if err != nil || !owned || r == nil {
		return owned, err
	}
	defer d.Store.releaseContinuation(ctx, r)
	if r.Phase != "completed" {
		err = d.driveContinuation(ctx, r)
		if err != nil {
			if errors.Is(err, errContinuationPending) || d.now().Before(r.Selection.Deadline) {
				return true, nil
			}
			// Keep the original attempt and uncertain effects; no success is inferred.
			if err := d.Store.saveContinuation(ctx, r, "refused"); err != nil {
				return true, err
			}
			return false, nil
		}
	}
	return true, d.finishContinuation(ctx, r)
}
func (d *Deps) finishContinuation(ctx context.Context, r *continuationRecord) error {
	// This is bookkeeping of already coupled publication receipts, under the
	// existing maintenance writer. It does not touch site pointers or credentials.
	return d.Store.withDeploymentOpeningGate(ctx, r.Selection.PackageID, func(ctx context.Context) error {
		ctx = memql.ContextWithFreshRead(ctx)
		row, err := d.Store.deploymentById(ctx, r.Selection.DeploymentID)
		if err != nil {
			return err
		}
		if row == nil {
			return fmt.Errorf("continuation deployment disappeared")
		}
		if IsTerminal(rowString(row, "status")) {
			return nil
		}
		current, err := d.Store.readContinuation(ctx, r.Selection.DeploymentID)
		if err != nil {
			return err
		}
		if current == nil || current.SHA != r.SHA || current.Phase != "completed" || !continuationReceiptsComplete(current) || !sameShortId(rowString(row, "packageId"), current.Selection.PackageID) || rowString(row, "snapshotArtifactId") != current.Selection.SnapshotID || rowString(row, "sourceVersion") != current.Selection.SourceVersion {
			return fmt.Errorf("completed continuation has no exact publication evidence")
		}
		d.recordContinuationSource(ctx, current)
		out := continuationOutcome(current)
		out.Status = StatusSucceeded
		return d.Store.closeDeployment(ctx, deploymentClose{DeploymentId: r.Selection.DeploymentID, Status: StatusSucceeded, Deployables: out.Deployables, DslVersion: out.DslVersion, BuiltOn: out.BuiltOn, FinishedAt: d.now()})
	})
}

func continuationRollingPhase(r *continuationRecord) string {
	if r.Phase == "publishing" {
		return r.Phase
	}
	return "rolling"
}

func (d *Deps) recordContinuationSource(ctx context.Context, r *continuationRecord) {
	if candidatesOnly(r.Selection.Outcome.Deployables) {
		return
	}
	actorCtx, err := d.continuationAuthority(ctx, r)
	if err != nil {
		return
	}
	latest, err := d.Store.packageById(actorCtx, r.Selection.PackageID)
	if err != nil {
		return
	}
	known := rowString(latest, "latestKnownVersion")
	baseline := r.Selection.Report.UpstreamBaseline
	pending := known != "" && known != r.Selection.SourceVersion && (baseline == nil || known != *baseline)
	if at, err := continuationRowVersion(latest); err == nil {
		actorCtx = memql.ContextWithRowVersionFence(actorCtx, "v1:platform:package", r.Selection.PackageID, at)
	} else {
		return
	}
	if err := d.Store.recordDeployedVersion(actorCtx, r.Selection.PackageID, r.Selection.SourceVersion, pending); err != nil {
		d.log().Warn("packages: continuation source bookkeeping was not confirmed", "deployment", r.Selection.DeploymentID, "err", err)
	}
}

// Outcome notes derive from committed receipts, so replacing the controller or
// losing a reply cannot erase a nonfatal domain refusal or claim an unbound name.
func continuationOutcome(r *continuationRecord) DeployOutcome {
	out := r.Selection.Outcome
	out.Deployables = append([]DeployableOutcome(nil), out.Deployables...)
	for _, domain := range r.Selection.Domains {
		receipt, ok := r.DomainReceipts[domain.Key]
		if !ok || !memql.PackageDomainReceiptMatches(domain, receipt) {
			continue
		}
		for index := range out.Deployables {
			item := &out.Deployables[index]
			if !sameShortId(item.SiteId, domain.SiteID) {
				continue
			}
			if receipt.Refusal != "" {
				if item.DomainRefusal == nil {
					item.DomainRefusal = &Problem{Code: CodeDeployableDomainRefused, Scope: item.Name, Message: receipt.Refusal}
				} else {
					note := *item.DomainRefusal
					note.Message += "; " + receipt.Refusal
					item.DomainRefusal = &note
				}
			} else {
				item.OwnDomain = strings.TrimPrefix(item.OwnDomain+", "+domain.Hostname, ", ")
			}
		}
	}
	return out
}
