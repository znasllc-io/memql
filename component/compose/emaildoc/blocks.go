package emaildoc

import (
	"math"
	"strconv"
	"strings"
)

// The block types the parser does not need to name (see parse.go).
const (
	typeHeading   = "heading"
	typeHighlight = "highlight"
	typeDivider   = "divider"
	typeSpacer    = "spacer"
)

// The leaf blocks are written in the dialect of the plan's markup appendix,
// exactly: every cell, table and style as it lists them, with no newline.
//
// A block's own values win; the zero value of a length, size or weight means
// the block has none and takes its type's default, and so does an empty
// colour, font or alignment. Colours are written as the theme's value for the
// token, and fonts as the theme's family with the web-safe ones after it.

// rows writes a list of blocks as the rows of one table, a row per block.
// width is the width of the column the list fills.
func (r *renderer) rows(blocks []Block, width float64) error {
	r.out.WriteString(rowsOpen)
	for _, b := range blocks {
		if err := r.row(b, width); err != nil {
			return err
		}
		if err := r.grown(); err != nil {
			return err
		}
	}
	r.out.WriteString("</tbody></table>")
	return nil
}

func (r *renderer) row(b Block, width float64) error {
	switch b.Type {
	case typeImage, typeHeading, typeText, typeHighlight, typeButton, typeDivider, typeSpacer, typeHTML:
	default:
		// Sections, columns, social links and footers are containers, which
		// this renderer does not draw yet.
		return nil
	}
	r.out.WriteString("<tr>")
	var err error
	switch b.Type {
	case typeImage:
		err = r.image(b, width)
	case typeHeading:
		r.heading(b)
	case typeText:
		err = r.text(b)
	case typeHighlight:
		err = r.highlight(b)
	case typeButton:
		r.button(b)
	case typeDivider:
		r.divider(b)
	case typeSpacer:
		r.spacer(b)
	case typeHTML:
		r.markup(b)
	}
	if err != nil {
		return err
	}
	r.out.WriteString("</tr>")
	return nil
}

// image writes a picture at its width, centred unless aligned, linked when it
// has a link. Narrower than its column, it sits in a fixed-width table for
// Outlook, which ignores max-width. Without a picture it is a grey
// placeholder of the same size, and so is a picture that would take the HTML
// past what sending takes.
func (r *renderer) image(b Block, column float64) error {
	label := b.Alt
	if label == "" {
		label = b.ID
	}
	var pic picture
	if b.Src == "" {
		r.issue(IssueImageMissing, b.ID, pictureMissing(label))
	} else {
		var limit bool
		var err error
		if pic, limit, err = r.pics.look(r.ctx, b.Src); err != nil {
			return err
		}
		if limit {
			r.issue(IssueImagesLimit, b.ID, picturesOverBudget)
		}
		if pic.problem == IssueImageUnavailable {
			r.issue(IssueImageUnavailable, b.ID, pictureUnavailable(label))
		}
		if pic.uri != "" && r.out.Len()+len(pic.uri) > sendLimit {
			r.tooLargeToSend()
			pic = picture{}
		}
	}
	href := r.link(b.Href)
	pad := padding(b)
	narrower := float64(b.Width) < column-pad[1]-pad[3]
	w := strconv.Itoa(b.Width)
	out := &r.out
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="` + alignment(b.Align, "center") + `">`)
	if narrower {
		out.WriteString(`<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="` + w + `" style="width:` + w + `px"><tbody><tr><td><![endif]-->`)
	}
	out.WriteString(`<table cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:` + w + `px"><tbody><tr><td style="width:100%">`)
	if href != "" {
		out.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener" style="display:block;text-decoration:none;border:none;outline:none">`)
	}
	if pic.uri != "" {
		out.WriteString(`<img src="` + attr(pic.uri) + `" width="` + w + `" height="` + strconv.Itoa(pic.scaledHeight(b.Width)) + `" alt="` + attr(b.Alt) + `" style="display:block;width:` + w + `px;height:auto;max-width:100%">`)
	} else {
		h := b.Height
		if h == 0 {
			h = float64((b.Width + 1) / 2) // half the width, rounded
		}
		text := b.Alt
		if text == "" {
			text = "Picture"
		}
		out.WriteString(`<div style="width:100%;max-width:` + w + `px;height:` + number(h) + `px;background-color:#e4e7eb;color:#5f6670;font-family:Arial, Helvetica, sans-serif;font-size:13px;line-height:` + number(h) + `px;text-align:center">` + escapeText(text) + `</div>`)
	}
	if href != "" {
		out.WriteString(`</a>`)
	}
	out.WriteString(`</td></tr></tbody></table>`)
	if narrower {
		out.WriteString(`<!--[if mso]></td></tr></tbody></table><![endif]-->`)
	}
	out.WriteString(`</td></tr></tbody></table></td>`)
	return nil
}

