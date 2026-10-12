// Package emaildoc is the email document: the structured `*.email.json` file
// an email template is written in, the strict parser that admits it, and the
// renderer that writes it as an email.
//
// A document has exactly one of two modes:
//
//   - Layout mode: {subject, preheader, layout, theme, blocks}. The email is a
//     column of typed blocks (images, headings, Markdown text, buttons,
//     sections, columns, a footer, ...) coloured and set in the type of a
//     theme.
//   - Custom mode: {subject, preheader, textBody, htmlBody}. Pasted or
//     hand-written HTML with its plain-text alternative, as email templates
//     were before layouts.
//
// Parse is the way in. It refuses anything it does not understand (an unknown
// key, a colour that is not a theme colour, a link that is not a web address,
// markup outside the allowlist) with a sentence that names the block and the
// problem. JSON writes the file back: one canonical byte form, so a document
// that is parsed and written again is byte for byte what it was. Render writes
// the email: its HTML and plain-text parts, and the issues that keep it from
// being sent as it is. Render expects a document Parse returned. Given one
// built any other way it promises only what its own comment says (no markup,
// style, link or picture source a block was not meant to have, and no part
// past MaxRenderBytes); values Parse would refuse are otherwise written as
// they are.
//
// Layouts and Layout hand out the four built-in layouts, ready-made documents
// in a neutral example brand to start an email from. Layouts says what they
// hold and what they leave to whoever uses them.
//
// This package lives in the component/compose module. It may import only what
// that module already requires plus the parent package, and the parent must
// never import it: the parent's renderer sources travel inside rebuilt source
// packages (source_package.go).
package emaildoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

// Limits of a document.
const (
	// MaxBytes is the largest document Parse reads.
	MaxBytes = 2 << 20
	// MaxImages is the most distinct pictures one email may carry.
	MaxImages = 8
	// MaxImageBytes is the most picture data one email, or one picture, may carry.
	MaxImageBytes = 1 << 20
	// MaxBlocks is the most blocks one document may hold, counting every block
	// inside a section or a column.
	MaxBlocks = 300
)

// ErrFileTooLarge is what Parse returns for a file of more than MaxBytes. It is
// a value so that a caller who measures a document before it hands it to Parse,
// to spend nothing on one that is too large, says the same sentence.
var ErrFileTooLarge = errors.New("This email file is larger than " + strconv.Itoa(MaxBytes>>20) + " MiB. Use fewer or smaller pictures.")

// The link merge tags. A link may be exactly one of these (they are replaced
// with a per-recipient address when the email is sent) and no other merge tag
// may stand in for an address.
const (
	TagUnsubscribeURL = "{{unsubscribeUrl}}"
	TagPreferencesURL = "{{preferencesUrl}}"
	TagViewURL        = "{{viewUrl}}"
	TagConfirmURL     = "{{confirmUrl}}"
)

var linkTags = [...]string{TagUnsubscribeURL, TagPreferencesURL, TagViewURL, TagConfirmURL}

// LinkTags returns the four link merge tags, in the order of the constants
// above. The slice is the caller's to change.
func LinkTags() []string {
	return append([]string(nil), linkTags[:]...)
}

// Mode says which kind of document this is.
type Mode int

const (
	// ModeLayout is a document made of blocks.
	ModeLayout Mode = 1
	// ModeCustom is a document made of an HTML body and a text body.
	ModeCustom Mode = 2
)

// Theme is the colours and fonts blocks draw from. Blocks name a colour by its
// token (page, canvas, primary, secondary, accent, tint, text or muted, plus
// white); they never carry a colour value of their own.
type Theme struct {
	Page        string `json:"page"` // every colour "#rrggbb"
	Canvas      string `json:"canvas"`
	Primary     string `json:"primary"`
	Secondary   string `json:"secondary"`
	Accent      string `json:"accent"`
	Tint        string `json:"tint"`
	Text        string `json:"text"`
	Muted       string `json:"muted"`
	HeadingFont string `json:"headingFont"`
	BodyFont    string `json:"bodyFont"`
	WebFonts    bool   `json:"webFonts,omitempty"`
}

// Link is one link of a social block (Network, Href, Icon, Width) or of a
// footer block (Label, Href).
type Link struct {
	Label   string `json:"label,omitempty"`   // footer
	Network string `json:"network,omitempty"` // social
	Href    string `json:"href"`
	Icon    string `json:"icon,omitempty"`  // social; same forms as an image src
	Width   int    `json:"width,omitempty"` // social icon width, default 47
}

