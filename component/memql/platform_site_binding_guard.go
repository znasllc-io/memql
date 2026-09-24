package memql

import (
	"context"
	"fmt"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// The storefront binding NAMES a store, and this is the guard that decides
// which stores a caller may name (epic memql#5530, issue memql#5538).
// Sibling of platform_site_hostname_policy.go and
// platform_package_source_policy.go, wired into the same executeWrite path.
//
// # What it decides
//
// A v1:platform:site of kind shopify_storefront carries `binding: {storeId}`,
// a reference to a v1:shopify:store row. This refuses two writes: one naming
// a store row the CALLER CANNOT READ, and one still carrying the retired
// {storeDomain, storefrontTokenRef} copy.
//
// # Why it is Go rather than DSL
//
// "May this caller read the row this value names" is a CROSS-ROW read, and no
// mutation body can make one -- the same reason platform_package_source_policy.go
// gives for living here. A filter grammar sees the value being written; it
// cannot go and ask whether a different row of a different concept comes back
// for this actor.
//
// # Why the readability check is the substantive gate
//
// @requiresCapability("execute", "app:deployables/store") names the SURFACE:
// who may reach updateSiteStoreBinding at all. It is a grant on an app part,
// and a grant cannot know which rows a cluster holds. A site is owner-tier, a
// store is cluster-owner-tier, so without this check a site owner could bind
// their own deployable to ANY store in the cluster -- and the edge, which
// resolves the binding under a synthetic cluster-owner actor, would then serve
// that store's Storefront token and domain under the site owner's own
// hostname. The capability says who may bind; this says to what.
//
// # Why the retired shape is refused rather than ignored
//
// Pre-release means no shim (CLAUDE.md). A write still carrying the copied
// domain is a caller nobody migrated, and silently dropping the keys would
// leave a row whose binding looks converted while the caller that wrote it
// goes on believing it wrote a domain. Refusing names the caller.

// storeReadable reports whether the calling actor may read the named
// v1:shopify:store row. Injected so the guard's own tests need no database;
// the engine passes canReadStore.
type storeReadable func(ctx context.Context, storeId string) (bool, error)

// bindingStoreIdKey is the ONE key a shopify_storefront binding carries.
const bindingStoreIdKey = "storeId"

// retiredBindingKeys are the two the binding used to copy off the store row.
// They are refused rather than ignored: pre-release means no shim, and a write
// that still carries them is a caller nobody migrated.
var retiredBindingKeys = []string{"storeDomain", "storefrontTokenRef"}

// validateSiteStoreBinding refuses a `binding` delta that names a store the
// caller cannot read, or that still carries the retired copied shape.
//
// A write that names no `binding` key passes untouched: a publish, a rename
// and a status flip inherit the stored object through the read-merge, and
// re-judging an inherited value would refuse every unrelated write to a site
// bound before the rule. An explicit null, an empty object and an empty
// storeId are the UNBOUND state and pass reading no store at all -- a
// storefront is created before its store is attached, and a store can be
// detached, so clearing must stay expressible (updateSiteSettings' reason).
func (e *MemQLEngine) validateSiteStoreBinding(
	ctx context.Context,
	payload map[string]any,
	actor string,
	readable storeReadable,
) error {
	if payload == nil {
		return nil
	}
	raw, present := payload["binding"]
	if !present || raw == nil {
		return nil
	}
	binding, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: binding must be an object naming the store this storefront fronts, {storeId: \"...\"}; got %T",
			raw,
		)
	}

	var retired []string
	for _, key := range retiredBindingKeys {
		if _, found := binding[key]; found {
			retired = append(retired, key)
		}
	}
	if len(retired) > 0 {
		// Sorted so a refusal names the same keys in the same order on every
		// attempt: a map walked in iteration order reads as a moving target.
		sort.Strings(retired)
		return fmt.Errorf(
			"v1:platform:site: binding carries %s, which the storefront binding no longer holds (epic memql#5530). The store row holds the domain and the Storefront token reference; the binding names it with storeId and the edge resolves both through it.",
			strings.Join(retired, " and "),
		)
	}

	storeId := strings.TrimSpace(stringFromAny(binding[bindingStoreIdKey]))
	if storeId == "" {
		// The unbound state, and it must stay expressible: a storefront can be
		// created before its store is attached, and a store can be detached.
		return nil
	}
	return requireReadableStore(ctx, storeId, actor, "bind this storefront to", readable)
}

// requireReadableStore refuses a store the caller cannot read. Shared by the
// serving binding and the preview binding: both name a store, and the edge
// serves the Storefront token of either (the preview one under a preview
// grant) under the site's own hostname. `act` names which of the two was
// refused, so the refusal says what the caller tried to do.
func requireReadableStore(ctx context.Context, storeId, actor, act string, readable storeReadable) error {
	if readable == nil {
		return fmt.Errorf("v1:platform:site: cannot check that %q is readable -- no store reader is wired", storeId)
	}
	ok, err := readable(ctx, storeId)
	if err != nil {
		return fmt.Errorf("v1:platform:site: could not read v1:shopify:store %q to %s it: %w", storeId, strings.TrimSuffix(act, " to"), err)
	}
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: %q may not %s v1:shopify:store %q -- it is not a store this caller can read. A store is cluster-owner-tier; binding a storefront to one you cannot read would publish that store's Storefront token under your own hostname.",
			actor, act, storeId,
		)
	}
	return nil
}

