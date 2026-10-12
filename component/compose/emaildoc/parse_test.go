package emaildoc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func neutralTheme() string {
	return `{"page":"#eef0f3","canvas":"#ffffff","primary":"#1f5f8b","secondary":"#5b8fb9","accent":"#d9534f","tint":"#f6f4ef","text":"#2b2b2b","muted":"#8a8f98","headingFont":"Georgia","bodyFont":"Arial"}`
}

func layoutJSON(blocks string) string {
	return `{"subject":"Hello","theme":` + neutralTheme() + `,"blocks":[` + blocks + `]}`
}

func TestParseAcceptsBothModes(t *testing.T) {
	d, err := Parse([]byte(layoutJSON(`{"id":"headline","type":"heading","text":"Hi {{displayName}}"}`)))
	if err != nil || d.Mode() != ModeLayout || d.Blocks[0].Text != "Hi {{displayName}}" {
		t.Fatalf("layout: %+v %v", d, err)
	}
	c, err := Parse([]byte(`{"subject":"Plain","textBody":"Hello","htmlBody":""}`))
	if err != nil || c.Mode() != ModeCustom || c.HTMLBody != "" {
		t.Fatalf("custom: %+v %v", c, err)
	}
}

func TestParseRefuses(t *testing.T) {
	big := `{"subject":"x","textBody":"` + strings.Repeat("a", MaxBytes) + `","htmlBody":""}`
	var blocks []string
	for n := 0; n <= MaxBlocks; n++ {
		blocks = append(blocks, fmt.Sprintf(`{"id":"s-%d","type":"spacer","height":4}`, n))
	}
	cases := []struct{ name, doc, want string }{
		{"both modes", `{"subject":"x","textBody":"y","htmlBody":"","blocks":[],"theme":` + neutralTheme() + `}`, "either a layout (blocks) or custom HTML (textBody and htmlBody), not both"},
		{"neither mode", `{"subject":"x"}`, "needs blocks (a layout) or textBody and htmlBody (custom HTML)"},
		{"unknown top key", `{"subject":"x","textBody":"y","htmlBody":"","extra":1}`, `unknown field "extra"`},
		{"unknown block key", layoutJSON(`{"id":"h","type":"heading","text":"x","src":"y"}`), `block "h": unknown field "src"`},
		{"unknown type", layoutJSON(`{"id":"h","type":"marquee"}`), `block "h": unknown block type "marquee"`},
		{"bad id", layoutJSON(`{"id":"Hero_1","type":"spacer","height":4}`), `block id "Hero_1" must be lowercase letters, digits and hyphens`},
		{"duplicate id", layoutJSON(`{"id":"a","type":"spacer","height":4},{"id":"s","type":"section","blocks":[{"id":"a","type":"spacer","height":4}]}`), `block id "a" is used twice`},
		{"hex colour", layoutJSON(`{"id":"h","type":"heading","text":"x","color":"#ff0000"}`), `block "h": color must name a theme colour`},
		{"theme hex", `{"subject":"x","theme":` + strings.Replace(neutralTheme(), `"#eef0f3"`, `"red"`, 1) + `,"blocks":[]}`, "theme: page must be a #rrggbb colour"},
		{"theme font", `{"subject":"x","theme":` + strings.Replace(neutralTheme(), `"Georgia"`, `"1x"`, 1) + `,"blocks":[]}`, "theme: headingFont must be a font family name"},
		{"nested section", layoutJSON(`{"id":"s","type":"section","blocks":[{"id":"t","type":"section","blocks":[]}]}`), `block "t": a section cannot contain another section`},
		{"columns in columns", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"d","type":"columns","columns":[]}]}]}`), `block "d": columns cannot contain columns`},
		{"footer in section", layoutJSON(`{"id":"s","type":"section","blocks":[{"id":"f","type":"footer","lines":["x"]}]}`), `block "f": a footer belongs at the top level`},
		{"wide image", layoutJSON(`{"id":"i","type":"image","width":601}`), `block "i": width must be between 1 and 600 pixels`},
		{"remote image", layoutJSON(`{"id":"i","type":"image","width":100,"src":"https://cdn.example/a.png"}`), `block "i": a picture must be a Library file (artifact:) or an embedded PNG, JPEG or GIF`},
		{"script href", layoutJSON(`{"id":"b","type":"button","label":"Go","href":"javascript:alert(1)"}`), `block "b": a link must be a web address, an email address, a phone number or one of`},
		{"field tag href", layoutJSON(`{"id":"b","type":"button","label":"Go","href":"{{fields.url}}"}`), `block "b": a link must be`},
		{"markdown script link", layoutJSON(`{"id":"t","type":"text","text":"[x](javascript:alert(1))"}`), `block "t": a link must be`},
		{"multiline subject", `{"subject":"a\nb","textBody":"y","htmlBody":""}`, "email subject must be a nonempty single line under 999 bytes"},
		{"long preheader", `{"subject":"x","preheader":"` + strings.Repeat("p", 201) + `","textBody":"y","htmlBody":""}`, "the preheader must be one line of at most 200 characters"},
		{"empty text body", `{"subject":"x","textBody":" ","htmlBody":""}`, "email needs a plain-text body"},
		{"too large", big, "This email file is larger than 2 MiB. Use fewer or smaller pictures."},
		{"trailing", `{"subject":"x","textBody":"y","htmlBody":""} {}`, "email document contains trailing content"},
		{"too many blocks", layoutJSON(strings.Join(blocks, ",")), "an email can have at most 300 blocks"},
		{"html image", layoutJSON(`{"id":"x","type":"html","html":"<img src=\"data:image/png;base64,AA\">"}`), `block "x": html blocks cannot contain pictures; use an image block`},
		{"html script", layoutJSON(`{"id":"x","type":"html","html":"<script>1</script>"}`), `block "x": html blocks cannot use <script>`},
		{"html handler", layoutJSON(`{"id":"x","type":"html","html":"<p onclick=\"x\">a</p>"}`), `block "x": html blocks cannot use the onclick attribute`},
		{"html css url", layoutJSON(`{"id":"x","type":"html","html":"<p style=\"background:url(x)\">a</p>"}`), `block "x": html block styles cannot load resources`},
		{"html conditional", layoutJSON(`{"id":"x","type":"html","html":"<!--[if mso]><v:rect/><![endif]-->"}`), `block "x": html blocks cannot contain comments`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestDocumentJSONRoundTripsAndKeepsMarkupReadable(t *testing.T) {
	src := layoutJSON(`{"id":"x","type":"html","html":"<table><tr><td>A &amp; B</td></tr></table>"}`)
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.JSON()
	// "u003c" only appears when encoding/json escaped a "<"; the file must keep markup readable.
	if err != nil || !bytes.HasSuffix(out, []byte("}\n")) || bytes.Contains(out, []byte("u003c")) || !bytes.Contains(out, []byte("<table>")) {
		t.Fatalf("canonical bytes: %s %v", out, err)
	}
	again, err := Parse(out)
	if err != nil || !reflect.DeepEqual(again, d) {
		t.Fatalf("round trip changed the document: %v", err)
	}
	custom, _ := Parse([]byte(`{"subject":"x","textBody":"y","htmlBody":""}`))
	cb, _ := custom.JSON()
	if !bytes.Contains(cb, []byte(`"htmlBody": ""`)) {
		t.Fatalf("custom mode must keep an empty htmlBody: %s", cb)
	}
}

// refusal is one document Parse must refuse, and the sentence it must say.
type refusal struct{ name, doc, want string }

func expectRefused(t *testing.T, cases []refusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func spacer(id string) string { return `{"id":` + jsonString(id) + `,"type":"spacer","height":4}` }

func repeatJoin(n int, one func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = one(i)
	}
	return strings.Join(parts, ",")
}

func encodedImage(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func imageURI(t *testing.T, format string) string {
	t.Helper()
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(encodedImage(t, format))
}

func TestParseRefusesMalformedDocuments(t *testing.T) {
	theme := neutralTheme()
	expectRefused(t, []refusal{
		{"empty", ``, "email document is empty"},
		{"only whitespace", " \n", "email document is empty"},
		{"truncated", `{"subject":`, "email document is not valid JSON"},
		{"syntax error", `{subject}`, "email document is not valid JSON"},
		{"array", `[]`, "email document must be a JSON object"},
		{"null", `null`, "email document must be a JSON object"},
		{"string", `"x"`, "email document must be a JSON object"},
		{"number", `5`, "email document must be a JSON object"},
		{"trailing garbage", `{"subject":"x","textBody":"y","htmlBody":""} garbage`, "email document contains trailing content"},
		{"duplicate top key", `{"subject":"a","subject":"b","textBody":"y","htmlBody":""}`, `duplicate field "subject"`},
		{"duplicate block key", layoutJSON(`{"id":"h","type":"heading","text":"a","text":"b"}`), `block "h": duplicate field "text"`},
		{"duplicate theme key", `{"subject":"x","theme":` + strings.Replace(theme, `{"page"`, `{"page":"#000000","page"`, 1) + `,"blocks":[]}`, `theme: duplicate field "page"`},
		// encoding/json matches keys case-insensitively and folds some Unicode
		// letters; a strict document must not.
		{"top key in another case", `{"Subject":"x","textBody":"y","htmlBody":""}`, `unknown field "Subject"`},
		{"block key in another case", layoutJSON(`{"id":"h","type":"heading","Text":"x"}`), `block "h": unknown field "Text"`},
		{"block key folded to ascii", layoutJSON("{\"id\":\"h\",\"type\":\"heading\",\"ſize\":12}"), "block \"h\": unknown field"},
		{"blocks null", `{"subject":"x","theme":` + theme + `,"blocks":null}`, "blocks must be a list of blocks"},
		{"blocks object", `{"subject":"x","theme":` + theme + `,"blocks":{}}`, "blocks must be a list of blocks"},
		{"block is a number", layoutJSON(`5`), "a block must be an object"},
		{"missing theme", `{"subject":"x","blocks":[]}`, "theme is required"},
		{"null theme", `{"subject":"x","theme":null,"blocks":[]}`, "theme is required"},
		{"theme is text", `{"subject":"x","theme":"dark","blocks":[]}`, "theme must be an object"},
		{"unknown theme key", `{"subject":"x","theme":` + strings.Replace(theme, `{"page"`, `{"shade":"#000000","page"`, 1) + `,"blocks":[]}`, `theme: unknown field "shade"`},
		{"missing body font", `{"subject":"x","theme":` + strings.Replace(theme, `,"bodyFont":"Arial"`, ``, 1) + `,"blocks":[]}`, "theme: bodyFont must be a font family name"},
		{"missing canvas", `{"subject":"x","theme":` + strings.Replace(theme, `"canvas":"#ffffff",`, ``, 1) + `,"blocks":[]}`, "theme: canvas must be a #rrggbb colour"},
		{"short hex", `{"subject":"x","theme":` + strings.Replace(theme, `"#eef0f3"`, `"#eef"`, 1) + `,"blocks":[]}`, "theme: page must be a #rrggbb colour"},
		{"font with punctuation", `{"subject":"x","theme":` + strings.Replace(theme, `"Georgia"`, `"Georgia; color:red"`, 1) + `,"blocks":[]}`, "theme: headingFont must be a font family name"},
		{"web fonts not a boolean", `{"subject":"x","theme":` + strings.Replace(theme, `"bodyFont":"Arial"`, `"bodyFont":"Arial","webFonts":"yes"`, 1) + `,"blocks":[]}`, "theme: webFonts must be true or false"},
		{"layout provenance", `{"subject":"x","layout":"article","theme":` + theme + `,"blocks":[]}`, "layout must be empty, builtin:<name> or org:<id>"},
		{"layout provenance upper case", `{"subject":"x","layout":"builtin:Article","theme":` + theme + `,"blocks":[]}`, "layout must be empty, builtin:<name> or org:<id>"},
		{"subject is a number", `{"subject":5,"textBody":"y","htmlBody":""}`, "subject must be text"},
		{"blank subject", `{"subject":"  ","textBody":"y","htmlBody":""}`, "email subject must be a nonempty single line under 999 bytes"},
		{"subject of 999 bytes", `{"subject":"` + strings.Repeat("s", 999) + `","textBody":"y","htmlBody":""}`, "email subject must be a nonempty single line under 999 bytes"},
		{"tab in preheader", `{"subject":"x","preheader":"a\tb","textBody":"y","htmlBody":""}`, "the preheader must be one line of at most 200 characters"},
		{"html without text", `{"subject":"x","htmlBody":"<p>x</p>"}`, "email needs a plain-text body"},
		{"text body is a number", `{"subject":"x","textBody":5}`, "textBody must be text"},
		{"theme in custom mode", `{"subject":"x","textBody":"y","htmlBody":"","theme":` + theme + `}`, `unknown field "theme"`},
		{"layout in custom mode", `{"subject":"x","textBody":"y","layout":"builtin:letter"}`, `unknown field "layout"`},
		{"text body in layout mode", `{"subject":"x","theme":` + theme + `,"blocks":[],"textBody":"y"}`, "either a layout (blocks) or custom HTML (textBody and htmlBody), not both"},
	})
}

func TestParseRefusesBadBlockValues(t *testing.T) {
	expectRefused(t, []refusal{
		{"missing id", layoutJSON(`{"type":"spacer","height":4}`), `block id "" must be lowercase letters, digits and hyphens`},
		{"id starts with a hyphen", layoutJSON(`{"id":"-a","type":"spacer"}`), `block id "-a" must be lowercase letters, digits and hyphens`},
		{"id of 65 characters", layoutJSON(`{"id":"` + strings.Repeat("a", 65) + `","type":"spacer"}`), "must be lowercase letters, digits and hyphens"},
		{"id is a number", layoutJSON(`{"id":5,"type":"spacer"}`), "a block id must be text"},
		{"missing type", layoutJSON(`{"id":"h"}`), `block "h": unknown block type ""`},
		{"type is a number", layoutJSON(`{"id":"h","type":5}`), `block "h": type must be text`},
		{"bad role", layoutJSON(`{"id":"h","type":"heading","role":"hero"}`), `block "h": role must be "brand" or "content"`},
		{"hex background", layoutJSON(`{"id":"h","type":"highlight","background":"#fff"}`), `block "h": background must name a theme colour`},
		{"named text colour", layoutJSON(`{"id":"b","type":"button","label":"Go","textColor":"red"}`), `block "b": textColor must name a theme colour`},
		{"named link colour", layoutJSON(`{"id":"t","type":"text","linkColor":"blue"}`), `block "t": linkColor must name a theme colour`},
		{"colour list in the sentence", layoutJSON(`{"id":"h","type":"heading","color":"red"}`), "(page, canvas, primary, secondary, accent, tint, text, muted or white)"},
		{"bad align", layoutJSON(`{"id":"h","type":"heading","align":"justify"}`), `block "h": align must be left, center or right`},
		{"bad weight", layoutJSON(`{"id":"h","type":"heading","weight":500}`), `block "h": weight must be 400 or 700`},
		{"bad font", layoutJSON(`{"id":"h","type":"heading","font":"mono"}`), `block "h": font must be heading, body or base`},
		{"letter spacing in px", layoutJSON(`{"id":"h","type":"heading","letterSpacing":"2px"}`), `block "h": letterSpacing must be a length in em`},
		{"letter spacing with three digits", layoutJSON(`{"id":"h","type":"heading","letterSpacing":"100em"}`), `block "h": letterSpacing must be a length in em`},
		{"size below 8", layoutJSON(`{"id":"h","type":"heading","size":7}`), `block "h": size must be between 8 and 96 pixels`},
		{"size above 96", layoutJSON(`{"id":"h","type":"heading","size":97}`), `block "h": size must be between 8 and 96 pixels`},
		{"negative line height", layoutJSON(`{"id":"h","type":"heading","lineHeight":-1}`), `block "h": lineHeight must be between 0 and 200 pixels`},
		{"line height above 200", layoutJSON(`{"id":"h","type":"heading","lineHeight":201}`), `block "h": lineHeight must be between 0 and 200 pixels`},
		{"list line height", layoutJSON(`{"id":"t","type":"text","listLineHeight":201}`), `block "t": listLineHeight must be between 0 and 200 pixels`},
		{"negative gap", layoutJSON(`{"id":"t","type":"text","gap":-1}`), `block "t": gap must be between 0 and 200 pixels`},
		{"footer after", layoutJSON(`{"id":"f","type":"footer","after":201}`), `block "f": after must be between 0 and 200 pixels`},
		{"section cell padding", layoutJSON(`{"id":"s","type":"section","cellPadding":201}`), `block "s": cellPadding must be between 0 and 200 pixels`},
		{"button vertical padding", layoutJSON(`{"id":"b","type":"button","label":"Go","paddingY":201}`), `block "b": paddingY must be between 0 and 200 pixels`},
		{"spacer taller than 2000", layoutJSON(`{"id":"s","type":"spacer","height":2001}`), `block "s": height must be between 0 and 2000 pixels`},
		{"section max width", layoutJSON(`{"id":"s","type":"section","maxWidth":2001}`), `block "s": maxWidth must be between 0 and 2000 pixels`},
		{"negative stack point", layoutJSON(`{"id":"c","type":"columns","stackBelow":-1,"columns":[{"width":100,"blocks":[]}]}`), `block "c": stackBelow must be between 0 and 2000 pixels`},
		{"padding of three", layoutJSON(`{"id":"h","type":"heading","padding":[1,2,3]}`), `block "h": padding must be four numbers (top, right, bottom, left)`},
		{"padding of five", layoutJSON(`{"id":"h","type":"heading","padding":[1,2,3,4,5]}`), `block "h": padding must be four numbers (top, right, bottom, left)`},
		{"padding is a number", layoutJSON(`{"id":"h","type":"heading","padding":5}`), `block "h": padding must be four numbers (top, right, bottom, left)`},
		{"padding above 200", layoutJSON(`{"id":"h","type":"heading","padding":[0,201,0,0]}`), `block "h": padding values must be between 0 and 200 pixels`},
		{"negative inset", layoutJSON(`{"id":"s","type":"section","inset":[0,0,-1,0]}`), `block "s": inset values must be between 0 and 200 pixels`},
		{"padding on a spacer", layoutJSON(`{"id":"s","type":"spacer","height":4,"padding":[0,0,0,0]}`), `block "s": unknown field "padding"`},
		{"text on a section", layoutJSON(`{"id":"s","type":"section","text":"x"}`), `block "s": unknown field "text"`},
		{"html on a heading", layoutJSON(`{"id":"h","type":"heading","html":"<p>x</p>"}`), `block "h": unknown field "html"`},
		{"size is text", layoutJSON(`{"id":"h","type":"heading","size":"big"}`), `block "h": size must be a number`},
		{"fractional image width", layoutJSON(`{"id":"i","type":"image","width":100.5}`), `block "i": width must be a whole number`},
		{"italic is text", layoutJSON(`{"id":"h","type":"heading","italic":"yes"}`), `block "h": italic must be true or false`},
		{"text is a number", layoutJSON(`{"id":"h","type":"heading","text":5}`), `block "h": text must be text`},
		{"lines is text", layoutJSON(`{"id":"f","type":"footer","lines":"x"}`), `block "f": lines must be a list of text`},
		{"separator longer than 40 bytes", layoutJSON(`{"id":"f","type":"footer","separator":"` + strings.Repeat("x", 41) + `"}`), `block "f": separator must be at most 40 bytes`},
	})
}

func TestParseRefusesBadContainers(t *testing.T) {
	expectRefused(t, []refusal{
		{"section in a column", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"t","type":"section","blocks":[]}]}]}`), `block "t": a column cannot contain a section`},
		{"footer in a column", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"f","type":"footer"}]}]}`), `block "f": a footer belongs at the top level`},
		{"columns missing", layoutJSON(`{"id":"c","type":"columns"}`), `block "c": columns needs 1 to 4 columns`},
		{"no columns", layoutJSON(`{"id":"c","type":"columns","columns":[]}`), `block "c": columns needs 1 to 4 columns`},
		{"five columns", layoutJSON(`{"id":"c","type":"columns","columns":[` + repeatJoin(5, func(int) string { return `{"width":10,"blocks":[]}` }) + `]}`), `block "c": columns needs 1 to 4 columns`},
		{"column of zero width", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":0,"blocks":[]}]}`), `block "c": column 1: width must be greater than 0 percent`},
		{"column widths over 100", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":33.34,"blocks":[]},{"width":33.34,"blocks":[]},{"width":33.34,"blocks":[]}]}`), `block "c": column widths add up to more than 100 percent`},
		{"column with an unknown key", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":50,"blocks":[],"x":1}]}`), `block "c": column 1: unknown field "x"`},
		{"column is not an object", layoutJSON(`{"id":"c","type":"columns","columns":[5]}`), `block "c": column 1 must be an object`},
		{"column cell padding", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":50,"cellPadding":201,"blocks":[]}]}`), `block "c": column 1: cellPadding must be between 0 and 200 pixels`},
		{"column width is text", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":"50","blocks":[]}]}`), `block "c": column 1: width must be a number`},
		{"column blocks is not a list", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":50,"blocks":5}]}`), `block "c": column 1: blocks must be a list of blocks`},
		{"section blocks is not a list", layoutJSON(`{"id":"s","type":"section","blocks":{}}`), `block "s": blocks must be a list of blocks`},
		{"social without links", layoutJSON(`{"id":"s","type":"social","links":[]}`), `block "s": social needs 1 to 8 links`},
		{"social with no links key", layoutJSON(`{"id":"s","type":"social"}`), `block "s": social needs 1 to 8 links`},
		{"social with nine links", layoutJSON(`{"id":"s","type":"social","links":[` + repeatJoin(9, func(int) string { return `{"network":"n","href":""}` }) + `]}`), `block "s": social needs 1 to 8 links`},
		{"empty network", layoutJSON(`{"id":"s","type":"social","links":[{"network":"","href":""}]}`), `block "s": link 1: network must be 1 to 40 bytes`},
		{"network of 41 bytes", layoutJSON(`{"id":"s","type":"social","links":[{"network":"` + strings.Repeat("n", 41) + `","href":""}]}`), `block "s": link 1: network must be 1 to 40 bytes`},
		{"label on a social link", layoutJSON(`{"id":"s","type":"social","links":[{"network":"n","label":"x"}]}`), `block "s": link 1: unknown field "label"`},
		{"remote social icon", layoutJSON(`{"id":"s","type":"social","links":[{"network":"n","icon":"https://cdn.example/i.png"}]}`), `block "s": link 1: a picture must be`},
		{"wide social icon", layoutJSON(`{"id":"s","type":"social","links":[{"network":"n","width":601}]}`), `block "s": link 1: width must be between 0 and 600 pixels`},
		{"script social link", layoutJSON(`{"id":"s","type":"social","links":[{"network":"n","href":"javascript:1"}]}`), `block "s": link 1: a link must be`},
		{"social link is not an object", layoutJSON(`{"id":"s","type":"social","links":["a"]}`), `block "s": link 1 must be an object`},
		{"footer with nine links", layoutJSON(`{"id":"f","type":"footer","links":[` + repeatJoin(9, func(int) string { return `{"label":"l","href":""}` }) + `]}`), `block "f": a footer can have at most 8 links`},
		{"footer link without a label", layoutJSON(`{"id":"f","type":"footer","links":[{"label":"","href":""}]}`), `block "f": link 1: label must be 1 to 80 characters`},
		{"footer link label of 81", layoutJSON(`{"id":"f","type":"footer","links":[{"label":"` + strings.Repeat("l", 81) + `","href":""}]}`), `block "f": link 1: label must be 1 to 80 characters`},
		{"network on a footer link", layoutJSON(`{"id":"f","type":"footer","links":[{"label":"a","network":"b"}]}`), `block "f": link 1: unknown field "network"`},
		{"data footer link", layoutJSON(`{"id":"f","type":"footer","links":[{"label":"a","href":"data:text/html,x"}]}`), `block "f": link 1: a link must be`},
		{"footer with thirteen lines", layoutJSON(`{"id":"f","type":"footer","lines":[` + repeatJoin(13, func(int) string { return `"x"` }) + `]}`), `block "f": a footer can have at most 12 lines`},
		{"footer line of 301", layoutJSON(`{"id":"f","type":"footer","lines":["` + strings.Repeat("x", 301) + `"]}`), `block "f": line 1 is longer than 300 characters`},
		{"html over 64 KiB", layoutJSON(`{"id":"x","type":"html","html":"` + strings.Repeat("a", 64<<10+1) + `"}`), `block "x": an html block can be at most 64 KiB`},
		{"html with a bad link", layoutJSON(`{"id":"x","type":"html","html":"<a href=\"javascript:1\">x</a>"}`), `block "x": a link must be`},
		{"html style element", layoutJSON(`{"id":"x","type":"html","html":"<style>p{}</style>"}`), `block "x": html blocks cannot use <style>`},
		{"html body element", layoutJSON(`{"id":"x","type":"html","html":"<body>x</body>"}`), `block "x": html blocks cannot use <body>`},
		{"html not text", layoutJSON(`{"id":"x","type":"html","html":5}`), `block "x": html must be text`},
		{"299 blocks and a section of two", layoutJSON(repeatJoin(299, func(i int) string { return spacer(fmt.Sprintf("s-%d", i)) }) + `,{"id":"sec","type":"section","blocks":[` + spacer("in-1") + `,` + spacer("in-2") + `]}`), "an email can have at most 300 blocks"},
	})
}

func TestParseRefusesBadPicturesAndLinks(t *testing.T) {
	over := strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxImageBytes)+4)
	notAnImage := base64.StdEncoding.EncodeToString([]byte("hello"))
	picture := func(src string) string {
		return layoutJSON(`{"id":"i","type":"image","width":100,"src":"` + src + `"}`)
	}
	button := func(href string) string {
		return layoutJSON(`{"id":"b","type":"button","label":"Go","href":"` + href + `"}`)
	}
	pngAsJPEG := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encodedImage(t, "png"))
	expectRefused(t, []refusal{
		{"image without width", layoutJSON(`{"id":"i","type":"image"}`), `block "i": width must be between 1 and 600 pixels`},
		{"image of zero width", layoutJSON(`{"id":"i","type":"image","width":0}`), `block "i": width must be between 1 and 600 pixels`},
		{"svg picture", picture("data:image/svg+xml;base64,PHN2Zy8+"), `block "i": a picture must be a Library file`},
		{"upper case mime", picture("data:IMAGE/PNG;base64,AAAA"), `block "i": a picture must be a Library file`},
		{"data picture without base64", picture("data:image/png,AAAA"), `block "i": a picture must be a Library file`},
		{"artifact without an id", picture("artifact:"), `block "i": a picture must be a Library file`},
		{"artifact id with a space", picture("artifact:a b"), `block "i": a picture must be a Library file`},
		{"artifact id of 161", picture("artifact:" + strings.Repeat("a", 161)), `block "i": a picture must be a Library file`},
		{"scheme-relative picture", picture("//cdn.example/a.png"), `block "i": a picture must be a Library file`},
		{"data picture that is not base64", picture("data:image/png;base64,%%%"), `block "i": the embedded picture is not valid base64`},
		{"data picture with a line break", picture(`data:image/png;base64,AAAA\nAAAA`), `block "i": the embedded picture is not valid base64`},
		{"data picture that is not an image", picture("data:image/png;base64," + notAnImage), `block "i": email image must contain valid PNG, JPEG, or GIF bytes`},
		{"png bytes labelled jpeg", picture(pngAsJPEG), `block "i": email image must contain valid PNG, JPEG, or GIF bytes`},
		{"data picture over 1 MiB", picture("data:image/png;base64," + over), `block "i": email image exceeds 1 MiB`},
		{"data picture with no bytes", picture("data:image/png;base64,"), `block "i": the embedded picture is empty`},
		{"button without a label", layoutJSON(`{"id":"b","type":"button","href":"https://example.com"}`), `block "b": label must be 1 to 200 bytes`},
		{"button label of 201 bytes", layoutJSON(`{"id":"b","type":"button","label":"` + strings.Repeat("l", 201) + `"}`), `block "b": label must be 1 to 200 bytes`},
		{"button wider than 600", layoutJSON(`{"id":"b","type":"button","label":"Go","width":601}`), `block "b": width must be between 0 and 600 pixels`},
		{"link with a space", button("https://example.com/a b"), `block "b": a link cannot contain spaces or control characters`},
		{"link with a line break", button(`https://example.com/a\nb`), `block "b": a link cannot contain spaces or control characters`},
		{"link of 2001 bytes", button("https://example.com/" + strings.Repeat("a", 1981)), `block "b": a link can be at most 2000 bytes`},
		{"tag with a suffix", button("{{unsubscribeUrl}}?x=1"), `block "b": a link must be`},
		{"tag with a prefix", button("x{{viewUrl}}"), `block "b": a link must be`},
		{"tag with a space", button(" {{viewUrl}}"), `block "b": a link cannot contain spaces or control characters`},
		{"two tags", button("{{viewUrl}}{{confirmUrl}}"), `block "b": a link must be`},
		{"an unknown link tag", button("{{unsubscribe}}"), `block "b": a link must be`},
		{"scheme only", button("https://"), `block "b": a link must be`},
		{"mailto only", button("mailto:"), `block "b": a link must be`},
		{"data link", button("data:text/html;base64,AAAA"), `block "b": a link must be`},
		{"ftp link", layoutJSON(`{"id":"i","type":"image","width":100,"href":"ftp://x"}`), `block "i": a link must be`},
		{"markdown link without a destination", layoutJSON(`{"id":"t","type":"text","text":"[x]()"}`), `block "t": a link must be`},
		{"markdown link in a list", layoutJSON(`{"id":"t","type":"text","text":"- [x](javascript:1)"}`), `block "t": a link must be`},
		{"markdown link without a label", layoutJSON(`{"id":"t","type":"text","text":"[](https://example.com)"}`), `block "t": a link needs a label`},
		{"markdown link with a space", layoutJSON(`{"id":"t","type":"text","text":"[x](https://example.com/a b)"}`), `block "t": a link cannot contain spaces or control characters`},
	})
}

