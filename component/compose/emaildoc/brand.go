package emaildoc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// brand.go -- an account's brand kit.
//
// A layout is structure only. A brand kit supplies its look (the theme's
// colours and fonts) and the content of the six brand slots (logo, signature,
// follow, social, footer, reply). ApplyBrand writes a kit into a layout; a
// kit is read and written as JSON like a document is, and BrandWarnings says
// what a kit would do to the built-in layouts without refusing it.

// BrandPicturesWarnBytes is the pictures of a kit past which BrandWarnings
// says they leave an email little room for its own (MaxImageBytes is the most
// one email carries).
const BrandPicturesWarnBytes = 512 << 10

// Limits of a kit.
const (
	maxBrandSocial      = 8
	maxBrandFooterLinks = 8
	maxBrandFooterLines = 12
	maxBrandLineChars   = 300
	maxBrandFollowChars = 200
)

// brandHeadroom is what a document keeps of MaxBytes for everything but the
// kit's pictures (a layout's structure and the kit's words), so that a kit
// ParseBrandKit accepts always makes a document Parse accepts.
const brandHeadroom = 128 << 10

const brandUploadHint = "Upload pictures through the Brand page"

// BrandPicture is the logo: a data: picture, its description and where it links.
type BrandPicture struct{ Src, Alt, Href string }

// BrandSignature is the signature: its parts (a photo, the name as a picture or
// as text, at most one of NameImage and NameText, and a title), and Picture, the
// whole signature drawn as one picture. A kit may hold the parts, the picture
// or both.
type BrandSignature struct{ Photo, NameImage, NameText, Title, Picture string }

// BrandFooter is the footer's lines, links and the separator between links.
type BrandFooter struct {
	Lines     []string
	Links     []Link
	Separator *string
}

// BrandReply is the invitation to reply.
type BrandReply struct{ Heading, ButtonLabel, ButtonHref, Note string }

// BrandKit is an account's brand.
type BrandKit struct {
	Theme     Theme
	Logo      BrandPicture
	Signature *BrandSignature
	Follow    string
	Social    []Link
	Footer    BrandFooter
	Reply     *BrandReply
}

type kitFile struct {
	Theme     Theme         `json:"theme"`
	Logo      kitLogo       `json:"logo"`
	Signature *kitSignature `json:"signature,omitempty"`
	Follow    string        `json:"follow,omitempty"`
	Social    []Link        `json:"social,omitempty"`
	Footer    kitFooter     `json:"footer"`
	Reply     *kitReply     `json:"reply,omitempty"`
}

type kitLogo struct {
	Src  string `json:"src"`
	Alt  string `json:"alt,omitempty"`
	Href string `json:"href,omitempty"`
}

type kitName struct {
	Src  string `json:"src,omitempty"`
	Text string `json:"text,omitempty"`
}

type kitSignature struct {
	Photo   string   `json:"photo,omitempty"`
	Name    *kitName `json:"name,omitempty"`
	Title   string   `json:"title,omitempty"`
	Picture string   `json:"picture,omitempty"`
}

type kitFooter struct {
	Lines     []string `json:"lines,omitempty"`
	Links     []Link   `json:"links"`
	Separator *string  `json:"separator,omitempty"`
}

type kitButton struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

type kitReply struct {
	Heading string    `json:"heading"`
	Button  kitButton `json:"button"`
	Note    string    `json:"note,omitempty"`
}

