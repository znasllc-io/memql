package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE GUARD'S WIRING (epic memql#5531, issue memql#5548).
//
// site_preview_rules_test.go beside this one covers the RULES -- pure functions
// over values, every case a row in a table. This file covers the other half,
// which fails differently: platform_site_preview_guard.go decides WHICH rule
// applies to a given write delta, and that is where the interesting mistakes
// are. Three of them would each be silent:
//
//   - judging an INHERITED candidateRef would refuse every ordinary publish,
//     rename and settings edit on any deployable that has ever had a candidate;
//   - judging the MERGED status rather than the TRANSITION would refuse a
//     rename on any live storefront bound to a development store, because every
//     write to a live site inherits `status: "live"` through the read-merge;
//   - failing to recognise a promotion BY ITS SHAPE would leave promotion
//     ungated entirely, since no flag says "this is a promotion".
//
// WHY THESE CASES RUN AGAINST A ZERO-VALUE ENGINE, and why that is the sharp
// part rather than a shortcut. Two of the four rules read a v1:shopify:store
// row, which needs a database; the other two compare against the PRIOR row and
// need nothing. So a case that passes here is one that reached its answer
// WITHOUT a cross-concept read -- and that is exactly the property each of
// these tests is about. It is also how the rollback claim becomes checkable:
// `updateSiteBundle` pointed back at a previous version touches none of this
// guard's fields, so it must return before any read. A guard that had grown a
// store lookup on that path fails here rather than silently costing a query
// per rollback.
//
// The two store-reading rules are covered at the rules layer and, against real
// rows, by test/clustere2e/storefront_preview_test.go.

// previewGuardDelta is one write, as executeWrite hands it to the guard: the
// DELTA plus the facts about the prior row that a mutation body cannot see.
type previewGuardDelta struct {
	priorExisted     bool
	priorSystemOwned bool
	priorStatus      string
	priorKind        string
	priorBundleRef   string
	priorCandidate   string
	priorBindingID   string
	payload          map[string]any
	actor            string
}

func runPreviewGuard(t *testing.T, d previewGuardDelta) error {
	t.Helper()
	actor := d.actor
	if actor == "" {
		actor = "v1:identity:user:operator"
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: actor,
		Role:   auth.RoleOwner,
	})
	return (&MemQLEngine{}).validateSitePreview(ctx, d.payload, d.priorExisted, d.priorSystemOwned,
		d.priorStatus, d.priorKind, d.priorBundleRef, d.priorCandidate, d.priorBindingID, actor)
}

// ROLLBACK IS ONE ROW WRITE, UNCHANGED BY THIS EPIC -- stated as something a
// test can fail rather than only as a sentence in a comment.
//
// `updateSiteBundle` pointed back at a previous version names no candidate, no
// preview binding and no status, so the guard short-circuits before it reaches
// for a store. That is why rollback needed nothing new and was deliberately
// given no ceremony of its own: the version a promotion replaced is still
// sitting under its own prefix in object storage, and pointing back at it is
// the same write publishing is, in the other direction.
func TestARollbackIsNotGatedByThePreviewGuard(t *testing.T) {
	err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "live",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/3/",
		priorBindingID: "acme",
		payload:        map[string]any{"id": "s", "bundleRef": "blob://sites/s/v/2/"},
	})
	if err != nil {
		t.Fatalf("a rollback on a live storefront was refused: %v", err)
	}
}

// AN ORDINARY WRITE IS NOT JUDGED ON AN INHERITED VALUE. A publish, a rename
// and a settings edit all inherit candidateRef and previewBinding through the
// read-merge; judging an inherited value would refuse every ordinary write to
// every deployable that has ever had a candidate, which is the trap the
// presence check exists to avoid.
func TestAnOrdinaryWriteIsNotJudgedOnAnInheritedCandidateOrStatus(t *testing.T) {
	// The delta carries no candidateRef key at all, which is what the mutation
	// templates produce for a mutation that does not name the field. The stored
	// value is deliberately one the guard WOULD refuse if it judged it.
	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "live",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/3/",
		priorCandidate: "blob://sites/s/v/3/",
		priorBindingID: "acme",
		payload:        map[string]any{"id": "s", "title": "Acme, renamed"},
	}); err != nil {
		t.Fatalf("a rename on a live storefront carrying a candidate was refused: %v", err)
	}

	// GOING LIVE IS A TRANSITION, not a value. Every ordinary write to a live
	// site inherits `status: "live"`, and judging the merged value rather than
	// the transition would refuse a rename on any live storefront bound to a
	// development store -- which is also a case that would reach for a store
	// row, so against a zero-value engine it fails loudly rather than subtly.
	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "live",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/3/",
		priorBindingID: "acme",
		payload:        map[string]any{"id": "s", "status": "live", "title": "Acme"},
	}); err != nil {
		t.Fatalf("a write inheriting status=live on a live storefront was refused: %v", err)
	}
}

