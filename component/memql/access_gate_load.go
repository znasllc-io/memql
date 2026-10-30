package memql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// access_gate_load.go -- the load-time half of @accessGate (design record
// 2026-10-29, section 7), shaped like shopper_extension_load.go beside it.
//
// An access gate is DSL, which loads during MemQLEngine.Init, so the rules are
// held here: after every construct is in the registry, folded onto the
// LoadReport, and refused by strict boot exactly as an unresolvable tool
// handler is. That tier is also the only one that reaches a product bundle no
// Go test in this repository walks, and the one package analysis reports as
// dsl_refuses_boot BEFORE a deploy, so a wrong order of rollout (a package
// using @accessGate on an engine that has never heard of it) fails safely at
// analysis rather than crash-looping a node.

// accessGateComponent is the LoadReport component a registration fault lands
// under, which is what a reader greps a strict-boot failure for.
const accessGateComponent = "memql.accessGateRegistration"

// accessGateExample is the spelling every message points an author at.
const accessGateExample = `@accessGate(deployable="client-desk", gate="dashboard")`

// AccessGateDecl is the parsed @accessGate(deployable=, gate=) annotation: the
// gate this construct belongs to, on the deployable it belongs to.
//
// It carries the NAMES and nothing derived from them. What the construct takes
// as arguments is its args block, and a second copy of that here is a second
// copy to drift.
type AccessGateDecl struct {
	Deployable string
	Gate       string
}

func (d *AccessGateDecl) clone() *AccessGateDecl {
	if d == nil {
		return nil
	}
	out := *d
	return &out
}

var (
	// The deployable's shape is the manifest's: lowercase, digits and hyphens,
	// at most 63 characters. The gate's must start with a letter, so a gate
	// reads as a word in a URL, an audit row and an operator's list.
	accessDeployablePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	accessGatePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
)

// checkAccessGateNames holds both names to their shapes. Used where the
// annotation is read AND where an entry is made, because a Function can be
// built without going through the reader.
func checkAccessGateNames(deployable, gate string) error {
	if !accessDeployablePattern.MatchString(deployable) {
		return fmt.Errorf("@accessGate deployable=%q is not a deployable name: write 1 to 63 lowercase "+
			"letters, digits or hyphens, starting with a letter or digit, as the site's package names its "+
			"deployable (for example \"client-desk\")", deployable)
	}
	if !accessGatePattern.MatchString(gate) {
		return fmt.Errorf("@accessGate gate=%q is not a gate name: write 1 to 40 lowercase letters, "+
			"digits or hyphens, starting with a letter (for example \"dashboard\")", gate)
	}
	return nil
}

// accessGateDeclaration reads @accessGate(deployable=, gate=) off a query or a
// mutation definition, and nil when there is none.
//
// Both keys are REQUIRED and neither has a default. A default deployable would
// silently attach a construct to whichever product the engine guessed, and a
// default gate to a part of the app its author never named; either guess puts
// the construct in reach of codes it was never meant for.
//
// It reads the annotation off every kind of construct, not only the two it is
// legal on. The annotation registry refuses it elsewhere at parse time, so a
// logic never gets this far; but a reader that quietly ignored the annotation
// on a kind it did not expect would turn a gate somebody believed they had
// written into no gate at all, and the conversion below refuses the kind
// instead.
func accessGateDeclaration(funcDef *languageParser.FunctionDef) (*AccessGateDecl, error) {
	if funcDef == nil {
		return nil, nil
	}
	var decl *AccessGateDecl
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != languageParser.AttrAccessGate {
			continue
		}
		if decl != nil {
			return nil, errors.New("@accessGate is written twice; a construct opens one gate, and a reader " +
				"scanning top-down could not tell which of the two takes effect. Keep one")
		}

		var missing []string
		values := map[string]string{}
		for _, key := range []string{"deployable", "gate"} {
			raw, written := attr.Args[key]
			if !written {
				missing = append(missing, key+"=")
				continue
			}
			text, isString := raw.(string)
			if !isString {
				return nil, fmt.Errorf("@accessGate %s= must be a quoted string, as in %s", key, accessGateExample)
			}
			if text = strings.TrimSpace(text); text == "" {
				missing = append(missing, key+"=")
				continue
			}
			values[key] = text
		}
		if len(missing) > 0 {
			verb := "is"
			if len(missing) > 1 {
				verb = "are"
			}
			return nil, fmt.Errorf("@accessGate requires both deployable= and gate= (%s %s missing), as in %s: "+
				"neither has a default, because a guessed value would attach this construct to a gate its "+
				"author never named", strings.Join(missing, " and "), verb, accessGateExample)
		}
		if err := checkAccessGateNames(values["deployable"], values["gate"]); err != nil {
			return nil, err
		}
		decl = &AccessGateDecl{Deployable: values["deployable"], Gate: values["gate"]}
	}
	return decl, nil
}