// JSON returns the canonical bytes of the kit: the key order of the format,
// two-space indentation, markup unescaped and one trailing newline.
// ParseBrandKit(k.JSON()) is k.
func (k BrandKit) JSON() ([]byte, error) {
	file := kitFile{
		Theme:  k.Theme,
		Logo:   kitLogo{Src: k.Logo.Src, Alt: k.Logo.Alt, Href: k.Logo.Href},
		Follow: k.Follow,
		Social: k.Social,
		Footer: kitFooter{Lines: k.Footer.Lines, Links: k.Footer.Links, Separator: k.Footer.Separator},
	}
	if s := k.Signature; s != nil {
		sig := &kitSignature{Photo: s.Photo, Title: s.Title, Picture: s.Picture}
		switch {
		case s.NameImage != "":
			sig.Name = &kitName{Src: s.NameImage}
		case s.NameText != "":
			sig.Name = &kitName{Text: s.NameText}
		}
		file.Signature = sig
	}
	if r := k.Reply; r != nil {
		file.Reply = &kitReply{Heading: r.Heading, Button: kitButton{Label: r.ButtonLabel, Href: r.ButtonHref}, Note: r.Note}
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

var (
	kitKeys      = map[string]bool{"theme": true, "logo": true, "signature": true, "follow": true, "social": true, "footer": true, "reply": true}
	logoKeys     = map[string]bool{"src": true, "alt": true, "href": true}
	sigKeys      = map[string]bool{"photo": true, "name": true, "title": true, "picture": true}
	nameKeys     = map[string]bool{"src": true, "text": true}
	footerKeys   = map[string]bool{"lines": true, "links": true, "separator": true}
	replyKeys    = map[string]bool{"heading": true, "button": true, "note": true}
	buttonKeys   = map[string]bool{"label": true, "href": true}
	kitSocialKey = map[string]bool{"network": true, "href": true, "icon": true}
	kitFooterKey = map[string]bool{"label": true, "href": true}
)

// kitObject reads the object at where, refusing a repeated or unknown key. An
// absent or null member is nil, false.
func kitObject(raw json.RawMessage, where string, keys map[string]bool) (map[string]json.RawMessage, bool, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, false, nil
	}
	fields, duplicate, err := readObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("%s must be an object", where)
	}
	if duplicate != "" {
		return nil, false, fmt.Errorf("%s: duplicate field %s", where, quote(duplicate))
	}
	if key := unknownKey(fields, keys); key != "" {
		return nil, false, fmt.Errorf("%s: unknown field %s", where, quote(key))
	}
	return fields, true, nil
}

func kitText(fields map[string]json.RawMessage, key, where string) (string, error) {
	s, err := textOf(fields[key])
	if err != nil {
		return "", fmt.Errorf("%s: %s must be text", where, key)
	}
	return s, nil
}

// kitPicture says what is wrong with a kit's picture, or "": it is a data:
// picture that pictureProblem accepts. A kit stores its pictures; it does not
// point at the Library.
func kitPicture(src string) string {
	if strings.HasPrefix(src, "artifact:") {
		return brandUploadHint
	}
	if problem := pictureProblem(src); problem != "" {
		return problem
	}
	if src != "" && !strings.HasPrefix(src, "data:") {
		return brandUploadHint
	}
	return ""
}

// kitHref says what is wrong with a link of a kit, or "". Link merge tags are
// for footer links only.
func kitHref(href string, tagsAllowed bool) string {
	if href == "" {
		return hrefHint
	}
	if problem := hrefProblem(href, false); problem != "" {
		return problem
	}
	if !tagsAllowed && linkTagOf(href) != "" {
		return "a link tag can only be a footer link"
	}
	return ""
}

