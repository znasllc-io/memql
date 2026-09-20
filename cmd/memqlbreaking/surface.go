package main

// The authoring SURFACE of one tree: the thing a bundle author writes against,
// captured as data so two of them can be diffed (memql#5389, D21).
//
// It is deliberately NOT the whole spec. A surface holds the names and shapes a
// change to which breaks somebody else's file; it excludes documentation,
// ordering and anything generated FROM the surface, because a diff that reports
// a reworded doc comment is a diff whose real findings are buried.
//
// # What is in, and why each one earns its place
//
//   - Constructs, from the parser's own keyword tables through dslspec: the
//     word a declaration opens with, the signature it takes, and the clauses
//     its body admits. Take any of the three away and a file stops parsing.
//   - Annotations, from component/language/annotations -- the registry every
//     parser checks an annotation against, at PARSE time. An annotation's
//     surface is three facts: the name, the receivers that accept it, and the
//     argument contract each of those receivers holds it to.
//   - Functions, from component/language/functions: every call a v1 expression
//     may make, by the key it is found under, with its signature and tier.
//   - Shapes, from the DSL tree itself: each shape's projected keys, which are
//     the names a CLIENT reads a row by. They are the wire, and they are the
//     one half of the surface that is not derivable from the Go registries.
//
// # What is deliberately out
//
//   - Every doc string, on every construct, annotation, function and operator.
//     Documentation is the thing that changes most and breaks nothing.
//   - Ordering. The surface is maps and sorted key lists; a declaration moving
//     in its file is not a finding.
//   - GrammarVersion as a DIFFABLE fact. It is captured, because a baseline
//     that does not say which grammar it was taken from is a baseline nobody
//     can date -- but it is not compared. The constant bumps on every grammar
//     epic, WIDENINGS INCLUDED, so comparing it would report a break on every
//     addition; and whatever break the bump does carry is enumerated, by name,
//     by the rest of this surface. See Diff's own comment.
//   - Operators. They have no reservation section in the ledger and no
//     retirement table keyed by symbol, so an operator break cannot be
//     accepted the way a name can. Stated here as a boundary rather than left
//     for a reader to discover from an absence (memql#5389 follow-up).

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	langast "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslspec"
	"github.com/znasllc-io/memql/component/language/functions"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// Item is one named thing on the surface. Form is its contract where it has
// one; On is where it may be written.
type Item struct {
	Name string   `json:"name"`
	Form string   `json:"form,omitempty"` // the argument form, where one exists
	On   []string `json:"on,omitempty"`   // the receivers that accept it
}

// Surface is one tree's authoring surface, as a baseline file holds it.
type Surface struct {
	Edition        string              `json:"edition"`
	GrammarVersion string              `json:"grammarVersion"`
	Constructs     map[string]Item     `json:"constructs"`
	Annotations    map[string]Item     `json:"annotations"`
	Functions      map[string]Item     `json:"functions"`
	Shapes         map[string][]string `json:"shapes"`
}

// Capture builds the REGISTRY half of the surface of the tree this binary was
// compiled from: constructs, annotations and functions, plus the two version
// facts. It touches no filesystem and cannot fail.
//
// The shapes half is CaptureShapes, which loads the DSL tree and therefore can.
// The split is load-bearing rather than tidy: the changes this command exists
// to classify are exactly the ones that ALSO stop the tree loading -- remove an
// annotation the tree itself writes and every file carrying it refuses -- so a
// capture that failed as one unit would refuse to classify at the commit it was
// built for. Two halves means the registry finding is still produced, named,
// while the shape half is reported as unread. main.go holds the rest of that
// reasoning.
func Capture() Surface {
	return Surface{
		Edition:        langparser.Edition,
		GrammarVersion: langparser.GrammarVersion,
		Constructs:     captureConstructs(),
		Annotations:    captureAnnotations(),
		Functions:      captureFunctions(),
		Shapes:         map[string][]string{},
	}
}

// captureConstructs projects every top-level construct: the keyword, the
// signature form it takes, and the body clauses it admits.
//
// The DOC is dropped on purpose. So is Category, which buckets a construct for
// an editor's sections and is not something an author writes.
func captureConstructs() map[string]Item {
	out := map[string]Item{}
	for _, c := range dslspec.Build().Constructs {
		form := c.Keyword + " <name>"
		if c.ConceptInSignature {
			form = c.Keyword + " <Concept> <name>"
		}
		blocks := append([]string(nil), c.BodyBlocks...)
		sort.Strings(blocks)
		out[c.Keyword] = Item{Name: c.Keyword, Form: form, On: blocks}
	}
	return out
}

