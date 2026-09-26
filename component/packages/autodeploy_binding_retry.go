package packages

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Only the old bundle-pointer guard's exact refusal is eligible. Other publish
// failures (including a refused store CHANGE) are not infrastructure retries.
// This reads persisted failure evidence; it grants no publication authority.
var inheritedBindingPublishFailure = regexp.MustCompile(`^edge: pointing (\S+) at (blob://sites/\S+/): v1:platform:site: ("(?:[^"\\]|\\.)*") may not bind this storefront to v1:shopify:store ("(?:[^"\\]|\\.)*") -- it is not a store this caller can read\. Stores are read by developers and cluster owners; binding a storefront to one you cannot read would publish that store's Storefront token under this storefront's hostname\.$`)

// retryableInheritedBindingFailure recognizes runs blocked by the pre-0.23.4
// guard rechecking an unchanged store when updateSiteBundle wrote only bytes.
// The current site must still belong to this source/owner and name the rejected
// store. The normal retry pipeline then rechecks the snapshot, confirmation,
// and publication authority under the same rankless owner. This consumes the
// existing bounded retry budget, never a fresh one.
func (d *Deps) retryableInheritedBindingFailure(ctx context.Context, pkg, prior map[string]any) (bool, error) {
	owner := rowString(pkg, "ownerUserId")
	if rowString(prior, "status") != StatusFailed || rowBool(prior, "cancelRequested") ||
		!sameShortId(owner, rowString(prior, "ownerUserId")) || !sameShortId(owner, rowString(prior, "requestedBy")) {
		return false, nil
	}
	problem, _ := prior["error"].(map[string]any)
	if rowString(problem, "code") != "deploy_failed" || !rowBool(problem, "fatal") {
		return false, nil
	}
	message := rowString(problem, "message")
	match := inheritedBindingPublishFailure.FindStringSubmatch(message)
	if match == nil || !strings.HasPrefix(match[2], "blob://sites/"+match[1]+"/") {
		return false, nil
	}
	actor, actorErr := strconv.Unquote(match[3])
	storeID, storeErr := strconv.Unquote(match[4])
	if actorErr != nil || storeErr != nil || !sameShortId(actor, owner) || strings.TrimSpace(storeID) == "" {
		return false, nil
	}
	var outcomes []DeployableOutcome
	encoded, err := json.Marshal(prior["deployables"])
	if err != nil || json.Unmarshal(encoded, &outcomes) != nil {
		return false, nil
	}
	for _, outcome := range outcomes {
		if !sameShortId(outcome.SiteId, match[1]) || outcome.Name == "" || outcome.Refusal == nil ||
			outcome.Refusal.Code != "deployable_publish_failed" || !outcome.Refusal.Fatal ||
			outcome.Refusal.Scope != outcome.Name || outcome.Refusal.Message != message {
			continue
		}
		site, err := d.Store.siteById(ctx, outcome.SiteId)
		if err != nil {
			return false, err
		}
		return sameShortId(rowString(site, "id"), outcome.SiteId) &&
			rowString(site, "kind") == "shopify_storefront" &&
			sameShortId(rowString(site, "ownerUserId"), owner) &&
			sameShortId(rowString(site, "packageId"), rowString(pkg, "id")) &&
			rowString(site, "packageDeployableName") == outcome.Name &&
			sameShortId(boundStoreId(site), storeID), nil
	}
	return false, nil
}