// THE GUARD REFUSES A CANDIDATE THAT IS ALREADY SERVING, and the refusal
// carries its CODE -- which is what the OS keys its copy on when a refusal
// reaches the engine anyway, because the readiness answer was stale or because
// the caller never asked.
func TestTheGuardRefusesACandidateThatIsAlreadyServing(t *testing.T) {
	setCandidate := func(serving, candidate string) error {
		return runPreviewGuard(t, previewGuardDelta{
			priorExisted:   true,
			priorStatus:    "draft",
			priorKind:      "spa",
			priorBundleRef: serving,
			payload:        map[string]any{"id": "s", "candidateRef": candidate},
		})
	}

	err := setCandidate("blob://sites/s/v/3/", "blob://sites/s/v/3/")
	if err == nil {
		t.Fatal("setting the serving version as the candidate was allowed")
	}
	if !strings.Contains(err.Error(), PreviewRefusalCandidateIsServing) {
		t.Errorf("the refusal does not name %q: %v", PreviewRefusalCandidateIsServing, err)
	}

	// The reachable positive: a DIFFERENT version is accepted on the same path,
	// so the refusal above is the comparison rather than a path that is closed.
	if err := setCandidate("blob://sites/s/v/3/", "blob://sites/s/v/4/"); err != nil {
		t.Fatalf("setting a genuine candidate was refused: %v", err)
	}
	// And withdrawing one is always allowed: clearSiteCandidate is the plain
	// inverse and is how every open preview of a version ends at once.
	if err := setCandidate("blob://sites/s/v/3/", ""); err != nil {
		t.Fatalf("withdrawing a candidate was refused: %v", err)
	}
}

// A PROMOTION IS RECOGNISED BY ITS SHAPE -- the one write that moves the
// serving version and clears the candidate in the same delta -- and it must
// NAME the candidate it is promoting.
//
// A mutation body cannot read the row's own stored field, so the candidate
// arrives as an argument; an argument nobody checked would let a candidate
// republished between the reading and the click be promoted by surprise. The
// work spine's approvals carry an artifact hash for exactly this reason.
//
// The kind here is `spa` deliberately: this rule answers BEFORE the go-live
// rule would reach for a store, so a pass proves both that the promotion shape
// was recognised and that the check which can be made locally ran first.
func TestTheGuardRefusesAPromotionWhoseCandidateMoved(t *testing.T) {
	promote := func(stored, named string) error {
		return runPreviewGuard(t, previewGuardDelta{
			priorExisted:   true,
			priorStatus:    "live",
			priorKind:      "spa",
			priorBundleRef: "blob://sites/s/v/3/",
			priorCandidate: stored,
			payload:        map[string]any{"id": "s", "bundleRef": named, "candidateRef": ""},
		})
	}

	err := promote("blob://sites/s/v/5/", "blob://sites/s/v/4/")
	if err == nil {
		t.Fatal("a promotion naming a version that is not the stored candidate was allowed")
	}
	if !strings.Contains(err.Error(), PreviewRefusalCandidateMoved) {
		t.Errorf("the refusal does not name %q: %v", PreviewRefusalCandidateMoved, err)
	}

	err = promote("", "blob://sites/s/v/4/")
	if err == nil {
		t.Fatal("a promotion against a deployable with no candidate was allowed")
	}
	if !strings.Contains(err.Error(), PreviewRefusalNoCandidate) {
		t.Errorf("the refusal does not name %q: %v", PreviewRefusalNoCandidate, err)
	}

	// The reachable positive: the promotion that names the stored candidate is
	// allowed, so the two refusals above are the comparison rather than a
	// promotion path that is simply shut.
	if err := promote("blob://sites/s/v/4/", "blob://sites/s/v/4/"); err != nil {
		t.Fatalf("a promotion naming the stored candidate was refused: %v", err)
	}
}

