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
	"strings"
	"testing"
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
		{"too large", big, "email document exceeds 2 MiB"},
		{"trailing", `{"subject":"x","textBody":"y","htmlBody":""} {}`, "email document contains trailing content"},
		{"too many blocks", layoutJSON(strings.Join(blocks, ",")), "an email can have at most 300 blocks"},
		{"html image", layoutJSON(`{"id":"x","type":"html","html":"<img src=\"data:image/png;base64,AA\">"}`), `block "x": html blocks cannot contain pictures; use an image block`},
		{"html script", layoutJSON(`{"id":"x","type":"html","html":"<script>1</script>"}`), `block "x": html blocks cannot use <script>`},
		{"html handler", layoutJSON(`{"id":"x","type":"html","html":"<p onclick=\"x\">a</p>"}`), `block "x": html blocks cannot use the onclick attribute`},
		{"html css url", layoutJSON(`{"id":"x","type":"html","html":"<p style=\"background:url(x)\">a</p>"}`), `block "x": html block styles cannot load resources`},
		{"html conditional", layoutJSON(`{"id":"x","type":"html","html":"<!--[if mso]><v:rect/><![endif]-->"}`), `block "x": html blocks cannot contain conditional comments`},
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

func spacer(id string) string { return fmt.Sprintf(`{"id":%q,"type":"spacer","height":4}`, id) }

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
		doc := layoutJSON(`{"id":"t","type":"text","text":` + mustQuote(text) + `}`)
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("text %q: %v", text, err)
		}
	}
}

func mustQuote(s string) string { return fmt.Sprintf("%q", s) }

