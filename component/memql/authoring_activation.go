package memql

// authoring_activation.go -- Gate 3 (user approval) + the activation
// transition of the planner-authored-automations pipeline (epic memql#954,
// issue #961).
//
// Gate 1 (authoring_sandbox*.go) proves a bundle's closure compiles + binds.
// Gate 2 (authoring_dryrun.go) runs its automation behaviorally and produces
// the dry-run report (trace + side-effect manifest + cost estimate). Gate 3 is
// the USER APPROVAL gate: the report is surfaced as an approval artifact, the
// user approves (optionally after a full-sandbox-live staging pass), and the
// bundle ACTIVATES:
//
//   1. status dryRunPassed -> active (mutationActivateAuthoringBundle), member
//      constructs draft -> active (mutationSetConstructStatus).
//   2. its constructs register into the owner-scoped authored runtime
//      (AuthoredRuntimeRegistry, #959) so they resolve + fire live; the
//      automation construct is handed to the authored scheduler via an
//      injected register hook (wired to AuthoredScheduler.Activate in app/).
//   3. its NET-NEW dependency constructs promote into the owner's reusable
//      catalog (PromoteConstructToCatalog, #957) so later bundles compose them.
//   4. an activation audit event is emitted (#identity audit path).
//
// The orchestration is split so the policy/shape decisions are pure + unit
// testable (BuildApprovalArtifact, PlanBundleActivation) and the engine-backed
// side (ActivateBundle) drives them through a narrow activationStore the tests
// fake -- mirroring how the worker dispatcher (integrations/agent/worker) keeps
// its gate logic testable behind a store seam.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BundleStatus mirrors the v1:authoring:bundle.status enum so the activation
// path can reason about lifecycle transitions in typed Go.
type BundleStatus string

const (
	BundleDraft        BundleStatus = "draft"
	BundleValidated    BundleStatus = "validated"
	BundleDryRunPassed BundleStatus = "dryRunPassed"
	BundleActive       BundleStatus = "active"
	BundlePaused       BundleStatus = "paused"
	BundleRetired      BundleStatus = "retired"
	BundleFailed       BundleStatus = "failed"
)

// AuthoringBundleRow is the parsed v1:authoring:bundle projection the activation
// path needs: identity, lifecycle, supersession, the reuse edges, and the
// dry-run report (the Gate-3 approval artifact source). It is the Go view of a
// bundleFull query row.
type AuthoringBundleRow struct {
	Id                 string             `json:"id"`
	OwnerUserId        string             `json:"ownerUserId"`
	ResponsibilityId   string             `json:"responsibilityId"`
	Title              string             `json:"title"`
	Summary            string             `json:"summary"`
	Status             BundleStatus       `json:"status"`
	Version            int                `json:"version"`
	SupersedesBundleId string             `json:"supersedesBundleId"`
	DryRunReport       BundleDryRunReport `json:"dryRunReport"`
}

