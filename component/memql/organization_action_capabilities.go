package memql

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func organizationDeployableCapability(resource string) bool {
	switch resource {
	case "app:deployables/deploy", "app:deployables/publish", "app:deployables/retire", "app:pipelines/repositories", "app:deployables/preview", "app:deployables/store", "app:deployables/access", "app:deployables/reviews":
		return true
	}
	return false
}

// Only explicit, authoritative targets participate. Personal credentials,
// preview credentials/probes and cluster domain operations retain their
// existing global guards even when they share a deployables capability.
func organizationCapabilityTarget(fn *Function) (concept, argument string) {
	if fn == nil || fn.RequiresCapability.Verb != auth.VerbExecute || !organizationDeployableCapability(fn.RequiresCapability.Resource) {
		return "", ""
	}
	if fn.FunctionKind == "mutation" {
		// Product-owned constructs have these argument contracts. A user's
		// bound mutation may choose other names and keeps the public DSL's
		// original capability semantics; its writes still meet row guards.
		switch fn.Name {
		case "updateSiteStatus", "updateSiteStoreBinding", "setSiteCandidate", "clearSiteCandidate", "promoteSiteCandidate", "updateSitePreviewBinding", "deleteSite":
			if fn.BoundConcept == "v1:platform:site" {
				return fn.BoundConcept, "siteId"
			}
		case "setPackageSourceRemoved", "createPackage", "updatePackageSource", "disablePackageDeployables", "enablePackageDeployables", "setPackageAutoDeploy":
			if fn.BoundConcept == "v1:platform:package" {
				return fn.BoundConcept, "packageId"
			}
		}
	}
	if fn.FunctionKind == "builtin" {
		if fn.Name == "packageSourceRegister" && fn.Executor == "integration.packages.sourceRegister" {
			return "v1:platform:package", "packageId"
		}
		switch fn.Name {
		case "sitePublishFromArtifact", "siteArchive", "siteRestore", "siteDelete", "sitePreviewOpen":
			return "v1:platform:site", "siteId"
		// Access codes (design record 2026-10-29-deployable-access-codes). Minting,
		// listing the gates and listing the codes are asked about the SITE: no code
		// exists yet when minting, and the other two ask about every code of one
		// site. Every act on an existing code is asked about the CODE, whose
		// accountId was copied off its site at mint time, so the organization
		// resolves without reading the site a second time.
		//
		// THE LIST IS HERE BECAUSE IT IS A BUILTIN, and it is a builtin because this
		// gate judges builtins and mutations and NEVER a query: a client-callable list
		// would let a person who held the part in one organization read the codes of a
		// site in another whose row tier admitted them. So every client-callable read
		// and write on access codes is a builtin in this table, and every query and
		// mutation beneath them is @serverOnly
		// (TestEveryClientCallableAccessCodeActIsAGatedBuiltin).
		case "siteAccessCodeCreate", "siteAccessGuestPassCreate", "siteAccessGates", "siteAccessCodesForSite":
			return "v1:platform:site", "siteId"
		case "siteAccessCodeRegenerate", "siteAccessCodeSetExpiry", "siteAccessCodeRemove":
			return "v1:platform:siteAccessCode", "codeId"
		case "packageAnalyze", "packageDeploy", "packageCancelDeployment", "packageRollback", "packageArchive", "packageDeactivateDeployable", "packageRestore", "packageSetAutoDeploy":
			return "v1:platform:package", "packageId"
		// Review moderation (the reviews pack). An act or read on one review is
		// asked about the REVIEW, whose accountId was copied off its site when
		// it was written; one on a store's reviews as a whole -- the moderation
		// list, the decision log, the settings, the display toggle, issuing a
		// link, the export, who can moderate it -- is asked about the SITE the
		// store is served through, which the pack checks is bound to that store.
		// Beneath the builtins the rows stay row-tier: a review row is admitted
		// only to its merchant, its organization's people and the cluster's
		// owners.
		case "reviewsDecide", "reviewsEraseReviewer", "reviewsGet":
			return "v1:reviews:review", "reviewId"
		case "reviewsList", "reviewsDecisionLog", "reviewsSettings", "reviewsUpdateSettings", "reviewsSetPublicDisplay", "reviewsIssueRequest", "reviewsExportStore", "reviewsPeople":
			return "v1:platform:site", "siteId"
		}
	}
	return "", ""
}

