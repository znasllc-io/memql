package emaildoc

import (
	"math"
	"sort"
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

// Every block is written in the dialect of the plan's markup appendix,
// exactly: every cell, table and style as it lists them, with no newline.
//
// A block's own values win; the zero value of a length, size or weight means
// the block has none and takes its type's default, and so does an empty
// colour, font or alignment. Colours are written as the theme's value for the
// token, and fonts as the theme's family with the web-safe ones after it.

// rows writes a list of blocks as the rows of one table, a row per block.
// width is the width of the column the list fills. inside says the list is a
// section's or a column's, whose blocks have no padding at the sides and
// whose last block has none at all, unless they have their own. The ceiling
// is checked after every block, so the blocks of a container count as they
// are written.
func (r *renderer) rows(blocks []Block, width float64, inside bool) error {
	r.out.WriteString(rowsOpen)
	for n, b := range blocks {
		if err := r.row(b, width, padding(b, inside, inside && n == len(blocks)-1)); err != nil {
			return err
		}
		if err := r.grown(); err != nil {
			return err
		}
	}
	r.out.WriteString("</tbody></table>")
	return nil
}

// row writes one block as a row, with its padding.
func (r *renderer) row(b Block, width float64, pad [4]float64) error {
	if _, known := blockKeys[b.Type]; !known {
		return nil // a type Parse refuses: nothing is written
	}
	r.out.WriteString("<tr>")
	var err error
	switch b.Type {
	case typeImage:
		err = r.image(b, width, pad)
	case typeHeading:
		r.heading(b, pad, r.dataBlock(b))
	case typeText:
		err = r.text(b, pad)
	case typeHighlight:
		err = r.highlight(b, pad)
	case typeButton:
		r.button(b, pad)
	case typeDivider:
		r.divider(b, pad)
	case typeSpacer:
		r.spacer(b)
	case typeHTML:
		r.markup(b, pad)
	case typeSection:
		err = r.section(b, width, pad)
	case typeColumns:
		err = r.columns(b, width, pad)
	case typeSocial:
		err = r.social(b, width, pad)
	case typeFooter:
		err = r.footer(b, pad)
	}
	if err != nil {
		return err
	}
	r.out.WriteString("</tr>")
	return nil
}

// msoOpen and msoClose put what is between them in a table of a fixed width
// for Outlook, which ignores max-width.
func msoOpen(width string) string {
	return `<!--[if mso]><table cellpadding="0" cellspacing="0" border="0" width="` + width + `" style="width:` + width + `px"><tbody><tr><td><![endif]-->`
}

const msoClose = `<!--[if mso]></td></tr></tbody></table><![endif]-->`

// readPicture reads the picture of src for one use of it in block, and says
// what keeps it from the email: images_limit for the use that takes the email
// past the picture budget (once for the email), image_unavailable when it
// cannot be read or sent, named by label. The picture's uri is "" when it
// cannot be embedded at all.
func (r *renderer) readPicture(src, block, label string) (picture, error) {
	pic, limit, err := r.pics.look(r.ctx, src)
	if err != nil {
		return picture{}, err
	}
	if limit {
		r.issue(IssueImagesLimit, block, picturesOverBudget)
	}
	if pic.problem == IssueImageUnavailable {
		r.issue(IssueImageUnavailable, block, pictureUnavailable(label))
	}
	return pic, nil
}

// fits says whether a picture can be embedded after the HTML written so far
// and ahead bytes more: whether the HTML up to the last byte of its data URI
// is within what sending takes. Every use of a picture asks, so no picture,
// however often it is used, takes the email past what sending takes. A
// picture that does not fit is said once for the email (email_too_large), and
// the caller writes what stands in for it.
func (r *renderer) fits(pic picture, ahead int) bool {
	if pic.uri == "" {
		return false
	}
	if r.out.Len()+ahead+len(pic.uri) > sendLimit {
		r.tooLargeToSend()
		return false
	}
	return true
}

// image writes a picture at its width, centred unless aligned, linked when it
// has a link. Narrower than the room it has (its column less its padding),
// it sits in a fixed-width table for Outlook, which ignores max-width.
// Without a picture it is a grey placeholder of the same size, and so is a
// picture that cannot be sent, or that would take the HTML past what sending
// takes.
func (r *renderer) image(b Block, column float64, pad [4]float64) error {
	label := b.Alt
	if label == "" {
		label = b.ID
	}
	var pic picture
	if b.Src == "" {
		r.issue(IssueImageMissing, b.ID, pictureMissing(label))
	} else {
		var err error
		if pic, err = r.readPicture(b.Src, b.ID, label); err != nil {
			return err
		}
	}
	href := r.link(b.Href)
	narrower := float64(b.Width) < column-pad[1]-pad[3]
	w := strconv.Itoa(b.Width)
	out := &r.out
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="` + alignment(b.Align, "center") + `">`)
	if narrower {
		out.WriteString(msoOpen(w))
	}
	out.WriteString(`<table cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:` + w + `px"><tbody><tr><td style="width:100%">`)
	if href != "" {
		out.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener" style="display:block;text-decoration:none;border:none;outline:none">`)
	}
	if r.fits(pic, len(`<img src="`)) {
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
		out.WriteString(msoClose)
	}
	out.WriteString(`</td></tr></tbody></table></td>`)
	return nil
}

// The default text sizes, which the writers and the phone rules share.
func headingSize(b Block) float64   { return orNumber(b.Size, 28) }
func textSize(b Block) float64      { return orNumber(b.Size, 16) }
func highlightSize(b Block) float64 { return orNumber(b.Size, 18) }

// heading writes a heading: one cell, its line breaks kept. mark is the
// block's preview mark, or "" for a heading that is part of another block.
func (r *renderer) heading(b Block, pad [4]float64, mark string) {
	size := headingSize(b)
	lineHeight := b.LineHeight
	if lineHeight == 0 {
		lineHeight = round2(size * 1.1)
	}
	lh := number(lineHeight)
	r.out.WriteString(`<td` + mark + ` dir="ltr"` + phoneSize(size) + ` style="color:` + r.colour(b.Color, "primary") + `;font-size:` + number(size) + `px;` +
		emphasis(orWeight(b.Weight, 700), b.Italic, b.LetterSpacing) + r.fontFamily(b.Font, "heading") +
		`white-space:pre-wrap;text-align:` + alignment(b.Align, "center") + `;padding:` + padCSS(pad) + `;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">` +
		escapeText(b.Text) + `</td>`)
}

// text writes a text block: one inner row per paragraph or list item. A
// paragraph is followed by the gap, list items by nothing, the last item of a
// list by the gap, and the block's last element by nothing. A row is far
// longer than its Markdown, so the ceiling is checked line by line: after each
// line of a paragraph and after each list item.
func (r *renderer) text(b Block, pad [4]float64) error {
	size := textSize(b)
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
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody>`)
	elements := markdownOf(b.Text)
	for n, element := range elements {
		after := gap
		if n == len(elements)-1 {
			after = 0
		}
		if element.Kind == mdParagraph {
			out.WriteString(`<tr><td dir="ltr"` + efs + ` style="color:` + colour + `;font-size:` + s + `px;` + style + `white-space:pre-wrap;text-align:` + align +
				`;padding:0px 0px ` + number(after) + `px;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">`)
			if err := r.inline(element.Inline, linkColour); err != nil {
				return err
			}
			out.WriteString(`</td></tr>`)
			if err := r.grown(); err != nil {
				return err
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
				`<td` + efs + ` style="font-size:` + s + `px;color:` + colour + `;` + style + `white-space:pre-wrap;vertical-align:top">`)
			if err := r.inline(item.Inline, linkColour); err != nil {
				return err
			}
			out.WriteString(`</td></tr></tbody></table></td></tr>`)
			if err := r.grown(); err != nil {
				return err
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

// inline writes the runs of a paragraph or a list item into the email,
// their words escaped as they are written, and checks the ceiling after each
// line. A link whose address breaks the link rule is written as its words.
func (r *renderer) inline(runs []mdInline, linkColour string) error {
	out := &r.out
	words := func(text string) {
		_, _ = textEscaper.WriteString(out, text) // a strings.Builder takes every write
	}
	for _, run := range runs {
		switch run.Kind {
		case mdBold:
			out.WriteString(`<span style="font-weight:700">`)
			words(run.Text)
			out.WriteString(`</span>`)
		case mdItalic:
			out.WriteString(`<span style="font-style:italic">`)
			words(run.Text)
			out.WriteString(`</span>`)
		case mdBoldItalic:
			out.WriteString(`<span style="font-weight:700;font-style:italic">`)
			words(run.Text)
			out.WriteString(`</span>`)
		case mdLink:
			href := r.link(run.URL)
			if href == "" {
				words(run.Text)
				continue
			}
			out.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener" style="color:` + linkColour + `;text-decoration:underline">`)
			words(run.Text)
			out.WriteString(`</a>`)
		case mdBreak:
			out.WriteString(`<br>`)
			if err := r.grown(); err != nil {
				return err
			}
		default:
			words(run.Text)
		}
	}
	return nil
}

// highlight writes a highlighted statement: one inner row per line, the
// line's text on the highlight colour, the gap after every line but the last.
func (r *renderer) highlight(b Block, pad [4]float64) error {
	size := highlightSize(b)
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
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody>`)
	lines := strings.Split(oneNewline(b.Text), "\n")
	for n, line := range lines {
		after := gap
		if n == len(lines)-1 {
			after = 0
		}
		out.WriteString(`<tr><td dir="ltr"` + efs + ` style="color:` + colour + `;font-size:` + s + `px;` + style + `text-align:` + align +
			`;padding:0px 0px ` + number(after) + `px;line-height:` + lh + `px;mso-line-height-alt:` + lh + `px">` +
			`<span style="background-color:` + background + `;white-space:pre-wrap">` + escapeText(line) + `</span></td></tr>`)
		if err := r.grown(); err != nil {
			return err
		}
	}
	out.WriteString(`</tbody></table></td>`)
	return nil
}

