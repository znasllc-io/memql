package memql

import "strings"

// site_preview_rules.go -- the RULES of storefront preview as pure functions
// over values: no engine, no database, no rows (epic memql#5531).
//
// # Why they are functions here rather than checks inside the guard
//
// Two places have to agree about when an act is refused, and the agreement has
// to be exact. The GUARD is platform_site_preview_guard.go beside this file,
// wired into executeWrite, and it is what actually refuses a write however it
// arrives -- a mutation, a tool handler, a raw insert. The READINESS ANSWER is
// integrations/sitepreview's `readiness` capability, and it is what lets the OS
// make an act that is not legal ABSENT rather than drawing it and having it
// fail (issue memql#5546 asks for exactly that, and it needs the refusal to be
// knowable before the click).
//
// Stated twice they would drift, and the drift would be the worst possible
// shape: a control the OS offers because its copy of the rule says yes, refused
// by a guard whose copy says no. So the rule is one function and both call it.
//
// # They live in this package rather than in a leaf of their own
//
// component/work is the precedent for a pure-decisions package and would have
// been the tidier home -- but the ROOT module requires component/memql, so a
// package in the root that component/memql imported back would be a module
// CYCLE, which is what CI's module-boundaries lane refuses. Everything that
// needs these rules already imports this package: the guard is in it, and
// integrations/sitepreview, integrations/shopify and component/edge all depend
// on it already. A new nested module to hold three hundred lines would have
// bought a build-graph problem and no separation that matters.
//
// # Nothing here reads a row
//
// Every input is a value somebody else resolved, which is what makes the rules
// falsifiable on a table of cases rather than against a mock.

// PreviewRefusal is a typed reason an act is not available, and the ACT THAT CLEARS IT.
//
// THE REMEDY IS PART OF THE ANSWER, not a nicety. A refusal that names only what
// is wrong leaves an operator on a screen with a missing button and nothing to
// do; issue memql#5546 asks that the refusal name the store and the act, and
// that is what Remedy carries.
type PreviewRefusal struct {
	// Code is the stable machine name. The OS keys its copy on it.
	Code string
	// Message is one sentence, naming the thing that is wrong.
	Message string
	// Remedy is the act that clears it, in the words of the surface that
	// offers that act.
	Remedy string
}

// Empty reports the absence of a refusal -- the act is available.
func (r PreviewRefusal) Empty() bool { return r.Code == "" }

// The refusal vocabulary. CLOSED, and each value is a distinct situation an
// operator can act on; two situations that need the same act would be one code.
const (
	// PreviewRefusalServingBindingIsDevelopment -- the deployable is bound to a
	// development store and the act would put it in front of shoppers.
	PreviewRefusalServingBindingIsDevelopment = "serving_binding_is_development_store"
	// PreviewRefusalBindingIsNotDevelopment -- the preview binding names the
	// store shoppers reach, so exercising it would take real orders.
	PreviewRefusalBindingIsNotDevelopment = "preview_binding_is_not_development_store"
	// PreviewRefusalStoreUnreadable -- the store a binding names cannot be read by
	// whoever is asking, so neither question above can be answered.
	PreviewRefusalStoreUnreadable = "bound_store_unreadable"
	// PreviewRefusalNoCandidate -- there is no candidate version to preview or promote.
	PreviewRefusalNoCandidate = "no_candidate_version"
	// PreviewRefusalCandidateIsServing -- the candidate names the version already
	// serving, so there is nothing to exercise and nothing to promote.
	PreviewRefusalCandidateIsServing = "candidate_is_serving_version"
	// PreviewRefusalCandidateMoved -- the candidate stored is not the one the caller
	// named, so a promotion would promote a version nobody read.
	PreviewRefusalCandidateMoved = "candidate_moved"
	// PreviewRefusalSystemOwned -- the platform's own site is exempt from this whole
	// axis, as it is from the status axis and the settings axis.
	PreviewRefusalSystemOwned = "site_is_system_owned"
	// PreviewRefusalNoPreviewBinding -- a storefront with no development store
	// attached has nothing to exercise against.
	PreviewRefusalNoPreviewBinding = "no_preview_binding"
)