// AuthoringConstructRow is the parsed v1:authoring:construct projection: the
// member construct's identity + kind + source + catalog state. The Go view of a
// constructFull query row.
type AuthoringConstructRow struct {
	Id              string `json:"id"`
	OwnerUserId     string `json:"ownerUserId"`
	BundleId        string `json:"bundleId"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	TargetNamespace string `json:"targetNamespace"`
	Source          string `json:"source"`
	Catalogued      bool   `json:"catalogued"`
}

// BundleApprovalArtifact is the Gate-3 review surface a user approves against:
// what the capability does, plus the dry-run evidence (the behavioral trace,
// the tiered side-effect manifest, and the metered cost estimate). It is pure
// data derived from a dryRunPassed bundle row -- the same {trace,
// sideEffectManifest, costEstimate} triple the dry-run produced, framed for the
// approver. Reuses the planner approval-gate shape (estimate + side effects +
// trace) rather than inventing a new contract.
type BundleApprovalArtifact struct {
	BundleId         string `json:"bundleId"`
	Title            string `json:"title"`
	Summary          string `json:"summary"`
	Version          int    `json:"version"`
	ResponsibilityId string `json:"responsibilityId,omitempty"`
	// Trace is the behavioral trace of the dry-run -- every step the automation
	// ran, in order, so the approver sees what it actually does.
	Trace []DryRunStep `json:"trace"`
	// SideEffectManifest is the tiered catalog of mutations / reads / blocked
	// webhooks the automation produced under the isolated tier.
	SideEffectManifest SideEffectManifest `json:"sideEffectManifest"`
	// CostEstimate is the metered cost of the run's real reads -- what the
	// automation will spend in production each time it fires.
	CostEstimate CostEstimate `json:"costEstimate"`
	// FullLiveStaged is true when the dry-run that produced this artifact ran
	// under the optional full-sandbox-live staging tier, so the approver knows
	// the evidence reflects a live-webhook (capture-sink) staging pass.
	FullLiveStaged bool `json:"fullLiveStaged"`
}

// BuildApprovalArtifact frames a dryRunPassed bundle row as the Gate-3 approval
// surface. It does NOT mutate anything -- the management UI renders this, the
// user approves, and ActivateBundle then runs the transition. Returns an error
// if the bundle is not in dryRunPassed (you can only approve what passed Gate
// 2).
func BuildApprovalArtifact(bundle AuthoringBundleRow) (BundleApprovalArtifact, error) {
	if bundle.Status != BundleDryRunPassed {
		return BundleApprovalArtifact{}, fmt.Errorf("authoring: bundle %q is %q, only a dryRunPassed bundle can be presented for approval", bundle.Id, bundle.Status)
	}
	return BundleApprovalArtifact{
		BundleId:           bundle.Id,
		Title:              bundle.Title,
		Summary:            bundle.Summary,
		Version:            bundle.Version,
		ResponsibilityId:   bundle.ResponsibilityId,
		Trace:              bundle.DryRunReport.Trace,
		SideEffectManifest: bundle.DryRunReport.SideEffectManifest,
		CostEstimate:       bundle.DryRunReport.CostEstimate,
		FullLiveStaged:     bundle.DryRunReport.Mode == DryRunModeFullSandboxLive,
	}, nil
}

// CatalogPromotionTarget is one net-new construct an activation promotes into
// the owner's reusable catalog: the (id, kind, name, source) the catalog-write
// path (PromoteConstructToCatalog) needs.
type CatalogPromotionTarget struct {
	ConstructId string `json:"constructId"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Source      string `json:"source"`
}

// BundleActivationPlan is the computed-but-not-yet-applied result of approving
// a bundle: the authored-runtime constructs to register, the net-new deps to
// promote into the catalog, and the audit detail. Pure data -- computing it
// touches no engine -- so the register/promote/audit decisions are unit
// testable without a DB, exactly like PlanCatalogPromotion (#957).
type BundleActivationPlan struct {
	BundleId    string `json:"bundleId"`
	OwnerUserId string `json:"ownerUserId"`
	Version     int    `json:"version"`
	// SupersedesBundleId is the prior-version bundle this activation retires (the
	// edit/version path); empty for a first-version bundle.
	SupersedesBundleId string `json:"supersedesBundleId"`
	// Register is every construct that becomes live in the authored runtime,
	// already stamped with the owner + version. The automation entries also feed
	// the authored scheduler.
	Register []*AuthoredConstruct `json:"register"`
	// PromoteToCatalog is the net-new (not-yet-catalogued) dependency constructs
	// to promote so later bundles can compose them. The bundle's headline
	// automation is NOT a reusable dependency, so it is excluded.
	PromoteToCatalog []CatalogPromotionTarget `json:"promoteToCatalog"`
	// ConstructIds are every member construct id (for flipping status active).
	ConstructIds []string `json:"constructIds"`
}

