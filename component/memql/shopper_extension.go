package memql

import (
	"errors"
	"fmt"
	"strings"
)

// shopper_extension.go -- HOW A CLIENT'S OWN FIELDS REACH A CLIENT'S OWN
// CONCEPT (design record 2026-09-21).
//
// # The sentence this file exists to make true
//
// The storefront program's record states as settled law that
// "client-specific fields are a related concept, not a blob"
// (2026-09-20, section 7). It was unimplementable. RegisterShopperForm
// above is a Go function called from a pack's Register, a product
// repository ships no Go, and shopperArgs drops an undeclared input by
// design -- so the fourteen fields memql-fylo's wholesale form collects
// beyond the pack's three went nowhere, silently. The northwind fixture
// declares a concept carrying an EIN that nothing could ever write a row
// of, and the two-client test passed because it asks about approval
// PROCESSES rather than about FIELDS.
//
// # An extension attaches to a route; it never opens one (D1)
//
// The obvious alternative was to let a DSL domain declare a shopper FORM
// of its own. It was rejected on this file's neighbour's own grounds:
// "declaring a form puts an endpoint on a hosted site's own origin that
// anybody on the internet may post to". The population that may do that
// stays "Go compiled into the engine" rather than becoming "anyone who can
// author DSL" -- which is every product developer on every cluster.
//
// The difference is not theoretical for wholesale in particular. That
// pack's form is a BUILTIN rather than a mutation precisely because an
// application has a precondition, applicationsOpen, and "a plain POST to
// the endpoint would sail past it, which is not a closed door". A
// client-declared route would make that gate something the client's own
// logic had to remember to call. An extension cannot skip it: the pack's
// construct runs first and a refusal there ends the request.
//
// # Registration is LOAD-TIME, and that is why this is a second registry
//
// A pack's declaration is written once at init and panics on a fault,
// "because every caller is a pack's Register running before the node
// serves anything". A DSL domain -- embedded, or mounted at
// MEMQL_DSL_PATH -- loads long after init, so an extension registers
// during MemQLEngine.Init, RETURNS AN ERROR rather than panicking, lands
// on the LoadReport and is refused by strict boot. That is the tier every
// other contract gate already lives in, and the only one that reaches a
// product bundle no Go test in this repository walks.
//
// The lifetimes differ in the other direction too: this registry is
// rebuilt on every load (ResetShopperExtensions), so a domain dropped from
// a bundle stops extending on the next boot. The pack registry is not, and
// must not be -- clearing it would unregister routes whose Register will
// never run again.

// ShopperExtension is one client domain's addition to a form a pack
// declares: the fields the pack does not know about, and the mutation in
// that client's own domain which stores them.
type ShopperExtension struct {
	// Domain is the DECLARING client domain -- the one the mutation lives
	// in. It is not a path segment: an extension adds no route, so nothing
	// about it is ever addressed from outside.
	Domain string
	// Pack and Form name the declared form being extended. Together they
	// are the registry key, because at most one extension may attach to a
	// route (D3).
	Pack string
	Form string
	// Construct is the MUTATION the extension's fields are passed to.
	//
	// A MUTATION, NEVER A LOGIC (D2). A logic may call builtins, and
	// pointing a public form at one would put arbitrary builtin calls
	// within reach of the internet under the site owner's borrowed
	// authority. A mutation writes one concept and that is the whole blast
	// radius. This file cannot check the construct's KIND -- it does not
	// see the construct registry -- so the loader does, and its refusal is
	// the one that matters.
	Construct string
	// Fields are the inputs the pack does not declare. The stamped names
	// are refused here exactly as they are on a form, and a name the PACK
	// already declares is refused too (D7's corollary): two declarations
	// consuming one input name is ambiguous about whose rule applies, and
	// the client can read the pack's value off the row it relates to.
	Fields []ShopperField
	// Description is what the module inventory and the pack guide show.
	Description string
}

// shopperExtensions is keyed by the EXTENDED route, not by the declaring
// domain, because the handler's question is "does anything extend this
// form" and a domain-keyed map would answer it with a scan.
var shopperExtensions = map[string]ShopperExtension{}