func TestParseAcceptsEveryBlockType(t *testing.T) {
	src := strings.NewReplacer("PNG", imageURI(t, "png"), "JPEG", imageURI(t, "jpeg"), "GIF", imageURI(t, "gif")).Replace(`{
  "subject": "Subject {{displayName}}",
  "preheader": "Preview text",
  "layout": "org:abc_123-X",
  "theme": {"page":"#EEF0F3","canvas":"#ffffff","primary":"#1f5f8b","secondary":"#5b8fb9","accent":"#d9534f","tint":"#f6f4ef","text":"#2b2b2b","muted":"#8a8f98","headingFont":"Courier New","bodyFont":"Trebuchet MS","webFonts":true},
  "blocks": [
    {"id":"logo","type":"image","role":"brand","padding":[0,24,16,24],"src":"PNG","alt":"Logo","width":120,"height":40.5,"align":"center","href":"https://example.com","bleed":true},
    {"id":"photo","type":"image","width":600,"src":"artifact:abc_123-X"},
    {"id":"mascot","type":"image","width":80,"src":"JPEG"},
    {"id":"banner","type":"image","width":80,"src":"GIF"},
    {"id":"blank","type":"image","width":80},
    {"id":"headline","type":"heading","role":"content","text":"Hi {{displayName}}","size":28.5,"lineHeight":34,"color":"primary","align":"center","italic":true,"weight":700,"font":"heading","letterSpacing":"-0.02em"},
    {"id":"body","type":"text","text":"One **two** *three* [four](https://example.com/a)\n\n- a\n- b\n\n1. x\n2. y","size":16,"lineHeight":24,"listLineHeight":22,"gap":12,"color":"text","linkColor":"accent","align":"left","weight":400,"font":"body","letterSpacing":"0em"},
    {"id":"band","type":"highlight","text":"Statement","size":20,"lineHeight":26,"gap":0,"color":"white","background":"accent","align":"center","italic":false,"weight":700,"font":"base","letterSpacing":"0.05em"},
    {"id":"go","type":"button","label":"Go","href":"{{confirmUrl}}","color":"accent","textColor":"white","width":240,"align":"center","size":16,"paddingY":12.5,"font":"body"},
    {"id":"rule","type":"divider","color":"muted","height":1},
    {"id":"gap-1","type":"spacer","height":24.5},
    {"id":"panel","type":"section","role":"brand","background":"tint","inset":[8,8,8,8],"maxWidth":520,"cellPadding":12,"padding":[0,0,0,0],"blocks":[{"id":"inner","type":"text","text":"Inside"},{"id":"inner-cols","type":"columns","columns":[{"width":50,"blocks":[{"id":"deep","type":"text","text":"Deep"}]},{"width":50,"blocks":[]}]}]},
    {"id":"cols","type":"columns","background":"canvas","inset":[0,0,0,0],"maxWidth":560,"gap":16,"stackBelow":450,"columns":[{"width":33.33,"cellPadding":8,"blocks":[{"id":"left","type":"text","text":"Left"}]},{"width":33.33,"blocks":[{"id":"right","type":"image","width":100,"src":"artifact:icon1"}]},{"width":33.34,"blocks":[]}]},
    {"id":"follow","type":"social","title":"Follow us","links":[{"network":"Example","href":"https://example.com/x","icon":"artifact:icon1","width":32},{"network":"Mail","href":"mailto:hello@example.com"},{"network":"Phone","href":"tel:+15551234567"}],"maxWidth":300,"gap":8},
    {"id":"fine","type":"footer","links":[{"label":"Unsubscribe","href":"{{unsubscribeUrl}}"},{"label":"Preferences","href":"{{preferencesUrl}}"},{"label":"View online","href":"{{viewUrl}}"},{"label":"Plain","href":""}],"lines":["Acme Inc.","1 Example Street"],"background":"page","inset":[16,16,16,16],"maxWidth":560,"cellPadding":8,"size":12,"lineHeight":18,"letterSpacing":"0.01em","color":"muted","linkColor":"primary","separator":" | ","gap":4,"after":8,"font":"base"},
    {"id":"raw","type":"html","html":"<p style=\"color:#333\">Hello <a href=\"https://example.com\" target=\"_blank\" rel=\"noopener\">link</a> <a href=\"{{viewUrl}}\">view</a></p>"}
  ]
}`)
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode() != ModeLayout || d.Layout != "org:abc_123-X" || !d.Theme.WebFonts || d.Theme.Page != "#EEF0F3" || d.Preheader != "Preview text" {
		t.Fatalf("document fields: %+v", d)
	}
	logo := d.Blocks[0]
	if logo.Role != "brand" || logo.Padding == nil || *logo.Padding != [4]float64{0, 24, 16, 24} || logo.Height != 40.5 || !logo.Bleed || logo.Width != 120 {
		t.Fatalf("image block: %+v", logo)
	}
	if gap := d.Blocks[6].Gap; gap == nil || *gap != 12 {
		t.Fatalf("text gap: %v", gap)
	}
	if gap := d.Blocks[7].Gap; gap == nil || *gap != 0 {
		t.Fatalf("an explicit gap of 0 must survive: %v", gap)
	}
	panel := d.Blocks[11]
	if panel.Type != "section" || len(panel.Blocks) != 2 || panel.Blocks[1].Columns[0].Blocks[0].ID != "deep" || panel.Blocks[1].Columns[1].Blocks == nil {
		t.Fatalf("section: %+v", panel)
	}
	cols := d.Blocks[12]
	if len(cols.Columns) != 3 || cols.Columns[0].CellPadding == nil || *cols.Columns[0].CellPadding != 8 || cols.Columns[2].Width != 33.34 {
		t.Fatalf("columns: %+v", cols)
	}
	footer := d.Blocks[14]
	if len(footer.Links) != 4 || len(footer.Lines) != 2 || footer.Separator == nil || *footer.Separator != " | " {
		t.Fatalf("footer: %+v", footer)
	}

	out, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(out)
	if err != nil || !reflect.DeepEqual(again, d) {
		t.Fatalf("round trip changed the document: %v", err)
	}
	second, err := again.JSON()
	if err != nil || !bytes.Equal(second, out) {
		t.Fatalf("canonical bytes are not stable: %v", err)
	}
}