// button writes a rounded button: VML for Outlook, a link in a rounded cell
// for every other client. Its line height is 1.4 times its size to the tenth
// of a pixel; Outlook's shape is that and the padding above and below, rounded
// up, and the corners are half of it, rounded down.
func (r *renderer) button(b Block, pad [4]float64) {
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
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="` + alignment(b.Align, "center") + `">` +
		msoOpen(w) +
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
		msoClose +
		`</td></tr></tbody></table></td>`)
}

// divider writes a rule across the column.
func (r *renderer) divider(b Block, pad [4]float64) {
	t := number(orNumber(b.Height, 1))
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="center">` +
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
func (r *renderer) markup(b Block, pad [4]float64) {
	normalized, err := safeMarkup(b.HTML)
	if err != nil {
		normalized = ""
	}
	literalLinks(normalized, r.linked)
	r.out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `">` + normalized + `</td>`)
}

// The containers are written in one frame (the plan's markup appendix, A.3):
// the block's cell with its padding, a table across the column with the
// container's background, and in it, inside the inset, a centred table at
// most maxWidth wide whose one row holds the container's columns. A section
// and a footer are one column; columns are up to four, side by side with
// gaps between them; a social block puts each link in a column of its own.
// Columns and gaps of columns and social blocks carry the classes the phone
// rules in the head stack them by (see phoneRules).

