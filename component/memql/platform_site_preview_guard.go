package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// The candidate version, the preview binding, and the go-live guard (epic
// memql#5531, issues memql#5544 and memql#5546). Sibling of
// platform_site_settings_guard.go and platform_site_binding_guard.go, wired
// into the same executeWrite path.
//
// # What these decide
//
// Four rules, and every one of them is a comparison a mutation body cannot
// make -- either against the PRIOR row or against a DIFFERENT row:
//
//   - A candidate may not equal the serving version. (prior row)
//   - A promotion must NAME the candidate it is promoting. (prior row)
//   - A preview binding must name a DEVELOPMENT store. (another row)
//   - Going live, and promoting, are refused while the SERVING binding names
//     a development store, or no store, or a store with no Storefront token.
//     (another row)
//
// # Where the rule itself lives
//
// Not here. The rules are pure functions in component/sitepreview, and this
// file is the wiring that gives them rows to judge. That is not tidiness: the
// OS has to know the same answers BEFORE the click, so an act that is not legal
// can be ABSENT rather than drawn and disabled (issue memql#5546 asks for
// exactly that), and the readiness capability answers from the same functions.
// Stated twice they would drift into the worst possible shape -- a control the
// OS offers because its copy says yes, refused by a guard whose copy says no.
//
// # The store is read as the DEPLOYMENT, and that is deliberate
//
// v1:shopify:store is @rowAuthz(clusterOwner, rankFloor="developer"), so a
// read under the caller answers nothing for a site owner below developer rank
// -- and "nobody could tell me" would then refuse their go-live whenever a
// store is involved. The question
// here is not the caller's: it is "is this cluster about to point a storefront
// at a development store", a fact about the deployment. The caller's own
// authority was already settled twice over -- @requiresCapability names the
// surface, and platform_site_binding_guard.go refuses a binding -- serving or
// preview -- naming a store the CALLER cannot read, so a binding this guard
// judges was set by somebody who could see it.
//
// It mirrors component/edge exactly, which resolves the same row under the same
// kind of synthetic operator for the same reason.

// systemSiteGuardActor is the engine's own operator identity for the
// clusterOwner-tier store read this file makes. Synthetic, and scoped by what
// it is used for: one named query over one concept, answering one boolean.
const systemSiteGuardActor = "system:sitePreviewGuard"

// BoundStoreAsDeployment resolves a bound store under the deployment's own
// operator identity, into the facts the rules judge on. A miss is
// Readable=false, which the rules read as "nobody could answer" and refuse on
// -- never as "it is not a development store".
//
// ONE READ FOR THE GUARD AND THE READINESS ANSWER. The guard judges on it, and
// integrations/sitepreview builds its go-live facts from it, so readiness
// cannot withhold a Go live the guard would accept from a caller who may go
// live but not read the store. Domain is the deployment's view: an answer
// shown to a caller replaces it with what the caller's own read disclosed.
func BoundStoreAsDeployment(ctx context.Context, execute func(context.Context, string) (any, error), storeId string) (PreviewBoundStore, error) {
	storeId = strings.TrimSpace(storeId)
	if storeId == "" {
		return PreviewBoundStore{}, nil
	}
	claims := map[string]any{"sub": systemSiteGuardActor, "role": "owner"}
	readCtx := auth.ContextWithClaims(ctx, claims)
	readCtx = auth.ContextWithToken(readCtx, auth.BuildTokenInfo(claims))
	readCtx = auth.ContextWithAccess(readCtx, &auth.AccessContext{
		UserId: systemSiteGuardActor,
		Role:   auth.RoleOwner,
	})
	res, err := execute(readCtx, fmt.Sprintf("query storeById(storeId: %s)", languageParser.QuoteString(storeId)))
	if err != nil {
		return PreviewBoundStore{}, err
	}
	rows := MaterializeRows(res)
	if len(rows) == 0 {
		return PreviewBoundStore{ID: storeId}, nil
	}
	return PreviewBoundStore{
		ID:                 storeId,
		Readable:           true,
		IsDevelopment:      boolFromAny(rows[0]["isDevelopment"]),
		Domain:             stringFromAny(rows[0]["domain"]),
		HasStorefrontToken: strings.TrimSpace(stringFromAny(rows[0]["storefrontTokenRef"])) != "",
	}, nil
}