func TestParseAcceptsLinkForms(t *testing.T) {
	for _, href := range []string{
		"",
		"https://example.com/a?b=1&c=2#top",
		"http://example.com",
		"HTTPS://EXAMPLE.COM/A",
		"mailto:hello@example.com?subject=Hi%20there",
		"tel:+15551234567",
		"https://example.com/?e={{email}}",
		TagUnsubscribeURL, TagPreferencesURL, TagViewURL, TagConfirmURL,
		"https://example.com/" + strings.Repeat("a", 1980),
	} {
		doc := layoutJSON(`{"id":"b","type":"button","label":"Go","href":"` + href + `"}`)
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("href %q: %v", href, err)
		}
	}
	for _, text := range []string{
		"[docs](https://example.com/a?b=1)",
		"[unsubscribe]({{unsubscribeUrl}})",
		"[wiki](https://en.wikipedia.org/wiki/Foo_(bar))",
		"- [a](mailto:a@example.com)\n- [b](tel:+15551234567)",
	} {
		doc := layoutJSON(`{"id":"t","type":"text","text":` + jsonString(text) + `}`)
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("text %q: %v", text, err)
		}
	}
}

func TestParseKeepsMarkupInTextAsData(t *testing.T) {
	// Text is data. Parse keeps it exactly as written and the renderer escapes
	// it; the one place markup is read as markup is an html block, and that is
	// validated (markup_test.go).
	hostile := `<script>alert(1)</script> & "quoted" <b>x</b>`
	quoted, err := json.Marshal(hostile)
	if err != nil {
		t.Fatal(err)
	}
	h := string(quoted)
	d, err := Parse([]byte(layoutJSON(
		`{"id":"head","type":"heading","text":` + h + `},` +
			`{"id":"body","type":"text","text":` + h + `},` +
			`{"id":"pic","type":"image","width":10,"alt":` + h + `},` +
			`{"id":"btn","type":"button","label":` + h + `},` +
			`{"id":"fine","type":"footer","lines":[` + h + `]}`)))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"heading text": d.Blocks[0].Text, "text": d.Blocks[1].Text, "alt": d.Blocks[2].Alt,
		"button label": d.Blocks[3].Label, "footer line": d.Blocks[4].Lines[0],
	} {
		if got != hostile {
			t.Errorf("%s was changed: %q", name, got)
		}
	}
	out, _ := d.JSON()
	if !bytes.Contains(out, []byte(`<script>alert(1)</script>`)) {
		t.Errorf("the canonical file hides the text instead of keeping it readable:\n%s", out)
	}
}