// PreviewBoundStore is what a caller could learn about the store a binding names.
//
// THREE STATES, NOT TWO, and the third is the one that matters. `Readable`
// false does NOT mean "not a development store": it means nobody could answer,
// and an unanswerable question about whether a storefront is pointed at real
// money must refuse rather than assume. Reading an unreadable store as "not a
// development store" would let a go-live through precisely when the cluster had
// lost track of what the site is bound to.
type PreviewBoundStore struct {
	// ID is the store the binding names, or empty for an unbound deployable.
	ID string
	// Readable is whether the store row came back for whoever asked.
	Readable bool
	// IsDevelopment is v1:shopify:store.isDevelopment, meaningful only when
	// Readable.
	IsDevelopment bool
	// Domain is the myshopify.com host, for naming the store in a refusal.
	// Empty when the store was not readable, in which case the refusal names
	// the id instead -- an honest "this one, which you cannot see".
	Domain string
	// HasStorefrontToken is whether the store row names a Storefront token
	// (storefrontTokenRef non-empty), meaningful only when Readable. A store
	// without one is not connected: the edge would serve an empty token and
	// the storefront's catalog would not load.
	HasStorefrontToken bool
}

// name is how a store is referred to in a refusal: its domain when the reader
// could see it, its id when they could not.
func (s PreviewBoundStore) name() string {
	if d := strings.TrimSpace(s.Domain); d != "" {
		return d
	}
	if id := strings.TrimSpace(s.ID); id != "" {
		return id
	}
	return "the bound store"
}

// SiteGoLiveRefusal answers whether a deployable may be taken live, or a candidate
// promoted, given the store its SERVING binding names (issue memql#5546).
//
// ONE FUNCTION FOR BOTH ACTS, because they are one question asked twice: both
// are "may this version be put in front of shoppers", and a storefront bound to
// a development store has no shoppers to put it in front of. It would serve a
// catalog nobody can buy from and take orders into a store that is not the
// merchant's -- and it would look like a successful launch while doing it,
// which is why the refusal is typed rather than left to a person noticing.
//
// A NON-STOREFRONT DEPLOYABLE IS NEVER REFUSED. An `spa` or a `static` site has
// no binding and no store, so there is no question to ask; `storefront` false
// returns no refusal without looking at anything.
//
// A STOREFRONT THAT IS NOT CONNECTED IS REFUSED (Connect Shopify, D5), and that
// reverses what this rule used to allow. A storefront's first deploy now lands
// as a draft with no store whenever the manifest's store cannot be attached
// (component/packages, the publish stage), so "unbound" is no longer the
// harmless state before anybody wires one up: it is a storefront whose deploy
// succeeded and which has no catalog behind it. Taking it live would put an
// empty shop in front of shoppers and call that a launch. The same is true of
// a store row with no Storefront token -- the edge would serve an empty token
// and the catalog would not load. Both are one situation with one act that
// clears it, so they are one code.
//
// THE TOKEN IS JUDGED LAST. An unreadable store and a development store are
// refusals about WHICH store is bound, and connecting a token clears neither,
// so they are reported first.
func SiteGoLiveRefusal(storefront bool, serving PreviewBoundStore) PreviewRefusal {
	if !storefront {
		return PreviewRefusal{}
	}
	// An unattached storefront can serve its design and navigation. Commerce
	// readiness is shown separately on its Store panel.
	if strings.TrimSpace(serving.ID) == "" {
		return PreviewRefusal{}
	}
	if !serving.Readable {
		return PreviewRefusal{
			Code: PreviewRefusalStoreUnreadable,
			Message: "this storefront is bound to store " + serving.name() +
				", which cannot be read here -- so whether it is a development store cannot be answered, and a storefront may not go in front of shoppers on an unanswered question.",
			Remedy: "Ask an operator who can read the store to check the binding, or re-bind this storefront to a store you can read.",
		}
	}
	if serving.IsDevelopment {
		return PreviewRefusal{
			Code: PreviewRefusalServingBindingIsDevelopment,
			Message: "this storefront is bound to " + serving.name() +
				", which is a development store -- serving it to shoppers would show a catalog nobody can buy from and take orders into a store that is not the merchant's.",
			Remedy: "Bind the storefront to the store shoppers reach, then try again. The development store stays on the preview binding.",
		}
	}
	return PreviewRefusal{}
}