// heading writes a heading: one cell, its line breaks kept.
func (r *renderer) heading(b Block) {
	size := orNumber(b.Size, 28)
	lineHeight := b.LineHeight
	if lineHeight == 0 {
		lineHeight = round2(size * 1.1)
	}
	lh := number(lineHeight)
	r.out.WriteString(`<td` + r.dataBlock(b) + ` dir="ltr"` + phoneSize(size) + ` style="color:` + r.colour(b.Color, "primary") + `;font-size:` + number(size) + `px;` +
		emphasis(orWeight(b.Weight, 700), b.Italic, b.LetterSpacing) + r.fontFamily(b.Font, "heading") +
		`white-space:pre-wrap;text-align:` + alignment(b.Align, "center") + `;padding:` + padCSS(padding(b)) + `;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">` +
		escapeText(b.Text) + `</td>`)
}

// text writes a text block: one inner row per paragraph or list item. A
// paragraph is followed by the gap, list items by nothing, the last item of a
// list by the gap, and the block's last element by nothing. A row is far
// longer than its Markdown, so the ceiling is checked row by row.
func (r *renderer) text(b Block) error {
	size := orNumber(b.Size, 16)
	lineHeight := orNumber(b.LineHeight, 24)
	listLineHeight := orNumber(b.ListLineHeight, lineHeight)
	gap := 16.0
	if b.Gap != nil {
		gap = *b.Gap
	}
	colour := r.colour(b.Color, "text")
	linkColour := r.colour(b.LinkColor, "primary")
	align := alignment(b.Align, "left")
	s, lh, llh, efs := number(size), number(lineHeight), number(listLineHeight), phoneSize(size)
	style := emphasis(orWeight(b.Weight, 400), b.Italic, b.LetterSpacing) + r.fontFamily(b.Font, "body")
	out := &r.out
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(padding(b)) + `"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody>`)
	elements := markdownOf(b.Text)
	for n, element := range elements {
		after := gap
		if n == len(elements)-1 {
			after = 0
		}
		if element.Kind == mdParagraph {
			out.WriteString(`<tr><td dir="ltr"` + efs + ` style="color:` + colour + `;font-size:` + s + `px;` + style + `white-space:pre-wrap;text-align:` + align +
				`;padding:0px 0px ` + number(after) + `px;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">` + r.inline(element.Inline, linkColour) + `</td></tr>`)
			if out.Len() > MaxRenderBytes {
				return ErrTooLarge
			}
			continue
		}
		for i, item := range element.Items {
			below := 0.0
			if i == len(element.Items)-1 {
				below = after
			}
			marker := "•"
			if element.Kind == mdNumberedList {
				marker = strconv.Itoa(item.Number) + ".&nbsp;"
			}
			out.WriteString(`<tr><td dir="ltr" style="text-align:` + align + `;padding:0px 0px ` + number(below) + `px;line-height:` + llh + `px;mso-line-height-alt:` + llh + `px;font-size:0">` +
				`<table width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse;font-family:Arial, Helvetica, sans-serif"><tbody><tr>` +
				`<td style="width:24px;vertical-align:top;padding-right:8px;text-align:right;white-space:nowrap"><span style="font-size:` + s + `px;color:` + colour + `;display:inline-block">` + marker + `</span></td>` +
				`<td` + efs + ` style="font-size:` + s + `px;color:` + colour + `;` + style + `white-space:pre-wrap;vertical-align:top">` + r.inline(item.Inline, linkColour) + `</td>` +
				`</tr></tbody></table></td></tr>`)
			if out.Len() > MaxRenderBytes {
				return ErrTooLarge
			}
		}
	}
	out.WriteString(`</tbody></table></td>`)
	return nil
}