func TestCustomModeKeepsTheAuthorsHTML(t *testing.T) {
	// Custom mode is pasted or hand-written HTML, as every template was before
	// layouts, so it stays valid without a converter. Only the size, the
	// subject and the plain-text body are checked.
	html := `<html><body><script>1</script><img src="https://cdn.example/a.png"><a href="{{fields.x}}">x</a></body></html>`
	d, err := Parse([]byte(`{"subject":"x","textBody":"y","htmlBody":` + jsonString(html) + `}`))
	if err != nil || d.Mode() != ModeCustom || d.HTMLBody != html {
		t.Fatalf("custom html: %+v %v", d, err)
	}
}

func TestDocumentMode(t *testing.T) {
	if (Document{}).Mode() != ModeCustom || (Document{Blocks: nil}).Mode() != ModeCustom {
		t.Error("a document without a block list is custom")
	}
	if (Document{Blocks: []Block{}}).Mode() != ModeLayout {
		t.Error("a document with an empty block list is still a layout")
	}
}

func TestParseAcceptsTheLimits(t *testing.T) {
	theme := neutralTheme()
	cases := []struct{ name, doc string }{
		{"preheader of 200 characters", `{"subject":"x","preheader":"` + strings.Repeat("é", 200) + `","textBody":"y","htmlBody":""}`},
		{"subject of 998 bytes", `{"subject":"` + strings.Repeat("s", 998) + `","textBody":"y","htmlBody":""}`},
		{"text body without html body", `{"subject":"x","textBody":"y"}`},
		{"empty layout", `{"subject":"x","theme":` + theme + `,"blocks":[]}`},
		{"builtin provenance", `{"subject":"x","layout":"builtin:article","theme":` + theme + `,"blocks":[]}`},
		{"empty provenance", `{"subject":"x","layout":"","theme":` + theme + `,"blocks":[]}`},
		{"null optional fields", layoutJSON(`{"id":"h","type":"heading","text":null,"padding":null,"size":null}`)},
		{"300 blocks", layoutJSON(repeatJoin(MaxBlocks, func(i int) string { return spacer(fmt.Sprintf("s-%d", i)) }))},
		{"298 blocks and a section of one", layoutJSON(repeatJoin(298, func(i int) string { return spacer(fmt.Sprintf("s-%d", i)) }) + `,{"id":"sec","type":"section","blocks":[` + spacer("in-1") + `]}`)},
		{"four columns", layoutJSON(`{"id":"c","type":"columns","columns":[` + repeatJoin(4, func(int) string { return `{"width":25,"blocks":[]}` }) + `]}`)},
		{"eight social links", layoutJSON(`{"id":"s","type":"social","links":[` + repeatJoin(8, func(int) string { return `{"network":"n","href":""}` }) + `]}`)},
		{"eight footer links and twelve lines", layoutJSON(`{"id":"f","type":"footer","links":[` + repeatJoin(8, func(int) string { return `{"label":"l","href":""}` }) + `],"lines":[` + repeatJoin(12, func(int) string { return `"` + strings.Repeat("x", 300) + `"` }) + `]}`)},
		{"html of exactly 64 KiB", layoutJSON(`{"id":"x","type":"html","html":"<p>` + strings.Repeat("a", 64<<10-len("<p></p>")) + `</p>"}`)},
		{"separator of 40 bytes", layoutJSON(`{"id":"f","type":"footer","separator":"` + strings.Repeat("x", 40) + `"}`)},
		{"columns in a section", layoutJSON(`{"id":"s","type":"section","blocks":[{"id":"c","type":"columns","columns":[{"width":100,"blocks":[` + spacer("in") + `]}]}]}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseBoundsTextBlocks(t *testing.T) {
	// Reading Markdown costs memory in proportion to its paragraphs and list
	// items, so a text block may have at most 2,000 lines; a list item is one
	// line. A line break at the very end starts no new line.
	block := func(s string) string { return `{"id":"t","type":"text","text":` + jsonString(s) + `}` }
	textBlock := func(s string) string { return layoutJSON(block(s)) }
	const tooLong = `block "t": This text block is too long. Split it into smaller blocks.`
	expectRefused(t, []refusal{
		{"2,001 list items", textBlock(strings.Repeat("- a\n", 2001)), tooLong},
		{"2,001 lines", textBlock(strings.Repeat("a\n", 2000) + "a"), tooLong},
		{"2,001 lines, most of them blank", textBlock("a" + strings.Repeat("\n", 2000) + "b"), tooLong},
		{"2,001 lines ended by carriage returns", textBlock(strings.Repeat("a\r", 2001)), tooLong},
		{"2,001 numbered items in a section", layoutJSON(`{"id":"s","type":"section","blocks":[` + block(strings.Repeat("1. a\n", 2001)) + `]}`), tooLong},
	})
	for name, s := range map[string]string{
		"2,000 list items":                   strings.Repeat("- a\n", 2000),
		"2,000 lines":                        strings.Repeat("a\n", 1999) + "a",
		"2,000 lines ended by CRLF":          strings.Repeat("a\r\n", 2000),
		"2,000 lines, the last one blank":    strings.Repeat("a\n", 1999) + "\n",
		"one long line of 1,000,000 letters": strings.Repeat("a", 1_000_000),
	} {
		if _, err := Parse([]byte(textBlock(s))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The block is refused before its Markdown is read. Decoding the 1.2 MB
	// of JSON costs about 33 MB; reading the 300,000 list items would cost
	// about 90 MB more.
	huge := []byte(textBlock(strings.Repeat("- a\n", 300_000)))
	var err error
	if allocated := allocatedBy(func() { _, err = Parse(huge) }); err == nil || allocated > 64<<20 {
		t.Fatalf("want a refusal without reading the list, got %v after %d bytes", err, allocated)
	}
}

const tooMuchFormatting = "This email has too much formatted text. Shorten it or split it into more emails."

// textBlocks is a layout of one text block per text, with ids t0, t1, ...
func textBlocks(texts ...string) string {
	var blocks []string
	for n, text := range texts {
		blocks = append(blocks, fmt.Sprintf(`{"id":"t%d","type":"text","text":%s}`, n, jsonString(text)))
	}
	return layoutJSON(strings.Join(blocks, ","))
}

func TestParseBoundsFormattedTextPerEmail(t *testing.T) {
	// Reading Markdown costs memory in proportion to its lines and its
	// inline marks, so an email may have at most 5,000 lines of text blocks
	// and 10,000 inline marks (runs of "*", "_" or backticks, each "[" and
	// each "("), all its text blocks together, wherever they sit.
	items := func(n int) string { return strings.Repeat("- a\n", n) }
	runs := func(n int) string { return strings.Repeat("*a* ", n) } // two marks each
	inSection := func(text string) string {
		return `{"id":"s","type":"section","blocks":[{"id":"u","type":"text","text":` + jsonString(text) + `}]}`
	}
	inColumn := func(text string) string {
		return `{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"v","type":"text","text":` + jsonString(text) + `}]}]}`
	}
	for _, c := range []struct {
		name string
		doc  string
		ok   bool
	}{
		{"5,000 lines", textBlocks(items(2000), items(2000), items(1000)), true},
		{"5,001 lines", textBlocks(items(2000), items(2000), items(1001)), false},
		{"5,001 lines, some in a section and a column", layoutJSON(strings.Join([]string{`{"id":"t","type":"text","text":` + jsonString(items(2000)) + `}`, inSection(items(2000)), inColumn(items(1001))}, ",")), false},
		{"10,000 marks", textBlocks(runs(5000)), true},
		{"10,001 marks", textBlocks(runs(5000) + "["), false},
		{"10,001 marks over blocks", textBlocks(runs(2000), runs(2000), runs(1000)+"_"), false},
		{"10,001 marks, some in a section and a column", layoutJSON(strings.Join([]string{`{"id":"t","type":"text","text":` + jsonString(runs(4000)) + `}`, inSection(runs(500)), inColumn(runs(500) + "`")}, ",")), false},
		// A run of marks counts once, however long; an escaped mark is text.
		{"long runs count once", textBlocks(strings.Repeat("***a*** ", 5000)), true},
		{"runs of every kind", textBlocks(strings.Repeat("_a_ ", 1000) + strings.Repeat("``a`` ", 1000) + strings.Repeat("[a] ", 1000) + strings.Repeat("**a** ", 2500)), true},
		{"runs of every kind and one more", textBlocks(strings.Repeat("_a_ ", 1000) + strings.Repeat("``a`` ", 1000) + strings.Repeat("[a] ", 1000) + strings.Repeat("**a** ", 2500) + "_"), false},
		{"escaped marks are text", textBlocks(strings.Repeat(`\*a\* \[ \(`, 5000) + runs(5000)), true},
		// An open parenthesis is a mark: the reader pairs each one it meets.
		{"10,000 open parentheses", textBlocks(strings.Repeat("(", 10_000)), true},
		{"10,001 open parentheses", textBlocks("](" + strings.Repeat("(", 10_000)), false},
		{"10,001 marks with an open parenthesis", textBlocks(runs(5000) + "("), false},
		{"closing parentheses are not marks", textBlocks(strings.Repeat("(", 5000) + strings.Repeat(")", 50_000) + runs(2500)), true},
		// Other blocks' words are not Markdown.
		{"headings are not counted", layoutJSON(`{"id":"h","type":"heading","text":` + jsonString(runs(6000)+items(6000)) + `}`), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if c.ok && err != nil {
				t.Fatalf("want it accepted, got %v", err)
			}
			if !c.ok && (err == nil || err.Error() != tooMuchFormatting) {
				t.Fatalf("want %q, got %v", tooMuchFormatting, err)
			}
		})
	}
	// The Markdown of an email over the budget is never read (this cost
	// 742 MiB to read before the budget; the refusals measure 14 to 33 MiB,
	// all of it decoding the JSON, and the bound is about 2x that), and the
	// Markdown of one within it is still checked, its problems named by block
	// in document order.
	for _, c := range []struct {
		name string
		doc  []byte
	}{
		{"one paragraph of 500,000 emphasis runs", []byte(textBlocks(runs(500_000)))},
		{"200 blocks of 2,000 list items", []byte(textBlocks(func() []string {
			var texts []string
			for n := 0; n < 200; n++ {
				texts = append(texts, items(2000))
			}
			return texts
		}()...))},
		{"50 blocks of one line of 10,000 emphasis runs", []byte(textBlocks(func() []string {
			var texts []string
			for n := 0; n < 50; n++ {
				texts = append(texts, runs(10_000))
			}
			return texts
		}()...))},
	} {
		var err error
		if allocated := allocatedBy(func() { _, err = Parse(c.doc) }); err == nil || err.Error() != tooMuchFormatting || allocated > 64<<20 {
			t.Errorf("%s: want %q without reading the Markdown, got %v after %d bytes", c.name, tooMuchFormatting, err, allocated)
		}
	}
	if _, err := Parse([]byte(textBlocks("fine", "[x](javascript:1)", "[](https://example.com)"))); err == nil || !strings.HasPrefix(err.Error(), `block "t1": a link must be`) {
		t.Fatalf("want the first Markdown problem, of t1, got %v", err)
	}
}