// frame writes the frame of a container around the cells cells writes. mark
// is the block's preview mark ("" for the frame inside a social block).
func (r *renderer) frame(mark string, pad, inset [4]float64, background string, maxWidth float64, cells func() error) error {
	if background != "" {
		background = ";background-color:" + background
	}
	r.out.WriteString(`<td` + mark + ` style="padding:` + padCSS(pad) + `"><table border="0" cellpadding="0" cellspacing="0" align="center" style="display:table;border-spacing:0px;border-collapse:separate;width:100%;max-width:100%;table-layout:fixed;margin:0 auto` + background +
		`"><tbody><tr><td style="text-align:center;padding:` + padCSS(inset) + `"><table border="0" cellpadding="0" cellspacing="0" style="border-spacing:0px;border-collapse:separate;width:100%;max-width:` + number(maxWidth) + `px;table-layout:fixed;margin:0 auto"><tbody><tr>`)
	// Checked before the cells too, so containers nested in containers
	// (which only a Document built without Parse can hold) stop at the
	// ceiling however deep they go.
	if err := r.grown(); err != nil {
		return err
	}
	if err := cells(); err != nil {
		return err
	}
	r.out.WriteString(`</tr></tbody></table></td></tr></tbody></table></td>`)
	return nil
}

// cell writes one column of a frame, percent of its width, with cellPadding
// around what content writes. class is the column's class attribute for the
// phone rules, or "".
func (r *renderer) cell(percent float64, class string, cellPadding float64, content func() error) error {
	w := strconv.FormatFloat(percent, 'f', 2, 64)
	r.out.WriteString(`<td width="` + w + `%"` + class + ` style="width:` + w + `%;box-sizing:border-box;vertical-align:top"><table border="0" cellpadding="0" cellspacing="0" style="border-spacing:0px;border-collapse:separate;width:100%;table-layout:fixed"><tbody><tr><td style="padding:` + number(cellPadding) + `px">`)
	if err := content(); err != nil {
		return err
	}
	r.out.WriteString(`</td></tr></tbody></table></td>`)
	return r.grown()
}