// ParseBrandKit reads and validates a brand kit. It refuses, with a sentence
// that names the problem: a key it does not know, a theme Parse would refuse,
// a logo without a src, a picture that is not an embedded PNG, JPEG or GIF
// within the limits (a Library picture is told to be uploaded through the
// Brand page), a link that breaks the link rule or a link tag outside the
// footer, a footer without an {{unsubscribeUrl}} link, a signature name that
// is not exactly one of a picture and text, a reply without a heading, button
// label and button link, and more social links, footer links or footer lines
// than the limits. ApplyBrand takes every kit it accepts on every built-in layout.
func ParseBrandKit(data []byte) (BrandKit, error) {
	if len(data) > MaxBytes {
		return BrandKit{}, ErrFileTooLarge
	}
	fields, duplicate, err := readObject(data)
	if err != nil {
		return BrandKit{}, errors.New("a brand kit must be a JSON object")
	}
	if duplicate != "" {
		return BrandKit{}, fmt.Errorf("duplicate field %s", quote(duplicate))
	}
	if key := unknownKey(fields, kitKeys); key != "" {
		return BrandKit{}, fmt.Errorf("unknown field %s", quote(key))
	}
	var kit BrandKit
	if kit.Theme, err = parseTheme(fields["theme"]); err != nil {
		return BrandKit{}, err
	}

	logo, ok, err := kitObject(fields["logo"], "logo", logoKeys)
	if err != nil {
		return BrandKit{}, err
	}
	if !ok {
		return BrandKit{}, errors.New("logo is required: a picture, as logo.src")
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"src", &kit.Logo.Src}, {"alt", &kit.Logo.Alt}, {"href", &kit.Logo.Href}} {
		if *f.dst, err = kitText(logo, f.key, "logo"); err != nil {
			return BrandKit{}, err
		}
	}
	if kit.Logo.Src == "" {
		return BrandKit{}, errors.New("logo.src is required")
	}
	if strings.TrimSpace(kit.Logo.Alt) == "" {
		return BrandKit{}, errors.New("logo.alt is required: describe the logo for readers who cannot see it")
	}
	if problem := kitPicture(kit.Logo.Src); problem != "" {
		return BrandKit{}, fmt.Errorf("logo: %s", problem)
	}
	if kit.Logo.Href != "" {
		if problem := kitHref(kit.Logo.Href, false); problem != "" {
			return BrandKit{}, fmt.Errorf("logo: %s", problem)
		}
	}

	if kit.Signature, err = parseKitSignature(fields["signature"]); err != nil {
		return BrandKit{}, err
	}
	if kit.Follow, err = kitText(fields, "follow", "brand kit"); err != nil {
		return BrandKit{}, err
	}
	if utf8.RuneCountInString(kit.Follow) > maxBrandFollowChars {
		return BrandKit{}, fmt.Errorf("follow can be at most %d characters", maxBrandFollowChars)
	}

	if kit.Social, err = readLinks(fields["social"], "social", kitSocialKey, maxBrandSocial,
		fmt.Sprintf("at most %d social links", maxBrandSocial), func(l Link) string {
			if problem := socialLinkProblem(l); problem != "" {
				return problem
			}
			if problem := kitHref(l.Href, false); problem != "" {
				return problem
			}
			return kitPicture(l.Icon)
		}); err != nil {
		return BrandKit{}, err
	}
	if used := kitPictureChars(kit); used > MaxBytes-brandHeadroom {
		return BrandKit{}, fmt.Errorf("the kit's pictures take %d KiB written out; together they must stay under %d KiB. Use fewer or smaller pictures", used>>10, (MaxBytes-brandHeadroom)>>10)
	}
	if kit.Footer, err = parseKitFooter(fields["footer"]); err != nil {
		return BrandKit{}, err
	}
	if kit.Reply, err = parseKitReply(fields["reply"]); err != nil {
		return BrandKit{}, err
	}

	return kit, nil
}

func parseKitSignature(raw json.RawMessage) (*BrandSignature, error) {
	fields, ok, err := kitObject(raw, "signature", sigKeys)
	if err != nil || !ok {
		return nil, err
	}
	var s BrandSignature
	if s.Photo, err = kitText(fields, "photo", "signature"); err != nil {
		return nil, err
	}
	if s.Title, err = kitText(fields, "title", "signature"); err != nil {
		return nil, err
	}
	if problem := kitPicture(s.Photo); problem != "" {
		return nil, fmt.Errorf("signature photo: %s", problem)
	}
	if s.Picture, err = kitText(fields, "picture", "signature"); err != nil {
		return nil, err
	}
	if problem := kitPicture(s.Picture); problem != "" {
		return nil, fmt.Errorf("signature picture: %s", problem)
	}
	name, ok, err := kitObject(fields["name"], "signature.name", nameKeys)
	if err != nil {
		return nil, err
	}
	if ok {
		src, err := kitText(name, "src", "signature.name")
		if err != nil {
			return nil, err
		}
		text, err := kitText(name, "text", "signature.name")
		if err != nil {
			return nil, err
		}
		if (src == "") == (text == "") {
			return nil, errors.New("signature.name is a picture (src) or text, exactly one")
		}
		if problem := kitPicture(src); problem != "" {
			return nil, fmt.Errorf("signature name: %s", problem)
		}
		s.NameImage, s.NameText = src, text
	}
	return &s, nil
}

