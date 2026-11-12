// Package wholesalepack is the client-agnostic wholesale pack (epic
// memql#5533).
//
// It is a PACK, not core: dsl/commerce stays an engine domain and this
// package cannot shadow it. It links into every binary with no build tag,
// exactly as packs/reviewspack does, and its reach is governed by
// v1:platform:packState (epic memql#5532, issue memql#5549).
package wholesalepack

import (
	"io/fs"
	"log/slog"
	"time"

	"embed"

	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns.
const Domain = "wholesale"

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

const integrationName = "wholesale"

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it (epic memql#5532, issue memql#5549).
//
// FALSE, for the same reason reviewspack ships disabled and one more of
// its own: enabling this pack publishes a write endpoint on every
// deployable whose shopperForms is on, and what that endpoint collects is
// a named person's business contact details. That is an operator's
// decision, not an upgrade's.
const DefaultEnabled = false

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("wholesalepack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider.
//
// THREE SEAMS AND NOT AN ENGINE HANDLE PASSED AROUND, and the three are
// deliberately distinct because the CALL FORMS are: a query is `query
// name(args)`, a mutation is `name(args)`, a builtin is `builtin
// name(args)`. Only the engine can tell them apart, so keeping them in one
// seam would make the one difference a fake cannot model into the one
// difference nothing checks.
//
// reader decides, writer persists, caller reaches an integration. `engine`
// is kept because the seams are built from it and a future capability may
// need it directly.
type Provider struct {
	engine memql.IntegrationEngineAccess
	reader rowReader
	writer rowWriter
	caller Caller
	// extensions reads a client's own fields on an application (the engine's
	// ShopperExtensionRow); nil on an engine handle that has none.
	extensions extensionReader
	// hasExtension reports whether a client extends the application form; nil
	// asks the engine's extension registry. A test seam.
	hasExtension func() bool
	// now is the engine's clock (an export's date, a link's issue and expiry, an
	// acceptance's time); nil is the wall clock.
	now func() time.Time
	// logger takes the operator's line about a link that could not be served;
	// nil is the default logger.
	logger *slog.Logger
	// backfillWindow is how many applications the backfill takes from each
	// page of its walk; zero is the query's own window. A test seam, so a
	// walk over more than one window needs no more than a handful of rows.
	backfillWindow int
	// lane is the transactional row lane the applicant's emails go through.
	lane rowLane
	// osBase is the OS's address a staff notice links to; nil reads it from the
	// cluster's domain. A test seam.
	osBase func() string
}

func (p *Provider) IntegrationName() string { return integrationName }

func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "submitApplication",
			Description: "The shopper write path: a business applies for trade terms. Refused " +
				"when this store's applicationsOpen is false, absent settings are closed, and only a " +
				"site the store admits, carrying its organization, from its owner, is taken.",
			Handler: p.submitApplication,
			ArgsSchema: map[string]string{
				"storeId":        "string (required) - stamped from the binding the post arrived through",
				"siteId":         "string - stamped from the site the post arrived on; the store must admit it",
				"accountId":      "string - stamped from the site's organization; must be the site's, absent included",
				"companyName":    "string (required) - the business applying",
				"applicantName":  "string (required) - the person applying",
				"applicantEmail": "string (required) - unverified; how a decision reaches them",
			},
		},
		{
			Name: "recordDecision",
			Description: "Append one decision to an application's log. A person on the store's side " +
				"decides -- the cluster's operators are refused -- and is recorded as the decider; " +
				"refuses an illegal transition by name, and an approve while another application of " +
				"the same address on the store is approved; copies storeId off the application.",
			Handler: p.recordDecision,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application being decided",
				"transition":    "string (required) - approve, reject or revoke",
				"principalKind": "string (optional) - ignored",
				"decidedBy":     "string (optional) - ignored; the decider is the caller",
				"note":          "string (optional) - the reason, which survives the next decision",
			},
		},
		{
			Name: "retryAccess",
			Description: "Try a buyer's access again: provision an approved application again, or " +
				"revoke again a revoked one whose access is still on. Anything else is refused by name.",
			Handler: p.retryAccess,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application whose access to retry",
			},
		},
		{
			Name: "addBuyer",
			Description: "A person of the store adds a buyer by hand: a submitted application on the buyer's " +
				"behalf, which then goes through the same steps as an applicant's. Refuses the cluster's " +
				"operators and an address with a submitted or approved application on the store.",
			Handler: p.addBuyer,
			ArgsSchema: map[string]string{
				"siteId":         "string (required) - the store's site; it must serve the store",
				"storeId":        "string (required) - the store the buyer is added to",
				"companyName":    "string (required) - the business",
				"applicantName":  "string (required) - the person",
				"applicantEmail": "string (required) - how the terms and the decision reach them",
			},
		},
		{
			Name: "removeBuyer",
			Description: "A person of the store removes an approved buyer: a revoke decision recorded by the " +
				"caller. Refuses the cluster's operators and an application that is not approved.",
			Handler: p.removeBuyer,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the approved buyer's application",
				"note":          "string (optional) - the reason, kept on the decision",
			},
		},
		{
			Name: "storeSettings",
			Description: "One store's wholesale settings, the routes this engine has, and whether the " +
				"caller may set the store up or update it.",
			Handler: p.storeSettings,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose settings to read",
			},
		},
		{
			Name: "updateSettings",
			Description: "Merge a change into a store's wholesale settings once the operators have " +
				"set it up; a store with none is refused.",
			Handler: p.updateSettings,
			ArgsSchema: map[string]string{
				"siteId":             "string (required) - the store's site; it must serve the store",
				"storeId":            "string (required) - the store whose settings change",
				"applicationsOpen":   "bool (optional) - whether the shopper form accepts posts",
				"entitlementAdapter": "string (optional) - the route; empty or none means none",
				"introText":          "string (optional) - client-authored storefront copy",
				"staffEmail":         "string (optional) - the store's people's address, or empty",
				"termsRequired":      "bool (optional) - whether an applicant must accept the published terms",
				"linkLifetimeDays":   "int (optional) - how many days a link to an applicant stays usable, 1 to 365",
				"acceptanceText":     "string (optional) - the sentence an applicant accepts with; empty for the default",
				"senderIdentityId":   "string (optional) - the sender the applicant's emails go out from; the cluster's operators alone",
				"termsTemplateId":    "string (optional) - the terms email's template, one the settings list, or empty",
				"approvedTemplateId": "string (optional) - the approval email's template, or empty",
				"rejectedTemplateId": "string (optional) - the rejection email's template, or empty",
				"revokedTemplateId":  "string (optional) - the removal email's template, or empty",
			},
		},
		{
			Name: "publishDocument",
			Description: "Publish a new version of a store's terms, MAP policy or price sheet. Append-only: " +
				"a version is never changed, and the newest of a kind is its current one.",
			Handler: p.publishDocument,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose document this is",
				"kind":    "string (required) - terms, map or priceSheet",
				"label":   "string (required) - the version's name, 1 to 120 characters once trimmed",
				"body":    "string (required) - the Markdown text, 1 to 204800 bytes, stored as given",
			},
		},
		{
			Name:        "documents",
			Description: "A store's documents with their current versions and histories, or one version by id.",
			Handler:     p.documents,
			ArgsSchema: map[string]string{
				"siteId":    "string (required) - the store's site; it must serve the store",
				"storeId":   "string (required) - the store whose documents to read",
				"versionId": "string (optional) - one version of this store to answer, body included",
			},
		},
		{
			Name: "applicationState",
			Description: "The derived state of one application, folded from its append-only " +
				"decision log, and what is legal next.",
			Handler: p.applicationState,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application to fold",
			},
		},
		{
			Name: "list",
			Description: "One store's applications as the caller reads them, with their stages, every stage's " +
				"count and the two facts a client's own fields show first.",
			Handler: p.list,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose applications to list",
				"stage":   "string (optional) - waitingForTerms, readyToReview (default), approved, rejected, revoked or all",
				"search":  "string (optional) - a part of the company, applicant or email",
				"limit":   "int (optional) - rows per page, 50 by default, at most 200",
				"after":   "string (optional) - the previous page's next",
			},
		},
		{
			Name: "get",
			Description: "One application with its state, the client's own fields, its decisions, its " +
				"entitlement and the acts open to the caller.",
			Handler: p.get,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application to open, bare or canonical",
			},
		},
		{
			Name:        "decisionLog",
			Description: "One store's decisions, newest first, on the applications the caller reads.",
			Handler:     p.decisionLog,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose decisions to list",
				"search":  "string (optional) - a part of the company, applicant, email, decider or note",
				"limit":   "int (optional) - rows per page, 50 by default, at most 200",
				"after":   "string (optional) - the previous page's next",
			},
		},
		{
			Name:        "buyers",
			Description: "One store's approved applications with their access.",
			Handler:     p.buyers,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose buyers to list",
				"search":  "string (optional) - a part of the company, applicant or email",
				"limit":   "int (optional) - rows per page, 50 by default, at most 200",
				"after":   "string (optional) - the previous page's next",
			},
		},
		{
			Name:        "exportStore",
			Description: "One store's wholesale record as one JSON file, with no token hash.",
			Handler:     p.exportStore,
			ArgsSchema: map[string]string{
				"siteId":  "string (required) - the store's site; it must serve the store",
				"storeId": "string (required) - the store whose record to export",
			},
		},
		{
			Name: "termsForLink",
			Description: "The terms link's read: the store's current terms and MAP policy for the applicant whose " +
				"token this is. The token is the authority; refused by name as link_invalid, link_used, link_closed, " +
				"link_expired or documents_unavailable.",
			Handler: p.termsForLink,
			ArgsSchema: map[string]string{
				"requestToken": "string (required) - the link's token",
				"storeId":      "string (required) - stamped from the binding the request arrived through",
				"siteId":       "string (optional) - stamped from the site",
			},
		},
		{
			Name: "acceptTerms",
			Description: "The acceptance form's construct: an applicant accepts the terms and MAP policy shown on " +
				"their terms link, once. Refused by name as the terms read is, and a version that changed under the " +
				"page is terms_changed.",
			Handler: p.acceptTerms,
			ArgsSchema: map[string]string{
				"requestToken":    "string (required) - the link's token",
				"acceptTerms":     "string (required) - on",
				"acceptMap":       "string (required) - on",
				"signedName":      "string (required) - the name the applicant types",
				"termsSha256":     "string (required) - the terms version's fingerprint the page showed",
				"mapSha256":       "string (required) - the MAP policy version's fingerprint the page showed",
				"statementSha256": "string (required) - the acceptance sentence's fingerprint the page showed",
				"storeId":         "string (required) - stamped from the binding the post arrived through",
				"siteId":          "string (optional) - stamped from the site",
			},
		},
		{
			Name: "priceSheetForLink",
			Description: "The price-sheet link's read: the store's current price sheet for an approved buyer whose " +
				"access is granted. The token is the authority; refused by name as link_invalid, link_closed or " +
				"documents_unavailable.",
			Handler: p.priceSheetForLink,
			ArgsSchema: map[string]string{
				"requestToken": "string (required) - the link's token",
				"storeId":      "string (required) - stamped from the binding the request arrived through",
				"siteId":       "string (optional) - stamped from the site",
			},
		},
		{
			Name:        "setWholesaleSettings",
			Description: "Set a store up: write one store's wholesale settings. Cluster operators and internal origin only. Data, not source -- no rebuild.",
			Handler:     p.setWholesaleSettings,
			ArgsSchema: map[string]string{
				"storeId":            "string (required) - the store these settings govern",
				"siteId":             "string (optional) - the store's site; the row takes its organization and id",
				"applicationsOpen":   "bool (required) - whether the shopper form accepts posts",
				"entitlementAdapter": "string (optional) - the named adapter; refused when unknown",
				"introText":          "string (optional) - client-authored storefront copy",
			},
		},
		{
			Name: "provisionEntitlement",
			Description: "Grant wholesale prices through this store's chosen adapter. Only an " +
				"APPROVED application has one to provision. A failed push leaves the " +
				"application approved and the entitlement pending with its error.",
			Handler: p.provisionEntitlement,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the approved application",
			},
		},
		{
			Name: "revokeEntitlement",
			Description: "Take one back, through the adapter that GRANTED it rather than " +
				"whatever settings name today. Requires a revoke decision to have been " +
				"recorded first, so the history and the state agree.",
			Handler: p.revokeEntitlement,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the revoked application",
			},
		},
		{
			Name: "sendTermsEmail",
			Description: "The store's automation sends an applicant the terms email with their private terms link, " +
				"once, through the transactional row lane. Internal origin only; a send that does not go out is " +
				"recorded on the email's row, not returned.",
			Handler: p.sendTermsEmail,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application whose applicant is sent the terms link",
			},
		},
		{
			Name: "sendBuyerEmail",
			Description: "The store's automation sends an applicant the approval (once access is granted), rejection or " +
				"removal email, once, through the transactional row lane. Internal origin only.",
			Handler: p.sendBuyerEmail,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application whose applicant is told",
				"kind":          "string (required) - approved, rejected or revoked",
			},
		},
		{
			Name: "resendEmail",
			Description: "A person of the store sends the terms email or the approval email again, as a new attempt " +
				"with a new link. Refuses the cluster's operators.",
			Handler: p.resendEmail,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application whose applicant is sent the email again",
				"kind":          "string (required) - terms or approved",
			},
		},
		{
			Name: "notifyStaff",
			Description: "The store's automation tells the store's people, at the store's staff address, about one " +
				"application: ready to review, a decision, or access that did not land. Internal origin only; once per event.",
			Handler: p.notifyStaff,
			ArgsSchema: map[string]string{
				"applicationId": "string (required) - the application the notice is about",
				"event":         "string (required) - readyToReview, approved, rejected, revoked, accessNotGranted or accessNotRemoved",
				"subject":       "string (optional) - the subject, with placeholders; empty for the event's own",
				"body":          "string (optional) - the text, with placeholders; empty for the event's own",
				"decisionId":    "string (optional) - the decision a decision notice is about",
			},
		},
		{
			Name: "backfillAccounts",
			Description: "Tie every row written before organizations reached this pack to the " +
				"organization of its site. Cluster operators and internal origin only; a row " +
				"already tied is counted and never rewritten.",
			Handler: p.backfillAccounts,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - only this store's rows",
			},
		},
	}
}