func TestParseRefusesInvalidUTF8(t *testing.T) {
	// Reading JSON turns each byte that is not UTF-8 into U+FFFD, three bytes,
	// so a 2 MiB document of them would be a 6 MiB paragraph (239 MiB to read
	// and write, measured). Parse refuses the file before it reads any of it.
	const notUTF8 = "This email file isn't valid UTF-8. Save it as UTF-8 and try again."
	pre, post, _ := strings.Cut(layoutJSON(`{"id":"t","type":"text","text":"@@@"}`), "@@@")
	for _, c := range []struct{ name, text string }{
		{"2 MiB of 0xff", strings.Repeat("\xff", MaxBytes-len(pre)-len(post))},
		{"2 MiB of 0xff after ](", "](" + strings.Repeat("\xff", MaxBytes-len(pre)-len(post)-2)},
		{"an overlong lead byte", "a\xc0\xafb"},
		{"a lone continuation byte", "a\x80"},
		{"a sequence cut short", "a\xe2\x80"},
		{"a surrogate", "\xed\xa0\x80"},
	} {
		doc := []byte(pre + c.text + post)
		var err error
		if allocated := allocatedBy(func() { _, err = Parse(doc) }); err == nil || err.Error() != notUTF8 || allocated > 1<<20 {
			t.Errorf("%s: want %q before reading anything, got %v after %d bytes", c.name, notUTF8, err, allocated)
		}
	}
	for _, doc := range []string{
		"{\"subject\":\"x\xff\",\"textBody\":\"y\",\"htmlBody\":\"\"}",
		"{\"subject\":\"x\",\"textBody\":\"y\",\"htmlBody\":\"<p>\xff</p>\"}",
	} {
		if _, err := Parse([]byte(doc)); err == nil || err.Error() != notUTF8 {
			t.Errorf("custom %q: want %q, got %v", doc, notUTF8, err)
		}
	}
	if _, err := Parse([]byte(layoutJSON(`{"id":"t","type":"text","text":"é 日 😀 é"}`))); err != nil {
		t.Fatalf("UTF-8 of every width is fine: %v", err)
	}
}

// The size refusal is a sentence for a person, and it is one value the callers
// that check the size themselves (before they spend anything on a document)
// can return, so their sentence is this one.
func TestParseSizeRefusalIsASentenceForAPerson(t *testing.T) {
	const sentence = "This email file is larger than 2 MiB. Use fewer or smaller pictures."
	over := []byte(`{"subject":"x","textBody":"` + strings.Repeat("a", MaxBytes) + `","htmlBody":""}`)
	for _, doc := range [][]byte{over, make([]byte, MaxBytes+1)} {
		if _, err := Parse(doc); err != ErrFileTooLarge || err.Error() != sentence {
			t.Errorf("a file of %d bytes: want %q, got %v", len(doc), sentence, err)
		}
	}
	// Exactly at the limit is not over it.
	atLimit := []byte(`{"subject":"x","textBody":"` + strings.Repeat("a", MaxBytes-len(`{"subject":"x","textBody":"","htmlBody":""}`)) + `","htmlBody":""}`)
	if len(atLimit) != MaxBytes {
		t.Fatalf("the fixture is %d bytes, not %d", len(atLimit), MaxBytes)
	}
	if _, err := Parse(atLimit); err != nil {
		t.Errorf("a file of exactly %d bytes: %v", MaxBytes, err)
	}
}

// A control character cannot be saved: PostgreSQL's jsonb cannot hold NUL, the
// engine would store its six-character escape instead and change the email
// without a word, and none of the others means anything in an email. Tab,
// newline and carriage return are the three that are text.
func TestParseRefusesControlCharacters(t *testing.T) {
	// The sentence names where the character is, so that a person can find one
	// that cannot be seen: the field, or the block by its id and the field of it.
	// Where the file does not say (a character between the pieces of it), it is
	// the sentence without a place.
	const generic = "This email contains a character that can't be saved. Remove it and try again."
	at := func(where string) string {
		return "This email contains a character that can't be saved, " + where + ". Remove it and try again."
	}
	// Where a string can sit in a file. Each is a template with the string "@@@"
	// where the character goes, and the place the sentence names.
	places := []struct{ name, doc, where string }{
		{"subject", `{"subject":"a@@@b","textBody":"y","htmlBody":""}`, "in the subject"},
		{"preheader", `{"subject":"x","preheader":"a@@@b","textBody":"y","htmlBody":""}`, "in the preheader"},
		{"text body", `{"subject":"x","textBody":"a@@@b","htmlBody":""}`, "in the plain-text body"},
		{"html body", `{"subject":"x","textBody":"y","htmlBody":"<p>a@@@b</p>"}`, "in the HTML body"},
		{"heading", layoutJSON(`{"id":"h","type":"heading","text":"a@@@b"}`), `in block "h" (text)`},
		{"text block", layoutJSON(`{"id":"t","type":"text","text":"a@@@b"}`), `in block "t" (text)`},
		{"button label", layoutJSON(`{"id":"b","type":"button","label":"a@@@b","href":"https://example.com"}`), `in block "b" (label)`},
		{"button link", layoutJSON(`{"id":"b","type":"button","label":"Go","href":"https://example.com/a@@@b"}`), `in block "b" (href)`},
		{"image alt", layoutJSON(`{"id":"i","type":"image","width":100,"alt":"a@@@b"}`), `in block "i" (alt)`},
		{"html block", layoutJSON(`{"id":"x","type":"html","html":"<p>a@@@b</p>"}`), `in block "x" (html)`},
		{"footer line", layoutJSON(`{"id":"f","type":"footer","lines":["a@@@b"]}`), `in block "f" (lines)`},
		{"footer link label", layoutJSON(`{"id":"f","type":"footer","links":[{"label":"a@@@b","href":"https://example.com"}]}`), `in block "f" (links)`},
		{"inside a column", layoutJSON(`{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"t","type":"text","text":"a@@@b"}]}]}`), `in block "t" (text)`},
		{"block id", layoutJSON(`{"id":"h@@@","type":"spacer","height":4}`), "in the id of a block"},
		{"layout name", `{"subject":"x","layout":"builtin:a@@@b","theme":` + neutralTheme() + `,"blocks":[]}`, "in the layout name"},
		{"theme font", `{"subject":"x","theme":` + strings.Replace(neutralTheme(), `"Georgia"`, `"Geo@@@gia"`, 1) + `,"blocks":[]}`, "in the theme"},
		{"a key", `{"subject":"x","te@@@xtBody":"y","htmlBody":""}`, "in a field name"},
	}
	// Every C0 character but the three that are text, as the \u escape in both
	// cases of hexadecimal digits, and the short escapes of backspace and form
	// feed (which are C0 characters too).
	var escapes []string
	for c := 0; c < 0x20; c++ {
		if c != '\t' && c != '\n' && c != '\r' {
			escapes = append(escapes, fmt.Sprintf(`\u%04x`, c), fmt.Sprintf(`\u%04X`, c))
		}
	}
	escapes = append(escapes, `\b`, `\f`)
	for _, place := range places {
		// One report for each place, with the first escape that got through.
		var missed []string
		var first error
		for _, escape := range escapes {
			if _, err := Parse([]byte(strings.Replace(place.doc, "@@@", escape, 1))); err == nil || err.Error() != at(place.where) {
				if missed = append(missed, escape); first == nil {
					first = err
				}
			}
		}
		if len(missed) > 0 {
			t.Errorf("%s: %d of %d escapes were not refused with %q (%s, ...); the first got %v", place.name, len(missed), len(escapes), at(place.where), missed[0], first)
		}
	}

	t.Run("a control character written into the file itself", func(t *testing.T) {
		// Not valid JSON either, but a person is told what to remove, not that
		// the JSON is wrong. Inside a string and between the pieces of the file.
		for _, c := range []struct{ doc, want string }{
			{"{\"subject\":\"a\x00b\",\"textBody\":\"y\",\"htmlBody\":\"\"}", at("in the subject")},
			{"{\"subject\":\"a\x01b\",\"textBody\":\"y\",\"htmlBody\":\"\"}", at("in the subject")},
			// Between the pieces of the file it is in no field.
			{"{\"subject\":\"x\",\"textBody\":\"y\",\"htmlBody\":\"\"}\x00", generic},
			{"\x1f{\"subject\":\"x\",\"textBody\":\"y\",\"htmlBody\":\"\"}", generic},
			{"{\"subject\":\"x\",\x0b\"textBody\":\"y\",\"htmlBody\":\"\"}", generic},
		} {
			if _, err := Parse([]byte(c.doc)); err == nil || err.Error() != c.want {
				t.Errorf("%q: want %q, got %v", c.doc, c.want, err)
			}
		}
	})

	t.Run("tab, newline and carriage return are text", func(t *testing.T) {
		for _, text := range []string{`a\tb`, `a\nb`, `a\r\nb`, `a\u0009b`, `a\u000ab`, `a\u000Db`, `a\u000A\u000d\u0009b`} {
			for name, doc := range map[string]string{
				"text block": layoutJSON(`{"id":"t","type":"text","text":"` + text + `"}`),
				"text body":  `{"subject":"x","textBody":"` + text + `","htmlBody":""}`,
				"html body":  `{"subject":"x","textBody":"y","htmlBody":"<p>` + text + `</p>"}`,
				"html block": layoutJSON(`{"id":"x","type":"html","html":"<p>` + text + `</p>"}`),
			} {
				if _, err := Parse([]byte(doc)); err != nil {
					t.Errorf("%s with %s: %v", name, text, err)
				}
			}
		}
	})

	t.Run("a backslash is not an escape of what follows it", func(t *testing.T) {
		// "\\u0000" is a backslash and the five characters "u0000": text. "\\\u0000"
		// is a backslash and then a NUL. A scan that looked for "\u0000" in the
		// bytes would get both wrong.
		for _, c := range []struct {
			name, text string
			ok         bool
		}{
			{"an escaped backslash and the text u0000", `a\\u0000b`, true},
			{"two escaped backslashes and the text u001f", `a\\\\u001fb`, true},
			{"an escaped backslash and the text \\b", `a\\bb`, true},
			{"an escaped backslash and then a NUL", `a\\\u0000b`, false},
			{"two escaped backslashes and then a NUL", `a\\\\\u0000b`, false},
			{"an escaped quote and then a NUL", `a\"\u0000b`, false},
			{"an escaped quote and the text u0000", `a\"u0000b`, true},
		} {
			_, err := Parse([]byte(layoutJSON(`{"id":"t","type":"text","text":"` + c.text + `"}`)))
			switch {
			case c.ok && err != nil:
				t.Errorf("%s: %v", c.name, err)
			case !c.ok && (err == nil || err.Error() != at(`in block "t" (text)`)):
				t.Errorf("%s: want %q, got %v", c.name, at(`in block "t" (text)`), err)
			}
		}
	})

	t.Run("only C0 is refused", func(t *testing.T) {
		// Space, DEL, the C1 controls and no-break space are not C0.
		for _, escape := range []string{`\u0020`, `\u007f`, `\u0085`, `\u009f`, `\u00a0`, `\u2028`} {
			if _, err := Parse([]byte(layoutJSON(`{"id":"t","type":"text","text":"a` + escape + `b"}`))); err != nil {
				t.Errorf("%s: %v", escape, err)
			}
		}
	})

	t.Run("a file of nothing else is refused before any of it is read", func(t *testing.T) {
		// 2 MiB of NUL escapes would be 350,000 characters to read, write out
		// and check; the refusal is a scan of the bytes.
		pre, post, _ := strings.Cut(layoutJSON(`{"id":"t","type":"text","text":"@@@"}`), "@@@")
		doc := []byte(pre + strings.Repeat(`\u0000`, (MaxBytes-len(pre)-len(post))/6) + post)
		var err error
		if allocated := allocatedBy(func() { _, err = Parse(doc) }); err == nil || err.Error() != at(`in block "t" (text)`) || allocated > 1<<20 {
			t.Errorf("want %q before reading anything, got %v after %d bytes", at(`in block "t" (text)`), err, allocated)
		}
	})
}