// accessGateEntryFromFunction converts a loaded construct carrying @accessGate
// into the registry's own shape.
//
// PURE, and deliberately: everything it refuses is a property of the construct
// alone, so the refusals are testable without an engine. What it CANNOT know --
// whether another construct in its domain claims the same name under the
// deployable -- belongs to loadAccessGates, which holds them all.
//
// Every refusal names the construct and says what to change, because the
// reader is an author with a bundle open, not the engine's developer.
func accessGateEntryFromFunction(fn *Function) (AccessGateEntry, error) {
	if fn == nil || fn.AccessGate == nil {
		return AccessGateEntry{}, errors.New("access gate: no declaration")
	}
	decl := fn.AccessGate

	// A QUERY OR A MUTATION, AND NOTHING ELSE. A logic may call builtins, and a
	// gate pointed at one would put arbitrary builtin calls within reach of
	// anyone holding a code, under the site owner's borrowed authority. A
	// query reads and a mutation writes one concept, and that is the whole
	// blast radius. The annotation registry already confines the annotation to
	// those two receivers; this is the second half, at the only place that
	// sees what the construct actually loaded as. An empty kind is how
	// Function documents a query.
	kind := fn.FunctionKind
	if kind == "" {
		kind = AccessKindQuery
	}
	if kind != AccessKindQuery && kind != AccessKindMutation {
		return AccessGateEntry{}, fmt.Errorf("@accessGate is declared on %q (loaded as %s). It is legal on a "+
			"query or a mutation alone: a logic may call builtins, and a gate pointed at one would put arbitrary "+
			"builtin calls within reach of anyone holding a code. Move the annotation to the query or mutation "+
			"that does the work", fn.Name, describeFunctionKind(fn.FunctionKind))
	}
	if err := checkAccessGateNames(decl.Deployable, decl.Gate); err != nil {
		return AccessGateEntry{}, fmt.Errorf("%q: %w", fn.Name, err)
	}

	// THE DOMAIN THAT DECLARED THE GATE IS THE DOMAIN OF THE CONCEPT THE
	// CONSTRUCT IS BOUND TO (D5), taken from the binding as a shopper
	// extension's is: a binding is a fact about the construct, where a file
	// path is a fact about a checkout. It is what ties a gate to the package
	// that ships it, so that a deployable name two products happen to share
	// does not put one product's constructs in reach of the other's codes. A
	// construct bound to no concept has no domain to be tied by, and a gate
	// belonging to no package would be reachable from every one.
	domain := conceptRootDomain(fn.BoundConcept)
	if domain == "" {
		return AccessGateEntry{}, fmt.Errorf("%q carries @accessGate but binds no concept (%q) from which a DSL "+
			"domain can be read. A gate belongs to the package that ships it, through the domain of the concept "+
			"the construct reads or writes. Bind the construct to a concept of the domain that declares it",
			fn.Name, fn.BoundConcept)
	}

	// THE THREE THAT CONTRADICT A VISITOR'S CALL. It runs under the site
	// owner's borrowed authority and never as internal origin (D6), and each of
	// these is a statement about a different kind of caller.
	if fn.ServerOnly {
		return AccessGateEntry{}, fmt.Errorf("%q carries both @accessGate and @serverOnly, which contradict each "+
			"other: @serverOnly bars every client-originated call and a visitor's call is one, so the gate could "+
			"never open it. Remove @serverOnly, or remove @accessGate and reach the construct through an "+
			"operator surface instead", fn.Name)
	}
	if rank := strings.TrimSpace(fn.RequiresRank); rank != "" {
		return AccessGateEntry{}, fmt.Errorf("%q carries both @accessGate and @requiresRank(%q). A visitor's call "+
			"runs under the site owner's borrowed authority, a synthetic writer-role stand-in, so a rank floor "+
			"would be judged against a role the author did not mean. Remove @requiresRank: the access code is the "+
			"gate, and the construct's filter decides which rows it reaches", fn.Name, rank)
	}
	if fn.RequiresCapability.declared() {
		return AccessGateEntry{}, fmt.Errorf("%q carries both @accessGate and @requiresCapability(%q, %q). A visitor's "+
			"call runs under the site owner's borrowed authority, a synthetic writer-role stand-in, so a capability "+
			"would be judged against a role the author did not mean. Remove @requiresCapability: the access code "+
			"is the gate, and the construct's filter decides which rows it reaches",
			fn.Name, fn.RequiresCapability.Verb, fn.RequiresCapability.Resource)
	}

	entry := AccessGateEntry{
		Deployable: decl.Deployable,
		Gate:       decl.Gate,
		Construct:  fn.Name,
		Kind:       kind,
		Domain:     domain,
	}
	if fn.ArgsSchema == nil {
		return entry, nil
	}
	for _, field := range fn.ArgsSchema.Fields {
		if field == nil {
			continue
		}
		name := strings.TrimSpace(field.Name)
		if name == "" {
			continue
		}
		if _, reserved := reservedAccessArgSet[name]; !reserved {
			entry.Args = append(entry.Args, name)
			continue
		}
		// A STAMPED VALUE IS A STRING. The handler stamps ids and names as
		// text; a reserved argument declared as anything else would be a
		// stamp the construct's own validation refuses on every call, and the
		// only symptom would be a visitor's 502.
		if field.Type != "string" {
			return AccessGateEntry{}, fmt.Errorf("%q declares the reserved argument %s as %s, but the access "+
				"handler stamps it as a string. Declare it as string", fn.Name, name, field.Type)
		}
		entry.Stamped = append(entry.Stamped, name)
	}
	return entry, nil
}

