// Package compose is the Materializer's pure half: what a composed draft
// becomes as bytes, and what provenance those bytes carry (epic
// memql#4977, design record
// docs/superpowers/specs/2026-09-05-compose-materializer-design.md).
//
// THE ONE INVARIANT THIS PACKAGE EXISTS TO HOLD is honesty rather than
// universality: every format either embeds the provenance record in its
// own bytes, or reports that it cannot and says why in a sentence a
// person reads. There is no third outcome, no silent omission, and no
// invented metadata channel -- which is what
// TestEveryFormatAnswersProvenance checks, over the whole format table,
// with no cluster anywhere near it.
package compose

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Format is what a composition produces. The set is exactly what this
// package can WRITE (design D3).
//
// AUDIO AND VIDEO ARE ABSENT ON PURPOSE, and their absence is a
// statement rather than a gap. The epic's brief names both; audio wants
// a compose-then-speak pipeline with a provider dependency and a cost
// ceiling of its own, and video wants a generation provider this cluster
// has none of. A value here that nothing could write would be a form
// somebody can fill in and nothing can store -- the reasoning
// v1:platform:site.kind gives for having no ios/android value -- so the
// SURFACE names them and says why, and this table does not carry them.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatHTML     Format = "html"
	FormatText     Format = "txt"
	FormatCSV      Format = "csv"
	FormatJSON     Format = "json"
	FormatDOCX     Format = "docx"
	FormatPDF      Format = "pdf"
)

// Formats is every format this package can write, in the order a person
// meets them in the Target column: the ones somebody reads first, then
// the ones a machine reads, then the two that are documents.
func Formats() []Format {
	return []Format{FormatMarkdown, FormatHTML, FormatText, FormatCSV, FormatJSON, FormatDOCX, FormatPDF}
}

// ParseFormat resolves a caller-supplied format name.
//
// AN UNKNOWN NAME IS AN ERROR RATHER THAN A DEFAULT. Defaulting to
// markdown would answer "audio" with a text file and call it success,
// which is the shape of failure this whole package is built to avoid.
func ParseFormat(s string) (Format, error) {
	want := Format(strings.ToLower(strings.TrimSpace(s)))
	for _, f := range Formats() {
		if f == want {
			return f, nil
		}
	}
	if want == "audio" || want == "video" {
		return "", fmt.Errorf("compose: %q is named in the brief and not offered -- audio needs a compose-then-speak pipeline with a cost ceiling of its own, video a generation provider this cluster has none of", want)
	}
	return "", fmt.Errorf("compose: unknown format %q (offered: %s)", s, joinFormats(Formats()))
}

// MimeType is what the Library file row records for this format, and
// what the content route answers with.
func (f Format) MimeType() string {
	switch f {
	case FormatMarkdown:
		return "text/markdown"
	case FormatHTML:
		return "text/html"
	case FormatText:
		return "text/plain"
	case FormatCSV:
		return "text/csv"
	case FormatJSON:
		return "application/json"
	case FormatDOCX:
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case FormatPDF:
		return "application/pdf"
	}
	return "application/octet-stream"
}

// Extension is the filename suffix, without the dot.
func (f Format) Extension() string {
	if f == FormatMarkdown {
		return "md"
	}
	return string(f)
}

// LibraryFormat is the v1:library:file.format enum value this format
// maps to. That enum is the Library's own high-level classification and
// is deliberately COARSER than this one -- markdown and txt are
// different things to write and the same thing to browse.
func (f Format) LibraryFormat() string {
	switch f {
	case FormatMarkdown:
		return "markdown"
	case FormatHTML, FormatText, FormatCSV, FormatJSON:
		return "text"
	case FormatDOCX:
		return "document"
	case FormatPDF:
		return "pdf"
	}
	return "other"
}

// CarriesProvenance reports whether this format has a metadata channel
// the stamp step can write into (design D4).
//
// THE FALSE HALF IS THE INTERESTING ONE. txt, csv and json are written
// EXACTLY as composed -- nothing prepended, nothing appended, no
// companion file -- because a comment header in a CSV breaks the
// spreadsheet somebody opens it in, and a provenance key in a JSON
// document changes the shape a program parses. The record is then the
// only provenance that file has, and the row and the app both say so.
func (f Format) CarriesProvenance() bool {
	switch f {
	case FormatMarkdown, FormatHTML, FormatDOCX, FormatPDF:
		return true
	case FormatText, FormatCSV, FormatJSON:
		return false
	}
	return false
}