// THE PLATFORM'S OWN SITE IS EXEMPT FROM THE WHOLE AXIS, as it is from the
// status axis and the settings axis: it is deployed with the image and
// re-seeded at every boot, so a candidate written on it would silently undo
// itself and a person would watch it happen.
//
// THE PRIOR FLAG IS READ, NOT THE MERGED ONE, so a write smuggling
// `systemOwned: false` into the same delta cannot slip past -- the same
// same-delta bypass the delete guard and the status guard each refuse.
func TestTheGuardExemptsThePlatformsOwnSiteAndRefusesTheSameDeltaBypass(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"a candidate on the platform's own site": {"id": "s", "candidateRef": "blob://x/"},
		"clearing systemOwned in the same delta": {"id": "s", "systemOwned": false, "candidateRef": "blob://x/"},
		"a preview binding on it":                {"id": "s", "previewBinding": map[string]any{"storeId": "acme-dev"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := runPreviewGuard(t, previewGuardDelta{
				priorExisted:     true,
				priorSystemOwned: true,
				priorStatus:      "live",
				priorKind:        "spa",
				priorBundleRef:   "blob://os/v/1/",
				payload:          payload,
			})
			if err == nil {
				t.Fatal("the platform's own site accepted a write on the preview axis")
			}
			if !strings.Contains(err.Error(), "exempt from the preview axis") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
}

// A SYSTEM ACTOR IS EXEMPT, SYMMETRICALLY. The SeedMaterializer re-writes the
// platform site row on every boot and must not be refused by a rule about
// candidates it does not set -- without the exemption a boot would fail on a
// row the deployment wrote itself.
func TestASystemActorIsNotJudgedByThePreviewGuard(t *testing.T) {
	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:     true,
		priorSystemOwned: true,
		priorStatus:      "live",
		priorKind:        "spa",
		priorBundleRef:   "blob://os/v/1/",
		payload:          map[string]any{"id": "s", "candidateRef": "blob://os/v/1/"},
		actor:            "system:seedMaterializer",
	}); err != nil {
		t.Fatalf("the seed materializer was refused: %v", err)
	}
}

// A NIL OR EMPTY DELTA IS NOT A WRITE ON THIS AXIS. The guard runs on EVERY
// v1:platform:site write in the cluster, so the cheap exit has to be correct
// before anything else is.
func TestThePreviewGuardShortCircuitsOnAWriteThatTouchesNothing(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"nil":                               nil,
		"empty":                             {},
		"an unrelated field":                {"id": "s", "notes": "a note"},
		"the serving binding":               {"id": "s", "binding": map[string]any{"storeId": "acme"}},
		"a status that is not a transition": {"id": "s", "status": "draft"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runPreviewGuard(t, previewGuardDelta{
				priorExisted:   true,
				priorStatus:    "draft",
				priorKind:      storefrontSiteKind,
				priorBundleRef: "blob://sites/s/v/1/",
				priorBindingID: "acme",
				payload:        payload,
			}); err != nil {
				t.Fatalf("a write touching nothing on this axis was refused: %v", err)
			}
		})
	}
}

// --- the transition, the shape, and the binding the write leaves behind -----
//
// The four cases below are the remaining ways the guard can pick the WRONG
// rule for a delta, and each one fails silently in a different direction. They
// run against the same zero-value engine as the cases above, so a case that
// reaches for a v1:shopify:store row says so by name in its error -- which is
// what makes "which store did it judge" an assertable fact rather than an
// inference (see this file's header for why the zero-value engine is the sharp
// instrument here and not a shortcut).

// storeReadRefusal reports whether err is the guard reaching for a store row
// under an engine that has none, and which store it asked for. A zero-value
// engine cannot answer, so the read is the observable: the guard names the
// store it tried to read in the error it returns.
func storeReadRefusal(err error) (storeId string, reached bool) {
	if err == nil || !strings.Contains(err.Error(), "could not read v1:shopify:store") {
		return "", false
	}
	// The message is `... v1:shopify:store "<id>" ...`; the id is what it
	// quotes, and it is the only quoted value in it.
	parts := strings.SplitN(err.Error(), `"`, 3)
	if len(parts) < 3 {
		return "", true
	}
	return parts[1], true
}

