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
)

// PreviewBoundStore is what a caller could learn about the store a binding names.
//
// Empty bindings serve design preview. Readable bindings may select either
// store type. A nonempty unreadable binding is refused, because it cannot be
// validated under the caller's access.
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

// SiteGoLiveRefusal validates the Production binding. Store type does not
// determine the destination: an owner may use a sandbox on either URL. An
// unconnected storefront serves its design; an unreadable binding is refused.
func SiteGoLiveRefusal(storefront bool, serving PreviewBoundStore) PreviewRefusal {
	return SitePreviewBindingRefusal(storefront, serving)
}

// SitePreviewBindingRefusal validates the independent Testing binding. No
// binding means design preview. The store's own permissions still govern
// whether the caller may attach it; this rule never grants access to a row.
func SitePreviewBindingRefusal(storefront bool, store PreviewBoundStore) PreviewRefusal {
	if !storefront || strings.TrimSpace(store.ID) == "" || store.Readable {
		return PreviewRefusal{}
	}
	return PreviewRefusal{
		Code:    PreviewRefusalStoreUnreadable,
		Message: "The connected store " + store.name() + " cannot be read here.",
		Remedy:  "Connect a store you can read, or disconnect it to view the design.",
	}
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
