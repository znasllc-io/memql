package emaildoc

import (
	"errors"
	"fmt"
	"strings"
)

// merge.go -- writing an email into a layout.
//
// The Materializer writes an email by filling a layout: its model is shown the
// layout's content blocks (ContentSlots) and answers with what to write in them
// (Content), and Merge puts that into the layout. The layout's structure, theme
// and brand blocks come through untouched, so an email written into an
// organization's layout looks like the rest of its series apart from the words
// and the pictures.
//
// What the model wrote is not trusted to stay inside those lines, so Merge
// refuses, rather than drops, anything that does not: a block the layout does
// not have, a brand block, the same block twice, a field the block cannot
// carry, a picture that was not selected for inclusion, and a link tag
// replaced by an address. A refusal names the block, so the person reading why
// a composition failed can see what was asked for.

// Content is what is written into a layout: the email's subject and preheader,
// and what to write into its content blocks, by id.
type Content struct {
	Subject   string        `json:"subject"`
	Preheader string        `json:"preheader"`
	Blocks    []ContentItem `json:"content"`
}

// ContentItem is what is written into one content block. Each field is either
// set (it has more than white space in it) or not (nil, empty or white space
// only), and a field that is not set leaves the block as the layout has it.
// Text is for heading, text and highlight blocks; Label and Href for buttons;
// Image, Alt and Href for pictures, where Image names one of the pictures
// selected for inclusion by its handle.
type ContentItem struct {
	ID    string  `json:"id"`
	Text  *string `json:"text"`
	Label *string `json:"label"`
	Href  *string `json:"href"`
	Alt   *string `json:"alt"`
	Image *string `json:"image"`
}

// Slot is one content block of a layout as a writer is shown it: what it is,
// what it holds now (an example of the length and tone to write) and how large
// it is drawn. A picture's source is not shown: a writer chooses a picture by
// its handle, never by where one is kept.
type Slot struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Text  string  `json:"text,omitempty"`
	Label string  `json:"label,omitempty"`
	Href  string  `json:"href,omitempty"`
	Alt   string  `json:"alt,omitempty"`
	Size  float64 `json:"size,omitempty"`
	Width int     `json:"width,omitempty"`
}

// The two roles a block has once its container's is taken into account.
const (
	roleBrand   = "brand"
	roleContent = "content"
)

// writable is, for each type of block that is written, the fields of a
// ContentItem it takes. A block of any other type (a section, columns, a
// divider, a spacer, social links, the footer, an html block) takes none.
var writable = map[string]map[string]bool{
	typeHeading:   {"text": true},
	typeText:      {"text": true},
	typeHighlight: {"text": true},
	typeButton:    {"label": true, "href": true},
	typeImage:     {"image": true, "alt": true, "href": true},
}

// eachBlock visits every block of blocks depth first, in document order, with
// its role as Merge reads it: its own, else the role of the container it sits
// in. The blocks of a brand section or brand columns are brand unless they say
// otherwise.
func eachBlock(blocks []Block, container string, visit func(b *Block, role string)) {
	for i := range blocks {
		b := &blocks[i]
		role := b.Role
		if role == "" {
			role = container
		}
		visit(b, role)
		eachBlock(b.Blocks, role, visit)
		for c := range b.Columns {
			eachBlock(b.Columns[c].Blocks, role, visit)
		}
	}
}

// ContentSlots lists the blocks of d that are written when it is filled: its
// content heading, text, highlight, button and image blocks, depth first in
// document order. Brand blocks, containers and blocks with nothing to write are
// never listed.
func ContentSlots(d Document) []Slot {
	var slots []Slot
	eachBlock(d.Blocks, roleContent, func(b *Block, role string) {
		if role != roleContent || writable[b.Type] == nil {
			return
		}
		slot := Slot{ID: b.ID, Type: b.Type, Size: b.Size}
		switch b.Type {
		case typeButton:
			slot.Label, slot.Href, slot.Width = b.Label, b.Href, b.Width
		case typeImage:
			slot.Alt, slot.Href, slot.Width = b.Alt, b.Href, b.Width
		default:
			slot.Text = b.Text
		}
		slots = append(slots, slot)
	})
	return slots
}