// SitePreviewBindingRefusal answers whether a candidate may be exercised against the store
// the PREVIEW binding names -- the other direction of the same guard.
//
// THE FAILURE IT PREVENTS IS SILENT, which is why it exists at all. A preview
// pointed at the live store looks exactly like a preview: the catalog loads,
// the cart works, the checkout opens. It stops looking like a preview at the
// moment a test payment lands in the merchant's real orders, and by then it has
// happened.
//
// An unbound preview binding is refused even though the design may go live.
// Commerce checks need a development store: the act that clears this is
// attaching a development store, not connecting the shop. Exercising an
// unbound preview would fall back to no store at all and report four
// observations of nothing.
func SitePreviewBindingRefusal(storefront bool, preview PreviewBoundStore) PreviewRefusal {
	if !storefront {
		return PreviewRefusal{}
	}
	if strings.TrimSpace(preview.ID) == "" {
		return PreviewRefusal{
			Code:    PreviewRefusalNoPreviewBinding,
			Message: "this storefront has no development store on its preview binding, so there is nothing to exercise a candidate against.",
			Remedy:  "Attach a development store to the preview binding first.",
		}
	}
	if !preview.Readable {
		return PreviewRefusal{
			Code: PreviewRefusalStoreUnreadable,
			Message: "the preview binding names store " + preview.name() +
				", which cannot be read here -- so whether it is a development store cannot be answered.",
			Remedy: "Ask an operator who can read the store to check the preview binding, or point it at a store you can read.",
		}
	}
	if !preview.IsDevelopment {
		return PreviewRefusal{
			Code: PreviewRefusalBindingIsNotDevelopment,
			Message: "the preview binding names " + preview.name() +
				", which is the store shoppers reach -- exercising a candidate against it would put test carts and test payments in the merchant's real store.",
			Remedy: "Point the preview binding at a development store. Shopify marks one on the store row as isDevelopment.",
		}
	}
	return PreviewRefusal{}
}

// SiteCandidateRefusal answers whether `candidate` may be set as the candidate version of
// a deployable currently serving `serving`.
//
// A CANDIDATE THAT IS SERVING IS NOT A CANDIDATE. There would be nothing to
// exercise -- the preview would show exactly what the public already sees --
// and promoting it would be a write that changes nothing while reading like a
// release. Refusing it is cheaper than the half-hour somebody spends wondering
// why their change is not showing.
func SiteCandidateRefusal(serving, candidate string) PreviewRefusal {
	serving = strings.TrimSpace(serving)
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || serving == "" || serving != candidate {
		return PreviewRefusal{}
	}
	return PreviewRefusal{
		Code:    PreviewRefusalCandidateIsServing,
		Message: "that version is the one already serving, so there would be nothing to exercise and nothing to promote.",
		Remedy:  "Publish a new version and set it as the candidate.",
	}
}

// SitePromotionRefusal answers whether a promotion naming `named` may proceed against a
// deployable whose stored candidate is `stored`.
//
// THE CALLER NAMES WHAT IT IS PROMOTING, and this is the check that makes the
// naming worth anything. A mutation body cannot read the row's own stored field,
// so the candidate arrives as an argument; an argument nobody checked would let
// a candidate republished between the reading and the click be promoted by
// surprise. The work spine's approvals carry an artifact hash for exactly this
// reason -- a decision about one specific artifact must not carry to a
// different one.
func SitePromotionRefusal(stored, named string) PreviewRefusal {
	stored = strings.TrimSpace(stored)
	named = strings.TrimSpace(named)
	if stored == "" {
		return PreviewRefusal{
			Code:    PreviewRefusalNoCandidate,
			Message: "this deployable has no candidate version, so there is nothing to promote.",
			Remedy:  "Publish a version and set it as the candidate first.",
		}
	}
	if named != "" && named != stored {
		return PreviewRefusal{
			Code: PreviewRefusalCandidateMoved,
			Message: "the candidate version changed after this page read it: it is now " + stored +
				", and the promotion named " + named + ". Promoting would put a version nobody looked at in front of shoppers.",
			Remedy: "Reload the deployable, look at the candidate that is there now, and promote that.",
		}
	}
	return PreviewRefusal{}
}
