package emaildoc

import (
	"context"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/znasllc-io/memql/component/compose"
)

// RenderOptions are what a render needs beyond the document.
type RenderOptions struct {
	Preview        bool              // data-block attributes
	Sample         map[string]string // "displayName", "email", "accountName", "campaignName", "fields.<key>"
	RequireConfirm bool              // report confirm_missing when {{confirmUrl}} is absent
}

// Issue is something about a document that keeps it from being sent as it is.
// A preview shows them; publishing refuses a document that has any. Block is
// the id of the block it is about, when it is about one.
type Issue struct {
	Code    string `json:"code"`
	Block   string `json:"block,omitempty"`
	Message string `json:"message"`
}

// Result is a rendered email.
type Result struct {
	Subject string // sampled
	HTML    string
	Text    string
	Issues  []Issue // in document order; empty, never nil, when there are none
}

// The issue codes.
const (
	IssueImageMissing       = "image_missing"       // an image block without a picture
	IssueImageUnavailable   = "image_unavailable"   // a picture that cannot be read or sent
	IssueImagesLimit        = "images_limit"        // more than MaxImages pictures or MaxImageBytes bytes of them
	IssueLinkMissing        = "link_missing"        // a button without a link
	IssueUnsubscribeMissing = "unsubscribe_missing" // a layout without {{unsubscribeUrl}}
	IssueConfirmMissing     = "confirm_missing"     // no {{confirmUrl}}, when RequireConfirm
)

// Render writes a document as an email: its HTML part, its plain-text part,
// its subject with sample values in it, and the issues that keep it from being
// sent as it is.
//
// A layout is written in the table dialect of the emails layouts are taken
// from: a 600 pixel canvas that is full width on phones, Outlook conditionals,
// and every block's spacing, sizes and colours inline. The text part is made
// from the same blocks. A custom document keeps its own markup; Render
// embeds its Library pictures and puts the preheader after its body tag.
//
// Every picture is embedded as a data URI: artifact: pictures are read with
// images, which may be nil (every artifact: picture is then
// image_unavailable). Merge tags are filled from opts.Sample, escaped in the
// HTML and as written in the text and the subject; the four link tags are
// never sample values and stay as written.
//
// d should be a document Parse returned, which is how the renderer knows its
// links and markup are allowed; html blocks are spliced in the form safeMarkup
// gives them, never as written, and every string of the document is escaped
// where it is written.
//
// The error is for a cancelled ctx only; content problems are Issues.
func Render(ctx context.Context, d Document, images ImageResolver, opts RenderOptions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := &renderer{ctx: ctx, theme: d.Theme, preview: opts.Preview, pics: newPictures(images), issues: []Issue{}}
	var markup, plain string
	var err error
	if d.Mode() == ModeLayout {
		if markup, err = r.layout(d); err != nil {
			return Result{}, err
		}
		plain = layoutText(d.Blocks)
		// Only a layout must carry the link: sending appends a footer with
		// one to custom HTML that has none.
		if !strings.Contains(markup, TagUnsubscribeURL) {
			r.issue(IssueUnsubscribeMissing, "", "Add an unsubscribe link ("+TagUnsubscribeURL+") to the footer.")
		}
	} else {
		if markup, err = r.custom(d); err != nil {
			return Result{}, err
		}
		plain = d.TextBody
	}
	if opts.RequireConfirm && !strings.Contains(markup, TagConfirmURL) && !strings.Contains(plain, TagConfirmURL) {
		r.issue(IssueConfirmMissing, "", "Add a confirm link ("+TagConfirmURL+") so people can confirm their subscription.")
	}
	text, escaped := sampleReplacers(opts.Sample)
	return Result{
		Subject: text.Replace(d.Subject),
		HTML:    escaped.Replace(markup),
		Text:    text.Replace(plain),
		Issues:  r.issues,
	}, nil
}

// renderer holds one render: the output, the pictures read so far and the
// issues found.
type renderer struct {
	ctx     context.Context
	theme   Theme
	preview bool
	pics    *pictures
	out     strings.Builder
	issues  []Issue
}

func (r *renderer) issue(code, block, message string) {
	r.issues = append(r.issues, Issue{Code: code, Block: block, Message: message})
}

