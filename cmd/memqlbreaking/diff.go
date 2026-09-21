package main

// The classifier (memql#5389, D21).
//
// # The three categories, and the one rule that assigns them
//
//   - parse   -- something that WAS ACCEPTED is no longer accepted at all: a
//     name removed from the surface, a receiver removed from an annotation, a
//     body clause removed from a construct, the edition moving. The file that
//     wrote it stops loading.
//   - meaning -- a name that is STILL ACCEPTED now carries a different
//     contract: an annotation's argument form, a function's signature or tier,
//     a construct's signature. The file may still load; what it asks for has
//     changed.
//   - wire    -- a key a CLIENT reads a row by has changed: a shape's projected
//     keys. Nothing refuses to load; something downstream reads a field that is
//     no longer there.
//
// A shape change is very often also a parse change -- @cache(ttl=300) stops
// parsing when @cache narrows to a bare number -- and that is not a flaw in the
// scheme. The distinction the categories draw is the one that decides how an
// author READS the finding: "the word is gone" and "the word is still there and
// wants something else" are different fixes, and the second is the one that
// gets applied to the wrong line when the two are reported identically.
//
// # An ADDITION is never a finding
//
// memqlbreaking reports BREAKS. A command that reports every change is a
// command whose output nobody reads, and the one signal that matters drowns in
// it. A new annotation, a new construct, a new function, a new shape key and a
// widened argument form all produce nothing here.
//
// The single exception is a RESERVED name coming back, and it is the exception
// that makes the ledger mean something: see reserved.go.
//
// # GrammarVersion is captured and NOT compared
//
// parser.GrammarVersion bumps on every grammar epic, widenings included, and
// carries an 8-hex digest of the authored surface that moves whenever anything
// an author may write moves. Comparing it would put a finding on every release
// that added anything -- and the break it would be announcing is, by
// construction, one of the findings below, named. So the baseline records which
// grammar it was taken from, and the report prints both; neither is a break.

import (
	"fmt"
	"sort"
	"strings"
)

// Category is the axis a break falls on.
type Category string

const (
	// CategoryParse: a form that was accepted is refused.
	CategoryParse Category = "parse"
	// CategoryMeaning: the same text is accepted and asks for something else.
	CategoryMeaning Category = "meaning"
	// CategoryWire: a key a client reads by name has changed.
	CategoryWire Category = "wire"
)

// categoryRank orders a report: what stops loading first, what changed meaning
// next, what a client reads last. A reader triages in that order.
var categoryRank = map[Category]int{CategoryParse: 0, CategoryMeaning: 1, CategoryWire: 2}

// Finding is one classified break. Was and Now name both sides wherever the
// change has two, because a finding that names only what is gone leaves the
// reader to search for what replaced it.
type Finding struct {
	Category Category `json:"category"`
	Name     string   `json:"name"`
	Was      string   `json:"was,omitempty"`
	Now      string   `json:"now,omitempty"`
	Detail   string   `json:"detail"`
}

// String renders one finding as the command prints it.
func (f Finding) String() string {
	line := fmt.Sprintf("%-7s %s", string(f.Category), f.Name)
	switch {
	case f.Was != "" && f.Now != "":
		line += fmt.Sprintf(": %s -> %s", f.Was, f.Now)
	case f.Was != "":
		line += ": " + f.Was
	case f.Now != "":
		line += ": " + f.Now
	}
	if f.Detail != "" {
		line += "\n          " + f.Detail
	}
	return line
}

// Diff classifies every difference between two surfaces that breaks somebody
// else's file, and refuses a deletion that carries no reserved entry.
func Diff(old, now Surface, res Reservations) []Finding {
	var fs []Finding
	fs = append(fs, diffEdition(old, now)...)
	fs = append(fs, diffItems("construct", old.Constructs, now.Constructs, res.Constructs)...)
	fs = append(fs, diffItems("annotation", old.Annotations, now.Annotations, res.Annotations)...)
	fs = append(fs, diffItems("function", old.Functions, now.Functions, res.Functions)...)
	fs = append(fs, diffShapes(old.Shapes, now.Shapes)...)
	SortFindings(fs)
	return fs
}

// SortFindings orders a report: what stops loading first, what changed meaning
// next, what a client reads last, and alphabetically within each. It is its
// own function because a run that also holds the tree to the BASE COMMIT's
// ledger appends DiffLedger's findings to these, and two orderings of one
// report is two reports.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if categoryRank[fs[i].Category] != categoryRank[fs[j].Category] {
			return categoryRank[fs[i].Category] < categoryRank[fs[j].Category]
		}
		if fs[i].Name != fs[j].Name {
			return fs[i].Name < fs[j].Name
		}
		return fs[i].Was < fs[j].Was
	})
}

// diffEdition reports the edition move.
//
// An empty edition on the OLD side is a baseline taken before the field
// existed, not a move from nothing: comparing it would report a break on the
// first run after the field landed, which is the noise this command cannot
// afford.
func diffEdition(old, now Surface) []Finding {
	if old.Edition == "" || now.Edition == "" || old.Edition == now.Edition {
		return nil
	}
	return []Finding{{
		Category: CategoryParse,
		Name:     "edition",
		Was:      old.Edition,
		Now:      now.Edition,
		Detail: "the language edition moved. It is the one change that RECLASSIFIES every other finding in " +
			"this report: the retired-forms table, the annotation registry and the expression grammar are all " +
			"edition-scoped, so a form that merely changed contract under one edition may be refused outright " +
			"under the next. Read the rest of this report against the new edition, not the old one.",
	}}
}