// markdownOf reads a text block's Markdown. Parse has refused text the reader
// refuses; should such text get here anyway, it is one paragraph, as written.
func markdownOf(text string) []mdElement {
	elements, err := parseMarkdown(text)
	if err != nil {
		return []mdElement{{Kind: mdParagraph, Inline: []mdInline{{Kind: mdText, Text: text}}}}
	}
	return elements
}

// inline writes the runs of a paragraph or a list item. A link whose address
// breaks the link rule is written as its words.
func (r *renderer) inline(runs []mdInline, linkColour string) string {
	var b strings.Builder
	for _, run := range runs {
		switch run.Kind {
		case mdBold:
			b.WriteString(`<span style="font-weight:700">` + escapeText(run.Text) + `</span>`)
		case mdItalic:
			b.WriteString(`<span style="font-style:italic">` + escapeText(run.Text) + `</span>`)
		case mdBoldItalic:
			b.WriteString(`<span style="font-weight:700;font-style:italic">` + escapeText(run.Text) + `</span>`)
		case mdLink:
			href := r.link(run.URL)
			if href == "" {
				b.WriteString(escapeText(run.Text))
				continue
			}
			b.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener" style="color:` + linkColour + `;text-decoration:underline">` + escapeText(run.Text) + `</a>`)
		case mdBreak:
			b.WriteString(`<br>`)
		default:
			b.WriteString(escapeText(run.Text))
		}
	}
	return b.String()
}

// highlight writes a highlighted statement: one inner row per line, the
// line's text on the highlight colour, the gap after every line but the last.
func (r *renderer) highlight(b Block) error {
	size := orNumber(b.Size, 18)
	lineHeight := b.LineHeight
	if lineHeight == 0 {
		lineHeight = round2(size * 0.5)
	}
	gap := 16.0
	if b.Gap != nil {
		gap = *b.Gap
	}
	colour, background := r.colour(b.Color, "white"), r.colour(b.Background, "accent")
	align := alignment(b.Align, "center")
	s, lh, efs := number(size), number(lineHeight), phoneSize(size)
	style := emphasis(orWeight(b.Weight, 700), b.Italic, b.LetterSpacing) + r.fontFamily(b.Font, "heading")
	out := &r.out
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(padding(b)) + `"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody>`)
	lines := strings.Split(oneNewline(b.Text), "\n")
	for n, line := range lines {
		after := gap
		if n == len(lines)-1 {
			after = 0
		}
		out.WriteString(`<tr><td dir="ltr"` + efs + ` style="color:` + colour + `;font-size:` + s + `px;` + style + `text-align:` + align +
			`;padding:0px 0px ` + number(after) + `px;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">` +
			`<span style="background-color:` + background + `;white-space:pre-wrap">` + escapeText(line) + `</span></td></tr>`)
		if out.Len() > MaxRenderBytes {
			return ErrTooLarge
		}
	}
	out.WriteString(`</tbody></table></td>`)
	return nil
}