// Merge writes c into a copy of layout and returns the email, as Parse reads
// it. images maps the handle of each picture selected for inclusion to the
// source it takes in the email (an artifact: reference or a data: URI).
//
// The rules, for each item of c in order:
//
//   - the same block may be written once;
//   - the block must be one the layout has, and a content block;
//   - only the fields its type takes may be set (text for heading, text and
//     highlight; label and href for a button; image, alt and href for a
//     picture);
//   - a picture must be one of images, by its handle;
//   - a link that is one of the link tags (LinkTags) is kept: it is replaced per
//     recipient when the email is sent, and an address in its place would send
//     the person somewhere else. That is a button's or a picture's link, and a
//     link in a text block's Markdown: new text must link to each tag as often
//     as the layout's did.
//
// A field that is not set leaves the layout's value. The subject is required;
// the preheader is the draft's own, and an empty one leaves the email without
// one, since the layout's is an example written for another email. Values are
// trimmed of surrounding white space. What is written then goes through Parse
// with the rest of the document, so a link or a piece of Markdown it would
// refuse is refused here too. A refusal returns no document, and layout is
// never changed.
func Merge(layout Document, c Content, images map[string]string) (Document, error) {
	if layout.Mode() != ModeLayout {
		return Document{}, errors.New("this email is custom HTML, so it has no blocks to write into")
	}
	if !set(&c.Subject) {
		return Document{}, errors.New("the draft has no subject")
	}
	// A copy of its own, read back from the layout's bytes: a Document holds
	// pointers and slices, and the layout it was handed is often the one a cache
	// or a caller keeps.
	raw, err := layout.JSON()
	if err != nil {
		return Document{}, err
	}
	out, err := Parse(raw)
	if err != nil {
		return Document{}, fmt.Errorf("the layout is not a valid email document: %w", err)
	}
	type target struct {
		block *Block
		role  string
	}
	targets := map[string]target{}
	eachBlock(out.Blocks, roleContent, func(b *Block, role string) {
		targets[b.ID] = target{b, role}
	})

	written := map[string]bool{}
	for _, item := range c.Blocks {
		if written[item.ID] {
			return Document{}, fmt.Errorf("the draft wrote block %s twice", quote(item.ID))
		}
		written[item.ID] = true
		t, ok := targets[item.ID]
		if !ok {
			return Document{}, fmt.Errorf("the draft wrote to a block this layout does not have: %s", quote(item.ID))
		}
		if t.role == roleBrand {
			return Document{}, fmt.Errorf("the draft tried to change a brand block: %s", quote(item.ID))
		}
		b := t.block
		for _, field := range []struct {
			name  string
			value *string
		}{{"text", item.Text}, {"label", item.Label}, {"href", item.Href}, {"alt", item.Alt}, {"image", item.Image}} {
			if set(field.value) && !writable[b.Type][field.name] {
				return Document{}, fmt.Errorf("the draft set %s on %s block %s", field.name, b.Type, quote(item.ID))
			}
		}
		if set(item.Text) {
			text := strings.TrimSpace(*item.Text)
			if tag := droppedLinkTag(b, text); tag != "" {
				return Document{}, fmt.Errorf("the draft tried to replace the %s link on %s", tag, quote(item.ID))
			}
			b.Text = text
		}
		if set(item.Label) {
			b.Label = strings.TrimSpace(*item.Label)
		}
		if set(item.Alt) {
			b.Alt = strings.TrimSpace(*item.Alt)
		}
		if set(item.Image) {
			src, ok := images[strings.TrimSpace(*item.Image)]
			if !ok {
				return Document{}, errors.New("the draft used a picture that was not selected for inclusion")
			}
			b.Src = src
		}
		if set(item.Href) {
			href := strings.TrimSpace(*item.Href)
			if tag := linkTagOf(b.Href); tag != "" && href != tag {
				return Document{}, fmt.Errorf("the draft tried to replace the %s link on %s", tag, quote(item.ID))
			}
			b.Href = href
		}
	}
	out.Subject = strings.TrimSpace(c.Subject)
	out.Preheader = strings.TrimSpace(c.Preheader)
	merged, err := out.JSON()
	if err != nil {
		return Document{}, err
	}
	return Parse(merged)
}

// set reports whether a value of a draft says anything: it is there and holds
// more than white space.
func set(v *string) bool {
	return v != nil && strings.TrimSpace(*v) != ""
}

// linkTagOf returns href when it is one of the link tags, and "" otherwise.
func linkTagOf(href string) string {
	for _, tag := range linkTags {
		if href == tag {
			return tag
		}
	}
	return ""
}

// droppedLinkTag returns a link tag that a text block's Markdown links to and
// text, written in its place, links to fewer times, or "" when it keeps them
// all. A link tag in a text block is a link like a button's (an organization's
// layout may carry its confirm link in a sentence), and the same rule keeps it.
// Text whose Markdown cannot be read is left to Parse, which refuses it with a
// sentence that says why.
func droppedLinkTag(b *Block, text string) string {
	if b.Type != typeText {
		return ""
	}
	before := linkTagLinks(b.Text)
	if len(before) == 0 {
		return ""
	}
	after := linkTagLinks(text)
	if after == nil {
		return ""
	}
	for _, tag := range linkTags {
		if after[tag] < before[tag] {
			return tag
		}
	}
	return ""
}

// linkTagLinks counts the links of a text block's Markdown whose address is a
// link tag, by tag.
func linkTagLinks(text string) map[string]int {
	elements, err := parseMarkdown(text)
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	count := func(inline []mdInline) {
		for _, node := range inline {
			if node.Kind == mdLink && linkTagOf(node.URL) != "" {
				counts[node.URL]++
			}
		}
	}
	for _, element := range elements {
		count(element.Inline)
		for _, item := range element.Items {
			count(item.Inline)
		}
	}
	return counts
}
