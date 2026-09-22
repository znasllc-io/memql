package memql

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// shopper_extension_load.go -- the load-time half of the extension seam
// (design record 2026-09-21, section 4.2).
//
// A pack's shopper declaration is Go running at init. A client's is DSL,
// which loads during MemQLEngine.Init, so the registration happens here:
// after every construct is in the registry, folded onto the LoadReport,
// and refused by strict boot exactly as an unresolvable tool handler is.

// ShopperFormExtensionDecl is the parsed @shopperFormExtension(pack=,
// form=) annotation: the route this mutation adds its fields to.
//
// It carries the ROUTE and not the fields, because the fields are the args
// block (D7) and a second copy of them here is a second copy to drift.
type ShopperFormExtensionDecl struct {
	Pack string
	Form string
}

// shopperExtensionFromFunction converts a loaded mutation carrying
// @shopperFormExtension into the registry's own shape.
//
// PURE, and deliberately: everything it refuses is a property of the
// declaration alone, so the refusals are testable without an engine, a
// database or a loaded pack. What it CANNOT know -- whether the route
// exists, whether something else already extends it, whether a field
// collides with the pack's -- belongs to RegisterShopperExtension, which
// holds the registry.
func shopperExtensionFromFunction(fn *Function) (ShopperExtension, error) {
	if fn == nil || fn.ShopperFormExtension == nil {
		return ShopperExtension{}, fmt.Errorf("shopper extension: no declaration")
	}
	// D2, AND THIS IS THE REFUSAL THAT MATTERS. A logic may call builtins,
	// so a public form pointed at one would put arbitrary builtin calls
	// within reach of the internet under the site owner's borrowed
	// authority. A mutation writes one concept and that is the whole blast
	// radius. The annotation registry already confines the annotation to a
	// mutation receiver; this is the second half, at the only place that
	// sees what the construct actually loaded as.
	if fn.FunctionKind != "mutation" {
		return ShopperExtension{}, fmt.Errorf("@shopperFormExtension is declared on %q, which "+
			"loaded as a %s. It is legal on a MUTATION alone: a logic may call builtins, and a "+
			"public form pointed at one would reach them under the site owner's borrowed authority",
			fn.Name, describeFunctionKind(fn.FunctionKind))
	}
	// THE DECLARING DOMAIN IS THE DOMAIN OF THE CONCEPT THE MUTATION
	// WRITES. Taken from the binding rather than from the file path
	// because a path is a fact about a checkout and a bound concept is a
	// fact about the construct -- and because it makes "an extension
	// writes its own domain's concept" true by construction rather than
	// by a rule somebody has to enforce.
	domain := conceptRootDomain(fn.BoundConcept)
	if domain == "" {
		return ShopperExtension{}, fmt.Errorf("@shopperFormExtension on %q: the mutation binds no "+
			"concept (%q), so the domain declaring the extension is unknowable", fn.Name, fn.BoundConcept)
	}

	ext := ShopperExtension{
		Domain:      domain,
		Pack:        strings.TrimSpace(fn.ShopperFormExtension.Pack),
		Form:        strings.TrimSpace(fn.ShopperFormExtension.Form),
		Construct:   fn.Name,
		Description: fn.Description,
	}

	// D7: the field list is the args block MINUS the stamped names. The
	// stamped ones are DECLARED -- the body reads args.storeId to write it
	// -- and simply never offered to a shopper.
	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			if f == nil {
				continue
			}
			name := strings.TrimSpace(f.Name)
			if name == "" {
				continue
			}
			if _, stamped := reservedShopperFieldNames[name]; stamped {
				continue
			}
			ext.Fields = append(ext.Fields, ShopperField{
				Name:        name,
				Required:    !f.Optional,
				MaxLength:   f.MaxLength,
				Enum:        enumStrings(f.Enum),
				Numeric:     isNumericArgType(f.Type),
				Description: f.Description,
			})
		}
	}
	if len(ext.Fields) == 0 {
		return ShopperExtension{}, fmt.Errorf("@shopperFormExtension on %q declares no field of "+
			"its own -- every argument it takes is stamped server-side, so it adds nothing to "+
			"the form it extends", fn.Name)
	}
	return ext, nil
}