// gapCell writes the empty cell between two columns.
func (r *renderer) gapCell(width float64, class string) {
	g := number(width)
	r.out.WriteString(`<td width="` + g + `"` + class + ` style="width:` + g + `px;box-sizing:border-box;font-size:0">&nbsp;</td>`)
}

// stackClass is the class attribute of column i (kind "c") or gap i (kind
// "s") of the n-th columns or social block of the email.
func stackClass(n int, kind string, i int) string {
	return ` class="` + stackName(n, kind, i) + `"`
}

func stackName(n int, kind string, i int) string {
	return "c" + strconv.Itoa(n) + "-" + kind + strconv.Itoa(i)
}

// nextContainer numbers a columns or social block: from 1, in document order,
// a container before the blocks inside it, as phoneRules numbers them.
func (r *renderer) nextContainer() int {
	r.containers++
	return r.containers
}

// section writes a band of blocks: one column on the section's background
// (none by default), inside its inset (24 pixels at the sides by default),
// at most its maximum width (all there is by default) and with its cell
// padding (24 by default) around the blocks. The blocks have the smaller of
// the maximum width and the room inside the inset, less the cell padding on
// both sides.
func (r *renderer) section(b Block, width float64, pad [4]float64) error {
	inset := lengthsOr(b.Inset, [4]float64{0, 24, 0, 24})
	room := roomInside(width, pad, inset)
	maxWidth := orNumber(b.MaxWidth, room)
	cellPadding := lengthOr(b.CellPadding, 24)
	inner := math.Min(maxWidth, room) - 2*cellPadding
	return r.frame(r.dataBlock(b), pad, inset, r.colour(b.Background, ""), maxWidth, func() error {
		return r.cell(100, "", cellPadding, func() error { return r.rows(b.Blocks, inner, true) })
	})
}