// accessGateFault is one refusal of the registration pass, carrying the
// construct it is about so the LoadReport can name it and package analysis can
// point an author at the file.
type accessGateFault struct {
	Construct string
	Kind      string
	File      string
	Err       error
}

func (f *accessGateFault) Error() string { return f.Err.Error() }
func (f *accessGateFault) Unwrap() error { return f.Err }

// accessGateFile is a construct's file in the DSL tree, as a Skip names it.
func accessGateFile(origin string) string {
	return strings.TrimPrefix(origin, "unified:")
}

// accessGateCandidate is a construct that passed the conversion, waiting to be
// checked against the others.
type accessGateCandidate struct {
	fn    *Function
	entry AccessGateEntry
}

// loadAccessGates is the boot pass.
//
// It builds a fresh view, then publishes it atomically when publish is set. A
// domain dropped from a bundle has to stop opening anything, and a candidate
// validation (a package deploy's analysis) runs the same checks without
// touching what the node is serving.
//
// Every refusal is RETURNED rather than logged-and-skipped, so strict boot
// decides. A gate that silently failed to register is a code whose every call
// is a 404, which looks exactly like a gate nobody wrote. And a refused
// construct is left OUT of the view: the break-glass that lets a node boot
// past a skipped construct must never be the thing that opens a gate somebody
// was told was refused.
func loadAccessGates(functions *FunctionRegistry, publish bool) []error {
	view := map[accessGateSlot]AccessGateEntry{}
	if publish {
		defer func() { publishAccessGates(view) }()
	}
	if functions == nil {
		return nil
	}

	// Range walks in sorted-key order without cloning the whole registry, so a
	// tree with two faults names them in the same order on every boot and on
	// every machine, and only the rare gated construct is copied out.
	var declared []*Function
	functions.Range(func(_ string, fn *Function) bool {
		if fn != nil && fn.AccessGate != nil {
			declared = append(declared, fn.clone())
		}
		return true
	})

	var problems []error
	groups := map[accessGateSlot][]accessGateCandidate{}
	var order []accessGateSlot
	for _, fn := range declared {
		// Judged whether or not it is enabled: @disabled takes a construct out
		// of service, not out of the tree, and it stays a construct that must
		// be valid because re-enabling it is only removing the annotation.
		entry, err := accessGateEntryFromFunction(fn)
		if err != nil {
			problems = append(problems, newAccessGateFault(fn, err))
			continue
		}
		// A disabled construct is out of service. Its call would be refused by
		// the engine anyway; leaving it out answers a visitor with the same 404
		// as any construct nobody declared, and lets a live construct take
		// over its name.
		if !fn.Enabled {
			continue
		}
		// A collision is a collision INSIDE one domain. The same name under the
		// same deployable in another domain is another product's declaration,
		// and the package that ships that domain does not ship this one, so
		// neither can close the other.
		slot := accessGateSlot{entry.Domain, entry.Deployable, entry.Construct}
		if _, seen := groups[slot]; !seen {
			order = append(order, slot)
		}
		groups[slot] = append(groups[slot], accessGateCandidate{fn: fn, entry: entry})
	}

	for _, slot := range order {
		group := groups[slot]
		if len(group) > 1 {
			// BOTH STAY CLOSED. Keeping whichever loaded first would let load
			// order decide which construct a code reaches, and nothing
			// resolves load order.
			problems = append(problems, accessGateDuplicateFault(group))
			continue
		}
		view[slot] = group[0].entry
	}
	return problems
}

