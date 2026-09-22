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
	case "app:deployables/deploy", "app:deployables/publish", "app:deployables/retire", "app:deployables/sources", "app:deployables/preview", "app:deployables/store":
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
		case "createPackage", "updatePackageSource", "disablePackageDeployables", "enablePackageDeployables", "setPackageAutoDeploy":
			if fn.BoundConcept == "v1:platform:package" {
				return fn.BoundConcept, "packageId"
			}
		}
	}
	if fn.FunctionKind == "builtin" {
		switch fn.Name {
		case "sitePublishFromArtifact", "siteArchive", "siteRestore", "siteDelete", "sitePreviewOpen":
			return "v1:platform:site", "siteId"
		case "packageAnalyze", "packageDeploy", "packageCancelDeployment", "packageRollback", "packageArchive", "packageDeactivateDeployable", "packageRestore", "packageSetAutoDeploy":
			return "v1:platform:package", "packageId"
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
		if fn.Name != "createPackage" {
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
	if !e.OrganizationCapable(ctx, account, auth.VerbRead, auth.ResourceData) || !e.OrganizationCapable(ctx, account, auth.VerbRead, "app:deployables") || !e.OrganizationCapable(ctx, account, fn.RequiresCapability.Verb, fn.RequiresCapability.Resource) {
		return denied()
	}
	if requested != "" && BareShortId(requested) != BareShortId(account) && !e.OrganizationCapable(ctx, requested, fn.RequiresCapability.Verb, fn.RequiresCapability.Resource) {
		return denied()
	}
	return true, nil
}

// Sensitive deployable changes meet the same capability regardless of whether
// they arrived through a named action or raw upsert. Empty draft creation is
// ordinary data creation; serving bytes or arming deployment is not.
func (e *MemQLEngine) validateOrganizationSensitiveChanges(ctx context.Context, concept string, prior, delta map[string]any) error {
	if concept != "v1:platform:site" && concept != "v1:platform:package" {
		return nil
	}
	if ac, _ := auth.AccessFromContext(ctx); ac != nil && ac.Synthetic {
		return nil
	}
	account := stringFromAny(prior["accountId"])
	if account == "" {
		account = stringFromAny(delta["accountId"])
	}
	if account == "" {
		return nil
	}
	creating := prior == nil
	changed := func(field string) bool {
		next, set := delta[field]
		return set && !reflect.DeepEqual(prior[field], next)
	}
	checks := [][]string{}
	require := func(parts ...string) { checks = append(checks, parts) }
	if concept == "v1:platform:site" {
		if changed("status") {
			next, old := stringFromAny(delta["status"]), stringFromAny(prior["status"])
			if next == "live" || (!creating && old != "archived" && (next == "draft" || next == "disabled")) {
				require("publish")
			}
			if !creating && (next == "archived" || old == "archived") {
				require("retire")
			}
		}
		if changed("deleted") && !creating {
			require("retire")
		}
		if changed("bundleRef") && (!creating || strings.TrimSpace(stringFromAny(delta["bundleRef"])) != "") {
			require("deploy", "publish")
		}
		if changed("candidateRef") && (!creating || strings.TrimSpace(stringFromAny(delta["candidateRef"])) != "") {
			if changed("bundleRef") && stringFromAny(delta["candidateRef"]) == "" {
				require("preview", "publish")
			} else {
				require("preview")
			}
		}
		for _, field := range []string{"binding", "previewBinding"} {
			if next, present := delta[field]; present {
				oldBinding, _ := prior[field].(map[string]any)
				newBinding, _ := next.(map[string]any)
				if BareShortId(stringFromAny(oldBinding[bindingStoreIdKey])) != BareShortId(stringFromAny(newBinding[bindingStoreIdKey])) {
					require("store")
				}
			}
		}
	} else {
		if !creating && changed("status") {
			require("retire")
		}
		if changed("autoDeploy") && (!creating || boolFromAny(delta["autoDeploy"])) {
			require("sources")
		}
		if !creating {
			for _, field := range []string{"sourceKind", "repoUrl", "repoRef", "credentialId", "artifactId", "deploymentMode"} {
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
	if target := stringFromAny(delta["accountId"]); target != "" && BareShortId(target) != BareShortId(account) {
		accounts = append(accounts, target)
	}
	for _, target := range accounts {
		for _, alternatives := range checks {
			held := false
			for _, part := range alternatives {
				held = held || e.OrganizationCapable(ctx, target, auth.VerbExecute, "app:deployables/"+part)
			}
			if !held {
				return fmt.Errorf("%s: the selected organization does not permit this deployable change (%s)", CodeCapabilityNotHeld, strings.Join(alternatives, " or "))
			}
		}
	}
	return nil
}