// A CREATION DIRECTLY AT `live` IS A TRANSITION TOO, and it is the one the
// obvious reading of "judge the transition, not the value" misses: there is no
// prior status to differ from, so a guard that compared `next != prior` and
// stopped there would let a storefront go in front of shoppers for the first
// time, bound to a development store, without ever asking.
func TestACreationDirectlyAtLiveIsJudged(t *testing.T) {
	create := func(status string) error {
		return runPreviewGuard(t, previewGuardDelta{
			priorExisted: false,
			payload: map[string]any{
				"id":      "s",
				"kind":    storefrontSiteKind,
				"status":  status,
				"binding": map[string]any{"storeId": "acme-dev"},
			},
		})
	}

	storeId, reached := storeReadRefusal(create("live"))
	if !reached {
		t.Fatal("a storefront created directly at live was not judged -- the guard never asked " +
			"what store it is bound to, so a first publish onto a development store goes through")
	}
	if storeId != "acme-dev" {
		t.Errorf("the guard asked about store %q, want the one the creation binds", storeId)
	}

	// THE NEGATIVE CONTROL, and it has to fail for the right reason: the same
	// creation at `draft` is not a transition into shoppers' view, so it must
	// reach for nothing at all.
	if err := create("draft"); err != nil {
		t.Errorf("a storefront created at draft was judged: %v", err)
	}
}

// A PROMOTION IS RECOGNISED BY ITS SHAPE -- bundleRef MOVES and candidateRef
// CLEARS in one delta -- AND NOTHING ELSE IS.
//
// The positive is covered above. This is the other half, and it is the half
// that goes wrong quietly: a looser recognition would apply the
// candidate-moved rule to an ordinary publish, refusing it with a message
// about a promotion nobody attempted. Each delta below carries a STORED
// CANDIDATE that differs from anything it names, so a delta mistaken for a
// promotion is refused rather than passed -- which is what makes these
// assertions able to fail.
func TestOnlyThePromotionShapeIsAPromotion(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		// A plain publish: the serving version moves and the candidate is not
		// named, so it stays where it is.
		"bundleRef moves alone": {"id": "s", "bundleRef": "blob://sites/s/v/4/"},
		// Withdrawing a candidate: clearSiteCandidate's whole delta.
		"candidateRef clears alone": {"id": "s", "candidateRef": ""},
		// A re-publish of the version already serving, alongside a withdrawal.
		// bundleRef is PRESENT and CLEARED-candidate is present, but the
		// serving version does not move, so nothing was promoted.
		"bundleRef restated and candidateRef cleared": {
			"id": "s", "bundleRef": "blob://sites/s/v/3/", "candidateRef": "",
		},
		// Publishing and staging a new candidate in one write: both keys move,
		// but the candidate is SET rather than cleared.
		"bundleRef moves and candidateRef is set": {
			"id": "s", "bundleRef": "blob://sites/s/v/4/", "candidateRef": "blob://sites/s/v/5/",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runPreviewGuard(t, previewGuardDelta{
				priorExisted: true,
				priorStatus:  "live",
				// `spa`, so nothing here can reach the go-live rule and the
				// only thing that could refuse is the promotion rule firing on
				// a delta that is not one.
				priorKind:      "spa",
				priorBundleRef: "blob://sites/s/v/3/",
				priorCandidate: "blob://sites/s/v/9/",
				payload:        payload,
			})
			if err != nil {
				t.Fatalf("a delta that is not a promotion was judged as one: %v", err)
			}
		})
	}

	// THE REACHABLE POSITIVE, in the same test: the shape that IS a promotion,
	// against the same stored candidate, is refused for naming the wrong one.
	// Without it every assertion above would pass against a guard that had
	// stopped recognising promotions altogether.
	err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "live",
		priorKind:      "spa",
		priorBundleRef: "blob://sites/s/v/3/",
		priorCandidate: "blob://sites/s/v/9/",
		payload:        map[string]any{"id": "s", "bundleRef": "blob://sites/s/v/4/", "candidateRef": ""},
	})
	if err == nil || !strings.Contains(err.Error(), PreviewRefusalCandidateMoved) {
		t.Fatalf("the promotion shape is no longer recognised, so the four cases above prove "+
			"nothing: %v", err)
	}
}