// Column is one column of a columns block. Width is a percentage.
type Column struct {
	Width       float64  `json:"width"` // percent
	CellPadding *float64 `json:"cellPadding,omitempty"`
	Blocks      []Block  `json:"blocks"`
}

// Block is one block of a layout. Which fields a block may carry depends on its
// Type; Parse refuses a field the type does not have. Lengths are pixels and
// may be fractional, except the integer Width of an image or a button.
type Block struct {
	ID             string      `json:"id"`
	Type           string      `json:"type"`
	Role           string      `json:"role,omitempty"` // "brand" | "content"; empty inherits the enclosing container's role (content at the top level)
	Padding        *[4]float64 `json:"padding,omitempty"`
	Text           string      `json:"text,omitempty"`
	Size           float64     `json:"size,omitempty"`
	LineHeight     float64     `json:"lineHeight,omitempty"`
	ListLineHeight float64     `json:"listLineHeight,omitempty"`
	Gap            *float64    `json:"gap,omitempty"`
	Color          string      `json:"color,omitempty"`
	TextColor      string      `json:"textColor,omitempty"`
	LinkColor      string      `json:"linkColor,omitempty"`
	Background     string      `json:"background,omitempty"`
	Align          string      `json:"align,omitempty"`
	Italic         bool        `json:"italic,omitempty"`
	Weight         int         `json:"weight,omitempty"`
	Font           string      `json:"font,omitempty"` // "heading" | "body" | "base"
	LetterSpacing  string      `json:"letterSpacing,omitempty"`
	Src            string      `json:"src,omitempty"`
	Alt            string      `json:"alt,omitempty"`
	Width          int         `json:"width,omitempty"`
	Height         float64     `json:"height,omitempty"`
	Href           string      `json:"href,omitempty"`
	Bleed          bool        `json:"bleed,omitempty"`
	Label          string      `json:"label,omitempty"`
	PaddingY       float64     `json:"paddingY,omitempty"`
	Inset          *[4]float64 `json:"inset,omitempty"`
	MaxWidth       float64     `json:"maxWidth,omitempty"`
	CellPadding    *float64    `json:"cellPadding,omitempty"`
	StackBelow     float64     `json:"stackBelow,omitempty"`
	Blocks         []Block     `json:"blocks,omitempty"`
	Columns        []Column    `json:"columns,omitempty"`
	Title          string      `json:"title,omitempty"`
	Links          []Link      `json:"links,omitempty"`
	Lines          []string    `json:"lines,omitempty"`
	Separator      *string     `json:"separator,omitempty"`
	After          float64     `json:"after,omitempty"`
	HTML           string      `json:"html,omitempty"` // as the author wrote it; Render splices safeMarkup(HTML), never HTML itself
}

// Document is a parsed email. Parse fills the layout-mode fields (Layout,
// Theme, Blocks) or the custom-mode fields (TextBody, HTMLBody), never both.
type Document struct {
	Subject   string
	Preheader string
	Layout    string  // "builtin:<name>" | "org:<id>" | ""; provenance only
	Theme     Theme   // layout mode
	Blocks    []Block // layout mode
	TextBody  string  // custom mode
	HTMLBody  string  // custom mode; may be ""
}

// Mode reports ModeLayout when the document has a block list (even an empty
// one) and ModeCustom otherwise.
func (d Document) Mode() Mode {
	if d.Blocks != nil {
		return ModeLayout
	}
	return ModeCustom
}

// layoutFile and customFile are the two shapes of the file. Their field order
// is the key order of the canonical bytes.
type layoutFile struct {
	Subject   string  `json:"subject"`
	Preheader string  `json:"preheader,omitempty"`
	Layout    string  `json:"layout,omitempty"`
	Theme     Theme   `json:"theme"`
	Blocks    []Block `json:"blocks"`
}

type customFile struct {
	Subject   string `json:"subject"`
	Preheader string `json:"preheader,omitempty"`
	TextBody  string `json:"textBody"`
	HTMLBody  string `json:"htmlBody"` // always written, even when empty
}

// JSON returns the canonical bytes of the file: two-space indentation, keys in
// the order of the format, markup left readable (HTML escaping is off, so "<"
// and "&" are written as they are) and one trailing newline. Parse(d.JSON())
// is d, and a document parsed from canonical bytes writes those bytes again.
func (d Document) JSON() ([]byte, error) {
	var file any
	if d.Mode() == ModeLayout {
		file = layoutFile{Subject: d.Subject, Preheader: d.Preheader, Layout: d.Layout, Theme: d.Theme, Blocks: d.Blocks}
	} else {
		file = customFile{Subject: d.Subject, Preheader: d.Preheader, TextBody: d.TextBody, HTMLBody: d.HTMLBody}
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(file); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
