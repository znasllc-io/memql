package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
// rebuilt on every load, so a domain dropped from
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
	// AcceptsAccount is true when the extension's mutation declares an
	// accountId argument, and only then is the site's organization stamped
	// into its arguments. An extension that declares none is handed none
	// and still writes, so a client mutation written before organizations
	// reached the shopper path keeps working unchanged. Set by the loader;
	// accountId is never offered to a shopper either way.
	AcceptsAccount bool
	// Concept is the concept the extension's mutation writes, its row at the
	// submission's id. Set by the loader from the mutation's binding; it is how
	// a pack shows the row back to the store's people (ShopperExtensionRow).
	Concept string
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
	shopperMu.Lock()
	defer shopperMu.Unlock()
	return registerShopperExtensionIn(ext, shopperForms, shopperExtensions)
}

// The same contract can validate a candidate without publishing its routes.
func registerShopperExtensionIn(ext ShopperExtension, forms map[string]ShopperForm, extensions map[string]ShopperExtension) error {
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

	key := shopperKey(ext.Pack, ext.Form)
	form, declared := forms[key]
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
		if shopperFormCount(forms, ext.Pack) == 0 {
			return errShopperPackInert
		}
		return fmt.Errorf("shopper extension declared by %q names the form %s. The pack %q is "+
			"loaded and declares forms, but not that one -- check the spelling against them",
			ext.Domain, key, ext.Pack)
	}
	// REFUSAL 2, naming BOTH domains: the operator has to know which two
	// declarations to reconcile, and one name sends them looking for the
	// other.
	if prior, taken := extensions[key]; taken {
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
	extensions[key] = ext
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
	return shopperFormCount(shopperForms, pack)
}