// THE GUARD JUDGES THE BINDING THE WRITE LEAVES BEHIND, NOT THE STORED ONE --
// and the failure that reading the stored value would cause is the sharp one:
// it would refuse EXACTLY THE WRITE THAT FIXES THE PROBLEM.
//
// A storefront bound to a development store cannot go live. The act that
// clears that refusal is "bind it to the store shoppers reach, then go live",
// and an operator doing both in one delta -- which is what a single form
// submission produces -- would be judged against the development store it is
// in the middle of replacing. The refusal would name the store the write is
// removing, and there would be no order of operations that got past it.
func TestGoingLiveIsJudgedAgainstTheBindingTheWriteLeavesBehind(t *testing.T) {
	// Re-binding to a NAMED store and going live in one delta: the guard must
	// ask about the store the write NAMES. Against a zero-value engine the
	// read itself is the observable, and the store it names is the assertion.
	storeId, reached := storeReadRefusal(runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "draft",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/1/",
		priorBindingID: "acme-dev",
		payload: map[string]any{
			"id":      "s",
			"status":  "live",
			"binding": map[string]any{"storeId": "acme-live"},
		},
	}))
	if !reached {
		t.Fatal("a storefront going live was not judged against any store at all")
	}
	if storeId == "acme-dev" {
		t.Fatal("the guard judged the STORED development store rather than the one the write " +
			"binds -- so re-binding to the live store and going live in one delta is refused by " +
			"a message naming the store it is removing, and no order of operations gets past it")
	}
	if storeId != "acme-live" {
		t.Errorf("the guard asked about store %q, want acme-live -- the binding the write leaves behind",
			storeId)
	}

	// AND THE WRITE THAT LEAVES NO BINDING IS JUDGED ON THAT, which is the
	// same property with the answer available locally: detaching the
	// development store and going live in one delta leaves the storefront
	// connected to nothing, so it is refused as not connected (Connect
	// Shopify, D5) -- and the zero-value engine is never asked, because an
	// empty binding needs no store read. A guard reading the stored value
	// would have tried to read `acme-dev` instead.
	detach := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "draft",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/1/",
		priorBindingID: "acme-dev",
		payload: map[string]any{
			"id":      "s",
			"status":  "live",
			"binding": map[string]any{"storeId": ""},
		},
	})
	if storeId, reached := storeReadRefusal(detach); reached {
		t.Fatalf("detaching the store and going live reached for store %q -- the guard is judging "+
			"the stored binding rather than the one the write leaves behind", storeId)
	}
	if detach != nil {
		t.Fatalf("detached design review refused: %v", detach)
	}

	// THE REACHABLE NEGATIVE: with no binding key in the delta, the STORED one
	// is what is judged -- the guard falls back to it rather than reading
	// nothing, which is what makes a plain `status: live` write on an
	// already-bound storefront a judged act at all.
	storeId, reached = storeReadRefusal(runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "draft",
		priorKind:      storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/1/",
		priorBindingID: "acme-dev",
		payload:        map[string]any{"id": "s", "status": "live"},
	}))
	if !reached || storeId != "acme-dev" {
		t.Errorf("a go-live naming no binding asked about store %q (reached=%v), want the stored "+
			"acme-dev -- without the fallback, going live is ungated for every storefront that "+
			"does not re-bind in the same write", storeId, reached)
	}
}

// A NON-STOREFRONT DEPLOYABLE READS NO STORE ON THE WAY LIVE, and this is the
// case that proves the short-circuit is about the KIND rather than about the
// fields. An `spa` going live with a store id somehow on its binding -- by
// accident, or by somebody probing -- must reach for nothing.
func TestANonStorefrontGoingLiveReadsNoStore(t *testing.T) {
	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted:   true,
		priorStatus:    "draft",
		priorKind:      "spa",
		priorBundleRef: "blob://sites/s/v/1/",
		priorBindingID: "acme-dev",
		payload:        map[string]any{"id": "s", "status": "live"},
	}); err != nil {
		t.Fatalf("an spa going live read a store: %v", err)
	}
}