// PlanBundleActivation computes the activation plan from a dryRunPassed bundle +
// its member constructs. It is deterministic and side-effect-free:
//
//   - every member construct becomes an AuthoredConstruct (owner-scoped,
//     stamped with the bundle's version) for the runtime registry;
//   - every NET-NEW (not-yet-catalogued) dependency construct -- i.e. every
//     member that is not the bundle's headline automation -- is a catalog
//     promotion target so later bundles compose it;
//   - the audit/version bookkeeping (version, supersedes) carries through.
//
// It validates the bundle is dryRunPassed and owns its constructs; a construct
// belonging to another bundle or owner is rejected (no cross-bundle activation).
func PlanBundleActivation(bundle AuthoringBundleRow, constructs []AuthoringConstructRow) (BundleActivationPlan, error) {
	if bundle.Status != BundleDryRunPassed {
		return BundleActivationPlan{}, fmt.Errorf("authoring: bundle %q is %q, only a dryRunPassed bundle can activate", bundle.Id, bundle.Status)
	}
	if strings.TrimSpace(bundle.OwnerUserId) == "" {
		return BundleActivationPlan{}, fmt.Errorf("authoring: bundle %q has no ownerUserId", bundle.Id)
	}

	plan := BundleActivationPlan{
		BundleId:           bundle.Id,
		OwnerUserId:        bundle.OwnerUserId,
		Version:            bundle.Version,
		SupersedesBundleId: bundle.SupersedesBundleId,
	}

	// The headline automation is the live capability, not a reusable dependency.
	// Identify it so it is registered but NOT catalog-promoted. A bundle carries
	// exactly one automation (the planner emit pass contract); if a malformed
	// bundle carries more, every automation is treated as headline (registered,
	// not promoted) -- a conservative default that never over-promotes.
	for _, c := range constructs {
		if c.BundleId != bundle.Id {
			return BundleActivationPlan{}, fmt.Errorf("authoring: construct %q belongs to bundle %q, not %q", c.Id, c.BundleId, bundle.Id)
		}
		if c.OwnerUserId != bundle.OwnerUserId {
			return BundleActivationPlan{}, fmt.Errorf("authoring: construct %q owner %q does not match bundle owner %q", c.Id, c.OwnerUserId, bundle.OwnerUserId)
		}

		plan.ConstructIds = append(plan.ConstructIds, c.Id)
		plan.Register = append(plan.Register, &AuthoredConstruct{
			OwnerUserId: c.OwnerUserId,
			Kind:        c.Kind,
			Name:        c.Name,
			BundleId:    bundle.Id,
			Version:     bundle.Version,
			Source:      c.Source,
		})

		// Net-new deps (everything that is not the headline automation and is not
		// already catalogued) are promoted so the compose-first matcher can find
		// them next time.
		if c.Kind == "automation" {
			continue
		}
		if c.Catalogued {
			continue
		}
		plan.PromoteToCatalog = append(plan.PromoteToCatalog, CatalogPromotionTarget{
			ConstructId: c.Id,
			Kind:        c.Kind,
			Name:        c.Name,
			Source:      c.Source,
		})
	}

	// Stable ordering so the activation is deterministic (test-friendly + a
	// stable audit detail).
	sort.Slice(plan.Register, func(i, j int) bool {
		if plan.Register[i].Kind != plan.Register[j].Kind {
			return plan.Register[i].Kind < plan.Register[j].Kind
		}
		return plan.Register[i].Name < plan.Register[j].Name
	})
	sort.Slice(plan.PromoteToCatalog, func(i, j int) bool {
		if plan.PromoteToCatalog[i].Kind != plan.PromoteToCatalog[j].Kind {
			return plan.PromoteToCatalog[i].Kind < plan.PromoteToCatalog[j].Kind
		}
		return plan.PromoteToCatalog[i].Name < plan.PromoteToCatalog[j].Name
	})
	return plan, nil
}