// The document around the blocks, in the order it is written. The listings in
// the plan's markup appendix wrap these for reading; the email has no newline.
const (
	docHead = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">` +
		`<html xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office"><head>` +
		`<meta name="viewport" content="width=device-width, initial-scale=1.0">` +
		`<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">` +
		`<meta name="format-detection" content="telephone=no, date=no, address=no, email=no">` +
		`<meta name="x-apple-disable-message-reformatting">`
	docStyle = `<style>body{margin:0;padding:0}table{mso-table-lspace:0;mso-table-rspace:0}p,span,h1,h2,h3,h4,h5,h6{margin:0;padding:0}p{line-height:inherit}a[x-apple-data-detectors]{color:inherit!important;text-decoration:inherit!important}#MessageViewBody a{color:inherit;text-decoration:none}img+div{display:none}@media (max-width:599px){.ecw{width:100%!important;min-width:0!important}}</style>` +
		`<!--[if mso]><div><noscript><xml><w:WordDocument xmlns:w="urn:schemas-microsoft-com:office:word"><w:DontUseAdvancedTypographyReadingMail/></w:WordDocument><o:OfficeDocumentSettings><o:AllowPNG/><o:PixelsPerInch>96</o:PixelsPerInch></o:OfficeDocumentSettings></xml></noscript></div><![endif]-->`
	docColumnOpen = `<tr><td style="vertical-align:top;padding:0px 0px 0px 0px"><table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation"><tbody><tr><td style="vertical-align:top">`
	docTail       = `</td></tr></tbody></table></td></tr>` +
		`<tr><td height="100%" style="height:100%;font-size:0;line-height:0" aria-hidden="true">&nbsp;</td></tr>` +
		`</tbody></table>` +
		`<!--[if mso]></td></tr></tbody></table></center><![endif]-->` +
		`</td></tr></tbody></table></body></html>`
	// rowsOpen opens the table that holds a list of blocks, one row each.
	rowsOpen = `<table align="center" width="100%" border="0" cellpadding="0" cellspacing="0" role="presentation" style="color:#000;font-style:normal;font-weight:normal;font-size:16px;line-height:1.4;letter-spacing:0;text-align:left;direction:ltr;border-collapse:collapse;font-family:Arial, Helvetica, sans-serif;white-space:normal;word-wrap:break-word;word-break:break-word"><tbody>`
)

// canvasWidth is the width of the email's own column.
const canvasWidth = 600

// layout writes the HTML of a layout document.
func (r *renderer) layout(d Document) (string, error) {
	page := attr(r.theme.Page)
	w := &r.out
	w.WriteString(docHead)
	if r.theme.WebFonts {
		w.WriteString(webFonts(r.theme))
	}
	w.WriteString(docStyle)
	w.WriteString(`</head><body style="width:100%;-webkit-text-size-adjust:100%;text-size-adjust:100%;background-color:` + page + `;margin:0;padding:0">`)
	if d.Preheader != "" {
		w.WriteString(preheader(d.Preheader, page))
	}
	w.WriteString(`<table width="100%" border="0" cellpadding="0" cellspacing="0" bgcolor="` + page + `" style="background-color:` + page + `"><tbody><tr><td style="background-color:` + page + `">`)
	w.WriteString(`<!--[if mso]><center><table align="center" border="0" cellpadding="0" cellspacing="0" width="600"><tbody><tr><td><![endif]-->`)
	w.WriteString(`<table align="center" width="600" border="0" cellpadding="0" cellspacing="0" role="presentation" class="ecw" style="max-width:600px;min-height:600px;margin:0 auto;background-color:` + attr(r.theme.Canvas) + `;width:600px;min-width:600px"><tbody>`)
	w.WriteString(docColumnOpen)
	if err := r.rows(d.Blocks, canvasWidth); err != nil {
		return "", err
	}
	w.WriteString(docTail)
	return w.String(), nil
}

// webFonts links the theme's fonts from Google Fonts, one family when the two
// are the same.
func webFonts(t Theme) string {
	family := func(name string) string {
		return attr(strings.ReplaceAll(name, " ", "+")) + ":400,400i,700,700i"
	}
	families := family(t.HeadingFont)
	if t.BodyFont != t.HeadingFont {
		families += "|" + family(t.BodyFont)
	}
	return `<link href="https://fonts.googleapis.com/css?family=` + families + `&amp;display=swap" rel="stylesheet">`
}

// preheader is the hidden line an inbox shows after the subject, padded with
// invisible characters so the inbox does not fill the rest of the line from
// the email's first words. colour is the page colour, written escaped.
func preheader(text, colour string) string {
	return `<div style="display:none;font-size:1px;color:` + colour + `;line-height:1px;max-height:0px;max-width:0px;opacity:0;overflow:hidden;mso-hide:all">` +
		escapeText(text) + strings.Repeat("&#847;&zwnj;&nbsp;", 60) + `</div>`
}

