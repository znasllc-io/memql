package wholesalepack

import "github.com/znasllc-io/memql/component/memql"

// shopper.go -- THE PACK'S DECLARED SHOPPER SURFACE (epic memql#5533,
// issues memql#5550, memql#5551 and memql#5555).
//
// # This file is the entire reach a member of the public has into wholesale
//
// A shopper is nobody to MemQL: no user row, no session, no bearer, no
// actor. What they can do is exactly what is declared here and nothing
// else, on a deployable whose shopperForms is on, under the site owner's
// borrowed authority, scoped to the store that deployable is bound to.
//
// Everything else the pack ships -- the decision path, the provisioning
// builtin, the settings write, the merchant's own lists -- is untouched by
// this and stays exactly as unreachable as it was.
//
// # NO PUBLIC READ, AND THE ASYMMETRY WITH reviewspack IS THE POINT
//
// reviewspack declares a public read, because a storefront renders reviews.
// This pack declares none. An application is a named person's business
// contact details against a named company; there is no version of "render the
// applications" that belongs on a public page, and an applicant reading their
// OWN back would need the verified Shopify customer principal that issue
// memql#5550 recorded as the answer for a buyer's own data and deliberately
// did not build.
//
// # THE PRIVATE LINKS (U8)
//
// What it does declare beside the application form is keyed by a SECRET: the
// token of a private link the store sent one applicant (links.go). The terms
// read and the price-sheet read are Private -- never cached, no Referer -- and
// answer only the documents, never the applicant's email, an id or a hash; the
// acceptance form asks for Evidence, so the address the edge saw and the
// browser's User-Agent reach its construct on the context. Reach is declared
// per ROUTE, so these are additions rather than a rewrite.
//
// # THE WRITE IS A BUILTIN, NOT A MUTATION, AND THAT IS THE GATE
//
// reviewspack's form names a mutation because a review has no precondition.
// An application has one: v1:wholesale:wholesaleSettings.applicationsOpen.
// A mutation body cannot read another concept's row, so a mutation here
// would leave "applications are closed" as something the storefront merely
// declines to RENDER -- and a plain POST to the endpoint would sail past
// it, which is not a closed door. ShopperForm.Kind exists for exactly this
// case: "a pack whose write needs Go -- deriving a row id, or writing more
// than one row from one submission."

// registerShopperSurface declares the routes. Called from Register, so
// a pack this cluster has disabled declares nothing: its Register never
// runs the behavioural half, and a route nobody declared is a 404.
func registerShopperSurface() {
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack:      Domain,
		Name:      "application",
		Construct: "wholesaleSubmitApplication",
		Kind:      memql.ShopperReadKindBuiltin,
		Description: "A business applies for trade terms on this store. Plain HTML form " +
			"post, no JavaScript; answers 303. Refused when this store's " +
			"applicationsOpen is false.",
		Fields: applicationFields(),
		// The application records the organization of the site it arrived on,
		// in its own accountId, so the construct asks the handler for it.
		AcceptsAccount: true,
		// SITE-RELATIVE, and the merchant owns both pages. A refusal renders
		// in their design and their language with ?reason= naming what
		// happened, rather than in ours.
		RedirectOK:    "/wholesale/thank-you",
		RedirectError: "/wholesale/problem",
	})
	memql.RegisterShopperForm(acceptanceForm())
	memql.RegisterShopperRead(termsRead())
	memql.RegisterShopperRead(priceSheetRead())
}