// storeBindingCapability is what CHANGING a storefront's store takes: the store
// part, the requirement updateSiteStoreBinding and updateSitePreviewBinding
// declare on themselves.
var storeBindingCapability = CapabilityRequirement{Verb: "execute", Resource: "app:deployables/store"}

// validateSiteStoreBindingChange asks for the store part when a write CHANGES
// which store a site is bound to: a create that binds, or a rewrite that
// re-points.
//
// validateSiteStoreBinding answers "to what" -- a store the caller can read --
// and before developers could read stores, reading implied the store part,
// because only owners read them and owners hold every part. Once developers
// read every store (D3), reading alone would let one bind through createSite,
// which takes a binding and declares no capability, or through a raw insert(),
// which names no construct at all. So the capability is asked here, at the
// seam every write passes, the way the two binding mutations ask it of
// themselves.
//
// The organization boundary (memql#5598, validateOrganizationSensitiveChanges)
// asks for the same part at the site's ORGANIZATION on every attributed write
// by a non-synthetic actor. This check does not depend on that one: it asks
// the caller's own capability on every write that reaches the seam, so the D3
// property holds wherever the store tier is widened, whatever the boundary
// decides to exempt.
//
// Only a CHANGE is judged. The payload here is the merged row, so a check on
// "the payload names a store" would demand the store part for a publish or a
// settings edit on a site someone else bound. Clearing the binding is not a
// change this check judges, because detaching names no store (the
// organization boundary still asks for the part at the site's organization).
func (e *MemQLEngine) validateSiteStoreBindingChange(ctx context.Context, payload map[string]any, priorStoreId string) error {
	storeId := changedStoreId(payload, "binding", priorStoreId)
	if storeId == "" {
		return nil
	}
	return e.refuseBelowRequiredCapability(ctx, &Function{RequiresCapability: storeBindingCapability},
		"binding a storefront to v1:shopify:store "+storeId)
}

// validateSitePreviewBindingChange gives the PREVIEW binding the serving
// binding's two checks, on a write that changes it: the caller must be able to
// read the store it names, and must hold the store part (Connect Shopify 009).
//
// The preview guard reads the store too, but as the deployment, to learn
// whether it is a development store -- a fact that answers the same for every
// caller, so it never said whether THIS caller could read it. And
// updateSitePreviewBinding's own @requiresCapability is never met by a raw
// insert(), which names no construct. So both checks are asked here, at the
// seam every write passes.
//
// The organization boundary (memql#5598, validateOrganizationSensitiveChanges)
// asks for the store part at the site's ORGANIZATION too, and nothing there
// asks whether the caller can read the store. As with the serving binding,
// neither check here depends on the boundary.
//
// Both judge only a CHANGE, against the prior preview binding, for
// validateSiteStoreBindingChange's reason: the payload is the merged row, and
// re-judging an inherited binding would refuse the site owner's own rename
// because a cluster owner once pointed the preview at a store they cannot read.
// Clearing names no store, so neither check judges it (the organization
// boundary still asks for the part at the site's organization).
func (e *MemQLEngine) validateSitePreviewBindingChange(
	ctx context.Context,
	payload map[string]any,
	priorStoreId string,
	actor string,
	readable storeReadable,
) error {
	storeId := changedStoreId(payload, "previewBinding", priorStoreId)
	if storeId == "" {
		return nil
	}
	if err := requireReadableStore(ctx, storeId, actor, "point this storefront's preview at", readable); err != nil {
		return err
	}
	return e.refuseBelowRequiredCapability(ctx, &Function{RequiresCapability: storeBindingCapability},
		"pointing a storefront's preview at v1:shopify:store "+storeId)
}

// changedStoreId is the store a write moves a {storeId} reference field TO, or
// "" when it moves it nowhere: absent, cleared, or the prior row's value.
func changedStoreId(payload map[string]any, field, priorStoreId string) string {
	storeId, _ := previewBindingStoreIdFrom(payload, field)
	if storeId == strings.TrimSpace(priorStoreId) {
		return ""
	}
	return storeId
}

// canReadStore is the engine's own reader: the named query, under the CALLER's
// actor, deliberately -- the whole point is that the answer is the caller's,
// not the deployment's. v1:shopify:store's tier (clusterOwner, with a read
// floor at developer) answers anyone below the floor with zero rows rather
// than an error, which is the answer this guard wants: not readable.
//
// The argument is quoted with languageParser.QuoteString rather than Go's %q,
// because a call string is MemQL source and Go's escapes are not MemQL's.
func (e *MemQLEngine) canReadStore(ctx context.Context, storeId string) (bool, error) {
	if !e.canResolve() {
		return false, ErrEngineNotInitialized
	}
	res, err := e.Execute(ctx, fmt.Sprintf("query storeById(storeId: %s)", languageParser.QuoteString(storeId)))
	if err != nil {
		return false, err
	}
	return len(MaterializeRows(res)) > 0, nil
}
