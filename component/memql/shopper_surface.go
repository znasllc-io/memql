package memql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// shopper_surface.go -- THE WHOLE OF WHAT A SHOPPER CAN REACH (epic
// memql#5532, issue memql#5550, design record Q2).
//
// # The decision
//
// A shopper is NOBODY to MemQL. They have no user row, no session, no
// bearer and no actor -- not even an anonymous one. Three shapes were on
// the table (record, Q2): a declared unauthenticated route per pack with
// the shopper staying anonymous and their email or Shopify customer id as
// DATA; a Shopify customer verified from a Customer Account API token and
// given a constrained actor; or the first for submitting and the second for
// anything that reads a buyer's own quotes, reorder lists or credit limit.
//
// The owner chose the third, and this file builds the FIRST HALF. The
// verified-customer principal is the recorded answer for a buyer's own data
// and is built when the wholesale pack needs it; nothing here forecloses
// it, because reach is declared per ROUTE and a second kind of caller on
// the same route is an addition rather than a rewrite.
//
// # Why not the anonymous row-authz tier
//
// @rowAuthz(public) already exists (epic memql#4541, D4) and admits an
// anonymous actor to a concept declaring it. It is the wrong instrument
// here, and the reason is worth keeping: it publishes a whole CONCEPT. A
// review is owned by the MERCHANT so it can be moderated, so the concept
// cannot be public without making every review writable and readable by
// everyone -- and a wholesale application is somebody's tax identifier.
//
// So reach is declared per ROUTE instead. A pack names the forms a shopper
// may post and the reads a shopper may make; everything else on that pack,
// and every other pack, is exactly as unreachable as it was. The route runs
// the named construct under the SITE OWNER'S borrowed authority -- the
// campaigns precedent, where the drain worker runs everything owned under
// auth.ContextWithUserActor for a value copied off a row the starting
// caller had already read (component/campaigns). The merchant is the owner
// of what is written through their storefront; the shopper is data on it.
//
// # What a declaration costs, stated plainly
//
// Declaring a form puts an endpoint on a hosted site's own origin that
// anybody on the internet may post to. Four controls stand in front of it
// and none of them is this registry: the site's own shopperForms switch
// (off by default), the per-address and per-site rate limit, the body size
// cap, and the fact that the bff route is classified
// servedButNotExternallyRouted so the edge is the only way in. This
// registry's job is narrower and it is the one nothing else can do: it
// bounds WHAT a reachable request may name.
//
// # The registry is init-time and fails loudly
//
// Every refusal here is a panic, because every caller is a pack's Register
// running before the node serves anything. A pack that declares a
// server-stamped field name, a duplicate route, an absolute redirect or a
// path segment that is not one is a programming error, and a programming
// error that reaches a running cluster as a silently-dropped declaration is
// a shopper surface nobody can find and nobody can audit.

// ---------------------------------------------------------------------------
// THE WIRE CONTRACT BETWEEN THE EDGE AND THE BFF
// ---------------------------------------------------------------------------
//
// IT LIVES HERE BECAUSE THIS IS THE ONE PACKAGE BOTH SIDES CAN REACH.
// component/server is its own module and cannot import component/edge (which
// lives in the unsplit root module, outside that tier's replace directives) --
// site_bundle_handler.go records the same constraint. The tree's existing
// answer to that is a duplicated literal with a comment naming the other
// side, which is how TrackingPaths and campaigns.TrackingOpenPath are kept
// in step. A duplicated literal is wrong before it drifts, and both modules
// already require component/memql, so the contract is declared once here and
// imported by both.
//
// If these ever disagree the symptom is silent and ugly: the edge stamps a
// header the bff does not read, the bff refuses every shopper request for
// want of a site, and nothing anywhere names the cause.