// RegisterShopperExtension declares one client domain's addition to a
// pack's form.
//
// IT RETURNS AN ERROR RATHER THAN PANICKING, which is the whole difference
// from RegisterShopperForm beside it. See the file comment: the caller is
// a DSL load, not an init, and a load fault belongs on the LoadReport
// where strict boot and cmd/memqllint both already read it.
func RegisterShopperExtension(ext ShopperExtension) error {
	ext.Domain = strings.TrimSpace(ext.Domain)
	ext.Pack = strings.TrimSpace(ext.Pack)
	ext.Form = strings.TrimSpace(ext.Form)
	ext.Construct = strings.TrimSpace(ext.Construct)

	// THE DOMAIN IS NOT HELD TO shopperNamePattern, and that is deliberate.
	// That pattern bounds a PATH SEGMENT on a public origin, which is why it
	// is narrower than an identifier needs to be. An extension adds no route,
	// so its domain is never addressed from outside and never appears in a
	// URL -- it is a DSL namespace, and holding a namespace to a URL's rules
	// would refuse names the loader itself accepts.
	if ext.Domain == "" {
		return fmt.Errorf("shopper extension: no declaring domain")
	}
	// The route halves, the field names and the reserved-name refusals are
	// the SAME rules a pack's own declaration passes. Sharing the function
	// rather than restating them is what keeps an extension from being a
	// second, weaker way to declare a field.
	if err := validateShopperRoute(ext.Pack, ext.Form, ext.Construct, ext.Fields); err != nil {
		return fmt.Errorf("shopper extension declared by %q: %w", ext.Domain, err)
	}
	// REFUSAL 5. A pack adding a field to its own form declares it.
	if ext.Domain == ext.Pack {
		return fmt.Errorf("shopper extension: %q may not extend its own form %s/%s -- a pack "+
			"that wants another field declares it", ext.Domain, ext.Pack, ext.Form)
	}

	shopperMu.Lock()
	defer shopperMu.Unlock()

	key := shopperKey(ext.Pack, ext.Form)
	form, declared := shopperForms[key]
	if !declared {
		// A DISABLED PACK IS NOT A TYPO, AND THE DIFFERENCE HAD TO BE DRAWN.
		//
		// This refused outright at first, on the reasoning that an extension
		// of a route nobody serves is a declaration whose author believes
		// something false. That is true of a misspelling and false of the
		// ordinary case, and the ordinary case is unbootable: a storefront
		// pack ships DISABLED, so a product whose DSL extends one would
		// refuse boot -- and an operator cannot enable a pack on a cluster
		// that will not start. The conformance corpus found this by being
		// an environment with the packs at their shipped default, which is
		// what every new cluster is.
		//
		// A disabled pack is MOUNTED-INERT: its concepts load so imports and
		// relationships resolve, and every behavioural construct is skipped.
		// An extension is a behavioural construct attached to a behavioural
		// surface, so it is inert on exactly the same terms.
		//
		// THE DISCRIMINATOR IS WHETHER THE PACK DECLARES ANY FORM AT ALL.
		// A disabled pack declares none, because its Register never runs the
		// half that declares them. A pack that is live and declaring forms,
		// none of them this one, is somebody's spelling mistake -- and that
		// still refuses, loudly, which is what the refusal was for.
		if shopperFormsDeclaredForLocked(ext.Pack) == 0 {
			return errShopperPackInert
		}
		return fmt.Errorf("shopper extension declared by %q names the form %s. The pack %q is "+
			"loaded and declares forms, but not that one -- check the spelling against them",
			ext.Domain, key, ext.Pack)
	}
	// REFUSAL 2, naming BOTH domains: the operator has to know which two
	// declarations to reconcile, and one name sends them looking for the
	// other.
	if prior, taken := shopperExtensions[key]; taken {
		return fmt.Errorf("shopper extension: %s is already extended by %q, and %q may not "+
			"extend it as well. Which one ran would depend on load order, which is resolved "+
			"by nothing", key, prior.Domain, ext.Domain)
	}
	// REFUSAL 6.
	packFields := make(map[string]struct{}, len(form.Fields))
	for _, f := range form.Fields {
		packFields[f.Name] = struct{}{}
	}
	for _, f := range ext.Fields {
		if _, clash := packFields[f.Name]; clash {
			return fmt.Errorf("shopper extension declared by %q redeclares %q, which the pack's "+
				"own form %s already accepts. An extension ADDS fields; the pack's own arrive "+
				"on its row, and %q reads them through the relationship it declares",
				ext.Domain, f.Name, key, ext.Domain)
		}
	}
	shopperExtensions[key] = ext
	return nil
}

// errShopperPackInert says the named pack is not declaring any shopper form
// on this cluster, so the extension has nothing to attach to YET.
//
// NOT A LOAD FAILURE. It is the same state a disabled pack's own tools and
// mutations are in, and it resolves itself the moment an operator flips the
// packState row and the nodes restart -- which is exactly how a pack is
// meant to be turned on.
var errShopperPackInert = errors.New("the pack declares no shopper form on this cluster")

// shopperFormsDeclaredForLocked counts a pack's declared forms. Callers hold
// shopperMu.
func shopperFormsDeclaredForLocked(pack string) int {
	n := 0
	prefix := strings.TrimSpace(pack) + "/"
	for key := range shopperForms {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

// ShopperExtensionFor resolves the extension attached to a form, or nil.
func ShopperExtensionFor(pack, form string) *ShopperExtension {
	shopperMu.RLock()
	defer shopperMu.RUnlock()
	e, ok := shopperExtensions[shopperKey(pack, form)]
	if !ok {
		return nil
	}
	return &e
}

// ResetShopperExtensions clears every extension.
//
// PRODUCTION, NOT A TEST SEAM. The DSL tree is loaded into a registry that
// must reflect the tree as it now is: a domain removed from a bundle, or a
// pack disabled under it, has to stop extending. Called by the loader
// before it walks, so a failed load cannot leave half of the previous one
// behind.
func ResetShopperExtensions() {
	shopperMu.Lock()
	defer shopperMu.Unlock()
	shopperExtensions = map[string]ShopperExtension{}
}