// The place a control character is named is read from the file by its shape,
// without reading the file: the block is the nearest object in a list called
// blocks, named by its id wherever in the object that is written, and the field
// is the member of it the character is in.
func TestParseControlCharacterNamesWhereItIs(t *testing.T) {
	at := func(where string) string {
		return "This email contains a character that can't be saved, " + where + ". Remove it and try again."
	}
	const generic = "This email contains a character that can't be saved. Remove it and try again."
	long := strings.Repeat("k", 300)
	for _, c := range []struct{ name, doc, want string }{
		{"a block is named by an id written after the field", layoutJSON(`{"type":"text","text":"a\u0000b","id":"late"}`), at(`in block "late" (text)`)},
		{"a block is named by an id written before it", layoutJSON(`{"id":"early","type":"text","text":"a\u0000b"}`), at(`in block "early" (text)`)},
		{"the nearest block of nested ones", layoutJSON(`{"id":"outer","type":"section","blocks":[{"id":"inner","type":"text","text":"a\u0000b"}]}`), at(`in block "inner" (text)`)},
		{"a block in a column of a section", layoutJSON(`{"id":"s","type":"section","blocks":[{"id":"c","type":"columns","columns":[{"width":100,"blocks":[{"id":"deep","type":"heading","text":"a\u0000b"}]}]}]}`), at(`in block "deep" (text)`)},
		{"the second block, not the first", layoutJSON(`{"id":"one","type":"text","text":"fine"},{"id":"two","type":"text","text":"a\u0000b"}`), at(`in block "two" (text)`)},
		{"the first of two characters", layoutJSON(`{"id":"one","type":"text","text":"a\u0001b"},{"id":"two","type":"text","text":"a\u0000b"}`), at(`in block "one" (text)`)},
		{"a block without an id", layoutJSON(`{"type":"text","text":"a\u0000b"}`), at(`in a block (text)`)},
		{"an id that is not text", layoutJSON(`{"id":7,"type":"text","text":"a\u0000b"}`), at(`in a block (text)`)},
		{"an id with an escape in it", layoutJSON(`{"id":"caf\u00e9","type":"text","text":"a\u0000b"}`), at(`in block "café" (text)`)},
		// The first character is the one named; the id read after it may hold another.
		{"an id with a character in it, after the first", layoutJSON(`{"type":"text","text":"a\u0000b","id":"h\u0001"}`), at(`in block "h\x01" (text)`)},
		{"an id with a character in it comes first", layoutJSON(`{"id":"h\u0001","type":"text","text":"a\u0000b"}`), at("in the id of a block")},
		{"an id written twice", layoutJSON(`{"id":"first","id":"second","type":"text","text":"a\u0000b"}`), at(`in block "first" (text)`)},
		{"a member of the block that holds a list", layoutJSON(`{"id":"f","type":"footer","lines":["fine","a\u0000b"]}`), at(`in block "f" (lines)`)},
		{"the id of the block itself", layoutJSON(`{"id":"x\u0000","type":"text","text":"fine"}`), at("in the id of a block")},
		// Not the id itself but something in the field the id should be.
		{"a list where the id should be", layoutJSON(`{"id":["a\u0000b"],"type":"text"}`), at("in a block (id)")},
		// The id read is the block's own, not that of a block it holds.
		{"the id of the block and not of the one inside it", layoutJSON(`{"type":"section","blocks":[{"id":"inner","type":"text","text":"x"}],"id":"outer","background":"a\u0000b"}`), at(`in block "outer" (background)`)},
		{"a block that holds blocks and has no id", layoutJSON(`{"type":"section","blocks":[{"id":"inner","type":"text","text":"x"}],"background":"a\u0000b"}`), at("in a block (background)")},
		{"a field with an odd name is not named", layoutJSON(`{"id":"t","type":"text","te xt":"a\u0000b"}`), at(`in block "t"`)},
		{"a field with a very long name is not named", layoutJSON(`{"id":"t","type":"text","` + long + `":"a\u0000b"}`), at(`in block "t"`)},
		{"a field the format does not have", `{"subject":"x","mystery":"a\u0000b","textBody":"y","htmlBody":""}`, at(`in the field "mystery"`)},
		{"a field with a very long name at the top", `{"subject":"x","` + long + `":"a\u0000b","textBody":"y","htmlBody":""}`, generic},
		{"the theme", `{"subject":"x","theme":{"page":"a\u0000b"},"blocks":[]}`, at("in the theme")},
		{"in the list of blocks but not in a block", `{"subject":"x","theme":` + neutralTheme() + `,"blocks":["a\u0000b"]}`, at("in the blocks")},
		{"a file that is a list", `["a\u0000b"]`, generic},
		{"a file that is a string", `"a\u0000b"`, generic},
		{"a string after the end of the file's object", `{"subject":"x","textBody":"y","htmlBody":""} "a\u0000b"`, generic},
		{"a character written into a key", `{"subject":"x","te\u0000xtBody":"y","htmlBody":""}`, at("in a field name")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.doc)); err == nil || err.Error() != c.want {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}

	t.Run("a document nested past anything the format has says no place", func(t *testing.T) {
		doc := strings.Repeat("[", 100000) + `"a\u0000b"` + strings.Repeat("]", 100000)
		var err error
		if allocated := allocatedBy(func() { _, err = Parse([]byte(doc)) }); err == nil || err.Error() != generic || allocated > 1<<20 {
			t.Fatalf("want %q cheaply, got %v after %d bytes", generic, err, allocated)
		}
	})

	t.Run("an id too long to read is not named", func(t *testing.T) {
		doc := layoutJSON(`{"type":"text","text":"a\u0000b","id":"` + strings.Repeat("x", 2*maxIDBytes) + `"}`)
		if _, err := Parse([]byte(doc)); err == nil || err.Error() != at(`in a block (text)`) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a hostile id is quoted and clipped", func(t *testing.T) {
		doc := layoutJSON(`{"id":"` + strings.Repeat("x", 5000) + `","type":"text","text":"a\u0000b"}`)
		_, err := Parse([]byte(doc))
		if err == nil || !strings.HasPrefix(err.Error(), `This email contains a character that can't be saved, in block "xxx`) || len(err.Error()) > 300 {
			t.Fatalf("got %d bytes: %.200v", len(fmt.Sprint(err)), err)
		}
	})
}