func newAccessGateFault(fn *Function, err error) error {
	return &accessGateFault{
		Construct: fn.Name,
		Kind:      describeFunctionKind(fn.FunctionKind),
		File:      accessGateFile(fn.Origin),
		Err:       err,
	}
}

// accessGateDuplicateFault refuses two gated constructs that share a name under
// one deployable in one DSL domain, naming BOTH places: the author has to know
// which two declarations to reconcile, and one name sends them looking for the
// other.
func accessGateDuplicateFault(group []accessGateCandidate) error {
	first := group[0]
	places := make([]string, 0, len(group))
	for _, candidate := range group {
		file := accessGateFile(candidate.fn.Origin)
		if namespace := ConstructNamespaceForOrigin(candidate.fn.Origin); namespace != "" {
			places = append(places, fmt.Sprintf("%s (%s)", namespace, file))
		} else {
			places = append(places, file)
		}
	}
	return &accessGateFault{
		Construct: first.entry.Construct,
		Kind:      describeFunctionKind(first.fn.FunctionKind),
		File:      accessGateFile(first.fn.Origin),
		Err: fmt.Errorf("@accessGate: %q is declared under the deployable %q in the DSL domain %q by %s. A "+
			"visitor's call names its construct without a namespace, so one name under one deployable in one "+
			"domain has to reach one construct, and which of them ran would depend on load order, which "+
			"nothing resolves. Rename one of them",
			first.entry.Construct, first.entry.Deployable, first.entry.Domain, strings.Join(places, " and ")),
	}
}

// accessGateSkip is the LoadReport entry for one refusal of the registration
// pass. Shared by the engine's boot and by the tests, so a refusal is shaped
// the same on a node and under a test.
func accessGateSkip(err error) baseloader.Skip {
	skip := baseloader.Skip{
		Component: accessGateComponent,
		Keyword:   AccessKindQuery,
		Phase:     "resolve",
		Err:       err.Error(),
	}
	var fault *accessGateFault
	if errors.As(err, &fault) {
		skip.Keyword, skip.Name, skip.File = fault.Kind, fault.Construct, fault.File
	}
	return skip
}
