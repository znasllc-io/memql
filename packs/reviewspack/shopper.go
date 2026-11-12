package reviewspack

import "github.com/znasllc-io/memql/component/memql"

// shopper.go -- THE PACK'S DECLARED SHOPPER SURFACE (epic memql#5532,
// issues memql#5550 and memql#5553).
//
// # This file is the entire reach a member of the public has into reviews
//
// A shopper is nobody to MemQL: no user row, no session, no bearer, no
// actor. What they can do is exactly what is declared here and nothing
// else, on a deployable whose shopperForms is on, under the site owner's
// borrowed authority, scoped to the store that deployable is bound to.
//
// Everything else the pack ships -- the moderation and settings builtins,
// the export capability, the merchant's own lists -- is untouched by this and
// stays exactly as unreachable as it was.
//
// The form's construct is the pack's own Go, never a mutation: every reviews
// concept is @serverWritten, and the handler runs a construct under the site
// owner's borrowed authority WITHOUT internal origin, so a mutation a form
// named could write nothing. reviewsSubmitFromLink checks the link and then
// writes through writer.go, whose one inline stamp is the only way in.
//
// # Two routes, and the asymmetry between them is the point
//
// The FORM writes a review. It carries the fields a review needs to be one
// and nothing more: no id, no storeId, no ownerUserId -- the registry
// refuses those names outright, and the bff stamps them after the declared
// fields so a form-supplied copy could not win even if it did not.
//
// The READ lists what a storefront may render. It is the builtin rather
// than a query, because "only reviews no moderation decision hides, only
// while publicDisplay is true" is three reads with a gate between them
// (published.go says why at length).

// registerShopperSurface declares the two routes. Called from Register, so
// a pack this cluster has disabled declares nothing: its Register never
// runs the behavioural half, and a route nobody declared is a 404.
func registerShopperSurface() {
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack:      Domain,
		Name:      "review",
		Construct: "reviewsSubmitFromLink",
		// A BUILTIN, because a review now arrives through a buyer's link: the
		// construct must find the link by its token, judge it, and write the
		// first submission or the next revision of the same row -- reads a
		// mutation body cannot make.
		Kind: memql.ShopperReadKindBuiltin,
		Description: "A buyer submits, or edits, a review through the link their " +
			"review-request email carried. Plain HTML form post; answers 303.",
		Fields: []memql.ShopperField{
			{
				Name: "requestToken", Required: true, MaxLength: 200,
				Description: "The link's token, from its t parameter. Without one the review is refused as link_invalid.",
			},
			{
				Name: "productHandle", Required: true, MaxLength: 200,
				Description: "The product the link names.",
			},
			{
				Name: "body", Required: true, MaxLength: 4000,
				Description: "The review text.",
			},
			{
				Name: "title", MaxLength: 200,
				Description: "An optional short title.",
			},
			{
				// NUMERIC, so the builtin sees a number rather than the string
				// a form always sends.
				Name: "rating", Numeric: true, MaxLength: 2,
				Description: "An optional 1-5 star rating.",
			},
			{
				Name: "authorName", MaxLength: 120,
				Description: "What the shopper calls themselves. Data, never an identity.",
			},
			{
				Name: "authorEmail", MaxLength: 320,
				Description: "The shopper's email, unverified, so the merchant can reply. " +
					"Never projected by the public read.",
			},
		},
		// SITE-RELATIVE, and the merchant owns both pages. A refusal renders
		// in their design and their language with ?reason= naming what
		// happened, rather than in ours.
		RedirectOK:    "/reviews/thank-you",
		RedirectError: "/reviews/problem",
		// The link's own refusals, by name, so the merchant's page can say
		// "this link has expired" rather than "something went wrong".
		Refusals: LinkRefusals,
	})

	memql.RegisterShopperRead(memql.ShopperRead{
		Pack:      Domain,
		Name:      "draft",
		Construct: "reviewsDraftForLink",
		Kind:      memql.ShopperReadKindBuiltin,
		Description: "What the buyer wrote through their link, so reopening it pre-fills the form, " +
			"or which of the link's refusals applies, before anyone types.",
		Fields: []memql.ShopperField{
			{
				Name: "requestToken", Required: true, MaxLength: 200,
				Description: "The link's token.",
			},
			{
				Name: "productHandle", MaxLength: 200,
				Description: "The product the link names.",
			},
		},
		// PRIVATE: the answer is one buyer's own review, keyed by the token in
		// their link. Never cached, and the link never leaves in a Referer.
		Private:  true,
		Refusals: LinkRefusals,
	})

	memql.RegisterShopperRead(memql.ShopperRead{
		Pack:      Domain,
		Name:      "published",
		Construct: "reviewsPublishedForProduct",
		Kind:      memql.ShopperReadKindBuiltin,
		Description: "The reviews a storefront may render for one product: gated on this " +
			"store's publicDisplay and excluding every review a moderation decision hides.",
		Fields: []memql.ShopperField{
			{
				Name: "productHandle", Required: true, MaxLength: 200,
				Description: "The product whose reviews to return.",
			},
			{
				Name: "limit", Numeric: true, MaxLength: 3,
				Description: "How many to return, newest first. Bounded server-side.",
			},
		},
	})
}