func parseKitFooter(raw json.RawMessage) (BrandFooter, error) {
	fields, ok, err := kitObject(raw, "footer", footerKeys)
	if err != nil {
		return BrandFooter{}, err
	}
	if !ok {
		return BrandFooter{}, errors.New("footer is required, with an unsubscribe link ({{unsubscribeUrl}})")
	}
	var f BrandFooter
	if v := fields["lines"]; len(v) > 0 && !isNull(v) {
		var lines []string
		if err := json.Unmarshal(v, &lines); err != nil {
			return BrandFooter{}, errors.New("footer: lines must be a list of text")
		}
		if len(lines) > maxBrandFooterLines {
			return BrandFooter{}, fmt.Errorf("footer: at most %d lines", maxBrandFooterLines)
		}
		for n, line := range lines {
			if utf8.RuneCountInString(line) > maxBrandLineChars {
				return BrandFooter{}, fmt.Errorf("footer: line %d is longer than %d characters", n+1, maxBrandLineChars)
			}
		}
		if len(lines) > 0 {
			f.Lines = lines
		}
	}
	if f.Links, err = readLinks(fields["links"], "footer", kitFooterKey, maxBrandFooterLinks,
		fmt.Sprintf("at most %d links", maxBrandFooterLinks), func(l Link) string {
			if problem := footerLinkProblem(l); problem != "" {
				return problem
			}
			return kitHref(l.Href, true)
		}); err != nil {
		return BrandFooter{}, err
	}
	unsubscribe := false
	for _, l := range f.Links {
		unsubscribe = unsubscribe || l.Href == TagUnsubscribeURL
	}
	if !unsubscribe {
		return BrandFooter{}, errors.New("footer: needs a link to " + TagUnsubscribeURL + " so every email can be unsubscribed from")
	}
	if v := fields["separator"]; len(v) > 0 && !isNull(v) {
		sep, err := kitText(fields, "separator", "footer")
		if err != nil {
			return BrandFooter{}, err
		}
		f.Separator = &sep
	}
	return f, nil
}

func parseKitReply(raw json.RawMessage) (*BrandReply, error) {
	fields, ok, err := kitObject(raw, "reply", replyKeys)
	if err != nil || !ok {
		return nil, err
	}
	var r BrandReply
	if r.Heading, err = kitText(fields, "heading", "reply"); err != nil {
		return nil, err
	}
	if r.Note, err = kitText(fields, "note", "reply"); err != nil {
		return nil, err
	}
	button, _, err := kitObject(fields["button"], "reply.button", buttonKeys)
	if err != nil {
		return nil, err
	}
	if r.ButtonLabel, err = kitText(button, "label", "reply.button"); err != nil {
		return nil, err
	}
	if r.ButtonHref, err = kitText(button, "href", "reply.button"); err != nil {
		return nil, err
	}
	if r.Heading == "" || r.ButtonLabel == "" || r.ButtonHref == "" {
		return nil, errors.New("reply needs a heading, a button label and a button link")
	}
	if problem := kitHref(r.ButtonHref, false); problem != "" {
		return nil, fmt.Errorf("reply button: %s", problem)
	}
	return &r, nil
}

// findBlock returns the block with this id anywhere in blocks (in a section or
// a column too), or nil.
func findBlock(blocks []Block, id string) *Block {
	var found *Block
	eachBlock(blocks, roleContent, func(b *Block, _ string) {
		if found == nil && b.ID == id {
			found = b
		}
	})
	return found
}