// A sentence about a control character holds no control character of its own,
// and is found for any file whatever, without a panic: the place is read out of
// a file that is not yet known to be JSON.
func FuzzControlRefusal(f *testing.F) {
	for _, seed := range []string{
		layoutJSON(`{"id":"t","type":"text","text":"a\u0000b"}`),
		layoutJSON(`{"type":"text","text":"a\u0000b","id":"late"}`),
		`{"subject":"a\bb"}`,
		"{\"subject\":\"a\x01b\"",
		`[[[{"blocks":[{"id":"x\u0001"}]}]]]`,
		`{"blocks":[{"id":"\\\u0000`,
		`{"a":{"blocks":[{"blocks":[{"id":"q","t\u0000":1}]}]}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		off, found := firstControlCharacter(data)
		if !found {
			return
		}
		if off < 0 || off >= len(data) {
			t.Fatalf("offset %d outside a file of %d bytes", off, len(data))
		}
		message := controlRefusal(data, off).Error()
		if !strings.HasPrefix(message, "This email contains a character that can't be saved") ||
			!strings.HasSuffix(message, "Remove it and try again.") || len(message) > 400 {
			t.Fatalf("not the sentence: %q", message)
		}
		for _, r := range message {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("the sentence holds a control character: %q", message)
			}
		}
	})
}

func TestParseCostDoesNotDependOnHTMLCharacters(t *testing.T) {
	// Reading a block writes its values out again as JSON once; written with
	// HTML escaping, each "<", ">" and "&" would take six bytes for one, and
	// a document of them would cost three times as much to read (160 MiB
	// against 50 for 2 MiB of text, measured).
	whole := func(letter string) []byte {
		room := MaxBytes - len(layoutJSON(`{"id":"t","type":"text","text":""}`))
		return []byte(layoutJSON(`{"id":"t","type":"text","text":"` + strings.Repeat(letter, room) + `"}`))
	}
	plain, html := whole("a"), whole("&")
	var err error
	costs := map[string]uint64{}
	for name, doc := range map[string][]byte{"plain": plain, "html": html} {
		costs[name] = allocatedBy(func() { _, err = Parse(doc) })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if costs["html"] > costs["plain"]*3/2 {
		t.Fatalf("2 MiB of & cost %d bytes to read, 2 MiB of letters %d", costs["html"], costs["plain"])
	}
}

func TestParseKeepsRolesAndZeroValuesAsWritten(t *testing.T) {
	d, err := Parse([]byte(layoutJSON(`{"id":"a","type":"heading","role":"content","text":"x"},{"id":"b","type":"heading","text":"y"},{"id":"c","type":"footer","separator":"","gap":0}`)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocks[0].Role != "content" || d.Blocks[1].Role != "" {
		t.Fatalf("roles must be stored as written: %q %q", d.Blocks[0].Role, d.Blocks[1].Role)
	}
	out, _ := d.JSON()
	if !bytes.Contains(out, []byte(`"role": "content"`)) || !bytes.Contains(out, []byte(`"separator": ""`)) || !bytes.Contains(out, []byte(`"gap": 0`)) {
		t.Fatalf("written values were dropped:\n%s", out)
	}
}

func TestParseNormalisesEmptyLists(t *testing.T) {
	d, err := Parse([]byte(layoutJSON(`{"id":"s","type":"section","blocks":[]},{"id":"f","type":"footer","links":[],"lines":[]},{"id":"c","type":"columns","columns":[{"width":100}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocks[0].Blocks != nil || d.Blocks[1].Links != nil || d.Blocks[1].Lines != nil {
		t.Fatalf("empty lists must be nil so the canonical file omits them: %+v", d.Blocks)
	}
	if col := d.Blocks[2].Columns[0]; col.Blocks == nil || len(col.Blocks) != 0 {
		t.Fatalf("a column always has a block list: %#v", col.Blocks)
	}
	out, _ := d.JSON()
	again, err := Parse(out)
	if err != nil || !reflect.DeepEqual(again, d) {
		t.Fatalf("round trip changed the document: %v\n%s", err, out)
	}
	empty, _ := Parse([]byte(`{"subject":"x","theme":` + neutralTheme() + `,"blocks":[]}`))
	if empty.Mode() != ModeLayout || empty.Blocks == nil {
		t.Fatalf("an empty layout is still a layout: %#v", empty.Blocks)
	}
	eb, _ := empty.JSON()
	if !bytes.Contains(eb, []byte(`"blocks": []`)) {
		t.Fatalf("an empty layout must keep its blocks list:\n%s", eb)
	}
}

func TestDocumentJSONKeyOrder(t *testing.T) {
	layout, err := Parse([]byte(`{"blocks":[],"theme":` + neutralTheme() + `,"layout":"builtin:letter","preheader":"p","subject":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := layout.JSON()
	order := []string{`"subject"`, `"preheader"`, `"layout"`, `"theme"`, `"blocks"`}
	last := -1
	for _, key := range order {
		at := bytes.Index(out, []byte(key))
		if at < 0 || at < last {
			t.Fatalf("%s is out of order in:\n%s", key, out)
		}
		last = at
	}
	custom, err := Parse([]byte(`{"htmlBody":"<p>h</p>","textBody":"t","preheader":"p","subject":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := custom.JSON()
	want := "{\n  \"subject\": \"s\",\n  \"preheader\": \"p\",\n  \"textBody\": \"t\",\n  \"htmlBody\": \"<p>h</p>\"\n}\n"
	if string(cb) != want {
		t.Fatalf("custom canonical bytes:\n%s\nwant:\n%s", cb, want)
	}
	bare, _ := Parse([]byte(`{"subject":"s","textBody":"t"}`))
	bb, _ := bare.JSON()
	if bytes.Contains(bb, []byte("preheader")) || !bytes.Contains(bb, []byte(`"htmlBody": ""`)) {
		t.Fatalf("an empty preheader is omitted and an empty htmlBody is kept:\n%s", bb)
	}
}

func TestLinkTags(t *testing.T) {
	want := []string{"{{unsubscribeUrl}}", "{{preferencesUrl}}", "{{viewUrl}}", "{{confirmUrl}}"}
	if got := LinkTags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("LinkTags() = %v", got)
	}
	LinkTags()[0] = "changed"
	if LinkTags()[0] != TagUnsubscribeURL {
		t.Fatal("LinkTags must hand out a fresh slice")
	}
}

// jsonNames lists the JSON keys of a struct type, in field order.
func jsonNames(t *testing.T, v any) []string {
	t.Helper()
	var names []string
	typ := reflect.TypeOf(v)
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no JSON name", typ.Name(), typ.Field(i).Name)
		}
		names = append(names, name)
	}
	return names
}

func sortedKeys(set map[string]bool) []string {
	var keys []string
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The key tables are the strictness; the structs are what the keys decode into.
// If the two drift, a key is either refused though the struct holds it or
// accepted and silently dropped.
func TestKeySetsMatchTheStructs(t *testing.T) {
	union := map[string]bool{}
	for typ, keys := range blockKeys {
		for key := range keys {
			union[key] = true
		}
		for _, common := range []string{"id", "type", "role"} {
			if !keys[common] {
				t.Errorf("%s does not accept %q", typ, common)
			}
		}
		if (typ != "spacer") != keys["padding"] {
			t.Errorf("%s: padding must be accepted by every type except spacer", typ)
		}
	}
	want := jsonNames(t, Block{})
	sort.Strings(want)
	if got := sortedKeys(union); !reflect.DeepEqual(got, want) {
		t.Errorf("block keys differ from the Block struct:\n tables: %v\n struct: %v", got, want)
	}
	for _, c := range []struct {
		name string
		keys map[string]bool
		v    any
	}{
		{"theme", themeKeys, Theme{}},
		{"column", columnKeys, Column{}},
	} {
		want := jsonNames(t, c.v)
		sort.Strings(want)
		if got := sortedKeys(c.keys); !reflect.DeepEqual(got, want) {
			t.Errorf("%s keys differ from the struct:\n tables: %v\n struct: %v", c.name, got, want)
		}
	}
	links := map[string]bool{}
	for _, set := range []map[string]bool{socialLinkKeys, footerLinkKeys} {
		for key := range set {
			links[key] = true
		}
	}
	want = jsonNames(t, Link{})
	sort.Strings(want)
	if got := sortedKeys(links); !reflect.DeepEqual(got, want) {
		t.Errorf("link keys differ from the Link struct:\n tables: %v\n struct: %v", got, want)
	}
}

func TestParentPackageDoesNotImportEmaildoc(t *testing.T) {
	// The parent's renderer sources travel inside rebuilt source packages
	// (source_package.go), so it must stay independent of this package.
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile("../" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if bytes.Contains(data, []byte("component/compose/emaildoc")) {
			t.Errorf("%s imports emaildoc", entry.Name())
		}
	}
	if checked == 0 {
		t.Fatal("found no parent package sources to check")
	}
}

func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// The same address in the three places a document can carry one that is not a
// plain field: a button, a Markdown link and an anchor in an html block.
var addressContexts = []struct {
	name string
	doc  func(href string) string
}{
	{"button", func(href string) string {
		return layoutJSON(`{"id":"b","type":"button","label":"Go","href":` + jsonString(href) + `}`)
	}},
	{"markdown", func(href string) string {
		return layoutJSON(`{"id":"t","type":"text","text":` + jsonString("[x]("+href+")") + `}`)
	}},
	{"html", func(href string) string {
		return layoutJSON(`{"id":"x","type":"html","html":` + jsonString(`<a href="`+html.EscapeString(href)+`">x</a>`) + `}`)
	}},
}

func TestParseKeepsMergeTagsOutOfTheHost(t *testing.T) {
	// A merge tag is replaced with a recipient-supplied value. In the path,
	// query or fragment that only changes which page of the sender's choice a
	// link opens; in the host, user name or port it would choose the site.
	const host = "a web address cannot take a merge tag in its host, user name or port"
	const mail = "email and phone links cannot hold a merge tag"
	refused := []struct{ href, want string }{
		{"https://{{fields.host}}/x", host},
		{"https://{{fields.url}}", host},
		{"http://{{email}}", host},
		{"https://example.com{{fields.p}}", host},
		{"https://exa{{fields.part}}mple.com/", host},
		{"https://example.com@{{fields.host}}/", host},
		{"https://{{fields.user}}@example.com/", host},
		{"https://user:{{fields.pw}}@example.com/", host},
		{"https://example.com:{{fields.port}}/", host},
		{"https://example.com:{{fields.port}}", host},
		{"https://{{fields.host}}?x=1", host},
		{"https://{{fields.host}}#x", host},
		{"HTTPS://{{fields.host}}/", host},
		// A browser skips extra slashes after the two, so the host is what follows them.
		{"https:///{{fields.host}}/x", host},
		{"https:////{{fields.host}}/x", host},
		{"mailto:{{email}}", mail},
		{"mailto:a@example.com?subject={{fields.subject}}", mail},
		{"mailto:{{unsubscribeUrl}}", mail},
		{"MAILTO:{{email}}", mail},
		{"tel:{{fields.phone}}", mail},
		{"tel:+1555{{fields.extension}}", mail},
	}
	for _, c := range refused {
		for _, where := range addressContexts {
			t.Run(where.name+" "+c.href, func(t *testing.T) {
				if _, err := Parse([]byte(where.doc(c.href))); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("want %q, got %v", c.want, err)
				}
			})
		}
	}
	for _, link := range []string{
		`{"id":"s","type":"social","links":[{"network":"n","href":"https://{{fields.host}}/"}]}`,
		`{"id":"f","type":"footer","links":[{"label":"l","href":"https://{{fields.host}}/"}]}`,
		`{"id":"i","type":"image","width":10,"href":"https://{{fields.host}}/"}`,
	} {
		if _, err := Parse([]byte(layoutJSON(link))); err == nil || !strings.Contains(err.Error(), host) {
			t.Errorf("%s: got %v", link, err)
		}
	}
	for _, href := range []string{
		"https://example.com/?e={{email}}",
		"https://example.com?e={{email}}",
		"https://example.com/{{fields.slug}}",
		"https://example.com/a/{{fields.slug}}/b?x={{fields.y}}#{{fields.z}}",
		"https://example.com#{{fields.frag}}",
		"http://example.com:8080/?e={{email}}",
		"https://user@example.com/{{fields.p}}",
		"https://[::1]:8080/{{fields.p}}",
		"mailto:a@example.com?subject=Hello",
		"tel:+15551234567",
		TagViewURL,
	} {
		for _, where := range addressContexts {
			if _, err := Parse([]byte(where.doc(href))); err != nil {
				t.Errorf("%s %q: %v", where.name, href, err)
			}
		}
	}
}

func TestParseRefusesCharactersAnAddressMustEncode(t *testing.T) {
	// A valid address percent-encodes these, so one in a link is a mistake or an
	// attempt to leave the attribute it is written in.
	const sentence = "a link cannot contain quotes, angle brackets, backticks or backslashes"
	for name, href := range map[string]string{
		"double quote":        `https://example.com/a"b`,
		"less-than":           `https://example.com/a<b`,
		"greater-than":        `https://example.com/a>b`,
		"backtick":            "https://example.com/a`b",
		"backslash":           `https://example.com/a\b`,
		"quote in an email":   `mailto:a"b@example.com`,
		"quote in a phone":    `tel:+1"5`,
		"angle bracket form":  `<https://example.com>`,
		"quote after a query": `https://example.com/?q="x"`,
	} {
		for _, where := range addressContexts {
			t.Run(where.name+" "+name, func(t *testing.T) {
				if _, err := Parse([]byte(where.doc(href))); err == nil || !strings.Contains(err.Error(), sentence) {
					t.Fatalf("want %q, got %v", sentence, err)
				}
			})
		}
	}
	// An apostrophe is a valid address character.
	for _, where := range addressContexts {
		if _, err := Parse([]byte(where.doc("https://example.com/it's"))); err != nil {
			t.Errorf("%s: %v", where.name, err)
		}
	}
}

// sampleValue is, for each key a block can carry, a value that is valid on
// every type that accepts the key.
var sampleValue = map[string]string{
	"role": `"brand"`, "padding": `[1,2,3,4]`, "text": `"x"`, "size": `16`, "lineHeight": `20`, "listLineHeight": `20`,
	"gap": `4`, "color": `"text"`, "textColor": `"white"`, "linkColor": `"primary"`, "background": `"tint"`,
	"align": `"center"`, "italic": `true`, "weight": `700`, "font": `"body"`, "letterSpacing": `"0.05em"`,
	"src": `"artifact:abc"`, "alt": `"a"`, "width": `10`, "height": `10`, "href": `"https://example.com"`, "bleed": `true`,
	"label": `"Go"`, "paddingY": `8`, "inset": `[1,2,3,4]`, "maxWidth": `500`, "cellPadding": `4`, "stackBelow": `450`,
	"blocks": `[]`, "columns": `[{"width":100,"blocks":[]}]`, "title": `"t"`, "lines": `["x"]`, "separator": `"|"`,
	"after": `8`, "html": `"<p>x</p>"`,
}

// sampleFor is a valid value for the key on the type.
func sampleFor(typ, key string) string {
	if key == "links" {
		switch typ {
		case "social":
			return `[{"network":"n","href":"https://example.com","icon":"artifact:i","width":10}]`
		case "footer":
			return `[{"label":"l","href":"https://example.com"}]`
		}
		return `[]`
	}
	return sampleValue[key]
}

// blockOf writes a block of the type with the keys it cannot do without and
// then the extra keys, each with a sample value unless the pair gives one.
func blockOf(typ string, extra ...[2]string) string {
	members := map[string]string{"id": `"b"`, "type": jsonString(typ)}
	for _, key := range map[string][]string{"image": {"width"}, "button": {"label"}, "columns": {"columns"}, "social": {"links"}}[typ] {
		members[key] = sampleFor(typ, key)
	}
	for _, pair := range extra {
		members[pair[0]] = pair[1]
	}
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = jsonString(key) + ":" + members[key]
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// specKeys is the key table of the format, written out here a second time so
// the tests check the parser against the specification and not against its own
// table. Every type also takes id, type and role, and every type but spacer
// takes padding.
var specKeys = map[string]string{
	"image":     "src alt width height align href bleed",
	"heading":   "text size lineHeight color align italic weight font letterSpacing",
	"text":      "text size lineHeight listLineHeight gap color linkColor align italic weight font letterSpacing",
	"highlight": "text size lineHeight gap color background align italic weight font letterSpacing",
	"button":    "label href color textColor width align size paddingY font",
	"divider":   "color height",
	"spacer":    "height",
	"section":   "background inset maxWidth cellPadding blocks",
	"columns":   "background inset maxWidth gap stackBelow columns",
	"social":    "title links maxWidth gap",
	"footer":    "links lines background inset maxWidth cellPadding size lineHeight letterSpacing color linkColor separator gap after font",
	"html":      "html",
}

// specBlockKeys is the set of keys a block of the type accepts.
func specBlockKeys(typ string) map[string]bool {
	keys := wordSet("id type role " + specKeys[typ])
	if typ != "spacer" {
		keys["padding"] = true
	}
	return keys
}

func specAllBlockKeys() map[string]bool {
	every := map[string]bool{}
	for typ := range specKeys {
		for key := range specBlockKeys(typ) {
			every[key] = true
		}
	}
	return every
}

func TestKeySetsAreTheSpecifiedOnes(t *testing.T) {
	if got, want := len(blockKeys), len(specKeys); got != want {
		t.Errorf("%d block types, the format has %d", got, want)
	}
	for typ := range specKeys {
		got, ok := blockKeys[typ]
		if !ok {
			t.Errorf("block type %q is missing", typ)
			continue
		}
		if want := specBlockKeys(typ); !reflect.DeepEqual(sortedKeys(got), sortedKeys(want)) {
			t.Errorf("%s takes %v, the format says %v", typ, sortedKeys(got), sortedKeys(want))
		}
	}
	for _, c := range []struct {
		name string
		got  map[string]bool
		want string
	}{
		{"a layout document", layoutKeys, "subject preheader layout theme blocks"},
		{"a custom document", customKeys, "subject preheader textBody htmlBody"},
		{"the theme", themeKeys, "page canvas primary secondary accent tint text muted headingFont bodyFont webFonts"},
		{"a column", columnKeys, "width cellPadding blocks"},
		{"a social link", socialLinkKeys, "network href icon width"},
		{"a footer link", footerLinkKeys, "label href"},
	} {
		if want := wordSet(c.want); !reflect.DeepEqual(sortedKeys(c.got), sortedKeys(want)) {
			t.Errorf("%s takes %v, the format says %v", c.name, sortedKeys(c.got), sortedKeys(want))
		}
	}
}

func TestEveryTypeAcceptsEveryKeyItOwns(t *testing.T) {
	for typ := range specKeys {
		for _, key := range sortedKeys(specBlockKeys(typ)) {
			if key == "id" || key == "type" {
				continue
			}
			value := sampleFor(typ, key)
			if value == "" {
				t.Fatalf("no sample value for the key %q: add one to sampleValue", key)
			}
			t.Run(typ+"/"+key, func(t *testing.T) {
				doc := layoutJSON(blockOf(typ, [2]string{key, value}))
				if _, err := Parse([]byte(doc)); err != nil {
					t.Fatalf("%s: %v", doc, err)
				}
			})
		}
	}
}

func TestEveryTypeRefusesEveryKeyItDoesNotOwn(t *testing.T) {
	// The keys of the other types, and three that belong to none: a made-up
	// one and the two spellings encoding/json would have matched.
	foreign := append(sortedKeys(specAllBlockKeys()), "bogus", "Text", "ID")
	for typ := range specKeys {
		keys := specBlockKeys(typ)
		for _, key := range foreign {
			if keys[key] {
				continue
			}
			t.Run(typ+"/"+key, func(t *testing.T) {
				value := sampleValue[key]
				if value == "" {
					value = `1`
				}
				doc := layoutJSON(blockOf(typ, [2]string{key, value}))
				want := fmt.Sprintf("block \"b\": unknown field %q", key)
				if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s\nwant %q, got %v", doc, want, err)
				}
			})
		}
	}
}

func TestLinksAndColumnsRefuseTheKeysOfOtherObjects(t *testing.T) {
	every := specAllBlockKeys()
	// A social link takes network, href, icon and width; a footer link takes
	// label and href; a column takes width, cellPadding and blocks.
	for _, c := range []struct {
		name  string
		own   map[string]bool
		doc   func(member string) string
		where string
	}{
		{"social link", wordSet("network href icon width"), func(m string) string {
			return layoutJSON(`{"id":"b","type":"social","links":[{"network":"n",` + m + `}]}`)
		}, `block "b": link 1: unknown field `},
		{"footer link", wordSet("label href"), func(m string) string {
			return layoutJSON(`{"id":"b","type":"footer","links":[{"label":"l",` + m + `}]}`)
		}, `block "b": link 1: unknown field `},
		{"column", wordSet("width cellPadding blocks"), func(m string) string {
			return layoutJSON(`{"id":"b","type":"columns","columns":[{"width":100,` + m + `}]}`)
		}, `block "b": column 1: unknown field `},
	} {
		foreign := append(sortedKeys(every), "network", "bogus")
		for _, key := range foreign {
			if c.own[key] {
				continue
			}
			t.Run(c.name+"/"+key, func(t *testing.T) {
				doc := c.doc(jsonString(key) + `:1`)
				want := c.where + strconv.Quote(key)
				if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s\nwant %q, got %v", doc, want, err)
				}
			})
		}
	}
}

func TestNoOtherFieldCarriesAMergeTagIntoCSS(t *testing.T) {
	// Besides the style attribute of an html block, what a renderer writes into
	// CSS is a theme colour or font, the name of a theme colour, an enumerated
	// word, a number or a length in em. None of those can hold a merge tag, so
	// a value a recipient supplied never becomes CSS. The text fields (text,
	// label, alt, title, lines, network, separator) are written as HTML text or
	// attributes and never as CSS.
	theme := neutralTheme()
	withTheme := func(old, replacement string) string {
		return `{"subject":"x","theme":` + strings.Replace(theme, old, replacement, 1) + `,"blocks":[]}`
	}
	expectRefused(t, []refusal{
		{"theme colour as a tag", withTheme(`"#eef0f3"`, `"{{fields.x}}"`), "theme: page must be a #rrggbb colour"},
		{"theme colour with a tag after it", withTheme(`"#8a8f98"`, `"#8a8f98{{fields.x}}"`), "theme: muted must be a #rrggbb colour"},
		{"heading font as a tag", withTheme(`"Georgia"`, `"{{fields.x}}"`), "theme: headingFont must be a font family name"},
		{"heading font with a tag after it", withTheme(`"Georgia"`, `"Georgia{{fields.x}}"`), "theme: headingFont must be a font family name"},
		{"body font with a tag in a list", withTheme(`"Arial"`, `"Arial, {{fields.x}}"`), "theme: bodyFont must be a font family name"},
		{"block id as a tag", layoutJSON(`{"id":"{{fields.x}}","type":"spacer"}`), "must be lowercase letters, digits and hyphens"},
	})
	for _, c := range []struct{ typ, key, value, want string }{
		{"heading", "color", `"{{fields.x}}"`, "color must name a theme colour"},
		{"heading", "color", `"primary{{fields.x}}"`, "color must name a theme colour"},
		{"button", "textColor", `"{{fields.x}}"`, "textColor must name a theme colour"},
		{"text", "linkColor", `"{{fields.x}}"`, "linkColor must name a theme colour"},
		{"highlight", "background", `"{{fields.x}}"`, "background must name a theme colour"},
		{"heading", "letterSpacing", `"{{fields.x}}em"`, "letterSpacing must be a length in em"},
		{"heading", "letterSpacing", `"1{{fields.x}}em"`, "letterSpacing must be a length in em"},
		{"heading", "align", `"{{fields.x}}"`, "align must be left, center or right"},
		{"heading", "font", `"{{fields.x}}"`, "font must be heading, body or base"},
		{"heading", "weight", `"{{fields.x}}"`, "weight must be a whole number"},
		{"heading", "role", `"{{fields.x}}"`, `role must be "brand" or "content"`},
		{"heading", "size", `"{{fields.x}}"`, "size must be a number"},
		{"heading", "lineHeight", `"{{fields.x}}"`, "lineHeight must be a number"},
		{"text", "listLineHeight", `"{{fields.x}}"`, "listLineHeight must be a number"},
		{"text", "gap", `"{{fields.x}}"`, "gap must be a number"},
		{"spacer", "height", `"{{fields.x}}"`, "height must be a number"},
		{"button", "paddingY", `"{{fields.x}}"`, "paddingY must be a number"},
		{"section", "maxWidth", `"{{fields.x}}"`, "maxWidth must be a number"},
		{"section", "cellPadding", `"{{fields.x}}"`, "cellPadding must be a number"},
		{"columns", "stackBelow", `"{{fields.x}}"`, "stackBelow must be a number"},
		{"footer", "after", `"{{fields.x}}"`, "after must be a number"},
		{"heading", "padding", `["{{fields.x}}",0,0,0]`, "padding must be four numbers"},
		{"section", "inset", `["{{fields.x}}",0,0,0]`, "inset must be four numbers"},
		{"image", "width", `"{{fields.x}}"`, "width must be a whole number"},
		{"button", "width", `"{{fields.x}}"`, "width must be a whole number"},
	} {
		t.Run(c.typ+"/"+c.key+"/"+c.value, func(t *testing.T) {
			doc := layoutJSON(blockOf(c.typ, [2]string{c.key, c.value}))
			if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s\nwant %q, got %v", doc, c.want, err)
			}
		})
	}
	// A column's width is a number as well.
	doc := layoutJSON(`{"id":"b","type":"columns","columns":[{"width":"{{fields.x}}","blocks":[]}]}`)
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "column 1: width must be a number") {
		t.Errorf("column width: %v", err)
	}
}