// AN UNATTACHED STOREFRONT MAY NOT GO LIVE, and it is refused WITHOUT a store
// read: there is nothing bound to read (Connect Shopify, D5). This is the
// draft a first deploy leaves when the manifest's store could not be attached.
// A creation directly at live with no binding is the same act, and so is a
// promotion on an unattached storefront. The spa beside them is the negative
// control: the rule is about storefronts.
func TestAnUnattachedStorefrontMayGoLiveForDesignReview(t *testing.T) {
	for name, d := range map[string]previewGuardDelta{
		"a draft going live": {
			priorExisted: true, priorStatus: "draft", priorKind: storefrontSiteKind,
			priorBundleRef: "blob://sites/s/v/1/",
			payload:        map[string]any{"id": "s", "status": "live"},
		},
		"a creation directly at live": {
			payload: map[string]any{"id": "s", "kind": storefrontSiteKind, "status": "live"},
		},
		"a promotion": {
			priorExisted: true, priorStatus: "live", priorKind: storefrontSiteKind,
			priorBundleRef: "blob://sites/s/v/1/", priorCandidate: "blob://sites/s/v/2/",
			payload: map[string]any{"id": "s", "bundleRef": "blob://sites/s/v/2/", "candidateRef": ""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runPreviewGuard(t, d)
			if storeId, reached := storeReadRefusal(err); reached {
				t.Fatalf("an unattached storefront reached for store %q", storeId)
			}
			if err != nil {
				t.Fatalf("unattached design review refused: %v", err)
			}
		})
	}
	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted: true, priorStatus: "draft", priorKind: "spa",
		priorBundleRef: "blob://sites/s/v/1/",
		payload:        map[string]any{"id": "s", "status": "live"},
	}); err != nil {
		t.Fatalf("an spa with no store was refused going live: %v", err)
	}
}

// TURNING A LIVE SITE INTO A STOREFRONT IS GOING LIVE AS ONE, and it is the
// transition a status-only reading misses: the status does not change, so a
// write that re-runs createSite over a live spa, or a raw insert naming
// `kind: shopify_storefront`, would otherwise leave a live storefront with no
// store without ever being judged. The live storefront re-written as itself is
// the negative control: that is an ordinary write, not a transition.
func TestTurningALiveSiteIntoAStorefrontIsGoingLive(t *testing.T) {
	flip := func(binding string) previewGuardDelta {
		payload := map[string]any{"id": "s", "kind": storefrontSiteKind, "status": "live"}
		if binding != "" {
			payload["binding"] = map[string]any{"storeId": binding}
		}
		return previewGuardDelta{
			priorExisted: true, priorStatus: "live", priorKind: "spa",
			priorBundleRef: "blob://sites/s/v/1/", payload: payload,
		}
	}

	err := runPreviewGuard(t, flip(""))
	if err != nil {
		t.Fatalf("unattached design review refused: %v", err)
	}
	if storeId, reached := storeReadRefusal(runPreviewGuard(t, flip("acme-dev"))); !reached || storeId != "acme-dev" {
		t.Fatalf("a live spa turned into a storefront bound to acme-dev was not judged against it (reached=%v, store=%q)", reached, storeId)
	}

	if err := runPreviewGuard(t, previewGuardDelta{
		priorExisted: true, priorStatus: "live", priorKind: storefrontSiteKind,
		priorBundleRef: "blob://sites/s/v/1/", priorBindingID: "acme",
		payload: map[string]any{"id": "s", "kind": storefrontSiteKind, "status": "live"},
	}); err != nil {
		t.Fatalf("a live storefront re-written as a storefront was judged: %v", err)
	}
}

// The first built storefront can be tested before another build exists. Its
// sandbox is resolved on preview open; this write changes no serving binding.
func TestAStorefrontCanTestTheSameBuildAgainstItsSandbox(t *testing.T) {
	for _, status := range []string{"draft", "live"} {
		t.Run(status, func(t *testing.T) {
			err := runPreviewGuard(t, previewGuardDelta{
				priorExisted: true, priorStatus: status, priorKind: storefrontSiteKind,
				priorBundleRef: "blob://sites/s/v1/", priorBindingID: "production",
				payload: map[string]any{"id": "s", "candidateRef": "blob://sites/s/v1/"},
			})
			if err != nil {
				t.Fatalf("same-build storefront test was refused: %v", err)
			}
		})
	}
}