// registerShopperExtensions is the boot pass.
//
// IT CLEARS FIRST. The registry must reflect the tree as it now is: a
// domain dropped from a bundle, or a pack disabled under it, has to stop
// extending, and a half-cleared registry after a failed load would be
// worse than either outcome.
//
// Every refusal is RETURNED rather than logged-and-skipped, so strict boot
// decides. An extension that silently failed to register is a form that
// quietly stops collecting a client's fields, which is the failure mode
// this whole seam exists to end -- it would look exactly like the silent
// drop that made the seam necessary.
func registerShopperExtensions(functions *FunctionRegistry) []error {
	ResetShopperExtensions()
	if functions == nil {
		return nil
	}
	// SORTED, so a tree with two faults names them in the same order on
	// every boot and on every machine. Registry iteration is a map walk.
	var declared []*Function
	for _, fn := range functions.List() {
		if fn != nil && fn.ShopperFormExtension != nil {
			declared = append(declared, fn)
		}
	}
	sort.Slice(declared, func(i, j int) bool { return declared[i].Name < declared[j].Name })

	var problems []error
	var inert []string
	for _, fn := range declared {
		ext, err := shopperExtensionFromFunction(fn)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if err := RegisterShopperExtension(ext); err != nil {
			// AN INERT PACK IS NOT A PROBLEM. A storefront pack ships
			// disabled, so this is the state a freshly installed cluster is
			// in, and refusing the load would mean a product that extends a
			// pack cannot boot until the pack is enabled -- on a cluster
			// that will not start.
			if errors.Is(err, errShopperPackInert) {
				inert = append(inert, fn.Name+" -> "+ext.Pack+"/"+ext.Form)
				continue
			}
			problems = append(problems, err)
		}
	}
	if len(inert) > 0 {
		// Reported, never silent: an operator who enabled a pack and still
		// sees no client fields needs this line to exist.
		sort.Strings(inert)
		shopperExtensionsInert = inert
	} else {
		shopperExtensionsInert = nil
	}
	return problems
}

// shopperExtensionsInert names the extensions skipped because their pack
// declares no shopper form on this cluster. Read by the boot log.
var shopperExtensionsInert []string

// ShopperExtensionsInert reports extensions waiting on a pack to be enabled.
func ShopperExtensionsInert() []string { return append([]string(nil), shopperExtensionsInert...) }

func describeFunctionKind(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return "query"
	}
	return kind
}

func isNumericArgType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "number", "int", "integer":
		return true
	}
	return false
}

// enumStrings narrows a declared @enum to the string members a form can
// send. @enum takes string literals, so anything else is not expressible.
func enumStrings(values []any) []string {
	var out []string
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mutationShopperFormExtension reads @shopperFormExtension(pack=, form=)
// off a mutation definition.
//
// Both keys are REQUIRED and neither has a default. A default pack would
// silently attach a client's fields to whichever route the engine guessed,
// and the guess would be wrong on the first cluster running two packs.
func mutationShopperFormExtension(funcDef *languageParser.FunctionDef) (*ShopperFormExtensionDecl, error) {
	if funcDef == nil {
		return nil, nil
	}
	var decl *ShopperFormExtensionDecl
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrShopperFormExtension {
			continue
		}
		if decl != nil {
			return nil, fmt.Errorf("@shopperFormExtension is written twice; a mutation extends " +
				"one form, and a map of keyword arguments collapses last-wins so the route a " +
				"reader sees would not be the route the engine uses")
		}
		pack := strings.TrimSpace(attrArgString(attr.Args, "pack"))
		form := strings.TrimSpace(attrArgString(attr.Args, "form"))
		if pack == "" || form == "" {
			return nil, fmt.Errorf(`@shopperFormExtension requires both pack= and form=, e.g. ` +
				`@shopperFormExtension(pack="wholesale", form="application")`)
		}
		decl = &ShopperFormExtensionDecl{Pack: pack, Form: form}
	}
	return decl, nil
}

func attrArgString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}