func (e *MemQLEngine) refuseOrganizationActionCapability(ctx context.Context, fn *Function, args map[string]any) (bool, error) {
	concept, argument := organizationCapabilityTarget(fn)
	if concept == "" {
		return false, nil
	}
	denied := func() (bool, error) {
		return true, fmt.Errorf("%s: %q is not permitted for the selected organization's target", CodeCapabilityNotHeld, fn.Name)
	}
	id := strings.TrimSpace(stringFromAny(args[argument]))
	requested := strings.TrimSpace(stringFromAny(args["accountId"]))
	account := ""
	found := false
	if id != "" && e != nil && e.database() != nil {
		meta, err := memorynodes.Get(concept)
		if err != nil {
			return denied()
		}
		row, exists, err := e.loadPriorPayload(ctx, meta, concept+":"+BareShortId(id))
		if err != nil {
			return denied()
		}
		found = exists
		if found {
			account = stringFromAny(row["accountId"])
		}
	}
	if !found {
		if fn.Name != "createPackage" && fn.Name != "packageSourceRegister" {
			return denied()
		}
		account = requested
		if account == "" {
			var err error
			account, err = organizationDefaultAccount(organizationOperator(ctx), e.accountScopeFor(ctx))
			if err != nil {
				return true, err
			}
		}
	}
	if concept == "v1:platform:package" && found {
		for _, field := range []string{"deploymentId", "fromDeploymentId"} {
			runID := strings.TrimSpace(stringFromAny(args[field]))
			if runID == "" {
				continue
			}
			meta, err := memorynodes.Get("v1:platform:packageDeployment")
			if err != nil {
				return denied()
			}
			run, exists, err := e.loadPriorPayload(ctx, meta, "v1:platform:packageDeployment:"+BareShortId(runID))
			if err != nil || !exists || BareShortId(stringFromAny(run["packageId"])) != BareShortId(id) {
				return denied()
			}
			runAccount := stringFromAny(run["accountId"])
			if runAccount != "" && BareShortId(runAccount) != BareShortId(account) {
				return denied()
			}
		}
	}
	// Untied historic resources keep the global capability contract; the row
	// guard still checks their personal ownership before any write.
	if account == "" {
		if !e.GlobalDataCapable(ctx, auth.VerbRead) {
			return denied()
		}
		return false, nil
	}
	if !e.OrganizationCapable(ctx, account, auth.VerbRead, auth.ResourceData) || !e.OrganizationCapable(ctx, account, auth.VerbRead, strings.SplitN(fn.RequiresCapability.Resource, "/", 2)[0]) || !e.OrganizationCapable(ctx, account, fn.RequiresCapability.Verb, fn.RequiresCapability.Resource) {
		return denied()
	}
	if requested != "" && BareShortId(requested) != BareShortId(account) && !e.OrganizationCapable(ctx, requested, fn.RequiresCapability.Verb, fn.RequiresCapability.Resource) {
		return denied()
	}
	return true, nil
}

// siteFieldParts is EVERY v1:platform:site field and the deployable part a
// change to it asks on an EXISTING site; a create is judged by the rules in
// validateOrganizationSensitiveChanges alone (an empty draft is ordinary data).
//
// WHY IT LISTS EVERY FIELD. The boundary used to ask only for the fields with a
// rule of their own, so every other field was ordinary data: a member of the
// site's organization holding Edit on Data and no part could switch a live
// storefront's shopper forms off (its reviews and signups answer 404), replace
// its runtime settings (no API version, so no products), or change its kind,
// hostname or resolution tail, with one plain write. memQL OS already drew that
// editor only for a publish holder (DeployablePage's RuntimeSettingsPanel); the
// engine now agrees. TestEverySiteFieldHasAPart fails the build when a field is
// added to the concept and not here, so a new field is a decision, not a gap.
//
// No parts: judged by its own rule in the guard (status, deleted, bundleRef,
// candidateRef, binding, previewBinding), a label (title, notes), or the
// organization itself, whose move is judged by membership in both
// organizations. "deploy" or "publish": which version, artifact or package a
// deploy came from. "publish": what visitors get, or whose authority the site's
// shopper forms borrow (ownerUserId). Saving a newsletter connection on a site
// whose forms are off turns them on as the person saving it, so that now asks
// publish too (component/campaigns/newsletter.go).
//
// Regression test: TestASiteFieldThatChangesWhatVisitorsGetAsksPublish.
var siteFieldParts = []struct {
	field string
	parts []string
}{
	{"status", nil}, {"deleted", nil}, {"bundleRef", nil}, {"candidateRef", nil}, {"binding", nil}, {"previewBinding", nil},
	{"title", nil}, {"notes", nil}, {"accountId", nil},
	{"artifactId", []string{"deploy", "publish"}}, {"packageId", []string{"deploy", "publish"}}, {"packageDeployableName", []string{"deploy", "publish"}},
	{"hostname", []string{"publish"}}, {"kind", []string{"publish"}}, {"resolutionTail", []string{"publish"}},
	{"apiProxy", []string{"publish"}}, {"shopperForms", []string{"publish"}}, {"systemOwned", []string{"publish"}},
	{"extensionRuntimePath", []string{"publish"}}, {"settings", []string{"publish"}}, {"storeSettings", []string{"publish"}},
	{"ownerUserId", []string{"publish"}},
}