func TestParseRefusesUnsafeHTMLBlocks(t *testing.T) {
	// The renderer splices an html block inside a table cell. These are the
	// fragments that would otherwise run, hide the rest of the email or
	// close the cell; each is refused with a sentence that names the problem.
	htmlBlock := func(markup string) string {
		return layoutJSON(`{"id":"x","type":"html","html":` + jsonString(markup) + `}`)
	}
	const cutOff = `block "x": html blocks cannot end inside a tag; close every tag with >`
	const unbalanced = `block "x": html blocks must close the tags they open, in order`
	const comments = `block "x": html blocks cannot contain comments`
	expectRefused(t, []refusal{
		{"picture cut off", htmlBlock(`<img src="https://evil.example/p.gif"`), cutOff},
		{"link cut off", htmlBlock(`<a href="javascript:alert(1)"`), cutOff},
		{"script cut off", htmlBlock(`<script src=https://evil.example/x.js`), cutOff},
		{"handler cut off", htmlBlock(`<p onclick="alert(1)"`), cutOff},
		{"text then a cut-off tag", htmlBlock(`<p>hello</p><b`), cutOff},
		{"unterminated comment", htmlBlock(`<!-- unterminated`), comments},
		{"comment", htmlBlock(`<!-- a &lt; b -->`), comments},
		{"conditional comment", htmlBlock(`<!--[if mso]><v:rect/><![endif]-->`), comments},
		{"stray cell end", htmlBlock(`</td>`), unbalanced},
		{"closing the renderer's cell", htmlBlock(`</td></tr></table><p>after</p>`), unbalanced},
		{"left open", htmlBlock(`<p>one`), unbalanced},
		{"closed out of order", htmlBlock(`<p><b>x</p></b>`), unbalanced},
		{"merge tag in a style", htmlBlock(`<p style="color:{{fields.c}}">x</p>`), `block "x": html block styles cannot contain merge tags`},
		{"bare cell", htmlBlock(`<td>x</td>`), `block "x": html blocks can use <td> only inside a <table> the block opens`},
		{"bare row", htmlBlock(`<tr><td>x</td></tr>`), `block "x": html blocks can use <tr> only inside a <table> the block opens`},
		{"entity-encoded tab in a scheme", htmlBlock(`<a href="java&#x09;script:x">x</a>`), `block "x": a link cannot contain spaces or control characters`},
		{"space before a scheme", htmlBlock(`<a href=" javascript:x">x</a>`), `block "x": a link cannot contain spaces or control characters`},
		{"entity-encoded letter in a scheme", htmlBlock(`<a href="jav&#x61;script:x">x</a>`), `block "x": a link must be`},
		{"nested too deeply", htmlBlock(strings.Repeat("<div>", 600) + strings.Repeat("</div>", 600)), `block "x": html blocks are nested too deeply`},
	})
	for _, ok := range []string{
		`<table><tr><td>x</td></tr></table>`,
		`<p>one</p><p>two</p>`,
		`<a href="https://example.com">x</a>`,
	} {
		if _, err := Parse([]byte(htmlBlock(ok))); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

// refusedControlIn returns the first string of a decoded JSON value (a key or a
// value) that holds a C0 control character which is not tab, newline or
// carriage return, or "".
func refusedControlIn(v any) string {
	bad := func(s string) bool {
		return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' && r != '\n' && r != '\r' }) >= 0
	}
	switch v := v.(type) {
	case string:
		if bad(v) {
			return v
		}
	case []any:
		for _, item := range v {
			if found := refusedControlIn(item); found != "" {
				return found
			}
		}
	case map[string]any:
		for key, item := range v {
			if bad(key) {
				return key
			}
			if found := refusedControlIn(item); found != "" {
				return found
			}
		}
	}
	return ""
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"subject":"x","textBody":"y","htmlBody":""}`,
		`{"subject":"x","textBody":"y"}`,
		layoutJSON(``),
		layoutJSON(`{"id":"h","type":"heading","text":"Hi {{displayName}}","size":24,"padding":[0,1,2,3]}`),
		layoutJSON(`{"id":"t","type":"text","text":"a **b** *c* [d](https://example.com)\n- e\n1. f"}`),
		layoutJSON(`{"id":"s","type":"section","blocks":[{"id":"c","type":"columns","columns":[{"width":50,"blocks":[]},{"width":50}]}]}`),
		layoutJSON(`{"id":"f","type":"footer","links":[{"label":"u","href":"{{unsubscribeUrl}}"}],"lines":["x"],"separator":"|","gap":0}`),
		layoutJSON(`{"id":"x","type":"html","html":"<table><tr><td>A &amp; B</td></tr></table>"}`),
		layoutJSON(`{"id":"i","type":"image","width":10,"src":"artifact:abc"}`),
		layoutJSON(`{"id":"t","type":"text","text":"a\tb\nc\u0009\\u0000\u000d"}`),
		layoutJSON(`{"id":"t","type":"text","text":"a\u0000b\bc\f\u001F"}`),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := Parse(data)
		if err != nil {
			return
		}
		out, err := d.JSON()
		if err != nil {
			t.Fatalf("JSON of an accepted document: %v", err)
		}
		// Whatever way the file wrote a control character, an accepted document
		// has none of the refused ones in any key or value.
		var tree any
		if err := json.Unmarshal(out, &tree); err != nil {
			t.Fatalf("canonical bytes are not JSON: %v", err)
		}
		if bad := refusedControlIn(tree); bad != "" {
			t.Fatalf("an accepted document holds a control character: %q\n%s", bad, out)
		}
		again, err := Parse(out)
		if err != nil {
			t.Fatalf("canonical bytes were refused: %v\n%s", err, out)
		}
		if !reflect.DeepEqual(again, d) {
			t.Fatalf("round trip changed the document:\n%s", out)
		}
		second, err := again.JSON()
		if err != nil || !bytes.Equal(second, out) {
			t.Fatalf("canonical bytes are not stable: %v", err)
		}
	})
}