func shopperFormCount(forms map[string]ShopperForm, pack string) int {
	n := 0
	prefix := strings.TrimSpace(pack) + "/"
	for key := range forms {
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
// The boot loader replaces the complete extension view atomically instead;
// this explicit reset leaves the pack's declared forms intact.
func ResetShopperExtensions() {
	shopperMu.Lock()
	defer shopperMu.Unlock()
	shopperExtensions = map[string]ShopperExtension{}
}

// SHOWING A CLIENT'S OWN FIELDS BACK TO THE STORE'S PEOPLE (X14).
//
// A client's extension writes its own concept's row at the submission's id,
// the id the pack wrote its own row at. The pack that declared the form shows
// that row to whoever reads the pack's row, and has no way to name the
// client's concept or its fields: a pack may not know its clients. So the
// engine answers it here, from the extension registry and the concept's own
// declaration -- every declared field but the stamped names, each labelled by
// its own name in plain words, and the two facts the concept's @displayCard
// names first.
//
// THE ROW IS READ UNDER THE CALLER'S CONTEXT through the ordinary query planner,
// so the row tier decides exactly as it does for any named query: a caller who
// cannot read the client's row is answered nothing. MemQL has no generic row
// read; like the concept browser's, this one builds a request-local query bound
// to the concept, which registers nothing in the shared registry.

// ExtensionField is one field of a client extension's row, as a person reads it.
type ExtensionField struct {
	Name  string
	Label string
	Value any
}

// ExtensionRow is a client extension's row: its concept, the values of the two
// fields its display card names, and every field a person is shown.
type ExtensionRow struct {
	Concept, Primary, Secondary string
	Fields                      []ExtensionField
}

// ShopperExtensionRow reads the row the extension of pack/form wrote at rowID,
// under ctx. It answers false, with no error, when no extension is attached to
// the form or ctx reads no row there.
func (e *MemQLEngine) ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (ExtensionRow, bool, error) {
	ext := ShopperExtensionFor(pack, form)
	if ext == nil || strings.TrimSpace(ext.Concept) == "" || strings.TrimSpace(rowID) == "" {
		return ExtensionRow{}, false, nil
	}
	if e == nil || e.concepts == nil {
		return ExtensionRow{}, false, fmt.Errorf("shopper extension: the engine has no concept registry")
	}
	concept, err := e.concepts.Get(ext.Concept)
	if err != nil || concept == nil {
		return ExtensionRow{}, false, fmt.Errorf("shopper extension: %s/%s writes %q, which is not a loaded concept", pack, form, ext.Concept)
	}
	// The stored id is canonical; either form of rowID names it.
	want := concept.Name + ":" + BareShortId(rowID)
	root := &LogicalExpression{Op: LogicalAnd,
		Left:  &ComparisonExpression{Field: FieldReference{Raw: "concept", Parts: []string{"concept"}}, Operator: OpEq, Value: concept.Name},
		Right: &ComparisonExpression{Field: FieldReference{Raw: "id", Parts: []string{"id"}}, Operator: OpEq, Value: want},
	}
	sum := sha256.Sum256([]byte(concept.Name + "\x00" + want))
	name := "shopperExtensionRow" + hex.EncodeToString(sum[:8])
	functions := newFunctionRegistry()
	if err := functions.add(&Function{Name: name, Expr: root, ExprSource: canonicalExpression(root),
		BoundConcept: concept.Name, FunctionKind: "query", Enabled: true, CacheTTL: "0"}); err != nil {
		return ExtensionRow{}, false, err
	}
	result, err := e.executeWith(ctx, "query "+name+"()", functions, nil, false)
	if err != nil {
		return ExtensionRow{}, false, fmt.Errorf("shopper extension: reading %s: %w", want, err)
	}
	if result == nil || result.Bundle == nil {
		return ExtensionRow{}, false, nil
	}
	for _, node := range result.Bundle.GetNodes() {
		if node.GetConcept() != concept.Name || BareShortId(node.GetId()) != BareShortId(rowID) {
			continue
		}
		raw, err := node.GetPayload().MarshalJSON()
		if err != nil {
			return ExtensionRow{}, false, err
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return ExtensionRow{}, false, err
		}
		return extensionRowOf(concept, payload), true, nil
	}
	return ExtensionRow{}, false, nil
}

// extensionRowOf shows one payload of concept: every declared field a shape may
// project, but the stamped names and the concept's secrets, in name order.
func extensionRowOf(concept *memorynodes.Concept, payload map[string]any) ExtensionRow {
	row := ExtensionRow{Concept: concept.Name}
	shown := map[string]bool{}
	secret := map[string]bool{}
	for _, name := range concept.SecretFields() {
		secret[name] = true
	}
	for _, name := range concept.ProjectableFields() {
		if _, stamped := reservedShopperFieldNames[name]; stamped || secret[name] {
			continue
		}
		shown[name] = true
		row.Fields = append(row.Fields, ExtensionField{Name: name, Label: extensionFieldLabel(name), Value: payload[name]})
	}
	// A display card naming a field that is not shown shows nothing of it.
	fact := func(field string) string {
		v, ok := payload[strings.TrimSpace(field)]
		if !ok || v == nil || !shown[strings.TrimSpace(field)] {
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	if card := concept.DisplayCard; card != nil {
		row.Primary, row.Secondary = fact(card.Primary), fact(card.Secondary)
	}
	return row
}

// extensionFieldLabel is a field name in plain words: split at its case
// boundaries (an acronym is one word) and at digits, lower-cased, the first
// word capitalised -- taxNumber is "Tax number".
func extensionFieldLabel(name string) string {
	runes := []rune(strings.TrimSpace(name))
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || unicode.IsSpace(r):
			flush()
			continue
		case i > 0 && len(cur) > 0:
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower)) ||
				unicode.IsDigit(r) != unicode.IsDigit(prev) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	if len(words) == 0 {
		return ""
	}
	first := []rune(words[0])
	first[0] = unicode.ToUpper(first[0])
	words[0] = string(first)
	return strings.Join(words, " ")
}