// NewProvider builds the pack IntegrationProvider.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	// The engine shows a client's own fields back through ShopperExtensionRow;
	// a handle without it shows none.
	extensions, _ := pctx.Engine.(extensionReader)
	caller := &engineCaller{engine: pctx.Engine}
	p := &Provider{
		engine:     pctx.Engine,
		reader:     &engineRowReader{engine: pctx.Engine},
		writer:     &engineWriter{engine: pctx.Engine},
		caller:     caller,
		extensions: extensions,
		lane:       &engineLane{caller: caller},
	}
	// THE APPLICANT'S ADDRESS IS ANSWERED BY GO, never named by a call: the row
	// lane asks this resolver for an application's applicant, and only when it
	// is about to hand the message over. Registered HERE, at provider build, so
	// a cluster where the pack is disabled -- whose factory is skipped --
	// registers nothing.
	campaigns.RegisterRowAddress(campaigns.RowAddress{Concept: applicationConcept, Resolve: p.resolveRowMessage})
	return p, nil
}

// Register wires the pack into the engine registries.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
	// A developer may turn it on and off (Connect Shopify design, D4). A
	// declaration of its own, never inferred from the default above.
	memqldsl.RegisterStorefrontPack(domain)
	registerShopperSurface()
	// Bind the Go half to the pack domain so a v1:platform:packState
	// disable skips the factory and the module inventory folds this
	// integration under its pack row (memql#4183). Contract packs register
	// the plugin under the domain name, so the pair is (domain, domain).
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
	registerShippedAdapters()
}

// registerShippedAdapters adds the two adapters this pack ships.
//
// IN Register, NOT IN init(), for the reason the whole pack loads this way:
// a cluster that has this pack disabled runs no behavioural half at all,
// and an adapter registered from a package init would be listed in the
// module inventory of a cluster that cannot use it. It is also what lets a
// test register a fake adapter against a clean registry.
func registerShippedAdapters() {
	if _, err := AdapterByName(AdapterShopifyB2B); err != nil {
		RegisterAdapter(NewShopifyB2BAdapter())
	}
	if _, err := AdapterByName(AdapterCustomerTag); err != nil {
		RegisterAdapter(NewCustomerTagAdapter(""))
	}
}