// removeBlock removes the block with this id from the list it sits in.
func removeBlock(blocks *[]Block, id string) {
	for i := range *blocks {
		b := &(*blocks)[i]
		if b.ID == id {
			*blocks = append((*blocks)[:i:i], (*blocks)[i+1:]...)
			return
		}
		removeBlock(&b.Blocks, id)
		for c := range b.Columns {
			removeBlock(&b.Columns[c].Blocks, id)
		}
	}
}

// tidyColumns drops the columns of a columns block that hold nothing to see (no block, or spacers only) and gives
// their width to the ones that remain, in proportion, so the block stays valid.
func onlySpacers(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type != typeSpacer {
			return false
		}
	}
	return true
}

func tidyColumns(b *Block) {
	kept := b.Columns[:0:0]
	total, dropped := 0.0, 0.0
	for _, c := range b.Columns {
		if onlySpacers(c.Blocks) {
			dropped += c.Width
			continue
		}
		kept = append(kept, c)
		total += c.Width
	}
	if dropped == 0 || len(kept) == 0 {
		b.Columns = kept
		return
	}
	for i := range kept {
		if total > 0 {
			kept[i].Width += dropped * kept[i].Width / total
		} else {
			kept[i].Width = 100 / float64(len(kept))
		}
		kept[i].Width = float64(int(kept[i].Width*100)) / 100
	}
	b.Columns = kept
}

// compactSignature reports whether a signature slot is the compact form, which
// a kit's whole picture fills: columns or a section without a background of
// its own. A band, a slot with a background (Letter's), always shows the parts.
func compactSignature(slot *Block) bool {
	return (slot.Type == typeColumns || slot.Type == typeSection) && slot.Background == ""
}

// signWithPicture fills a compact signature slot with the kit's whole picture:
// the slot's blocks become one image, signature-picture, at the slot's full
// content width. That is the width the slot has in the email: its inset and
// maximum width framed the parts and are dropped (a section keeps its inset,
// its margin, and loses its cell padding), so the picture spans what the slot
// spans inside its padding, 552 pixels where it sits in the email's column.
// The picture is as tall as its shape makes it at that width, and is named for
// the title, else the name written as text, else "Signature". blocks is the
// whole document, which says where the slot sits.
func signWithPicture(blocks []Block, slot *Block, s *BrandSignature) {
	width, pad, _ := slotRoom(blocks, canvasWidth, false, slot.ID)
	slot.MaxWidth = 0
	if slot.Type == typeColumns {
		slot.Inset = nil
	} else {
		width = roomInside(width, pad, lengthsOr(slot.Inset, [4]float64{0, 24, 0, 24}))
		pad = [4]float64{}
		zero := 0.0
		slot.CellPadding = &zero
	}
	w := int(math.Max(1, math.Min(canvasWidth, math.Floor(width-pad[1]-pad[3]))))
	alt := "Signature"
	switch {
	case s.Title != "":
		alt = s.Title
	case s.NameText != "":
		alt = s.NameText
	}
	h := float64((w + 1) / 2)
	if pic, ok := pictureShape(s.Picture); ok {
		h = float64(pic.scaledHeight(w))
	}
	picture := Block{ID: "signature-picture", Type: typeImage, Src: s.Picture, Alt: alt, Width: w, Height: h}
	if slot.Type == typeColumns {
		slot.Columns = []Column{{Width: 100, Blocks: []Block{picture}}}
	} else {
		slot.Blocks = []Block{picture}
	}
}

// slotRoom finds the block with the id in blocks, which are written width
// pixels wide (in a section or a column when inside), and returns the width it
// is written in and its padding there, measured as the renderer measures them
// (rows, section and columns).
func slotRoom(blocks []Block, width float64, inside bool, id string) (float64, [4]float64, bool) {
	for n, b := range blocks {
		pad := padding(b, inside, inside && n == len(blocks)-1)
		if b.ID == id {
			return width, pad, true
		}
		switch b.Type {
		case typeSection:
			room := roomInside(width, pad, lengthsOr(b.Inset, [4]float64{0, 24, 0, 24}))
			inner := math.Min(orNumber(b.MaxWidth, room), room) - 2*lengthOr(b.CellPadding, 24)
			if w, p, ok := slotRoom(b.Blocks, inner, true, id); ok {
				return w, p, true
			}
		case typeColumns:
			room := roomInside(width, pad, lengthsOr(b.Inset, [4]float64{}))
			content := math.Min(orNumber(b.MaxWidth, room), room)
			for _, column := range b.Columns {
				inner := math.Floor(column.Width*content/100) - 2*lengthOr(column.CellPadding, 0)
				if w, p, ok := slotRoom(column.Blocks, inner, true, id); ok {
					return w, p, true
				}
			}
		}
	}
	return 0, [4]float64{}, false
}