func TestParseKeepsMarkupInTextAsData(t *testing.T) {
	// Text is data. Parse keeps it exactly as written and the renderer escapes
	// it; the one place markup is read as markup is an html block, and that is
	// validated (TestValidateMarkup).
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
	d, err := Parse([]byte(`{"subject":"x","textBody":"y","htmlBody":` + mustQuote(html) + `}`))
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

func TestValidateMarkup(t *testing.T) {
	accepted := []string{
		``,
		`plain text & more`,
		`<table width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; background:#fff"><tr><td align="center" style="padding:0 24px; font-family:Arial, sans-serif">A &amp; B<br/>C</td></tr></table>`,
		`<p><strong>bold</strong> <em>em</em> <a href="https://example.com/a?b=1" target="_blank" rel="noopener">link</a></p>`,
		`<a href="{{unsubscribeUrl}}">x</a><a href="{{preferencesUrl}}">x</a><a href="{{viewUrl}}">x</a><a href="{{confirmUrl}}">x</a>`,
		`<a href="mailto:a@example.com">x</a><a href="tel:+15551234567">x</a><a href="">x</a>`,
		`<UL><LI>upper case tags</LI></UL>`,
		// The tokenizer keeps the first of two equal attributes, as browsers do,
		// so only the first href is ever used.
		`<a href="https://ok" href="javascript:alert(1)">x</a>`,
		`<ul><li>one</li></ul><ol><li>two</li></ol><h1>h</h1><hr><blockquote>q</blockquote><pre>p</pre><code>c</code><center>c</center><font face="Arial" color="#333" size="2">f</font>`,
		`<!-- a plain comment -->`,
		`<!-- [note] -->`,
		`<div class="a" id="b" title="t" lang="en" dir="ltr" role="presentation" aria-label="l" aria-hidden="true">x</div>`,
		`<td bgcolor="#fff" valign="top" colspan="2" rowspan="2" scope="col" width="10" height="10">x</td>`,
		`<span style="font-size:14px; line-height:20px; color:#333333; letter-spacing:0.5px">x</span>`,
	}
	for _, src := range accepted {
		if err := validateMarkup(src); err != nil {
			t.Errorf("refused %q: %v", src, err)
		}
	}
	refused := []struct{ src, want string }{
		{`<img src="x">`, "html blocks cannot contain pictures; use an image block"},
		{`<IMG SRC="x">`, "html blocks cannot contain pictures; use an image block"},
		{`<img>`, "html blocks cannot contain pictures; use an image block"},
		{`<script>1</script>`, "html blocks cannot use <script>"},
		{`<SCRIPT>1</SCRIPT>`, "html blocks cannot use <script>"},
		{`<style>p{}</style>`, "html blocks cannot use <style>"},
		{`<svg><image href="https://x"/></svg>`, "html blocks cannot use <svg>"},
		{`<iframe src="x"></iframe>`, "html blocks cannot use <iframe>"},
		{`<form><input name="a"></form>`, "html blocks cannot use <form>"},
		{`<link rel="stylesheet" href="x">`, "html blocks cannot use <link>"},
		{`<meta http-equiv="refresh" content="0">`, "html blocks cannot use <meta>"},
		{`<base href="https://x">`, "html blocks cannot use <base>"},
		{`<object data="x"></object>`, "html blocks cannot use <object>"},
		{`<video src="x"></video>`, "html blocks cannot use <video>"},
		{`<picture><source srcset="x"></picture>`, "html blocks cannot use <picture>"},
		{`<html><body>x</body></html>`, "html blocks cannot use <html>"},
		{`<head></head>`, "html blocks cannot use <head>"},
		{`<title>x</title>`, "html blocks cannot use <title>"},
		{`<v:rect/>`, "html blocks cannot use <v:rect>"},
		{`<p onclick="x">a</p>`, "html blocks cannot use the onclick attribute"},
		{`<p onmouseover="x">a</p>`, "html blocks cannot use the onmouseover attribute"},
		{`<p data-x="1">a</p>`, "html blocks cannot use the data-x attribute"},
		{`<a href="https://x" ping="https://y">x</a>`, "html blocks cannot use the ping attribute"},
		{`<table background="https://x"><tr><td>x</td></tr></table>`, "html blocks cannot use the background attribute"},
		{`<div src="x">x</div>`, "html blocks cannot use the src attribute"},
		{`<div srcset="x">x</div>`, "html blocks cannot use the srcset attribute"},
		{`<div href="https://x">x</div>`, "html blocks cannot use the href attribute"},
		{`<p style="background:url(https://x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:URL(https://x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:u\72l(https://x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:u/**/rl(https://x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:u&#x72;l(https://x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:image('https://x')">x</p>`, "html block styles cannot load resources"},
		{`<p style="background:image-set('x' 1x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="width:expression(alert(1))">x</p>`, "html block styles cannot load resources"},
		{`<p style="behavior:url(x)">x</p>`, "html block styles cannot load resources"},
		{`<p style="-moz-binding:x">x</p>`, "html block styles cannot load resources"},
		{`<p style="@import 'x'">x</p>`, "html block styles cannot load resources"},
		{`<a href="javascript:alert(1)">x</a>`, "a link must be"},
		{`<a href="JavaScript:alert(1)">x</a>`, "a link must be"},
		{`<a href="jav&#x61;script:alert(1)">x</a>`, "a link must be"},
		{`<a href=" https://example.com">x</a>`, "a link cannot contain spaces or control characters"},
		{`<a href="https://example.com/a b">x</a>`, "a link cannot contain spaces or control characters"},
		{`<a href="{{fields.url}}">x</a>`, "a link must be"},
		{`<a href="#top">x</a>`, "a link must be"},
		{`<a href="/relative">x</a>`, "a link must be"},
		{`<a href="data:text/html;base64,AAAA">x</a>`, "a link must be"},
		{`<a href="javascript:alert(1)" href="https://ok">x</a>`, "a link must be"},
		{`<!--[if mso]><v:rect/><![endif]-->`, "html blocks cannot contain conditional comments"},
		{`<!--[if mso]>x<![endif]-->`, "html blocks cannot contain conditional comments"},
		{`<!-- a <b> inside a comment -->`, "html blocks cannot contain conditional comments"},
		{`<![if !mso]><p>x</p><![endif]>`, "html blocks cannot contain conditional comments"},
		{`<!--[if mso]>x-->`, "html blocks cannot contain conditional comments"},
	}
	for _, c := range refused {
		if err := validateMarkup(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want %q, got %v", c.src, c.want, err)
		}
	}
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