const (
	// ShopperFormPathPrefix and ShopperReadPathPrefix are the marker-stripped
	// roots the bff serves, which the edge reaches by stripping "/_memql"
	// from "/_memql/forms/..." and "/_memql/reads/...".
	ShopperFormPathPrefix = "/forms/"
	ShopperReadPathPrefix = "/reads/"

	// ShopperSiteHeader names the v1:platform:site row the request arrived
	// on. It is a POINTER, never an assertion: the bff re-reads that row and
	// re-checks everything about it, so the most a forged value can do is
	// name a site that really does have its shopper surface on.
	ShopperSiteHeader = "X-Memql-Shopper-Site"
	// ShopperStoreHeader names the store the write belongs to -- the serving
	// binding's, or the preview binding's when a grant is in force. The bff
	// refuses a value that is neither, so this cannot aim a row at a store
	// the site is not bound to.
	ShopperStoreHeader = "X-Memql-Shopper-Store"
	// ShopperOwnerHeader names the user whose authority the write borrows.
	// IT IS SELF-CHECKING: the bff reads the site row UNDER THIS USER, and a
	// user who does not own that site reads zero rows, so a forged owner
	// refuses itself without any comparison being written down.
	ShopperOwnerHeader = "X-Memql-Shopper-Owner"

	// ShopperSubmissionIDArg is the argument name the bff stamps one
	// freshly-minted id into, on BOTH the pack's construct and the client
	// extension's mutation (design record 2026-09-21, D4).
	//
	// DECLARED BY THE CONSTRUCT, NEVER BY THE FORM. It is in
	// reservedShopperFieldNames, so a declaration offering it to a shopper
	// is refused -- a shopper who could choose it could point their own row
	// at somebody else's submission. A pack's shopper construct declares it
	// in its args block and uses it as the row id it creates, which is what
	// gives a client extension something to relate to.
	ShopperSubmissionIDArg = "submissionId"
)

// shopperFieldDefaultMaxLength bounds a declared field that names no bound
// of its own.
//
// FOUR KILOBYTES, and the number is set by the review body it was written
// for rather than by anything about HTTP. A field with no bound at all is
// the shape that turns a size cap on the whole body into the only limit,
// and then one field can consume it.
const shopperFieldDefaultMaxLength = 4096

// ShopperReadKindBuiltin and ShopperReadKindQuery are the two construct
// kinds a declared read may name.
//
// A BUILTIN IS THE USUAL ANSWER and a query the exception, which inverts
// what a reader expects. A shopper read almost always has a gate that is
// not expressible in a filter -- "only while this store's publicDisplay is
// true" is a read of a SECOND row -- and putting that gate in the caller
// means every future caller has to remember it.
const (
	ShopperReadKindBuiltin = "builtin"
	ShopperReadKindQuery   = "query"
	// ShopperWriteKindMutation is a form's default. A bare `name(args)` is
	// the mutation invocation form, which is why it renders as an empty
	// prefix rather than as the word.
	ShopperWriteKindMutation = "mutation"
)

// reservedShopperFieldNames are stamped server-side and may never be
// declared as a form or read field.
//
// THIS IS THE LOAD-BEARING HALF OF D9. storeId is stamped from the binding
// the write arrived through, resolved by the edge, and a form that could
// supply its own would let a shopper aim a row at a store they are not on
// -- which is precisely the confusion between preview and live that D9
// exists to prevent. The rest are row identity and provenance: a caller
// that can set createdBy can forge one.
var reservedShopperFieldNames = map[string]string{
	"storeId":     "stamped from the binding the request arrived through (design D9)",
	"siteId":      "stamped from the site the request arrived on",
	"ownerUserId": "stamped from the site owner, whose authority the write borrows",
	"id":          "the row id, derived server-side",
	"concept":     "a row intrinsic",
	"createdAt":   "a row intrinsic",
	"createdBy":   "a row intrinsic; a caller that can set it can forge provenance",
	"submittedAt": "stamped from the server clock, never the shopper's",
	"submissionId": "minted per POST by the bff and stamped into both constructs, so one " +
		"submission has one id (design record 2026-09-21, D4)",
	"actor":       "a reserved engine root",
	"now":         "a reserved engine root",
	"partition":   "a reserved engine root",
	"config":      "a reserved engine root",
	"trace":       "a reserved engine root",
}

