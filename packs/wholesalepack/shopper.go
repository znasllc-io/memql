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
// # ONE ROUTE, AND THE ASYMMETRY WITH reviewspack IS THE POINT
//
// reviewspack declares a form AND a read, because a storefront renders
// reviews. This pack declares a FORM AND NO READ. An application is a named
// person's business contact details against a named company; there is no
// version of "render the applications" that belongs on a public page, and
// an applicant reading their OWN back would need the verified Shopify
// customer principal that issue memql#5550 recorded as the answer for a
// buyer's own data and deliberately did not build.
//
// Nothing here forecloses it. Reach is declared per ROUTE, so a second kind
// of caller on a second route is an addition rather than a rewrite.
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

// registerShopperSurface declares the one route. Called from Register, so
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
		// THE MINIMUM AN APPLICATION NEEDS TO BE ONE, and nothing else
		// (design record, section 7). The first client collects an EIN and
		// the next will not, so the EIN is a RELATED CONCEPT in that
		// client's own domain with an @relationship to the application --
		// never a field here and never a metadata blob. A pack that grew a
		// field per client would stop being a pack at the second one.
		Fields: []memql.ShopperField{
			{
				Name: "companyName", Required: true, MaxLength: 200,
				Description: "The business applying.",
			},
			{
				Name: "applicantName", Required: true, MaxLength: 120,
				Description: "The person applying. Data, never an identity.",
			},
			{
				Name: "applicantEmail", Required: true, MaxLength: 320,
				Description: "How a decision reaches them, and how an entitlement adapter " +
					"finds the Shopify customer to entitle. Unverified.",
			},
		},
		// SITE-RELATIVE, and the merchant owns both pages. A refusal renders
		// in their design and their language with ?reason= naming what
		// happened, rather than in ours.
		RedirectOK:    "/wholesale/thank-you",
		RedirectError: "/wholesale/problem",
	})
}