// ProvenanceNote is the sentence the app renders beside a composition
// whose output carries no provenance of its own, and the sentence
// stamped onto the row.
//
// IT NAMES THE FORMAT AND THE REASON, never just "no provenance". A
// false flag with no account of itself reads as a failure, and this is
// not one -- it is the honest consequence of a file type that has
// nowhere to put it.
func (f Format) ProvenanceNote() string {
	if f.CarriesProvenance() {
		switch f {
		case FormatMarkdown:
			return "Provenance is in this file's YAML front matter."
		case FormatHTML:
			return "Provenance is in this file's <meta> block."
		case FormatDOCX:
			return "Provenance is in this document's core and custom properties."
		case FormatPDF:
			return "Provenance is in this PDF's XMP packet and Info dictionary."
		}
	}
	return strings.ToUpper(string(f)) + " carries no metadata channel -- the record in the Materializer is the only provenance this file has."
}

// ModelContribution is one model that contributed to a composition.
// A LIST OF THESE rather than a single field is the epic's own wording:
// "every model that contributed". A composition drafted by one model and
// tightened by another has two entries, and one field would make the
// second invisible -- which is precisely the fact a provenance record
// exists to carry.
type ModelContribution struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Calls    int    `json:"calls"`
	Tokens   int    `json:"tokens"`
}

// Source is one thing a composition was made from, as the provenance
// record names it.
type Source struct {
	// Kind is concept_row, library_file or query.
	Kind string `json:"kind"`
	// Ref is a BARE id for the row and file kinds and a rendered call
	// string for the query kind. Never a composed canonical id: the
	// identifiers contract has clients neither composing nor parsing
	// those, and a provenance record is read by people.
	Ref string `json:"ref"`
	// Label is what a person calls it.
	Label string `json:"label"`
	// CapturedAt is when the source was resolved, which is what makes
	// the difference between two runs of one recipe explainable.
	CapturedAt time.Time `json:"capturedAt"`
}

// Provenance is what gets written into a file that can hold it, and what
// the composition row records for one that cannot. It is deliberately
// FLAT and small: a provenance block a person cannot read at a glance in
// a document's properties dialog is one nobody checks.
type Provenance struct {
	// Title is the composition's name.
	Title string
	// Statement is what the person asked for, in their own words.
	Statement string
	// AuthorName is the person the composition belongs to, as a human
	// reads it. AuthorId is their v1:identity:user id.
	AuthorName string
	AuthorId   string
	// Instance is the cluster's domain -- WHICH MemQL made this, which
	// is the question somebody holding the file six months later has.
	Instance string
	// CompositionId is the record's own id, so a file in somebody's
	// downloads folder can be traced back to the row that explains it.
	CompositionId string
	// GoalId is the v1:work:goal this materialization was.
	GoalId string
	// TemplateName is what it was rendered through, when it was.
	TemplateName string
	// Sources and Models are the two lists the epic names.
	Sources []Source
	Models  []ModelContribution
	// CreatedAt is when the bytes were written.
	CreatedAt time.Time
}

// ModelSummary renders the models list as one line, for the formats
// whose metadata channel holds strings rather than structures.
//
// AN EMPTY LIST RENDERS "no model" AND THAT IS THE POINT. A re-run that
// matched the catalog reached no provider at all, and that is the
// product's headline claim rather than a missing value -- so it is
// stated, not left blank for a reader to interpret as "unrecorded".
func (p Provenance) ModelSummary() string {
	if len(p.Models) == 0 {
		return "no model (replayed from a recorded run)"
	}
	parts := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		name := strings.TrimSpace(m.Model)
		if name == "" {
			name = "unnamed model"
		}
		if provider := strings.TrimSpace(m.Provider); provider != "" {
			name = provider + "/" + name
		}
		if m.Calls > 1 {
			name = fmt.Sprintf("%s (%d calls)", name, m.Calls)
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

// SourceSummary renders the sources list as one line, in the same
// vocabulary the app uses.
func (p Provenance) SourceSummary() string {
	if len(p.Sources) == 0 {
		return "no sources"
	}
	byKind := map[string]int{}
	for _, s := range p.Sources {
		byKind[s.Kind]++
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", byKind[k], humanKind(k, byKind[k])))
	}
	return strings.Join(parts, ", ")
}

func humanKind(kind string, n int) string {
	var one, many string
	switch kind {
	case "concept_row":
		one, many = "row", "rows"
	case "library_file":
		one, many = "file", "files"
	case "query":
		one, many = "query", "queries"
	default:
		one, many = kind, kind
	}
	if n == 1 {
		return one
	}
	return many
}

func joinFormats(fs []Format) string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return strings.Join(out, ", ")
}