// AuthoredAuditEvent is the activation/retirement audit signal the authoring
// runtime emits. It is the memql-side mirror of identity.AuditEvent (kept local
// so component/memql does not import component/identity -- the app wires the
// sink). Emitted on every activation + retirement so the governance trail is
// complete.
type AuthoredAuditEvent struct {
	Action      string         `json:"action"`
	OwnerUserId string         `json:"ownerUserId"`
	BundleId    string         `json:"bundleId"`
	Detail      map[string]any `json:"detail"`
	OccurredAt  time.Time      `json:"occurredAt"`
}

// AuthoredAuditSink receives activation/retirement audit events. The app wires
// it to the identity audit logger; tests record. A nil sink means audit is
// not wired on this binary (the slog line from the orchestrator still records
// the transition).
type AuthoredAuditSink interface {
	EmitAuthoredAudit(ctx context.Context, ev AuthoredAuditEvent)
}

// AuthoredRegisterHook hands one activated automation construct to the live
// authored scheduler so its trigger fires. The app wires it to
// AuthoredScheduler.Activate; tests record. A nil hook means the scheduler is
// not linked on this binary (the registry registration still happens, so the
// construct resolves -- it just won't be scheduled). Returns an error so a
// compile failure in the just-activated automation surfaces.
type AuthoredRegisterHook func(c *AuthoredConstruct) error

// AuthoredDeregisterHook tears down one retired automation's scheduler entry.
// Wired to AuthoredScheduler.Deactivate; tests record.
type AuthoredDeregisterHook func(owner, name string)

const (
	// AuditActionBundleActivated is the audit action stamped when a bundle
	// activates. Lower_snake_case to match the v1:identity:auditEvent.action
	// convention.
	AuditActionBundleActivated = "authored_bundle_activated"
	// AuditActionBundleRetired is the audit action stamped on retirement.
	AuditActionBundleRetired = "authored_bundle_retired"
)

// marshalDetail renders a plan's audit detail as a JSON-friendly map: the
// version, supersession, and the names of what was registered / promoted. Used
// in the activation audit event so the governance trail records exactly what
// went live.
func (p BundleActivationPlan) auditDetail() map[string]any {
	registered := make([]string, 0, len(p.Register))
	for _, c := range p.Register {
		registered = append(registered, c.Kind+":"+c.Name)
	}
	promoted := make([]string, 0, len(p.PromoteToCatalog))
	for _, c := range p.PromoteToCatalog {
		promoted = append(promoted, c.Kind+":"+c.Name)
	}
	detail := map[string]any{
		"version":           p.Version,
		"registeredCount":   len(p.Register),
		"registered":        registered,
		"promotedToCatalog": promoted,
		"promotedCount":     len(p.PromoteToCatalog),
	}
	if p.SupersedesBundleId != "" {
		detail["supersedesBundleId"] = p.SupersedesBundleId
	}
	return detail
}

// parseBundleRow decodes a single bundleFull query node payload into an
// AuthoringBundleRow. The row id intrinsic is taken from the node id.
func parseBundleRow(id string, payload []byte) (AuthoringBundleRow, error) {
	var row AuthoringBundleRow
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &row); err != nil {
			return AuthoringBundleRow{}, fmt.Errorf("authoring: decode bundle row: %w", err)
		}
	}
	if row.Id == "" {
		row.Id = id
	}
	if row.Version == 0 {
		row.Version = 1
	}
	return row, nil
}

// parseConstructRow decodes a single constructFull query node payload into an
// AuthoringConstructRow.
func parseConstructRow(id string, payload []byte) (AuthoringConstructRow, error) {
	var row AuthoringConstructRow
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &row); err != nil {
			return AuthoringConstructRow{}, fmt.Errorf("authoring: decode construct row: %w", err)
		}
	}
	if row.Id == "" {
		row.Id = id
	}
	return row, nil
}