// pictureShape reads the natural size of an embedded picture, as the renderer
// reads it; ok is false for anything else.
func pictureShape(src string) (picture, bool) {
	prefix, encoded, found := strings.Cut(strings.TrimSpace(src), ",")
	if !found || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
		return picture{}, false
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return picture{}, false
	}
	pic := raster(strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64"), data, src)
	return pic, pic.uri != "" && pic.width > 0
}

// ApplyBrand writes a kit into a copy of layout: the kit's theme replaces the
// layout's and each brand slot, found by its id wherever it sits, takes the
// kit's content. A slot the kit leaves empty is removed, and a kit slot the
// layout does not have is not used. A text signature name replaces the name's
// picture with a text block of the same id in the heading font. A kit's whole
// signature picture fills a compact signature slot in place of the parts (see
// signWithPicture); a band keeps the parts. Content blocks are never touched.
// The result is read back through JSON and Parse.
func ApplyBrand(layout Document, kit BrandKit) (Document, error) {
	if layout.Mode() != ModeLayout {
		return Document{}, errors.New("this email is custom HTML, so it has no brand slots")
	}
	raw, err := layout.JSON()
	if err != nil {
		return Document{}, err
	}
	out, err := Parse(raw)
	if err != nil {
		return Document{}, fmt.Errorf("the layout is not a valid email document: %w", err)
	}
	out.Theme = kit.Theme
	blocks := &out.Blocks

	// fill sets a slot with set, or removes it when the kit has nothing for it.
	fill := func(id string, present bool, set func(b *Block)) {
		if !present {
			removeBlock(blocks, id)
			return
		}
		if b := findBlock(*blocks, id); b != nil {
			set(b)
		}
	}
	fill(SlotLogo, true, func(b *Block) { b.Src, b.Alt, b.Href = kit.Logo.Src, kit.Logo.Alt, kit.Logo.Href })

	if s := kit.Signature; s == nil {
		removeBlock(blocks, SlotSignature)
	} else if slot := findBlock(*blocks, SlotSignature); slot != nil && s.Picture != "" && compactSignature(slot) {
		signWithPicture(*blocks, slot, s)
	} else {
		title := findBlock(*blocks, "signature-title")
		fill("signature-photo", s.Photo != "", func(b *Block) { b.Src = s.Photo })
		fill("signature-title", s.Title != "", func(b *Block) { b.Text = s.Title })
		fill("signature-name", s.NameImage != "" || s.NameText != "", func(b *Block) {
			if s.NameText == "" {
				b.Src = s.NameImage
				return
			}
			// The replaced picture's padding and alignment stay; the colour is
			// the title's, the band's; the name is the larger line above it.
			name := Block{ID: b.ID, Type: typeText, Text: s.NameText, Size: 22, LineHeight: 26, Weight: 700, Font: "heading",
				Color: "text", Align: b.Align, Padding: b.Padding}
			if title != nil {
				name.Color = title.Color
				if name.Align == "" {
					name.Align = title.Align
				}
			}
			*b = name
		})
	}

	if band := findBlock(*blocks, SlotSignature); band != nil {
		tidyColumns(band)
		if len(band.Columns) == 0 {
			removeBlock(blocks, SlotSignature)
		}
	}

	fill(SlotFollow, kit.Follow != "", func(b *Block) { b.Text = kit.Follow })
	fill(SlotSocial, len(kit.Social) > 0, func(b *Block) { b.Links = append([]Link(nil), kit.Social...) })
	fill(SlotFooter, true, func(b *Block) {
		b.Lines = append([]string(nil), kit.Footer.Lines...)
		b.Links = append([]Link(nil), kit.Footer.Links...)
		b.Separator = nil
		if kit.Footer.Separator != nil {
			sep := *kit.Footer.Separator
			b.Separator = &sep
		}
	})

	if r := kit.Reply; r == nil {
		removeBlock(blocks, SlotReply)
	} else {
		fill("reply-heading", true, func(b *Block) { b.Text = r.Heading })
		fill("reply-button", true, func(b *Block) { b.Label, b.Href = r.ButtonLabel, r.ButtonHref })
		fill("reply-note", r.Note != "", func(b *Block) { b.Text = r.Note })
	}

	applied, err := out.JSON()
	if err != nil {
		return Document{}, err
	}
	return Parse(applied)
}