// diffItems classifies one named bucket: constructs, annotations or functions.
//
// Three things happen here and their ORDER is the product:
//
//  1. A name that left with no reservation is a parse break. A name that left
//     WITH one is nothing -- that is what the ledger buys.
//  2. A name that ARRIVED and is reserved is a meaning break. An addition is
//     never a finding; a resurrection is not an addition.
//  3. A name present on both sides is compared receivers-first. When a receiver
//     was removed, the form comparison is SKIPPED: the form string is rendered
//     across the receivers, so a receiver leaving moves it too, and reporting
//     both would put two findings on one change.
func diffItems(kind string, old, now map[string]Item, reserved map[string]string) []Finding {
	var fs []Finding
	for _, name := range sortedKeys(old) {
		o := old[name]
		n, present := now[name]
		if !present {
			if _, ok := reserved[name]; ok {
				continue
			}
			fs = append(fs, Finding{
				Category: CategoryParse,
				Name:     name,
				Was:      o.Form,
				Detail: fmt.Sprintf("the %s %q was removed from the authoring surface and carries no entry in the "+
					"reservation ledger. Every file that writes it stops loading. If the removal is deliberate, "+
					"reserve the name: an unreserved deletion leaves the name free to be given to something else "+
					"later, and a bundle still carrying the old spelling would then load under the new meaning and "+
					"do the new thing silently.", kind, name),
			})
			continue
		}
		fs = append(fs, diffOneItem(kind, o, n)...)
	}
	for _, name := range sortedKeys(now) {
		if _, wasThere := old[name]; wasThere {
			continue
		}
		note, ok := reserved[name]
		if !ok {
			continue // an addition is never a finding
		}
		fs = append(fs, Finding{
			Category: CategoryMeaning,
			Name:     name,
			Now:      now[name].Form,
			Detail: fmt.Sprintf("the %s %q is RESERVED and has come back. The ledger says: %s\n          "+
				"A reserved name may never return with another meaning -- files in the wild still carry the old "+
				"spelling, and they would load under the new one without a word. If this really is the same thing "+
				"returning, remove its ledger entry in this same change, so the un-reservation is a line a reviewer "+
				"reads rather than an absence nobody sees.", kind, name, note),
		})
	}
	return fs
}

// diffOneItem compares a name present on both sides.
func diffOneItem(kind string, o, n Item) []Finding {
	if gone := missing(o.On, n.On); len(gone) > 0 {
		return []Finding{{
			Category: CategoryParse,
			Name:     o.Name,
			Was:      strings.Join(o.On, ", "),
			Now:      strings.Join(n.On, ", "),
			Detail: fmt.Sprintf("the %s %q is no longer accepted on %s. It still exists, so a search for the name "+
				"finds it and the refusal reads as a typo; every file that writes it THERE stops loading.",
				kind, o.Name, strings.Join(gone, ", ")),
		}}
	}
	if o.Form != n.Form {
		return []Finding{{
			Category: CategoryMeaning,
			Name:     o.Name,
			Was:      o.Form,
			Now:      n.Form,
			Detail: fmt.Sprintf("the %s %q still exists and its contract changed. The same text is read against the "+
				"new form, so a file that keeps loading may now be asking for something it did not ask for.",
				kind, o.Name),
		}}
	}
	return nil
}

// diffShapes classifies the wire half: a shape's projected keys are the names a
// client reads a row by.
//
// A shape that disappeared is ONE finding naming the shape, not one per key: a
// finding per key of a deleted shape buries the single fact that matters under
// its own consequences.
//
// A key that left while exactly one arrived is reported as the RENAME it almost
// certainly is, naming both sides. With more than one on either side the
// pairing is a guess, so each departure is reported on its own and the
// arrivals are listed as candidates rather than asserted as replacements.
func diffShapes(old, now map[string][]string) []Finding {
	var fs []Finding
	for _, name := range sortedKeys(old) {
		before := old[name]
		after, present := now[name]
		if !present {
			fs = append(fs, Finding{
				Category: CategoryWire,
				Name:     name,
				Was:      strings.Join(before, ", "),
				Detail: fmt.Sprintf("the shape %q was removed. Every client projecting through it reads nothing; "+
					"nothing refuses to load, which is why this class reaches a user rather than a build.", name),
			})
			continue
		}
		gone, arrived := missing(before, after), missing(after, before)
		if len(gone) == 0 {
			continue
		}
		if len(gone) == 1 && len(arrived) == 1 {
			fs = append(fs, Finding{
				Category: CategoryWire,
				Name:     name,
				Was:      gone[0],
				Now:      arrived[0],
				Detail: fmt.Sprintf("the shape %q projects %q where it projected %q. A client reading the old key "+
					"gets an absent value, not an error.", name, arrived[0], gone[0]),
			})
			continue
		}
		for _, key := range gone {
			f := Finding{
				Category: CategoryWire,
				Name:     name,
				Was:      key,
				Detail: fmt.Sprintf("the shape %q no longer projects %q. A client reading it gets an absent value, "+
					"not an error.", name, key),
			}
			if len(arrived) > 0 {
				f.Detail += fmt.Sprintf(" Keys that arrived in the same change, any of which may be the replacement: %s.",
					strings.Join(arrived, ", "))
			}
			fs = append(fs, f)
		}
	}
	return fs
}

// missing returns the members of a that are absent from b, sorted.
func missing(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, v := range b {
		have[v] = true
	}
	var out []string
	for _, v := range a {
		if !have[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns a map's keys in a stable order, so two runs over the same
// pair of surfaces produce byte-identical reports.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