// Sensitive deployable changes meet the same capability regardless of whether
// they arrived through a named action or raw upsert. Empty draft creation is
// ordinary data creation; serving bytes or arming deployment is not.
//
// next is the FINAL row -- merged, stamped and past every before-write hook --
// and prior the stored one, nil on a create (memql#5636). So a field next
// lacks and prior carries was REMOVED, which is a change like any other: a
// hook's field write with an absent value deletes the field.
func (e *MemQLEngine) validateOrganizationSensitiveChanges(ctx context.Context, concept string, prior, next map[string]any) error {
	// Tests are engine-owned instructions. Even an organization operator must
	// use the governed actions; a raw row write cannot retarget a queued test.
	if !auth.OriginFromContext(ctx).IsInternal() && (concept == "v1:campaigns:testSettings" ||
		concept == "v1:campaigns:campaign" && (stringFromAny(prior["testSourceCampaignId"]) != "" || stringFromAny(next["testSourceCampaignId"]) != "")) {
		return fmt.Errorf("campaign tests must be changed through their governed actions")
	}
	if concept != "v1:platform:site" && concept != "v1:platform:package" {
		return nil
	}
	if ac, _ := auth.AccessFromContext(ctx); ac != nil && ac.Synthetic {
		return nil
	}
	account := stringFromAny(prior["accountId"])
	if account == "" {
		account = stringFromAny(next["accountId"])
	}
	if account == "" {
		return nil
	}
	creating := prior == nil
	changed := func(field string) bool {
		return !reflect.DeepEqual(prior[field], next[field])
	}
	checks := [][]string{}
	require := func(parts ...string) { checks = append(checks, parts) }
	if concept == "v1:platform:site" {
		if changed("status") {
			status, old := stringFromAny(next["status"]), stringFromAny(prior["status"])
			if status == "live" || (!creating && old != "archived" && (status == "draft" || status == "disabled")) {
				require("publish")
			}
			if !creating && (status == "archived" || old == "archived") {
				require("retire")
			}
		}
		if changed("deleted") && !creating {
			require("retire")
		}
		if changed("bundleRef") && (!creating || strings.TrimSpace(stringFromAny(next["bundleRef"])) != "") {
			require("deploy", "publish")
		}
		if changed("candidateRef") && (!creating || strings.TrimSpace(stringFromAny(next["candidateRef"])) != "") {
			if changed("bundleRef") && stringFromAny(next["candidateRef"]) == "" {
				require("preview", "publish")
			} else {
				require("preview")
			}
		}
		for _, field := range []string{"binding", "previewBinding"} {
			oldBinding, _ := prior[field].(map[string]any)
			newBinding, _ := next[field].(map[string]any)
			if BareShortId(stringFromAny(oldBinding[bindingStoreIdKey])) != BareShortId(stringFromAny(newBinding[bindingStoreIdKey])) {
				require("store")
			}
		}
		if !creating {
			for _, f := range siteFieldParts {
				moved := changed(f.field)
				if f.field == "ownerUserId" || f.field == "artifactId" || f.field == "packageId" {
					// An id is stored canonical and often written bare: the same
					// person or row either way, as the bindings above compare.
					// TestAccountGrantAdmitsAWriteToATiedRow writes the owner bare.
					moved = BareShortId(stringFromAny(prior[f.field])) != BareShortId(stringFromAny(next[f.field]))
				}
				if len(f.parts) > 0 && moved {
					require(f.parts...)
				}
			}
		}
	} else {
		if !creating && changed("status") {
			require("retire")
		}
		if changed("autoDeploy") && (!creating || boolFromAny(next["autoDeploy"])) {
			require("sources")
		}
		if !creating {
			for _, field := range []string{"sourceRemoved", "sourceKind", "repoUrl", "repoRef", "credentialId", "sourceConnectionId", "artifactId", "deploymentMode"} {
				if changed(field) {
					require("sources")
				}
			}
			if changed("disabledDeployables") {
				require("retire")
			}
		}
	}
	accounts := []string{account}
	if target := stringFromAny(next["accountId"]); target != "" && BareShortId(target) != BareShortId(account) {
		accounts = append(accounts, target)
	}
	for _, target := range accounts {
		for _, alternatives := range checks {
			held := false
			for _, part := range alternatives {
				resource := "app:deployables/" + part
				if part == "sources" {
					resource = "app:pipelines/repositories"
				}
				held = held || e.OrganizationCapable(ctx, target, auth.VerbExecute, resource)
			}
			if !held {
				return fmt.Errorf("%s: the selected organization does not permit this deployable change (%s)", CodeCapabilityNotHeld, strings.Join(alternatives, " or "))
			}
		}
	}
	return nil
}