// button writes a rounded button: VML for Outlook, a link in a rounded cell
// for every other client. Its line height is 1.4 times its size to the tenth
// of a pixel; Outlook's shape is that and the padding above and below, rounded
// up, and the corners are half of it, rounded down.
func (r *renderer) button(b Block) {
	link := r.link(b.Href)
	if link == "" {
		r.issue(IssueLinkMissing, b.ID, `Add a link to "`+clip(b.Label, 60)+`".`)
	}
	width := b.Width
	if width == 0 {
		width = 300
	}
	size := orNumber(b.Size, 20)
	paddingY := orNumber(b.PaddingY, 11)
	lineHeight := round1(1.4 * size)
	outer := 2*paddingY + lineHeight
	w, s, lh, py := strconv.Itoa(width), number(size), number(lineHeight), number(paddingY)
	vh, radius := strconv.Itoa(int(math.Ceil(outer))), strconv.Itoa(int(math.Floor(outer/2)))
	fill, ink := r.colour(b.Color, "primary"), r.colour(b.TextColor, "white")
	href, label := attr(link), escapeText(b.Label)
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(padding(b)) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="` + alignment(b.Align, "center") + `">` +
		`<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="` + w + `" style="width:` + w + `px"><tbody><tr><td><![endif]-->` +
		`<table cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:` + w + `px"><tbody><tr><td style="width:100%">` +
		`<!--[if mso]><v:roundrect xmlns:v="urn:schemas-microsoft-com:vml" xmlns:w="urn:schemas-microsoft-com:office:word" href="` + href + `" style="height:` + vh + `px;width:` + w + `px;v-text-anchor:middle;" arcsize="50%" fillcolor="` + fill + `">` +
		`<v:stroke dashstyle="Solid" weight="0px" color="` + fill + `"/><w:anchorlock/><v:textbox inset="0px,0px,0px,0px">` +
		`<center dir="false" style="color:` + ink + `;font-family:sans-serif;font-size:` + s + `px;font-weight:700">` + label + `</center></v:textbox></v:roundrect><![endif]-->` +
		`<!--[if !mso]><!--><table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%" style="width:100%;max-width:100%;border-collapse:separate;border-spacing:0"><tbody><tr>` +
		`<td bgcolor="` + fill + `" style="background-color:` + fill + `;border-bottom:0px solid transparent;border-left:0px solid transparent;border-radius:` + radius + `px;border-right:0px solid transparent;border-top:0px solid transparent;mso-border-alt:none">` +
		`<a href="` + href + `" target="_blank" rel="noopener" style="color:` + ink + `;text-decoration:none;display:block;padding-top:` + py + `px;padding-bottom:` + py + `px;padding-left:8px;padding-right:8px;text-align:center;` +
		r.fontFamily(b.Font, "heading") + `font-size:` + s + `px;font-weight:700;letter-spacing:0em;line-height:` + lh + `px;box-sizing:border-box;mso-line-height-rule:exactly">` +
		`<span style="word-break:break-word;line-height:` + lh + `px;color:` + ink + `;mso-style-textfill-type:solid;mso-style-textfill-fill-color:` + ink + `">` + label + `</span></a>` +
		`</td></tr></tbody></table><!--<![endif]-->` +
		`</td></tr></tbody></table>` +
		`<!--[if mso]></td></tr></tbody></table><![endif]-->` +
		`</td></tr></tbody></table></td>`)
}

// divider writes a rule across the column.
func (r *renderer) divider(b Block) {
	t := number(orNumber(b.Height, 1))
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(padding(b)) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="center">` +
		`<table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr>` +
		`<td height="` + t + `" style="height:` + t + `px;line-height:` + t + `px;mso-line-height-rule:exactly;font-size:0;background-color:` + r.colour(b.Color, "primary") + `">&nbsp;</td>` +
		`</tr></tbody></table></td></tr></tbody></table></td>`)
}

// spacer writes empty space of its height. It has no padding.
func (r *renderer) spacer(b Block) {
	h := number(b.Height)
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="height:` + h + `px;line-height:` + h + `px;font-size:0;mso-line-height-rule:exactly">&nbsp;</td>`)
}

// markup writes an html block: the form safeMarkup gives it, never the source.
// Markup it refuses (Parse refuses it first) leaves the cell empty.
func (r *renderer) markup(b Block) {
	normalized, err := safeMarkup(b.HTML)
	if err != nil {
		normalized = ""
	}
	markupLinks(normalized, r.linked)
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(padding(b)) + `">` + normalized + `</td>`)
}

// dataBlock marks a block's outer cell with its id in a preview, so an editor
// can tell which block was clicked. A sent email carries no mark.
func (r *renderer) dataBlock(b Block) string {
	if !r.preview {
		return ""
	}
	return ` data-block="` + attr(b.ID) + `"`
}

// padding is the block's own padding or, without one, that of a block in the
// email's column: none above, 24 pixels at the sides and 16 below, and no
// sides for a picture that bleeds to the edges.
func padding(b Block) [4]float64 {
	switch {
	case b.Padding != nil:
		return *b.Padding
	case b.Type == typeImage && b.Bleed:
		return [4]float64{0, 0, 16, 0}
	}
	return [4]float64{0, 24, 16, 24}
}