// columns writes up to four columns of blocks side by side, gap pixels apart
// (16 by default), on the block's background (none by default), inside its
// inset (none by default) and at most its maximum width (all there is by
// default). A column's blocks have its share of that width, rounded down to
// a whole pixel, less its cell padding (none by default) on both sides. On a
// phone narrower than stackBelow (450 by default) the columns stack.
func (r *renderer) columns(b Block, width float64, pad [4]float64) error {
	n := r.nextContainer()
	inset := lengthsOr(b.Inset, [4]float64{})
	room := roomInside(width, pad, inset)
	maxWidth := orNumber(b.MaxWidth, room)
	content := math.Min(maxWidth, room)
	gap := lengthOr(b.Gap, 16)
	return r.frame(r.dataBlock(b), pad, inset, r.colour(b.Background, ""), maxWidth, func() error {
		for i, column := range b.Columns {
			if i > 0 {
				r.gapCell(gap, stackClass(n, "s", i-1))
			}
			cellPadding := lengthOr(column.CellPadding, 0)
			inner := math.Floor(column.Width*content/100) - 2*cellPadding
			blocks := column.Blocks
			if err := r.cell(column.Width, stackClass(n, "c", i), cellPadding, func() error { return r.rows(blocks, inner, true) }); err != nil {
				return err
			}
		}
		return nil
	})
}

// socialTitle is the title of a social block, set as a heading.
func socialTitle(title string) Block {
	return Block{Type: typeHeading, Text: title, Size: socialTitleSize, LineHeight: 20, Weight: 700, Italic: true, LetterSpacing: "-0.032em", Color: "primary", Font: "heading"}
}

const socialTitleSize = 20