// sampleReplacers builds the pair that fills merge tags with sample values:
// escaped for the HTML part, as written for the text part and the subject.
// Only the tags a recipient fills are sample keys (displayName, email,
// accountName, campaignName and fields.<key>), as at sending; a link tag is
// never one, so a preview shows the link tags as written. The two are built
// from one list, so a tag is never filled in one part and left in the other.
func sampleReplacers(sample map[string]string) (text, escaped *strings.Replacer) {
	keys := make([]string, 0, len(sample))
	for key := range sample {
		if isSampleKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var plain, markup []string
	for _, key := range keys {
		tag := "{{" + key + "}}"
		plain = append(plain, tag, sample[key])
		markup = append(markup, tag, htmlEscaper.Replace(sample[key]))
	}
	return strings.NewReplacer(plain...), strings.NewReplacer(markup...)
}

// isSampleKey reports whether a sample key names a merge tag a recipient fills.
// A field name holds no brace, so a key can never reach into the tag after it.
func isSampleKey(key string) bool {
	switch key {
	case "displayName", "email", "accountName", "campaignName":
		return true
	}
	field, ok := strings.CutPrefix(key, "fields.")
	return ok && field != "" && !strings.ContainsAny(field, "{}")
}

// custom writes the HTML of a custom document: the author's markup with its
// Library pictures embedded and the preheader after the body tag. Everything
// else stays as the author wrote it. The markup has no theme, so the hidden
// preheader is written in white.
func (r *renderer) custom(d Document) (string, error) {
	if d.HTMLBody == "" {
		return "", nil
	}
	alts := imgAlts(d.HTMLBody)
	body, err := compose.RewriteEmailImageSources(d.HTMLBody, func(src string) (string, error) {
		return r.customPicture(src, alts[src])
	})
	if err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		// Past a cancelled context, all it refuses is markup over 2 MiB,
		// which Parse never admits; it stays as written.
		body = d.HTMLBody
	}
	if d.Preheader != "" {
		body = afterBodyTag(body, preheader(d.Preheader, "#ffffff"))
	}
	return body, nil
}

// customPicture embeds one img source of custom markup, or says why it cannot.
// A Library picture becomes a data URI; an embedded one is checked and kept as
// written; a cid: or memql-asset: source is a picture nothing can resolve any
// more; any other source (a web address) is the author's to keep. alt is the
// img's alt, the name of the picture in an issue.
func (r *renderer) customPicture(src, alt string) (string, error) {
	trimmed := strings.TrimSpace(src)
	lower := strings.ToLower(trimmed)
	label := alt
	if label == "" {
		label = clip(trimmed, 40)
	}
	var key string
	switch {
	case strings.HasPrefix(lower, "artifact:"):
		id := trimmed[len("artifact:"):]
		key = "artifact:" + id
		if alt == "" {
			label = id
		}
	case strings.HasPrefix(lower, "data:"):
		key = trimmed
	case strings.HasPrefix(lower, "cid:"), strings.HasPrefix(lower, "memql-asset:"):
		r.issue(IssueImageUnavailable, "", pictureUnavailable(label))
		return src, nil
	default:
		return src, nil
	}
	pic, limit, err := r.pics.look(r.ctx, key)
	if err != nil {
		return "", err
	}
	if limit {
		r.issue(IssueImagesLimit, "", picturesOverBudget)
	}
	if pic.problem == IssueImageUnavailable {
		r.issue(IssueImageUnavailable, "", pictureUnavailable(label))
	}
	if pic.uri == "" || strings.HasPrefix(lower, "data:") {
		return src, nil
	}
	return pic.uri, nil
}

// imgAlts reads the img tags of custom markup for the words of the issues they
// may lead to: the alt of the first img with each source. It reads the tokens
// RewriteEmailImageSources reads, which keep the first of two equal
// attributes, as a browser does.
func imgAlts(markup string) map[string]string {
	alts := map[string]string{}
	z := html.NewTokenizer(strings.NewReader(markup))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return alts
		case html.StartTagToken, html.SelfClosingTagToken:
			token := z.Token()
			if token.Data != "img" {
				continue
			}
			var src, alt string
			for _, a := range token.Attr {
				switch a.Key {
				case "src":
					src = a.Val
				case "alt":
					alt = a.Val
				}
			}
			if _, seen := alts[src]; !seen {
				alts[src] = alt
			}
		}
	}
}

// afterBodyTag puts insert right after the first body start tag of markup, or
// before everything when it has none. Tags in comments and scripts do not
// count: the markup is read the way a browser reads it.
func afterBodyTag(markup, insert string) string {
	z := html.NewTokenizer(strings.NewReader(markup))
	offset := 0
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			return insert + markup
		}
		offset += len(z.Raw())
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			if name, _ := z.TagName(); string(name) == "body" {
				return markup[:offset] + insert + markup[offset:]
			}
		}
	}
}