// applicationFields is the application form's three fields, and the rules an
// application's contact is held to wherever it arrives from: the shopper's form
// post, and a person of the store adding a buyer (applicationContactOf).
//
// THE MINIMUM AN APPLICATION NEEDS TO BE ONE, and nothing else (design record,
// section 7). The first client collects an EIN and the next will not, so the
// EIN is a RELATED CONCEPT in that client's own domain with an @relationship to
// the application -- never a field here and never a metadata blob. A pack that
// grew a field per client would stop being a pack at the second one.
func applicationFields() []memql.ShopperField {
	return []memql.ShopperField{
		{
			Name: "companyName", Required: true, MaxLength: 200,
			Description: "The business applying.",
		},
		{
			Name: "applicantName", Required: true, MaxLength: 120,
			Description: "The person applying. Data, never an identity.",
		},
		{
			Name: "applicantEmail", Required: true, MaxLength: 320, Format: memql.ShopperFormatEmail,
			Description: "How a decision reaches them, and how an entitlement adapter " +
				"finds the Shopify customer to entitle. Shaped like an address; otherwise unverified.",
		},
	}
}

// tokenField is a private link's token, from its t parameter.
func tokenField() memql.ShopperField {
	return memql.ShopperField{Name: "requestToken", Required: true, MaxLength: 200,
		Description: "The link's token. Required: the surface refuses a request without one as invalid, " +
			"and a token that is no link of this store is link_invalid."}
}

// acceptanceForm is the form an applicant accepts the terms with, on their
// terms link.
func acceptanceForm() memql.ShopperForm {
	return memql.ShopperForm{
		Pack:      Domain,
		Name:      "acceptance",
		Construct: "wholesaleAcceptTerms",
		Kind:      memql.ShopperReadKindBuiltin,
		Description: "An applicant accepts the store's terms and MAP policy through their terms link. Plain " +
			"HTML form post; answers 303. Written once; a version that changed under the page is terms_changed.",
		Fields: []memql.ShopperField{
			tokenField(),
			// The field rules are the surface's (ruling 53): a box that is not on,
			// or a fingerprint that is not 64 hex characters, is the surface's own
			// invalid before the construct runs.
			{Name: "acceptTerms", Required: true, MaxLength: 8, Enum: []string{"on"}, Description: "The terms box: on."},
			{Name: "acceptMap", Required: true, MaxLength: 8, Enum: []string{"on"}, Description: "The MAP policy box: on."},
			{Name: "signedName", Required: true, MaxLength: 120, Description: "The name the applicant types to accept."},
			{Name: "termsSha256", Required: true, MaxLength: 64, Format: memql.ShopperFormatSHA256,
				Description: "The terms version's fingerprint, as the terms read answered it."},
			{Name: "mapSha256", Required: true, MaxLength: 64, Format: memql.ShopperFormatSHA256,
				Description: "The MAP policy version's fingerprint, as the terms read answered it."},
			{Name: "statementSha256", Required: true, MaxLength: 64, Format: memql.ShopperFormatSHA256,
				Description: "The acceptance sentence's fingerprint, as the terms read answered it."},
		},
		// THE ADDRESS AND THE BROWSER, on the context and never posted: what an
		// acceptance records as evidence of where the assent came from.
		Evidence:      true,
		RedirectOK:    "/wholesale/accepted",
		RedirectError: "/wholesale/link-problem",
		Refusals:      AcceptanceRefusals,
	}
}

// termsRead is the terms link's read: the documents an applicant accepts.
func termsRead() memql.ShopperRead {
	return memql.ShopperRead{
		Pack:      Domain,
		Name:      "terms",
		Construct: "wholesaleTermsForLink",
		Kind:      memql.ShopperReadKindBuiltin,
		Description: "The store's current terms and MAP policy for the applicant whose terms link this is, " +
			"or which of the link's refusals applies.",
		Fields: []memql.ShopperField{tokenField()},
		// PRIVATE: keyed by the token in one applicant's link.
		Private:  true,
		Refusals: LinkRefusals,
	}
}

// priceSheetRead is the price-sheet link's read.
func priceSheetRead() memql.ShopperRead {
	return memql.ShopperRead{
		Pack:        Domain,
		Name:        "priceSheet",
		Construct:   "wholesalePriceSheetForLink",
		Kind:        memql.ShopperReadKindBuiltin,
		Description: "The store's current price sheet for an approved buyer whose access is granted.",
		Fields:      []memql.ShopperField{tokenField()},
		Private:     true,
		Refusals:    LinkRefusals,
	}
}