// social writes a row of links, each in a column of its own, under a title
// when it has one. The columns share the block's maximum width (451 pixels by
// default) equally once the gaps (16 by default) are taken out, and stack on
// a phone. A link is its network's icon (47 pixels wide by default) when it
// has one that can be sent, else the network's name as a text link.
func (r *renderer) social(b Block, width float64, pad [4]float64) error {
	n := r.nextContainer()
	maxWidth := orNumber(b.MaxWidth, 451)
	gap := lengthOr(b.Gap, 16)
	out := &r.out
	out.WriteString(`<td` + r.dataBlock(b) + ` style="padding:` + padCSS(pad) + `"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="border-collapse:collapse"><tbody>`)
	if b.Title != "" {
		out.WriteString("<tr>")
		r.heading(socialTitle(b.Title), [4]float64{0, 0, 16, 0}, "")
		out.WriteString("</tr>")
	}
	percent := 0.0
	if links := float64(len(b.Links)); links > 0 {
		// Gaps wider than the block leave the links no room, not less.
		percent = math.Max(0, (maxWidth-(links-1)*gap)/links/maxWidth*100)
	}
	room := math.Floor(percent * math.Min(maxWidth, width-pad[1]-pad[3]) / 100)
	out.WriteString("<tr>")
	err := r.frame("", [4]float64{}, [4]float64{}, "", maxWidth, func() error {
		for i, link := range b.Links {
			if i > 0 {
				r.gapCell(gap, stackClass(n, "s", i-1))
			}
			if err := r.cell(percent, stackClass(n, "c", i), 0, func() error { return r.socialLink(b.ID, link, room) }); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	out.WriteString(`</tr></tbody></table></td>`)
	return nil
}

// socialLink writes what one column of a social block holds: the link's icon,
// linked when the link has an address, or else the network's name, linked
// the same way. An icon that cannot be read or sent, or that would take the
// HTML past what sending takes, gives way to the name. block names the social
// block in issues; room is the column's width.
func (r *renderer) socialLink(block string, link Link, room float64) error {
	href := r.link(link.Href)
	if href == "" {
		r.issue(IssueLinkMissing, block, `Add a link to "`+clip(link.Network, 60)+`".`)
	}
	var pic picture
	if link.Icon != "" {
		var err error
		if pic, err = r.readPicture(link.Icon, block, link.Network); err != nil {
			return err
		}
	}
	width := link.Width
	if width == 0 {
		width = 47
	}
	w := strconv.Itoa(width)
	narrower := float64(width) < room
	// The icon's markup up to its data URI, so whether it fits is known
	// before anything of it is written.
	var icon strings.Builder
	icon.WriteString(`<tr><td>`)
	if href != "" {
		icon.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener nofollow" style="display:block;text-decoration:none;border:none;outline:none" aria-label="` + attr(link.Network) + `">`)
	}
	icon.WriteString(`<table cellpadding="0" cellspacing="0" border="0" style="width:100%"><tbody><tr><td align="center">`)
	if narrower {
		icon.WriteString(msoOpen(w))
	}
	icon.WriteString(`<table cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:` + w + `px"><tbody><tr><td style="width:100%"><img src="`)
	out := &r.out
	out.WriteString(rowsOpen)
	if r.fits(pic, icon.Len()) {
		out.WriteString(icon.String() + attr(pic.uri) + `" width="` + w + `" height="` + strconv.Itoa(pic.scaledHeight(width)) + `" alt="` + attr(link.Network) + `" style="display:block;width:` + w + `px;height:auto;max-width:100%"></td></tr></tbody></table>`)
		if narrower {
			out.WriteString(msoClose)
		}
		out.WriteString(`</td></tr></tbody></table>`)
		if href != "" {
			out.WriteString(`</a>`)
		}
		out.WriteString(`</td></tr>`)
	} else {
		out.WriteString(`<tr><td style="text-align:center;font-family:Arial, Helvetica, sans-serif;font-size:14px;line-height:20px">`)
		style := `style="color:` + r.colour("primary", "") + `;text-decoration:underline">` + escapeText(link.Network)
		if href != "" {
			out.WriteString(`<a href="` + attr(href) + `" target="_blank" rel="noopener nofollow" ` + style + `</a>`)
		} else {
			out.WriteString(`<span ` + style + `</span>`)
		}
		out.WriteString(`</td></tr>`)
	}
	out.WriteString(`</tbody></table>`)
	return nil
}

// footer writes the compliance footer: one column on the footer's background
// (white by default), inside its inset (20 pixels above and below, 24 at the
// sides by default), at most its maximum width (552 by default), with its
// cell padding (13 by default), holding its links joined by the separator
// (a pipe with four spaces either side by default) and, under them, its lines.
// Its text is 14 pixels on 22 by default, the lines muted and the links in
// the secondary colour, gap pixels below the links and after below the lines
// (none by default). A link without an address (or with one that breaks the
// link rule) is its words alone, underlined like the others, and is reported
// as link_missing, as a button or a social link without one is.
func (r *renderer) footer(b Block, pad [4]float64) error {
	size, lineHeight := number(orNumber(b.Size, 14)), number(orNumber(b.LineHeight, 22))
	style := emphasis(400, false, b.LetterSpacing) + r.fontFamily(b.Font, "base")
	linkColour := r.colour(b.LinkColor, "secondary")
	separator := "    |    "
	if b.Separator != nil {
		separator = *b.Separator
	}
	inset := lengthsOr(b.Inset, [4]float64{20, 24, 20, 24})
	return r.frame(r.dataBlock(b), pad, inset, r.colour(b.Background, "white"), orNumber(b.MaxWidth, 552), func() error {
		return r.cell(100, "", lengthOr(b.CellPadding, 13), func() error {
			out := &r.out
			out.WriteString(rowsOpen)
			if len(b.Links) > 0 {
				out.WriteString(`<tr><td dir="ltr" style="font-size:` + size + `px;` + style + `text-align:center;padding:0px 0px ` + number(lengthOr(b.Gap, 0)) + `px;line-height:` + lineHeight + `px;mso-line-height-alt:` + lineHeight + `px">`)
				for i, link := range b.Links {
					if i > 0 {
						out.WriteString(`<span style="color:` + linkColour + `;white-space:pre-wrap">` + escapeText(separator) + `</span>`)
					}
					label := `<span style="text-decoration:underline;color:` + linkColour + `;white-space:pre-wrap">` + escapeText(link.Label) + `</span>`
					if href := r.link(link.Href); href != "" {
						label = `<a href="` + attr(href) + `" target="_blank" rel="noopener nofollow" style="color:` + linkColour + `;text-decoration:inherit">` + label + `</a>`
					} else {
						// Its words are drawn as a link, so a reader takes them for one.
						r.issue(IssueLinkMissing, b.ID, `Add a link to "`+clip(link.Label, 60)+`".`)
					}
					out.WriteString(label)
					if err := r.grown(); err != nil {
						return err
					}
				}
				out.WriteString(`</td></tr>`)
			}
			if len(b.Lines) > 0 {
				out.WriteString(`<tr><td dir="ltr" style="color:` + r.colour(b.Color, "muted") + `;font-size:` + size + `px;` + style + `white-space:pre-wrap;text-align:center;padding:0px 0px ` + number(b.After) + `px;line-height:` + lineHeight + `px;mso-line-height-alt:` + lineHeight + `px">`)
				for i, line := range b.Lines {
					if i > 0 {
						out.WriteString(`<br>`)
					}
					out.WriteString(escapeText(line))
					if err := r.grown(); err != nil {
						return err
					}
				}
				out.WriteString(`</td></tr>`)
			}
			out.WriteString(`</tbody></table>`)
			return nil
		})
	})
}

// walkBlocks visits blocks in document order, a container before the blocks
// inside it (a section's, then each column's in turn), as the writers write
// them, until visit returns false.
func walkBlocks(blocks []Block, visit func(Block) bool) bool {
	for _, b := range blocks {
		if !visit(b) {
			return false
		}
		switch b.Type {
		case typeSection:
			if !walkBlocks(b.Blocks, visit) {
				return false
			}
		case typeColumns:
			for _, column := range b.Columns {
				if !walkBlocks(column.Blocks, visit) {
					return false
				}
			}
		}
	}
	return true
}

// phoneRules writes the phone rules of a layout's head (the plan's markup
// appendix, A.3): for each columns and social block, in document order, the
// rule that stacks its columns below its stackBelow width, then one rule per
// size of text over 16 pixels, in ascending order, that shrinks it below 550
// pixels. They are written before the blocks, so their bytes count toward
// what sending takes like every other byte; each block is asked what its
// writer will mark.
func (r *renderer) phoneRules(blocks []Block) error {
	n := 0
	sizes := map[int]bool{}
	var err error
	walkBlocks(blocks, func(b Block) bool {
		switch b.Type {
		case typeColumns:
			n++
			r.out.WriteString(stackRule(n, len(b.Columns), orNumber(b.StackBelow, 450)))
		case typeSocial:
			n++
			r.out.WriteString(stackRule(n, len(b.Links), 450))
		}
		if tenths, ok := shrinkSize(b); ok {
			sizes[tenths] = true
		}
		err = r.grown()
		return err == nil
	})
	if err != nil || len(sizes) == 0 {
		return err
	}
	ascending := make([]int, 0, len(sizes))
	for tenths := range sizes {
		ascending = append(ascending, tenths)
	}
	sort.Ints(ascending)
	r.out.WriteString(`<style>@media(max-width:550px){`)
	for _, tenths := range ascending {
		r.out.WriteString(`.efs-` + strconv.Itoa(tenths) + `{font-size:` + number(float64(160+(tenths-159)/2)/10) + `px!important}`)
	}
	r.out.WriteString(`}</style>`)
	return r.grown()
}

// stackRule is the phone rule of the n-th columns or social block, of the
// given number of columns: below the width, every column takes the full width
// and every gap becomes 16 pixels of space between them. Without columns it
// has nothing to stack.
func stackRule(n, columns int, below float64) string {
	if columns == 0 {
		return ""
	}
	names := make([]string, columns)
	for i := range names {
		names[i] = "." + stackName(n, "c", i)
	}
	rule := strings.Join(names, ",") + "{display:block!important;width:100%!important}"
	if columns > 1 {
		gaps := make([]string, columns-1)
		for i := range gaps {
			gaps[i] = "." + stackName(n, "s", i)
		}
		rule += strings.Join(gaps, ",") + "{display:block!important;width:auto!important;height:16px;font-size:0}"
	}
	return `<!--[if !mso]><!--><style>@media (max-width:` + number(below) + `px){` + rule + `}</style><!--<![endif]-->`
}

// shrinkSize is the size, in tenths of a pixel, of the text a block marks to
// shrink on phones, if it marks any: a heading's, a highlight's, a text
// block's when it has words to write, and a social block's title. A text
// block has words to write exactly when it has more than white space: the
// Markdown reader makes a paragraph or an item of every line that is not
// blank, and falls back to the whole text, as one paragraph, only for text
// that is not blank. So the Markdown is not read here.
func shrinkSize(b Block) (int, bool) {
	switch b.Type {
	case typeHeading:
		return phoneTenths(headingSize(b))
	case typeHighlight:
		return phoneTenths(highlightSize(b))
	case typeText:
		tenths, ok := phoneTenths(textSize(b))
		return tenths, ok && strings.TrimSpace(b.Text) != ""
	case typeSocial:
		if b.Title != "" {
			return phoneTenths(socialTitleSize)
		}
	}
	return 0, false
}

// dataBlock marks a block's outer cell with its id in a preview, so an editor
// can tell which block was clicked. A sent email carries no mark.
func (r *renderer) dataBlock(b Block) string {
	if !r.preview {
		return ""
	}
	return ` data-block="` + attr(b.ID) + `"`
}

// padding is the block's own padding or, without one, the default for where
// it is. In a section or a column a block has none at the sides and 16 pixels
// below, and the last block none at all. In the email's column a block has 24
// pixels at the sides and 16 below; a section and a picture that bleeds to
// the edges have none at the sides, and a footer none at all.
func padding(b Block, inside, last bool) [4]float64 {
	switch {
	case b.Padding != nil:
		return *b.Padding
	case last:
		return [4]float64{}
	case inside, b.Type == typeSection, b.Type == typeImage && b.Bleed:
		return [4]float64{0, 0, 16, 0}
	case b.Type == typeFooter:
		return [4]float64{}
	}
	return [4]float64{0, 24, 16, 24}
}

// roomInside is the width inside a container's padding and inset, in a
// column width wide. Parse admits padding and insets that together are wider
// than the column; that leaves no room, not less.
func roomInside(width float64, pad, inset [4]float64) float64 {
	return math.Max(0, width-pad[1]-pad[3]-inset[1]-inset[3])
}

// lengthOr is a length the block may leave out (nil), or its default.
func lengthOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}

// lengthsOr is a padding or an inset the block may leave out, or its default.
func lengthsOr(v *[4]float64, fallback [4]float64) [4]float64 {
	if v == nil {
		return fallback
	}
	return *v
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

// phoneTenths is a size of text in tenths of a pixel, and whether it is over
// 16 pixels: text that shrinks on a phone.
func phoneTenths(size float64) (int, bool) {
	if size <= 16 {
		return 0, false
	}
	return int(math.Round(size * 10)), true
}

// phoneSize is the class that lets text over 16 pixels shrink on a phone,
// named for its size in tenths of a pixel.
func phoneSize(size float64) string {
	tenths, ok := phoneTenths(size)
	if !ok {
		return ""
	}
	return ` class="efs-` + strconv.Itoa(tenths) + `"`
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