// shopperNamePattern bounds a pack or route name. Both are PATH SEGMENTS
// on a public origin, so the set is deliberately narrower than an
// identifier needs to be: no dots, no hyphens, no encoding to get wrong.
var shopperNamePattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]{0,39}$`)

// ShopperField is one field a declared form accepts or a declared read
// takes as an argument.
type ShopperField struct {
	// Name is the form input name and the construct argument name. They are
	// the same on purpose: a mapping layer between them is a place for the
	// two to drift, and the form is authored against this declaration.
	Name string
	// Required refuses the request when the field is absent or blank.
	Required bool
	// MaxLength bounds the value. Zero means shopperFieldDefaultMaxLength;
	// there is no unbounded field.
	MaxLength int
	// Enum, when non-empty, is the closed set of accepted values.
	Enum []string
	// Numeric parses the value as an integer before it reaches the
	// construct, so a rating arrives as a number rather than a string.
	Numeric bool
	// Description is what the pack guide and the module inventory show.
	Description string
}

// EffectiveMaxLength resolves the declared bound. Exported because the
// bff handler enforces it and lives in another module.
func (f ShopperField) EffectiveMaxLength() int {
	if f.MaxLength > 0 {
		return f.MaxLength
	}
	return shopperFieldDefaultMaxLength
}

// ShopperForm is one declared WRITE: a plain HTML form post a shopper may
// make, and the mutation it writes through.
type ShopperForm struct {
	// Pack is the registering pack's domain. It is the first path segment.
	Pack string
	// Name is this form's route name. It is the second path segment, so
	// POST /_memql/forms/<Pack>/<Name> on the site's own origin.
	Name string
	// Construct is the construct the accepted fields are passed to. It runs
	// under the site owner's borrowed authority, so it must be an ordinary
	// @actor construct -- never @serverOnly, which the borrowed actor
	// deliberately cannot reach.
	Construct string
	// Kind is ShopperWriteKindMutation (the default and the usual answer)
	// or ShopperReadKindBuiltin, for a pack whose write needs Go -- deriving
	// a row id, or writing more than one row from one submission.
	//
	// SYMMETRIC WITH ShopperRead.Kind on purpose: the handler renders the
	// call from it, and a form and a read that named their construct
	// differently would be two spellings of one idea.
	Kind string
	// Fields is the closed set of accepted inputs. A field the form posts
	// that is not declared here is DROPPED, not refused: a browser sends
	// what the page contains and refusing an unexpected input would make
	// every unrelated page edit a server error.
	Fields []ShopperField
	// RedirectOK and RedirectError are SITE-RELATIVE paths the 303 names.
	// Site-relative and validated, because a redirect target a form could
	// influence is an open redirect and this one is on a merchant's own
	// origin.
	RedirectOK    string
	RedirectError string
	// Description is what the pack guide and the module inventory show.
	Description string
}

// CallPrefix is the keyword a construct invocation carries, or "" for a
// mutation -- `submitReview(...)` versus `builtin reviewsPublished(...)`
// versus `query reviewsForProduct(...)`.
//
// HERE RATHER THAN IN THE HANDLER, because it is a property of the
// declaration and the handler renders both forms and reads.
func ShopperCallPrefix(kind string) string {
	switch kind {
	case ShopperReadKindBuiltin:
		return "builtin "
	case ShopperReadKindQuery:
		return "query "
	default:
		return ""
	}
}

// ShopperRead is one declared READ: a query or builtin a shopper's browser
// may call for data the storefront renders.
type ShopperRead struct {
	Pack      string
	Name      string
	Construct string
	// Kind is ShopperReadKindBuiltin or ShopperReadKindQuery.
	Kind string
	// Fields are the arguments the caller may supply. storeId is NOT among
	// them and cannot be: it is stamped, exactly as on a form.
	Fields      []ShopperField
	Description string
}

// ShopperSurfaceEntry is one row of the whole declared surface, for the
// module inventory and the pack guide.
type ShopperSurfaceEntry struct {
	Pack        string
	Name        string
	Kind        string // "form" or "read"
	Construct   string
	Description string
}

var (
	shopperMu    sync.RWMutex
	shopperForms = map[string]ShopperForm{}
	shopperReads = map[string]ShopperRead{}
)

func shopperKey(pack, name string) string {
	return strings.TrimSpace(pack) + "/" + strings.TrimSpace(name)
}

// RegisterShopperForm declares a form a shopper may post. Panics on an
// invalid declaration -- see the file comment on why every refusal here is
// loud.
func RegisterShopperForm(form ShopperForm) {
	form.Pack = strings.TrimSpace(form.Pack)
	form.Name = strings.TrimSpace(form.Name)
	form.Construct = strings.TrimSpace(form.Construct)
	if err := validateShopperRoute(form.Pack, form.Name, form.Construct, form.Fields); err != nil {
		panic("memql.RegisterShopperForm: " + err.Error())
	}
	if form.Kind == "" {
		form.Kind = ShopperWriteKindMutation
	}
	if form.Kind != ShopperWriteKindMutation && form.Kind != ShopperReadKindBuiltin {
		panic(fmt.Sprintf("memql.RegisterShopperForm: Kind %q is neither %q nor %q",
			form.Kind, ShopperWriteKindMutation, ShopperReadKindBuiltin))
	}
	if err := validateShopperRedirect("RedirectOK", form.RedirectOK); err != nil {
		panic("memql.RegisterShopperForm: " + err.Error())
	}
	if err := validateShopperRedirect("RedirectError", form.RedirectError); err != nil {
		panic("memql.RegisterShopperForm: " + err.Error())
	}
	key := shopperKey(form.Pack, form.Name)
	shopperMu.Lock()
	defer shopperMu.Unlock()
	if _, taken := shopperForms[key]; taken {
		panic(fmt.Sprintf("memql.RegisterShopperForm: %q is already declared. A duplicate is "+
			"REFUSED rather than last-wins: the surface a shopper reaches must be readable off "+
			"the declarations, and last-wins makes which one is live depend on init order", key))
	}
	if _, taken := shopperReads[key]; taken {
		panic(fmt.Sprintf("memql.RegisterShopperForm: %q is already declared as a READ. One "+
			"(pack, name) names one route; a form and a read sharing it would differ only by "+
			"HTTP method, which is not something a form author can see", key))
	}
	shopperForms[key] = form
}

// RegisterShopperRead declares a read a shopper's browser may make.
func RegisterShopperRead(read ShopperRead) {
	read.Pack = strings.TrimSpace(read.Pack)
	read.Name = strings.TrimSpace(read.Name)
	read.Construct = strings.TrimSpace(read.Construct)
	read.Kind = strings.TrimSpace(read.Kind)
	if err := validateShopperRoute(read.Pack, read.Name, read.Construct, read.Fields); err != nil {
		panic("memql.RegisterShopperRead: " + err.Error())
	}
	if read.Kind != ShopperReadKindBuiltin && read.Kind != ShopperReadKindQuery {
		panic(fmt.Sprintf("memql.RegisterShopperRead: Kind %q is neither %q nor %q",
			read.Kind, ShopperReadKindBuiltin, ShopperReadKindQuery))
	}
	key := shopperKey(read.Pack, read.Name)
	shopperMu.Lock()
	defer shopperMu.Unlock()
	if _, taken := shopperReads[key]; taken {
		panic(fmt.Sprintf("memql.RegisterShopperRead: %q is already declared", key))
	}
	if _, taken := shopperForms[key]; taken {
		panic(fmt.Sprintf("memql.RegisterShopperRead: %q is already declared as a FORM", key))
	}
	shopperReads[key] = read
}

// validateShopperRoute holds the halves a form and a read share.
func validateShopperRoute(pack, name, construct string, fields []ShopperField) error {
	if !shopperNamePattern.MatchString(pack) {
		return fmt.Errorf("Pack %q is not a single lowerCamelCase path segment "+
			"(%s)", pack, shopperNamePattern)
	}
	if !shopperNamePattern.MatchString(name) {
		return fmt.Errorf("Name %q is not a single lowerCamelCase path segment "+
			"(%s)", name, shopperNamePattern)
	}
	if construct == "" {
		return fmt.Errorf("%s/%s declares no Construct. A route that names no "+
			"construct is reachable and does nothing", pack, name)
	}
	if len(fields) == 0 {
		return fmt.Errorf("%s/%s declares no fields. A declaration with no fields "+
			"accepts nothing, so it is a route nobody can use rather than an open one",
			pack, name)
	}
	seen := map[string]struct{}{}
	for _, f := range fields {
		fname := strings.TrimSpace(f.Name)
		if fname == "" {
			return fmt.Errorf("%s/%s declares a field with no name", pack, name)
		}
		if why, reserved := reservedShopperFieldNames[fname]; reserved {
			return fmt.Errorf("%s/%s declares the field %q, which is stamped server-side: %s. "+
				"A declared one would let the request supply its own", pack, name, fname, why)
		}
		if _, dup := seen[fname]; dup {
			return fmt.Errorf("%s/%s declares the field %q twice", pack, name, fname)
		}
		seen[fname] = struct{}{}
		if f.MaxLength < 0 {
			return fmt.Errorf("%s/%s declares a negative MaxLength on %q", pack, name, fname)
		}
		if f.Numeric && len(f.Enum) > 0 {
			return fmt.Errorf("%s/%s declares %q as both Numeric and an Enum; Enum members "+
				"are string literals, so the two cannot both hold", pack, name, fname)
		}
	}
	return nil
}

// validateShopperRedirect refuses anything but a site-relative path.
//
// THE 303 IS THE WHOLE POINT OF THE ENDPOINT (a plain form post answers
// with a redirect or the shopper stares at a blank page), so the target is
// on the request's critical path and an attacker-influenceable one would be
// an open redirect on a merchant's own origin. Relative, leading slash, no
// scheme, no authority, no dot-segment: the same "there is no legitimate
// request outside the bundle to repair" reasoning component/edge applies to
// its own paths.
func validateShopperRedirect(field, target string) error {
	t := strings.TrimSpace(target)
	if t == "" {
		return fmt.Errorf("%s is empty. Declare where the shopper lands; a form post "+
			"that answers no redirect leaves a blank page", field)
	}
	if !strings.HasPrefix(t, "/") {
		return fmt.Errorf("%s = %q is not site-relative. It must begin with '/'", field, t)
	}
	if strings.HasPrefix(t, "//") {
		return fmt.Errorf("%s = %q begins with '//', which a browser reads as a "+
			"protocol-relative URL to another host", field, t)
	}
	if strings.Contains(t, "://") || strings.Contains(t, "..") || strings.ContainsAny(t, "\r\n") {
		return fmt.Errorf("%s = %q must be a plain site-relative path: no scheme, no "+
			"dot-segment, no control character", field, t)
	}
	return nil
}

// ShopperFormFor resolves a declared form, or nil.
func ShopperFormFor(pack, name string) *ShopperForm {
	shopperMu.RLock()
	defer shopperMu.RUnlock()
	f, ok := shopperForms[shopperKey(pack, name)]
	if !ok {
		return nil
	}
	return &f
}

// ShopperReadFor resolves a declared read, or nil.
func ShopperReadFor(pack, name string) *ShopperRead {
	shopperMu.RLock()
	defer shopperMu.RUnlock()
	r, ok := shopperReads[shopperKey(pack, name)]
	if !ok {
		return nil
	}
	return &r
}

// ShopperSurface returns every declaration, sorted, for the module
// inventory and the pack guide. The surface is meant to be READ: an
// operator asking "what can the public reach on this cluster" gets one
// answer from one place.
func ShopperSurface() []ShopperSurfaceEntry {
	shopperMu.RLock()
	defer shopperMu.RUnlock()
	out := make([]ShopperSurfaceEntry, 0, len(shopperForms)+len(shopperReads))
	for _, f := range shopperForms {
		out = append(out, ShopperSurfaceEntry{Pack: f.Pack, Name: f.Name, Kind: "form",
			Construct: f.Construct, Description: f.Description})
	}
	for _, r := range shopperReads {
		out = append(out, ShopperSurfaceEntry{Pack: r.Pack, Name: r.Name, Kind: "read",
			Construct: r.Construct, Description: r.Description})
	}
	// EXTENSIONS ARE PART OF THE SURFACE. An operator asking what the public
	// can reach on this cluster must be told about the client's fields too:
	// they arrive on the same public endpoint and are written by the same
	// request, so an inventory that omitted them would understate exactly the
	// half nobody audited (design record 2026-09-21).
	for _, e := range shopperExtensions {
		out = append(out, ShopperSurfaceEntry{Pack: e.Pack, Name: e.Form, Kind: "extension",
			Construct: e.Construct, Description: e.Description})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pack != out[j].Pack {
			return out[i].Pack < out[j].Pack
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ReservedShopperFieldNames returns the server-stamped names, sorted, so
// the pack guide lists them rather than restating them.
func ReservedShopperFieldNames() []string {
	out := make([]string, 0, len(reservedShopperFieldNames))
	for n := range reservedShopperFieldNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ResetShopperSurfaceForTest clears the registry. Test seam only.
func ResetShopperSurfaceForTest() {
	shopperMu.Lock()
	defer shopperMu.Unlock()
	shopperForms = map[string]ShopperForm{}
	shopperReads = map[string]ShopperRead{}
	shopperExtensions = map[string]ShopperExtension{}
}