// boundStoreFacts is BoundStoreAsDeployment over this engine.
func (e *MemQLEngine) boundStoreFacts(ctx context.Context, storeId string) (PreviewBoundStore, error) {
	if strings.TrimSpace(storeId) != "" && !e.canResolve() {
		return PreviewBoundStore{}, ErrEngineNotInitialized
	}
	return BoundStoreAsDeployment(ctx, func(ctx context.Context, q string) (any, error) { return e.Execute(ctx, q) }, storeId)
}

// validateSitePreview is the whole of this file's contribution to executeWrite.
//
// ONE ENTRY POINT FOR FOUR RULES, rather than four calls at the call site, and
// the reason is the store read: three of the four need the same row, and four
// separate guards would each fetch it. It short-circuits before any read when
// the write touches none of the fields, which is every ordinary publish,
// rename, settings edit and status pause.
//
// # systemOwned is exempt from the whole axis
//
// As it is from the status axis and the settings axis, and for their reason:
// the platform's own site is re-seeded at every boot, so a candidate or a
// preview binding written onto it would be reverted and a person would watch it
// silently undo itself. The PRIOR flag is read, not the merged one, so a write
// smuggling `systemOwned: false` into the same delta cannot slip past -- the
// same-delta bypass the delete guard and the status guard each refuse.
//
// # A system actor is exempt, symmetrically
//
// The SeedMaterializer re-writes the platform site row on every boot and must
// not be refused by a rule about candidates it does not set.
func (e *MemQLEngine) validateSitePreview(
	ctx context.Context,
	payload map[string]any,
	priorExisted bool,
	priorSystemOwned bool,
	priorStatus string,
	priorKind string,
	priorBundleRef string,
	priorCandidateRef string,
	priorBindingStoreId string,
	actor string,
) error {
	if payload == nil {
		return nil
	}

	candidate, candidatePresent := previewFieldString(payload, "candidateRef")
	previewBinding, previewBindingPresent := previewBindingStoreId(payload)
	nextStatus := strings.TrimSpace(stringFromAny(payload["status"]))
	nextBundle, bundlePresent := previewFieldString(payload, "bundleRef")

	// A PROMOTION IS RECOGNISED BY ITS SHAPE, not by a flag: it is the one
	// write that moves the serving version and clears the candidate in the same
	// delta. Nothing else does that, and promoteSiteCandidate is the only
	// construct that produces it.
	promoting := bundlePresent && candidatePresent && candidate == "" && nextBundle != "" && nextBundle != priorBundleRef

	// GOING LIVE IS A TRANSITION, so it is judged against the PRIOR status and
	// not against the merged payload -- every ordinary write to a live site
	// inherits `status: "live"` through the read-merge, and judging that would
	// refuse a rename on any live storefront bound to a development store. A
	// CREATION directly at `live` is a transition too: it is a site going in
	// front of shoppers for the first time, which is exactly the act guarded.
	goingLive := nextStatus == siteStatusLive &&
		(!priorExisted || strings.TrimSpace(priorStatus) != siteStatusLive)

	priorStorefront := strings.TrimSpace(priorKind) == storefrontSiteKind
	storefront := priorStorefront
	if k, ok := previewFieldString(payload, "kind"); ok && strings.TrimSpace(k) != "" {
		storefront = strings.TrimSpace(k) == storefrontSiteKind
	}
	// BECOMING A STOREFRONT WHILE LIVE is going live as one: the status does
	// not move, so the transition above misses a write (a re-run createSite, a
	// raw insert) that turns a live spa into a storefront with no store.
	goingLive = goingLive || (storefront && !priorStorefront && nextStatus == siteStatusLive)

	touches := candidatePresent || previewBindingPresent || promoting || goingLive
	if !touches {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	if priorExisted && priorSystemOwned && (candidatePresent || previewBindingPresent || promoting) {
		return fmt.Errorf(
			"v1:platform:site: this is the surface the cluster is managed through and is exempt from the preview axis, as it is from the status and settings axes -- it is deployed with the image and re-seeded at every boot, so a candidate written here would silently undo itself")
	}

	// Rule 1 -- a candidate may not be the version already serving.
	if candidatePresent && candidate != "" {
		serving := priorBundleRef
		if bundlePresent && !promoting {
			serving = nextBundle
		}
		if refusal := SiteCandidateRefusal(serving, candidate); !refusal.Empty() {
			return previewRefusalError(refusal)
		}
	}

	// Rule 2 -- a promotion names the candidate it promotes.
	if promoting {
		if refusal := SitePromotionRefusal(priorCandidateRef, nextBundle); !refusal.Empty() {
			return previewRefusalError(refusal)
		}
	}

	// Rule 3 -- a preview binding names a DEVELOPMENT store.
	//
	// CLEARING IT IS ALWAYS ALLOWED. An empty storeId is the unbound state, and
	// detaching a development store must stay expressible for the reason
	// updateSiteSettings gives about clearing a value.
	if previewBindingPresent && previewBinding != "" {
		facts, err := e.boundStoreFacts(ctx, previewBinding)
		if err != nil {
			return fmt.Errorf("v1:platform:site: could not read v1:shopify:store %q to check the preview binding: %w", previewBinding, err)
		}
		if refusal := SitePreviewBindingRefusal(true, facts); !refusal.Empty() {
			return previewRefusalError(refusal)
		}
	}

	// Rule 4 -- going live, and promoting, are refused while the SERVING
	// binding names a development store, or nothing, or a store with no
	// Storefront token (Connect Shopify, D5). An empty binding reads no
	// store: boundStoreFacts answers it without a query.
	//
	// The binding judged is the one this write LEAVES BEHIND: a caller may
	// re-bind and go live in one delta, and judging the stored value would
	// refuse exactly the write that fixes the problem.
	if storefront && (goingLive || promoting) {
		servingStore := priorBindingStoreId
		if next, ok := previewBindingStoreIdFrom(payload, "binding"); ok {
			servingStore = next
		}
		facts, err := e.boundStoreFacts(ctx, servingStore)
		if err != nil {
			return fmt.Errorf("v1:platform:site: could not read v1:shopify:store %q to check what this storefront is bound to: %w", servingStore, err)
		}
		if refusal := SiteGoLiveRefusal(true, facts); !refusal.Empty() {
			return previewRefusalError(refusal)
		}
	}
	return nil
}

// previewRefusalError renders a rule's refusal as the engine's error.
//
// THE CODE IS IN THE STRING, and it is there for the OS: a refusal that reached
// the engine anyway (because the readiness answer was stale, or because the
// caller never asked) is matched on the code so the same copy is shown as the
// one the absent control would have carried. The remedy follows it, because a
// refusal that names only what is wrong leaves a person with nothing to do.
func previewRefusalError(r PreviewRefusal) error {
	return fmt.Errorf("v1:platform:site: %s: %s %s", r.Code, r.Message, r.Remedy)
}

// storefrontSiteKind is v1:platform:site.kind's storefront value. Spelled the
// same in component/edge and integrations/sitepreview; it is the concept's own
// enum value rather than a name any of the three invented.
const storefrontSiteKind = "shopify_storefront"

// previewFieldString reads a string field and reports whether the delta carried
// it at all.
//
// PRESENCE IS THE WHOLE QUESTION for every rule above. A publish, a rename and
// a status flip all inherit candidateRef and previewBinding through the
// read-merge, and judging an inherited value would refuse ordinary writes to
// every deployable that has ever had a candidate -- the same trap
// platform_site_binding_guard.go avoids by returning early on an absent key.
func previewFieldString(payload map[string]any, key string) (string, bool) {
	raw, present := payload[key]
	if !present {
		return "", false
	}
	if raw == nil {
		return "", true
	}
	return strings.TrimSpace(stringFromAny(raw)), true
}

// previewBindingStoreId reads previewBinding.storeId out of a delta.
func previewBindingStoreId(payload map[string]any) (string, bool) {
	return previewBindingStoreIdFrom(payload, "previewBinding")
}

func previewBindingStoreIdFrom(payload map[string]any, field string) (string, bool) {
	raw, present := payload[field]
	if !present {
		return "", false
	}
	if raw == nil {
		return "", true
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", true
	}
	return strings.TrimSpace(stringFromAny(obj[bindingStoreIdKey])), true
}