// captureAnnotations projects the annotation registry: for each name, the
// receivers that accept it and the argument contract they hold it to.
//
// The registry is read directly rather than through dslspec.Build(), because
// the spec's per-annotation view carries the receivers and the doc but not the
// FORMS -- and the forms are most of what an annotation's contract is. The two
// cannot disagree: dslspec projects the same placements.
func captureAnnotations() map[string]Item {
	byName := map[string][]annotations.Placement{}
	for _, p := range annotations.Placements() {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := make(map[string]Item, len(byName))
	for name, ps := range byName {
		recvs := make([]string, 0, len(ps))
		for _, p := range ps {
			recvs = append(recvs, string(p.Receiver))
		}
		sort.Strings(recvs)
		out[name] = Item{Name: name, Form: annotationForm(ps), On: recvs}
	}
	return out
}

// annotationForm renders one annotation's argument contract across every
// receiver that accepts it.
//
// Receivers sharing a contract are grouped, so the common case -- one contract
// everywhere -- renders as the contract alone and a diff of it reads as what it
// is. Where receivers genuinely differ, each group names its receivers, because
// collapsing them would hide a narrowing on one receiver behind a sibling that
// did not move.
func annotationForm(ps []annotations.Placement) string {
	groups := map[string][]string{}
	for _, p := range ps {
		f := placementForm(p)
		groups[f] = append(groups[f], string(p.Receiver))
	}
	if len(groups) == 1 {
		for f := range groups {
			return f
		}
	}
	rendered := make([]string, 0, len(groups))
	for f, recvs := range groups {
		sort.Strings(recvs)
		rendered = append(rendered, strings.Join(recvs, ",")+": "+f)
	}
	sort.Strings(rendered)
	return strings.Join(rendered, "; ")
}

// placementForm renders one placement's contract: the argument forms it
// accepts, its closed keyword-key set with each key's type, and whether it may
// be written more than once. The EXAMPLE and the DOC are left out -- both are
// prose about the contract rather than the contract.
func placementForm(p annotations.Placement) string {
	form := p.Forms.String()
	if len(p.Keys) > 0 {
		keys := make([]string, 0, len(p.Keys))
		for _, k := range p.Keys {
			keys = append(keys, k.Name+":"+k.Type)
		}
		sort.Strings(keys)
		form += "(" + strings.Join(keys, ", ") + ")"
	}
	if p.Repeatable {
		form += " repeatable"
	}
	return form
}

// captureFunctions projects the function catalog, keyed the way a call finds
// an entry: the bare name for a function, receiver.name for a method.
//
// The Form carries the signature and the TIER. A tier move is a contract
// change an author feels: a P function lowers to SQL, and the same filter that
// pushed down yesterday refuses to lower today.
func captureFunctions() map[string]Item {
	cat := functions.Catalog()
	out := make(map[string]Item, len(cat))
	for _, f := range cat {
		out[f.Key()] = Item{Name: f.Key(), Form: f.Signature() + " [" + string(f.Tier) + "]"}
	}
	return out
}

// CaptureShapes loads the DSL tree and projects every shape's template keys.
//
// It goes through dslimports.Load, which is the pipeline cmd/memqllint runs and
// the one the engine's own loader is built on -- not a second reader of .memql
// files. A tree that does not load is an error here rather than a partial
// capture: the shapes half of a baseline is the wire contract, and a silently
// short one reports the missing shapes as deleted on the very next run.
//
// The key of each path is its TERMINAL segment, which is what the engine's
// shape converter keys the template entry by (component/memql/shape_converter.go).
//
// A shape with an EMPTY BODY takes the default projection over its bound
// concept -- a third of the tree's shapes, and resolving their keys needs the
// concept registry this command does not build. Recording nothing for them
// would leave that third of the wire surface silent, so each is recorded with
// one pseudo-key naming its binding, `@concept=<name>`. A real key is a simple
// identifier, so the `=` cannot collide with one. What that buys is the break
// that IS visible from here: a body-less shape rebound to another concept
// projects an entirely different key set, and nothing else would report it.
// The keys themselves are the bound concept's projectable fields, and removing
// one of those is gated by the sibling ledger,
// component/conceptfields/concept-fields.snapshot.json.
func CaptureShapes(root fs.FS) (map[string][]string, error) {
	tree, err := dslimports.Load(root)
	if err != nil {
		return nil, fmt.Errorf("loading the DSL tree for the shape surface: %w", err)
	}
	out := map[string][]string{}
	seen := map[string]string{} // shape name -> the file that declared it first
	paths := make([]string, 0, len(tree.Files))
	for p := range tree.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		file := tree.Files[p]
		if file == nil {
			continue
		}
		for _, decl := range file.Definitions {
			shape, ok := decl.(*langast.ShapeDecl)
			if !ok || shape.Name == "" {
				continue
			}
			// A duplicate shape NAME across two domains is a defect the
			// engine's own registry has, not one to paper over here: record it
			// so the capture says so rather than letting the second silently
			// replace the first.
			if first, dup := seen[shape.Name]; dup {
				return nil, fmt.Errorf("two shapes are named %q (%s and %s); the surface keys a shape by its name, "+
					"so one would silently replace the other -- rename one", shape.Name, first, p)
			}
			seen[shape.Name] = p
			out[shape.Name] = shapeKeys(shape)
		}
	}
	return out, nil
}

// shapeKeys returns the template keys a shape body projects, sorted and
// de-duplicated. Order is not part of the surface, so it is normalised away.
// A body-less shape answers with its binding instead; CaptureShapes says why.
func shapeKeys(shape *langast.ShapeDecl) []string {
	if len(shape.Paths) == 0 {
		if shape.SignatureConcept == "" {
			return []string{}
		}
		return []string{"@concept=" + shape.SignatureConcept}
	}
	set := map[string]bool{}
	for _, raw := range shape.Paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if i := strings.LastIndexByte(p, '.'); i >= 0 && i+1 < len(p) {
			p = p[i+1:]
		}
		if p != "" {
			set[p] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// retiredEverywhereNames is every annotation the registry retires on every
// receiver that does not accept it. It is the population the reservation
// ledger has to cover, and it is read from the retirement table rather than
// restated, so a retirement landing without a reservation is caught by the
// test that reads this and not by whoever happens to remember.
func retiredEverywhereNames() []string {
	var out []string
	for _, r := range annotations.Retirements() {
		if r.Receiver != "" || r.Prefix {
			continue
		}
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}