// padCSS writes top, right, bottom and left padding.
func padCSS(p [4]float64) string {
	return number(p[0]) + "px " + number(p[1]) + "px " + number(p[2]) + "px " + number(p[3]) + "px"
}

// colour is the theme's value for a colour token, or for the default token
// when the block names none.
func (r *renderer) colour(token, fallback string) string {
	if token == "" {
		token = fallback
	}
	var value string
	switch token {
	case "page":
		value = r.theme.Page
	case "canvas":
		value = r.theme.Canvas
	case "primary":
		value = r.theme.Primary
	case "secondary":
		value = r.theme.Secondary
	case "accent":
		value = r.theme.Accent
	case "tint":
		value = r.theme.Tint
	case "text":
		value = r.theme.Text
	case "muted":
		value = r.theme.Muted
	case "white":
		value = "#ffffff"
	}
	return attr(value)
}

// fontFamily is the font-family declaration for a font token, or for the
// default token when the block names none. The base font has none: it is the
// column's own.
func (r *renderer) fontFamily(font, fallback string) string {
	if font == "" {
		font = fallback
	}
	var family string
	switch font {
	case "heading":
		family = r.theme.HeadingFont
	case "body":
		family = r.theme.BodyFont
	default:
		return ""
	}
	if family == "" { // left out by the theme check
		return ""
	}
	return "font-family:" + attr(family) + ", Arial, Helvetica, sans-serif;"
}

// emphasis is the weight, slant and letter spacing of text, as declarations.
// A weight of 400 is the column's own.
func emphasis(weight int, italic bool, letterSpacing string) string {
	var s string
	if weight == 700 {
		s += "font-weight:700;"
	}
	if italic {
		s += "font-style:italic;"
	}
	if spacingPattern.MatchString(letterSpacing) {
		s += "letter-spacing:" + letterSpacing + ";"
	}
	return s
}

// phoneSize is the class that lets text over 16 pixels shrink on a phone,
// named for its size in tenths of a pixel.
func phoneSize(size float64) string {
	if size <= 16 {
		return ""
	}
	return ` class="efs-` + strconv.Itoa(int(math.Round(size*10))) + `"`
}

// alignment is the block's alignment, or the default when it has none (or
// one that is not left, center or right).
func alignment(align, fallback string) string {
	switch align {
	case "left", "center", "right":
		return align
	}
	return fallback
}

// link is the address a link is written with: href as written when the link
// rule allows it, else "" and no link. Every address written is noted, so the
// render knows which link tags the email links to.
func (r *renderer) link(href string) string {
	if href = checkedHref(href); href != "" {
		r.linked[href] = true
	}
	return href
}

// checkedHref is href when the link rule allows it, else "".
func checkedHref(href string) string {
	if hrefProblem(href, true) != "" {
		return ""
	}
	return href
}

func orNumber(v, fallback float64) float64 {
	if v == 0 {
		return fallback
	}
	return v
}

func orWeight(weight, fallback int) int {
	if weight == 0 {
		return fallback
	}
	return weight
}

// round1 and round2 round a computed length to the tenth and the hundredth of
// a pixel, so the email says 30.8px and not 30.800000000000004px.
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// htmlEscaper escapes text for HTML as html.EscapeString does, and as sending
// escapes a recipient's values.
var htmlEscaper = strings.NewReplacer(`&`, "&amp;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `"`, "&#34;")

// textEscaper escapes text for an element's content, a line break as <br>.
var textEscaper = strings.NewReplacer(`&`, "&amp;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `"`, "&#34;", "\r\n", "<br>", "\r", "<br>", "\n", "<br>")

// attrEscaper escapes text for a quoted attribute, a line break as a character
// reference, so the markup itself has no newline.
var attrEscaper = strings.NewReplacer(`&`, "&amp;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;", `"`, "&#34;", "\r", "&#13;", "\n", "&#10;")

func escapeText(s string) string { return textEscaper.Replace(s) }

func attr(s string) string { return attrEscaper.Replace(s) }

// oneNewline writes every line break as "\n".
func oneNewline(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