// BrandWarning is something about a kit that is allowed but worth knowing.
// Code is "contrast" or "pictures_large".
type BrandWarning struct{ Code, Message string }

// BrandWarnings renders every built-in layout with the kit applied and says
// what a reader may struggle with: each pair of text and background colour
// below the WCAG minimum, once however many layouts and blocks draw it, and
// pictures that together leave an email little room of its own. It never
// refuses a kit.
func BrandWarnings(ctx context.Context, kit BrandKit) []BrandWarning {
	var warnings []BrandWarning
	type triple struct {
		fg, bg string
		large  bool
	}
	seen := map[triple]bool{}
	for _, info := range Layouts() {
		doc, err := ApplyBrand(info.Document, kit)
		if err != nil {
			continue
		}
		result, err := Render(ctx, doc, nil, RenderOptions{Preview: true})
		if err != nil {
			continue
		}
		runs, err := TextRuns(result.HTML)
		if err != nil {
			continue
		}
		for _, run := range runs {
			key := triple{strings.ToLower(run.Foreground), strings.ToLower(run.Background), run.Large()}
			if seen[key] {
				continue
			}
			if ratio := ContrastRatio(run.Foreground, run.Background); ratio < run.Required() {
				seen[key] = true
				warnings = append(warnings, BrandWarning{Code: "contrast", Message: fmt.Sprintf(
					"Text in %s on %s has a contrast of %.1f to 1; the minimum here is %s to 1.",
					run.Foreground, run.Background, ratio, number(run.Required()))})
			}
		}
	}
	if total := kitPictureBytes(kit); total > BrandPicturesWarnBytes {
		warnings = append(warnings, BrandWarning{Code: "pictures_large", Message: fmt.Sprintf(
			"The kit's pictures total %d KiB, over the %d KiB that leaves room for an email's own pictures.",
			(total+1023)>>10, BrandPicturesWarnBytes>>10)})
	}
	return warnings
}

// kitPictureBytes is the size of the kit's distinct pictures once decoded.
func kitPictureBytes(kit BrandKit) int {
	srcs := []string{kit.Logo.Src}
	if s := kit.Signature; s != nil {
		srcs = append(srcs, s.Photo, s.NameImage, s.Picture)
	}
	for _, l := range kit.Social {
		srcs = append(srcs, l.Icon)
	}
	seen := map[string]bool{}
	total := 0
	for _, src := range srcs {
		_, payload, ok := strings.Cut(src, ";base64,")
		if !ok || seen[src] {
			continue
		}
		seen[src] = true
		if data, err := base64.StdEncoding.DecodeString(payload); err == nil {
			total += len(data)
		}
	}
	return total
}

// kitPictureChars is the length of the pictures as a document writes them: one
// copy for every place the kit puts a picture.
func kitPictureChars(kit BrandKit) int {
	n := len(kit.Logo.Src)
	if s := kit.Signature; s != nil {
		n += len(s.Photo) + len(s.NameImage) + len(s.Picture)
	}
	for _, l := range kit.Social {
		n += len(l.Icon)
	}
	return n
}
