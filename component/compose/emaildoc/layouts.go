package emaildoc

import (
	"embed"
	"strings"
	"sync"
)

// The built-in layouts are four ready-made emails, each a layout-mode document
// in layouts/<name>.email.json: Confirmation, Letter, Article and Spotlight.
// They are written for a neutral example brand ("Acme", Georgia and Arial, a
// small blue and grey palette with a red accent) and carry no one's copy,
// pictures or logos. A person starts from one, or the Materializer fills one;
// an organization that wants its own look saves a finished email as a layout
// of its own.
//
// What a layout leaves to whoever uses it is what Render reports as issues.
// Every picture is a placeholder (image_missing), and every address is empty
// except the link merge tags: a made-up one would be sent as it is, with
// nothing to say it was never replaced, so a button, a social link or a
// footer link without one is reported (link_missing) until someone gives it a
// destination. The words are an example to write over.
//
// Each block has a role. Brand blocks are the frame an organization keeps the
// same from one email to the next (the logo, dividers, signature, follow links
// and footer, for example) and keep their content when a layout is filled;
// content blocks are what gets written. A block without a role of its own
// inside a section or columns takes the container's.

// layoutFiles holds the documents of the built-in layouts.
//
//go:embed layouts/*.email.json
var layoutFiles embed.FS

// LayoutInfo is a built-in layout as it is listed: the id it is chosen by, the
// name and description a picker shows, and the document itself.
type LayoutInfo struct {
	ID          string // "builtin:confirmation"
	Name        string // "Confirmation"
	Description string
	Document    Document // Layout field equals ID
}

// builtinLayouts is the catalogue, in the order Layouts lists it. An id is
// "builtin:" and the name of the file in layouts/ that holds the document.
var builtinLayouts = [...]struct{ id, name, description string }{
	{"builtin:confirmation", "Confirmation", "A welcome on your brand colour with one button that confirms the subscription."},
	{"builtin:letter", "Letter", "A personal note with a signature band, a promotion and social links."},
	{"builtin:article", "Article", "A banner, a tinted article body with a highlighted statement, and a signature."},
	{"builtin:spotlight", "Spotlight", "One large picture, a short message and one button."},
}

// Layouts lists the built-in layouts: Confirmation, Letter, Article and
// Spotlight, in that order. The documents are the caller's to change; see
// Layout.
func Layouts() []LayoutInfo {
	verifyLayouts()
	list := make([]LayoutInfo, len(builtinLayouts))
	for i, entry := range builtinLayouts {
		list[i] = LayoutInfo{ID: entry.id, Name: entry.name, Description: entry.description, Document: layoutDocument(entry.id)}
	}
	return list
}

// Layout returns the built-in layout with the id "builtin:<name>", as a
// document of its own: a caller may fill it in (the Materializer writes its
// content into the blocks) without changing the layout for the next caller.
// ok is false for any other id.
func Layout(id string) (Document, bool) {
	verifyLayouts()
	for _, entry := range builtinLayouts {
		if entry.id == id {
			return layoutDocument(id), true
		}
	}
	return Document{}, false
}

var layoutsVerified sync.Once

// verifyLayouts reads every embedded layout once, on first use, and panics if
// one is not a document Parse admits or does not name itself: a broken layout
// is a defect of the build that the tests of this package catch, not a
// condition for a caller to handle.
func verifyLayouts() {
	layoutsVerified.Do(func() {
		for _, entry := range builtinLayouts {
			layoutDocument(entry.id)
		}
	})
}

// layoutDocument reads one embedded layout. It parses the file again for each
// caller instead of keeping a Document to hand out, because a Document holds
// pointers and slices that a caller who changes the one it was given would
// change for everyone after it.
func layoutDocument(id string) Document {
	file := "layouts/" + strings.TrimPrefix(id, "builtin:") + ".email.json"
	data, err := layoutFiles.ReadFile(file)
	if err != nil {
		panic("emaildoc: the built-in layout " + id + " is not embedded: " + err.Error())
	}
	d, err := Parse(data)
	if err != nil {
		panic("emaildoc: the built-in layout " + id + " is not a valid email document: " + err.Error())
	}
	if d.Mode() != ModeLayout || d.Layout != id {
		panic("emaildoc: " + file + " must be a layout that names itself " + id)
	}
	return d
}
