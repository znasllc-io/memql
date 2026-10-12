package campaigns

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"testing"

	nethtml "golang.org/x/net/html"
)

// render_placement_test.go -- where the footer and the open pixel go in an HTML
// part, and what happens when they cannot go anywhere (memql#6022, F11).
//
// WHAT IS PROMISED. The footer and the pixel are one fragment, spliced in at one
// byte offset: the start of the last real closing body tag, else of the last
// real closing html tag, else the end. Every other byte of the template is the
// byte the author wrote. That is what lets an
// email keep the markup it was built with (conditional comments that hide
// things from Outlook, entities, tables with no tbody, a fragment with no
// html or body), so these tests compare bytes, not trees.
//
// The other half is refusal. A template that ends inside a comment, a tag or a
// raw-text element would swallow a fragment appended to it, and one that is a
// frameset has nowhere to show one. Those fail with a plain sentence for the
// template's author, in a campaign send and in a test send alike, rather than
// mailing a message with no footer.
//
// The tokenizer reads a template alone, with scripting on and no namespaces, and
// a mail client does not: it builds a tree with the HTML5 algorithm, scripting
// off. So a spot the tokenizer finds can be one the client does not show (inside
// a <template>, swallowed by a comment only the tree builder sees, inside an svg
// or an element Outlook hides), and the planner's work is checked against the
// tree. shownByAClient is that tree, written out again from the requirement.

const (
	wantEndsOpen    = "This template's HTML ends inside a comment or a tag, so its unsubscribe link cannot be placed. Fix the HTML and save it again."
	wantFrames      = "This template uses frames, which email cannot show."
	wantOutlookOpen = "This template leaves an Outlook-only section open at its end, so its unsubscribe link cannot be placed. Close the section and save it again."

	// here marks, in a test template, the byte offset the fragment is expected
	// at. It is removed before the template is rendered.
	here = "§"
)

// placement renders a template with opens tracked, so what is placed is the
// footer and the pixel, and returns the message's HTML and the fragment that
// should have been placed.
func placement(t *testing.T, template string) (html string, fragment string, err error) {
	t.Helper()
	c := trackingCampaign(true, false)
	r := Recipient{ID: "r-1", Email: "someone@example.test", DisplayName: "Ada"}
	opts := RenderOptions{Links: messageTestLinks(), Tracking: trackingConfig().trackingRenderFor(c, deliveryRowID(c.ID, r.ID))}

	footer, ferr := footerTemplate(footerSender(opts), opts.Links.Unsubscribe)
	if ferr != nil {
		t.Fatalf("footerTemplate: %v", ferr)
	}
	pixel := opts.Tracking.openPixel()
	if pixel == "" {
		t.Fatal("precondition: opens are tracked, so there is a pixel")
	}

	msg, err := renderMessage(c, Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: template}, r, opts)
	return msg.HTMLBody, footer + pixel, err
}

// unmarked removes the marker from a template and says where it was.
func unmarked(t *testing.T, marked string) (template string, at int) {
	t.Helper()
	at = strings.Index(marked, here)
	if at < 0 || strings.Count(marked, here) != 1 {
		t.Fatalf("a test template needs exactly one %q: %q", here, marked)
	}
	return marked[:at] + marked[at+len(here):], at
}

// pixelSource is the address of the open pixel in a fragment placement returned.
func pixelSource(t *testing.T, fragment string) string {
	t.Helper()
	m := regexp.MustCompile(`<img src="([^"]*)"`).FindStringSubmatch(fragment)
	if m == nil {
		t.Fatalf("no pixel in the fragment %q", fragment)
	}
	return html.UnescapeString(m[1])
}

// shownByAClient parses message the way a mail client does -- the HTML5 tree
// builder with scripting off -- and counts what a person would be shown of it:
// the links to unsubscribeURL and the images from pixelSrc that are elements of
// the HTML namespace with no <template>, <noscript> or foreign-content (svg,
// math) ancestor. A link a comment swallowed is not an element at all.
func shownByAClient(t *testing.T, message, unsubscribeURL, pixelSrc string) (links, pixels int) {
	t.Helper()
	doc, err := nethtml.ParseWithOptions(strings.NewReader(message), nethtml.ParseOptionEnableScripting(false))
	if err != nil {
		t.Fatalf("parsing %q: %v", message, err)
	}
	attr := func(n *nethtml.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return strings.TrimFunc(a.Val, isURLPadding)
			}
		}
		return ""
	}
	var walk func(n *nethtml.Node, hidden bool)
	walk = func(n *nethtml.Node, hidden bool) {
		if n.Type == nethtml.ElementNode {
			if !hidden && n.Namespace == "" {
				switch n.Data {
				case "a":
					if attr(n, "href") == unsubscribeURL {
						links++
					}
				case "img":
					if pixelSrc != "" && attr(n, "src") == pixelSrc {
						pixels++
					}
				}
			}
			hidden = hidden || n.Namespace != "" || n.Data == "template" || n.Data == "noscript"
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, hidden)
		}
	}
	walk(doc, false)
	return links, pixels
}

// TestTheFooterIsSplicedInAndEveryOtherByteIsKept: the message is the template
// with the fragment put at one offset and nothing else changed. Removing the
// fragment gives the template back, byte for byte.
func TestTheFooterIsSplicedInAndEveryOtherByteIsKept(t *testing.T) {
	for name, marked := range map[string]string{
		// Both conditional forms, an entity, a table with no tbody, XHTML doctype:
		// each is something a tree builder rewrites.
		"a full document with both conditional forms": "<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0 Transitional//EN\" \"http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd\">\n" +
			"<html xmlns=\"http://www.w3.org/1999/xhtml\">\n<head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=UTF-8\"/><title>x</title>\n" +
			"<!--[if mso]><style>td{font-family:Arial}</style><![endif]-->\n</head>\n<body style=\"margin:0\">\n" +
			"<!--[if mso]><table><tr><td><![endif]-->\n" +
			"<![if !mso]><p>downlevel-revealed</p><![endif]>\n" +
			"<!--[if !mso]><!--><p>modern only</p><!--<![endif]-->\n" +
			"<table width=\"600\"><tr><td>a&nbsp;b &copy; 'q' \"d\" &lt;&gt;</td></tr></table>\n" + here + "</body>\n</html>",
		// The shape a layout renderer produces: conditional comments around the
		// page wrapper, a button (a VML shape for Outlook, an anchor for the rest)
		// and a media query inside the not-Outlook form, with a link to the
		// preferences page already in the body.
		"a layout document with conditional comments around its button, columns and media query": "<html xmlns:v=\"urn:schemas-microsoft-com:vml\" xmlns:o=\"urn:schemas-microsoft-com:office:office\"><head>\n" +
			"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n<meta http-equiv=\"Content-Type\" content=\"text/html; charset=UTF-8\">\n" +
			"<style>body{margin:0;padding:0}table{mso-table-lspace:0;mso-table-rspace:0}a[x-apple-data-detectors]{color:inherit!important}@media (max-width:599px){.ecw{width:100%!important}}</style>\n" +
			"<!--[if mso]><div><noscript><xml><w:WordDocument xmlns:w=\"urn:schemas-microsoft-com:office:word\"><w:DontUseAdvancedTypographyReadingMail/></w:WordDocument></xml></noscript></div><![endif]-->\n" +
			"<!--[if !mso]><!--><style>@media (max-width:480px){.c1-c0,.c1-c1{display:block!important;width:100%!important}}</style><!--<![endif]--></head>\n" +
			"<body style=\"width:100%;margin:0;padding:0\">Preheader text\n" +
			"<table width=\"100%\" border=\"0\" cellpadding=\"0\" cellspacing=\"0\"><tbody><tr><td>\n" +
			"<!--[if mso]><center><table align=\"center\" border=\"0\" cellpadding=\"0\" cellspacing=\"0\" width=\"600\"><tbody><tr><td><![endif]-->\n" +
			"<table align=\"center\" width=\"600\" class=\"ecw\"><tbody><tr><td>" +
			"<!--[if mso]><v:roundrect xmlns:v=\"urn:schemas-microsoft-com:vml\" href=\"https://acme.test/shop\" style=\"height:44px;width:200px\" arcsize=\"50%\" fillcolor=\"#111111\"><w:anchorlock/><center>Shop</center></v:roundrect><![endif]-->" +
			"<!--[if !mso]><!--><table><tbody><tr><td bgcolor=\"#111111\"><a href=\"https://acme.test/shop\" target=\"_blank\" style=\"display:block\">Shop</a></td></tr></tbody></table><!--<![endif]-->" +
			"</td></tr></tbody></table>\n<!--[if mso]></td></tr></tbody></table></center><![endif]-->\n" +
			"<p>&copy; 2026 Acme &nbsp;&middot;&nbsp; <a href=\"https://api.example.test/email/preferences?token=l1.x\">Manage preferences</a></p>\n" +
			"</td></tr></tbody></table>" + here + "</body></html>",
		"a fragment with no html or body":         "<p>Hi&nbsp;there</p><table><tr><td>x</td></tr></table><br>" + here,
		"a fragment ending in a newline":          "<p>Hi</p>\n" + here,
		"a fragment ending in a closed tag":       "<div><p>Hi</p></div>" + here,
		"upper-case markup":                       "<BODY><P>Hi</P>" + here + "</BODY></HTML>",
		"a closing tag with a space":              "<body><p>Hi</p>" + here + "</body ></html>",
		"a closing tag with an attribute":         "<body><p>Hi</p>" + here + "</body foo=\"x\"></html>",
		"a closing tag with a slash":              "<body><p>Hi</p>" + here + "</body/></html>",
		"two closing tags, the last is used":      "<body><p>a</p></body><p>b</p>" + here + "</body>",
		"a closing html tag and no closing body":  "<html><body><p>Hi</p>" + here + "</html>",
		"a closing html tag and no body at all":   "<html><p>Hi</p>" + here + "</HTML >",
		"a closing body wins over a closing html": "<html><body><p>Hi</p>" + here + "</body></html>",
		"the last closing html tag":               "<html><p>a</p></html><p>b</p>" + here + "</html>",
		"a closing html tag in a comment":         "<p>Hi</p><!-- </html> -->" + here,
		"content after the document":              "<html><body><p>Hi</p>" + here + "</body></html>\n<p>trailer</p>",
		"carriage returns":                        "<html>\r\n<body>\r\n<p>Hi</p>\r\n" + here + "</body>\r\n</html>\r\n",
		"non-ASCII text":                          "<p>Café — “quoted” 日本語 🙂</p>" + here + "</body>",
		"comments of every spelling":              "<!-- a -- b --><!----><!--->< !-->\n<p>x</p><!-- --!>" + here + "</body>",
		"a body with a style and a script":        "<body><style>p{color:red}</style><script>var s='<b>';</script><p>x</p>" + here + "</body>",
		"attributes in every quoting":             "<a href=\"x\" title='y' class=z data-q=\"it's\">k</a><img alt=\"\" src=x>" + here + "</body>",
		"a template that is only a body":          "<body>" + here + "</body>",
		"a template that is only a tag":           "<br>" + here,
		"a template of one word":                  "Hello" + here,
		"a doctype and nothing else":              "<!DOCTYPE html>" + here,
		"a closed raw-text element at the end":    "<p>x</p><textarea>draft</textarea>" + here,
	} {
		t.Run(name, func(t *testing.T) {
			template, at := unmarked(t, marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("renderMessage: %v", err)
			}
			want := template[:at] + fragment + template[at:]
			if out != want {
				t.Errorf("the message is not the template with the fragment spliced in at byte %d.\n template: %q\n want:     %q\n got:      %q",
					at, template, want, out)
			}
			if strings.Count(out, fragment) != 1 || strings.Replace(out, fragment, "", 1) != template {
				t.Errorf("taking the fragment out does not give the template back.\n template: %q\n got:      %q",
					template, strings.Replace(out, fragment, "", 1))
			}
		})
	}
}

// TestOnlyARealClosingBodyTagMarksTheSpot: a closing body tag that is inside a
// comment, a raw-text element, an attribute or a bogus comment is text, and the
// footer does not go before it.
func TestOnlyARealClosingBodyTagMarksTheSpot(t *testing.T) {
	for name, marked := range map[string]string{
		"inside a comment":                      "<p>Hi</p><!-- </body> -->" + here + "</body>",
		"inside a script":                       "<p>Hi</p><script>var s = \"</body>\";</script>" + here + "</body>",
		"inside a style":                        "<p>Hi</p><style>/* </body> */</style>" + here + "</body>",
		"inside a textarea":                     "<p>Hi</p><textarea></body></textarea>" + here + "</body>",
		"inside a title":                        "<p>Hi</p><title></body></title>" + here + "</body>",
		"inside an iframe":                      "<p>Hi</p><iframe></body></iframe>" + here + "</body>",
		"inside a noscript":                     "<p>Hi</p><noscript></body></noscript>" + here + "</body>",
		"inside an xmp":                         "<p>Hi</p><xmp></body></xmp>" + here + "</body>",
		"inside an attribute":                   "<p>Hi</p><a title=\"</body>\">x</a>" + here + "</body>",
		"inside a bogus comment":                "<p>Hi</p><!x </body> y>" + here + "</body>",
		"inside a processing instruction":       "<p>Hi</p><?php echo \"</body>\"; ?>" + here + "</body>",
		"inside an upper-case script":           "<p>Hi</p><SCRIPT>x = '</body>'</SCRIPT>" + here + "</body>",
		"a real one after a fake one":           "<p>Hi</p></body><!-- </body> -->" + here + "</body>",
		"only fake ones":                        "<p>Hi</p><!-- </body> --><script>x = \"</body>\"</script>" + here,
		"only one in a comment":                 "<p>Hi</p><!-- </body> -->" + here,
		"a script that mentions a fake end tag": "<p>Hi</p><script>var s = \"</scr\" + \"ipt>\";</script>" + here + "</body>",
	} {
		t.Run(name, func(t *testing.T) {
			template, at := unmarked(t, marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("renderMessage: %v", err)
			}
			if want := template[:at] + fragment + template[at:]; out != want {
				t.Errorf("the fragment is not at byte %d.\n want: %q\n got:  %q", at, want, out)
			}
		})
	}
}

// TestATemplateThatEndsInsideSomethingIsRefused: if the template stops inside a
// comment, a tag or a raw-text element, anything appended would be part of it
// and no client would show it. So the message is not made, and the sentence says
// what to fix.
func TestATemplateThatEndsInsideSomethingIsRefused(t *testing.T) {
	for name, template := range map[string]string{
		"an unterminated comment":                               "<p>Hi</p><!--",
		"an unterminated comment with text":                     "<p>Hi</p><!-- draft footer ",
		"an unterminated conditional comment":                   "<p>Hi</p><!--[if mso]><table><tr><td>",
		"a comment whose dashes are all the opening's":          "<p>Hi</p><!---",
		"a comment closed by two dashes and no bracket":         "<p>Hi</p><!-- a --",
		"a comment closed by a bang and no bracket":             "<p>Hi</p><!-- a --!",
		"a comment that only looks closed":                      "<p>Hi</p><!--!>",
		"a comment that holds a bracket":                        "<p>Hi</p><!-- a >",
		"half a comment opening":                                "<p>Hi</p><!-",
		"a markup declaration with nothing after it":            "<p>Hi</p><!",
		"an unterminated bogus comment":                         "<p>Hi</p><!x draft",
		"an unterminated downlevel conditional":                 "<p>Hi</p><![if !mso]",
		"an unterminated CDATA section":                         "<p>Hi</p><![CDATA[ draft",
		"an unterminated processing instruction":                "<p>Hi</p><?xml version=\"1.0\"",
		"an unterminated bogus end tag":                         "<p>Hi</p></ bogus",
		"an unterminated doctype":                               "<!DOCTYPE html",
		"a doctype with nothing after the word":                 "<!DOCTYPE",
		"a start tag cut off in its name":                       "<p>Hi</p><di",
		"a start tag cut off in its attributes":                 "<p>Hi<a title=x",
		"a start tag cut off inside a quote":                    "<p>Hi</p><a title=\"x",
		"a start tag cut off inside a single quote":             "<p>Hi</p><img src=x alt='a",
		"a start tag cut off after a slash":                     "<p>Hi</p><br /",
		"an end tag cut off":                                    "<p>Hi</div",
		"an end tag cut off after the slash":                    "<p>Hi</p></",
		"a lone opening bracket":                                "<p>Hi</p><",
		"an unterminated style":                                 "<p>Hi</p><style>p{color:red",
		"an unterminated script":                                "<p>Hi</p><script>var x = 1;",
		"an unterminated textarea":                              "<p>Hi</p><textarea>draft",
		"an unterminated title":                                 "<p>Hi</p><title>draft",
		"an unterminated xmp":                                   "<p>Hi</p><xmp>draft",
		"an unterminated noscript":                              "<p>Hi</p><noscript>draft",
		"an unterminated iframe":                                "<p>Hi</p><iframe>draft",
		"an unterminated noembed":                               "<p>Hi</p><noembed>draft",
		"an unterminated noframes":                              "<p>Hi</p><noframes>draft",
		"an empty unterminated style":                           "<p>Hi</p><style>",
		"a self-closed script, which is still a script":         "<p>Hi</p><script/>draft",
		"a script whose end tag is only spelled in text":        "<p>Hi</p><script>var s = \"</scr\" + \"ipt>\";",
		"a plaintext element":                                   "<p>Hi</p><plaintext>everything after this is text <b>bold",
		"a plaintext element with an end tag, which it ignores": "<p>Hi</p><plaintext>stuff</plaintext><p>more</p>",
		"nothing but a plaintext element":                       "<plaintext>",
		"an unterminated comment after a closing body":          "<p>Hi</p></body><!--",
		"an unterminated style after a closed document":         "<html><body><p>Hi</p></body></html><style>x",
		"a comment that holds the only closing body":            "<p>Hi</p><!-- </body>",
		"a closing body tag that is itself cut off":             "<p>Hi</p></body",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := placement(t, template)
			if err == nil {
				t.Fatalf("a template that ends inside something was accepted: %q", template)
			}
			if err.Error() != wantEndsOpen {
				t.Errorf("the refusal reads %q, want %q", err.Error(), wantEndsOpen)
			}
		})
	}
}

// TestAFramesetIsRefused: a frameset document has no body, so there is nowhere
// to show a footer in it.
func TestAFramesetIsRefused(t *testing.T) {
	for name, template := range map[string]string{
		"a frameset":                 "<frameset cols=\"50%,50%\"><frame src=\"a.html\"><frame src=\"b.html\"></frameset>",
		"a frameset in a document":   "<html><head><title>x</title></head><frameset rows=\"*\"><frame src=x></frameset></html>",
		"upper-case markup":          "<FRAMESET><FRAME src=x></FRAMESET>",
		"a frameset after some text": "<p>Hi</p><frameset><frame src=x></frameset>",
		"a self-closed frameset":     "<frameset/>",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := placement(t, template)
			if err == nil {
				t.Fatalf("a frameset was accepted: %q", template)
			}
			if err.Error() != wantFrames {
				t.Errorf("the refusal reads %q, want %q", err.Error(), wantFrames)
			}
		})
	}

	// The word in a comment or in text is not a frameset.
	for name, marked := range map[string]string{
		"in a comment":    "<!-- <frameset> --><p>Hi</p>" + here,
		"in a script":     "<script>var s = '<frameset>';</script><p>Hi</p>" + here,
		"in an attribute": "<p title=\"<frameset>\">Hi</p>" + here,
	} {
		template, at := unmarked(t, marked)
		out, fragment, err := placement(t, template)
		if err != nil || out != template[:at]+fragment+template[at:] {
			t.Errorf("%s: err = %v, out = %q", name, err, out)
		}
	}
}

// TestATemplateThatStopsShortOfItsEndStillGetsItsFooter: a template that leaves
// an element open (an anchor, a table, a division) is not refused. The footer
// goes where the template's end is and a client shows it.
func TestATemplateThatStopsShortOfItsEndStillGetsItsFooter(t *testing.T) {
	for name, marked := range map[string]string{
		"an open anchor":                               "<p>Hi</p><a href=\"https://acme.test/x\">read more" + here,
		"an open table":                                "<table><tr><td>Hi" + here,
		"an open division":                             "<div><div><p>Hi" + here,
		"a body closed early":                          "<html><body><p>Hi</p>" + here + "</body></html><p>after</p>",
		"a template ending in a comment":               "<p>Hi</p><!-- the end -->" + here,
		"a template ending in a downlevel conditional": "<p>Hi</p><![endif]>" + here,
		"a template ending in a closed style":          "<p>Hi</p><style>p{}</style>" + here,
		"a template ending in a closed script":         "<p>Hi</p><script>var x;</script>" + here,
		"a template ending in a bogus comment":         "<p>Hi</p><!x draft>" + here,
	} {
		t.Run(name, func(t *testing.T) {
			template, at := unmarked(t, marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("renderMessage: %v", err)
			}
			if want := template[:at] + fragment + template[at:]; out != want {
				t.Errorf("the fragment is not at byte %d.\n want: %q\n got:  %q", at, want, out)
			}
			// And a client would show the link, once.
			if n := countOf(anchorHrefs(t, out), messageTestLinks().Unsubscribe); n != 1 {
				t.Errorf("a client would show %d unsubscribe links, want 1.\ngot: %s", n, out)
			}
		})
	}
}

// TestAFooterAClientWouldNotShowIsNotPlaced: the tokenizer reads a template with
// scripting on and no namespaces, and a client builds a tree with scripting off.
// Each row is a template where the two disagree about what is markup, so the
// spot the tokenizer finds is one where the client shows nothing. Such a template
// is placed at a spot the client does show, or refused with the sentence for a
// template that ends inside something. It is never mailed with a footer nobody
// sees, which is what these were before: each of the first five is a template an
// earlier planner accepted with no unsubscribe link in the message a client built.
func TestAFooterAClientWouldNotShowIsNotPlaced(t *testing.T) {
	for name, tc := range map[string]struct {
		marked string // the template; where the fragment goes, when it is placed, the marker is
		want   string // the refusal sentence, or "" when the fragment is placed at the marker
	}{
		// A closing body or html tag inside a <template> closes nothing: the
		// template's content is not the document's. The fragment goes after it.
		"a closing body tag inside a template":               {"<html><body><p>Hi</p><template></body></template>" + here + "</html>", ""},
		"a closing html tag inside a template":               {"<html><body><p>Hi</p><template></html></template>" + here, ""},
		"a closing html tag in a template and another after": {"<html><body><p>Hi</p><template></html></template>" + here + "</html>", ""},
		"both closing tags inside a template and none after": {"<p>Hi</p><template></body></html></template>" + here, ""},
		"closing tags in nested templates":                   {"<body><template><template></body></template></body></template>" + here + "</body>", ""},
		"a template closed before the closing body tag":      {"<body><template><p>x</p></template><p>Hi</p>" + here + "</body>", ""},
		// A slash on a <template> is ignored by the HTML parser: it opens a template.
		"a self-closed template still opens one": {"<html><body><template/></body></template>" + here + "</html>", ""},

		// Scripting off, what follows <noscript> is markup, so a comment or a style
		// that the tokenizer ended at </noscript> goes on and swallows the rest.
		"a noscript that opens a comment": {"<html><body><p>Hi</p><noscript><!--</noscript></body></html>", wantEndsOpen},
		"a noscript that opens a style":   {"<html><body><p>Hi</p><noscript><style></noscript></body></html>", wantEndsOpen},
		// ...and a </noscript> cannot close a noscript that has a paragraph open
		// inside it, so what follows is still inside the noscript.
		"a noscript whose paragraph its end tag cannot close": {"<html><body><p>Hi</p><noscript><p></noscript></body></html>", wantEndsOpen},

		// In svg a <style> is not raw text, and <![CDATA[ is a section whose text
		// holds everything up to ]]>, closing body tag included.
		"an svg style that opens a comment": {"<html><body><p>Hi</p><svg><style><!--</style></svg></body></html>", wantEndsOpen},
		"an svg CDATA section":              {"<html><body><p>Hi</p><svg><![CDATA[ > </body> ]]></svg>", wantEndsOpen},

		// Left open at the end, the fragment lands inside an element whose content
		// a client does not show as HTML.
		"a template left open":           {"<html><body><p>Hi</p><template>", wantEndsOpen},
		"a math element left open":       {"<p>Hi</p><math><mi>", wantEndsOpen},
		"an svg foreignObject left open": {"<p>Hi</p><svg><foreignObject>", wantEndsOpen},

		// The template's own link is judged the same way: a link the tokenizer
		// counts but a client does not see is no link, and the footer's spot must
		// be shown whether or not this message has a footer to place.
		"its own link swallowed":                          {`<html><body><svg><style><!--</style></svg><a href="{{unsubscribeUrl}}">Leave</a></body></html>`, wantEndsOpen},
		"the spot swallowed though its own link is shown": {`<html><body><a href="{{unsubscribeUrl}}">Leave</a><svg><style><!--</style></svg></body></html>`, wantEndsOpen},
		// A comment the tree builder opens in an svg style can end at a later one:
		// the link is swallowed, the spot after it is not. The tokenizer counted the
		// link, so no footer is placed, and the message would have none a person
		// could use.
		"its own link swallowed and the spot not": {`<html><body><svg><style><!--</style></svg><a href="{{unsubscribeUrl}}">Leave</a><!-- x -->MORE</body></html>`, wantEndsOpen},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want != "" {
				out, _, err := placement(t, tc.marked)
				if err == nil || err.Error() != tc.want {
					t.Fatalf("err = %v, want the sentence %q.\nthe message was: %q", err, tc.want, out)
				}
				return
			}
			template, at := unmarked(t, tc.marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("a template with a spot a client shows was refused: %v", err)
			}
			if want := template[:at] + fragment + template[at:]; out != want {
				t.Errorf("the fragment is not at byte %d.\n want: %q\n got:  %q", at, want, out)
			}
			links, pixels := shownByAClient(t, out, messageTestLinks().Unsubscribe, pixelSource(t, fragment))
			if links != 1 || pixels != 1 {
				t.Errorf("a client would show %d unsubscribe links and %d pixels, want 1 and 1.\ngot: %s", links, pixels, out)
			}
		})
	}
}

// TestAMessageNeedsAShownFooterSpotPixelAndOwnLink: what checkShown asks of the
// tree, one requirement at a time. A hidden pixel beside a shown footer is not a
// thing a parser makes of real markup (both are siblings at one spot), so the
// requirements are held apart here and not by templates.
func TestAMessageNeedsAShownFooterSpotPixelAndOwnLink(t *testing.T) {
	for _, footer := range []bool{false, true} {
		for _, pixel := range []bool{false, true} {
			for _, own := range []bool{false, true} {
				got := shownByClient{footer: footer, pixel: pixel, ownLink: own}
				for _, hasOwn := range []bool{false, true} {
					want := footer && pixel && (!hasOwn || own)
					if got.enough(hasOwn) != want {
						t.Errorf("%+v with a link of its own = %v: enough = %v, want %v", got, hasOwn, got.enough(hasOwn), want)
					}
				}
			}
		}
	}
}

// TestTheTreeIsReadForTheProbeAndForNothingElse: clientShows on tiny documents.
// Only the probe's own link and image, and the message's own unsubscribe link,
// are looked for, and only where a client shows them.
func TestTheTreeIsReadForTheProbeAndForNothingElse(t *testing.T) {
	const own = "https://api.example.test/unsubscribe?token=u3.x"
	probeLink := `<a href="` + probeUnsubscribeURL + `">u</a>`
	probeImage := `<img src="` + probePixelSrc + `">`
	for name, tc := range map[string]struct {
		html string
		want shownByClient
	}{
		"the probe in a body":           {probeLink + probeImage, shownByClient{footer: true, pixel: true}},
		"other links and images":        {`<a href="https://acme.test/x">x</a><img src="https://acme.test/i.png">`, shownByClient{}},
		"the message's own link":        {`<a href="` + own + `">u</a>`, shownByClient{ownLink: true}},
		"its own link padded by blanks": {"<a href=\" " + own + "\n\">u</a>", shownByClient{ownLink: true}},
		"a link with no address":        {`<a>u</a><img>`, shownByClient{}},
		"the probe in a template":       {`<template>` + probeLink + probeImage + `</template>`, shownByClient{}},
		// In the head a <noscript> is closed by the first thing that is not head
		// content, so the row is in the body.
		"the probe in a noscript":                   {`<p>x</p><noscript>` + probeLink + probeImage + `</noscript>`, shownByClient{}},
		"the probe in a comment":                    {`<!--` + probeLink + probeImage + `-->`, shownByClient{}},
		"the probe in an svg foreignObject":         {`<svg><foreignObject>` + probeLink + probeImage + `</foreignObject></svg>`, shownByClient{}},
		"the probe in math text":                    {`<math><mi>` + probeLink + probeImage + `</mi></math>`, shownByClient{}},
		"the link in a template, the image outside": {`<template>` + probeLink + `</template>` + probeImage, shownByClient{pixel: true}},
		"the link outside, the image in a template": {probeLink + `<template>` + probeImage + `</template>`, shownByClient{footer: true}},
		"the own link in a template":                {`<template><a href="` + own + `">u</a></template>`, shownByClient{}},
	} {
		doc, err := nethtml.ParseWithOptions(strings.NewReader(tc.html), nethtml.ParseOptionEnableScripting(false))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := clientShows(doc, own); got != tc.want {
			t.Errorf("%s: clientShows = %+v, want %+v", name, got, tc.want)
		}
	}
}

// TestAnOutlookSectionLeftOpenHidesTheFooter: <![if !mso]> and
// <!--[if !mso]><!--> open a section that Outlook does not show and every other
// client does, and <![endif]> and <!--<![endif]--> close it. A footer put where a
// section is still open is hidden from Outlook, so such a template is refused
// with a sentence that says what to close. Layouts close their sections around
// buttons, columns and media queries, and those are placed.
func TestAnOutlookSectionLeftOpenHidesTheFooter(t *testing.T) {
	emaildocShaped := "<html xmlns:v=\"urn:schemas-microsoft-com:vml\"><head>\n" +
		"<!--[if mso]><xml><o:OfficeDocumentSettings><o:PixelsPerInch>96</o:PixelsPerInch></o:OfficeDocumentSettings></xml><![endif]-->\n" +
		"<!--[if !mso]><!--><style>@media (max-width:480px){.c1-c0{display:block!important}}</style><!--<![endif]-->\n</head><body>\n" +
		"<!--[if mso]><table align=\"center\" width=\"600\"><tr><td><![endif]-->\n" +
		"<table><tbody><tr><td>" +
		"<!--[if mso]><v:roundrect href=\"https://acme.test/shop\" style=\"height:44px;width:200px\"><center>Shop</center></v:roundrect><![endif]-->" +
		"<!--[if !mso]><!--><a href=\"https://acme.test/shop\">Shop</a><!--<![endif]-->" +
		"</td></tr></tbody></table>\n" +
		"<![if !mso]><p>Only for clients that are not Outlook</p><![endif]>\n" +
		"<!--[if mso]></td></tr></table><![endif]-->\n" + here + "</body></html>"

	for name, tc := range map[string]struct {
		marked string
		want   string // the refusal sentence, or "" when the fragment is placed at the marker
	}{
		"a revealed section left open":                     {"<html><body><p>Hi</p><![if !mso]><p>modern</p></body></html>", wantOutlookOpen},
		"a comment-wrapped section left open":              {"<html><body><p>Hi</p><!--[if !mso]><!--><p>modern</p></body></html>", wantOutlookOpen},
		"a section left open and no closing tags at all":   {"<p>Hi</p><![if !mso]><p>modern</p>", wantOutlookOpen},
		"a section left open and only a closing html tag":  {"<p>Hi</p><![if !mso]><p>modern</p></html>", wantOutlookOpen},
		"a section opened again after one was closed":      {"<body><![if !mso]>a<![endif]><![if !mso]>b</body>", wantOutlookOpen},
		"an inner section closed and the outer left open":  {"<body><!--[if !mso]><!--><![if !mso]>a<![endif]>b</body>", wantOutlookOpen},
		"a section closed only after the closing body tag": {"<body><![if !mso]><p>x</p></body><![endif]>", wantOutlookOpen},
		"capitals, spaces and parentheses":                 {"<body><![IF  !(MSO) ]>x</body>", wantOutlookOpen},
		"an open section holding the template's own link":  {`<body><![if !mso]><a href="{{unsubscribeUrl}}">Unsubscribe</a></body>`, wantOutlookOpen},

		"a revealed section that is closed":                 {"<body><![if !mso]><p>modern</p><![endif]>" + here + "</body>", ""},
		"a comment-wrapped section that is closed":          {"<body><!--[if !mso]><!--><p>modern</p><!--<![endif]-->" + here + "</body>", ""},
		"a section Outlook shows, left open":                {"<body><![if mso]><p>both</p>" + here + "</body>", ""},
		"an Outlook-only comment":                           {"<body><!--[if mso]><p>Outlook</p><![endif]-->" + here + "</body>", ""},
		"a comment that hides a section from Outlook":       {"<body><!--[if !mso]><p>never shown</p><![endif]-->" + here + "</body>", ""},
		"sections closed around a button and a media query": {emaildocShaped, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want != "" {
				out, _, err := placement(t, tc.marked)
				if err == nil || err.Error() != tc.want {
					t.Fatalf("err = %v, want the sentence %q.\nthe message was: %q", err, tc.want, out)
				}
				return
			}
			template, at := unmarked(t, tc.marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("a template whose sections are closed was refused: %v", err)
			}
			if want := template[:at] + fragment + template[at:]; out != want {
				t.Errorf("the fragment is not at byte %d.\n want: %q\n got:  %q", at, want, out)
			}
		})
	}
}

// TestALinkInASectionOutlookHidesIsNotTheTemplatesOwn: a template that links to
// {{unsubscribeUrl}} is not given a footer, but only if a person sees the link,
// and a link inside a section Outlook hides is not seen by Outlook. It does not
// stand in for the footer, which goes in as for any other template. A link
// outside every section does.
func TestALinkInASectionOutlookHidesIsNotTheTemplatesOwn(t *testing.T) {
	const footerWords = "You are receiving this because"
	for name, tc := range map[string]struct {
		template   string
		wantFooter bool
		wantLinks  int // links to the unsubscribe page a client would show
	}{
		"in a closed revealed section":        {`<body><![if !mso]><a href="{{unsubscribeUrl}}">Unsubscribe</a><![endif]></body>`, true, 2},
		"in a closed comment-wrapped section": {`<body><!--[if !mso]><!--><a href="{{unsubscribeUrl}}">Unsubscribe</a><!--<![endif]--></body>`, true, 2},
		"outside every section":               {`<body><a href="{{unsubscribeUrl}}">Unsubscribe</a><![if !mso]><p>x</p><![endif]></body>`, false, 1},
		"after a section that was closed":     {`<body><![if !mso]><p>x</p><![endif]><a href="{{unsubscribeUrl}}">Unsubscribe</a></body>`, false, 1},
		// A template with a link of its own is given no footer, so a section left
		// open after the link hides nothing the footer needed.
		"outside every section, and one left open after it": {`<body><a href="{{unsubscribeUrl}}">Unsubscribe</a><![if !mso]><p>x</p></body>`, false, 1},
		"in a section Outlook shows":                        {`<body><![if mso]><a href="{{unsubscribeUrl}}">Unsubscribe</a><![endif]></body>`, false, 1},
		"in a comment only Outlook reads":                   {`<body><!--[if mso]><a href="{{unsubscribeUrl}}">Unsubscribe</a><![endif]--></body>`, true, 1},
		"in the second of two closed sections":              {`<body><![if !mso]>a<![endif]><![if !mso]><a href="{{unsubscribeUrl}}">U</a><![endif]></body>`, true, 2},
		"in a section nested inside one that is open":       {`<body><!--[if !mso]><!--><![if mso]><a href="{{unsubscribeUrl}}">U</a><![endif]><!--<![endif]--></body>`, true, 2},
	} {
		t.Run(name, func(t *testing.T) {
			out, fragment, err := placement(t, tc.template)
			if err != nil {
				t.Fatalf("renderMessage: %v", err)
			}
			if got := strings.Contains(out, footerWords); got != tc.wantFooter {
				t.Errorf("footer placed = %v, want %v.\ngot: %s", got, tc.wantFooter, out)
			}
			links, _ := shownByAClient(t, out, messageTestLinks().Unsubscribe, pixelSource(t, fragment))
			if links != tc.wantLinks {
				t.Errorf("a client would show %d unsubscribe links, want %d.\ngot: %s", links, tc.wantLinks, out)
			}
		})
	}
}

// TestAFaultIsRefusedWhetherOrNotAnythingWouldBePlaced: whether a message needs a
// footer depends on the template, and whether it needs a pixel depends on the
// campaign. A template with a fault must not pass the test send, which tracks
// nothing, and then fail the campaign, which tracks opens. So the fault is the
// template's, whatever is to be placed.
func TestAFaultIsRefusedWhetherOrNotAnythingWouldBePlaced(t *testing.T) {
	campaign := Campaign{ID: "camp-1", OwnerUserID: "u-1", Name: "Spring"}
	recipient := Recipient{ID: "r-1", Email: "someone@example.test", DisplayName: "Ada"}
	for name, tc := range map[string]struct{ template, want string }{
		"its own link and then a comment that never ends": {`<a href="{{unsubscribeUrl}}">Leave</a><!--`, wantEndsOpen},
		"its own link and then a style that never ends":   {`<a href="{{unsubscribeUrl}}">Leave</a><style>p{`, wantEndsOpen},
		"its own link and then a tag cut off":             {`<a href="{{unsubscribeUrl}}">Leave</a><p class="`, wantEndsOpen},
		"a frameset":                                      {`<frameset><frame src=x></frameset>`, wantFrames},
	} {
		// Nothing tracked: the only thing the footer could be is the template's own link.
		_, err := renderMessage(campaign, Template{ID: "t1", Subject: "s", TextBody: "t", HTMLBody: tc.template},
			recipient, RenderOptions{Links: messageTestLinks()})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// TestATemplateWithNoHTMLPartHasNothingToRefuse: the check is of the HTML part.
func TestATemplateWithNoHTMLPartHasNothingToRefuse(t *testing.T) {
	msg := renderLinks(t, Template{ID: "t1", Subject: "s", TextBody: "Hi <!-- not html"}, RenderOptions{})
	if msg.HTMLBody != "" {
		t.Errorf("a template with no HTML part got one: %q", msg.HTMLBody)
	}
}

// templatePieces are what the random templates are made of: the markup an email
// is made of, and the constructs that decide where a document ends (comments of
// every state, quotes, raw-text elements, brackets, closing tags).
//
// <svg>, <math>, <template>, <select>, <![CDATA[ and the Outlook sections are in
// the pieces: the tokenizer cannot say what a client shows of them, and the tree
// check after the splice is what stands between a template made of them and a
// message with no footer a person can see. Not in the pieces: <frameset>, which
// is refused for a different reason (the test of where a refusal comes from adds
// it to its own alphabet).
var templatePieces = []string{
	"<p>", "</p>", "<div>", "</div>", "<table>", "<tr>", "<td>", "</td>", "</tr>", "</table>", "<br>", "<hr/>",
	"<b>", "</b>", "<span style=\"color:red\">", "</span>", "<a href=\"https://acme.test/x\">", "</a>",
	"<img src=x alt=\"a\">", "<center>", "</center>", "<font size=2>", "</font>", "<ul>", "<li>", "</ul>",
	"<html>", "</html>", "<head>", "</head>", "<body>", "</body>", "<body style=\"margin:0\">", "</BODY>",
	"<!DOCTYPE html>", "<!--", "-->", "--!>", "<!-->", "<!--->", "<!---->", "<!-", "<!", "<!x", "<?x", "<![if !mso]>", "<![endif]>",
	"<!--[if mso]>", "<![endif]-->", "<!--[if !mso]><!-->", "<!--<![endif]-->", "</ ", "</>",
	"<script>", "</script>", "<style>", "</style>", "<title>", "</title>", "<textarea>", "</textarea>",
	"<noscript>", "</noscript>", "<xmp>", "</xmp>", "<iframe>", "</iframe>", "<plaintext>", "<noembed>", "<noframes>",
	"<", ">", "</", "/", "=", "\"", "'", " ", "\n", "\r\n", "\t", "x", "Hi", "&nbsp;", "&amp;", "&lt;", "é", "日本",
	"<a title=\"", "<img alt='", "title=", "href=", "<a ", "<p class=x",
	"<template>", "</template>", "<svg>", "</svg>", "<math>", "</math>", "<select>", "</select>", "<option>",
	"<foreignObject>", "<![CDATA[", "]]>", "<![if mso]>", "<!--[if mso]>", "<!--[if !mso]>",
}

// templateGenerator is a deterministic source of random templates: the same seed
// gives the same templates, so a failure names a template that can be run again.
func templateGenerator(seed uint64) func() string {
	next := func(n int) int {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return int(seed % uint64(n))
	}
	return func() string {
		var b strings.Builder
		for n := 1 + next(14); n > 0; n-- {
			b.WriteString(templatePieces[next(len(templatePieces))])
		}
		return b.String()
	}
}

// TestAnAcceptedTemplateShowsItsFooterToARealParser: the tokenizer decides where
// the fragment goes and whether it can go anywhere, and a client decides what is
// shown with a tree builder. This puts thirty thousand random templates
// (templatePieces) through both. A template is refused with one of the three
// sentences, or the message is the template with the fragment spliced in once,
// and a client's tree (scripting off) shows the unsubscribe link once and the
// pixel once.
//
// It does not say whether a refusal was called for. TestARefusalIsExactlyAnEndThatSwallows
// does.
func TestAnAcceptedTemplateShowsItsFooterToARealParser(t *testing.T) {
	generate := templateGenerator(0x2545F4914F6CDD1D)

	refused, placed := 0, 0
	url := messageTestLinks().Unsubscribe
	for i := 0; i < 30000; i++ {
		template := generate()
		if strings.TrimSpace(template) == "" {
			continue
		}

		out, fragment, err := placement(t, template)
		if err != nil {
			refused++
			if e := err.Error(); e != wantEndsOpen && e != wantFrames && e != wantOutlookOpen {
				t.Fatalf("template %q was refused with something that is not one of the three sentences: %v", template, err)
			}
			continue
		}
		placed++
		if strings.Count(out, fragment) != 1 || strings.Replace(out, fragment, "", 1) != template {
			t.Fatalf("template %q: taking the fragment out of %q does not give the template back", template, out)
		}
		if links, pixels := shownByAClient(t, out, url, pixelSource(t, fragment)); links != 1 || pixels != 1 {
			t.Fatalf("template %q was accepted, but a client would show %d unsubscribe links and %d pixels in %q", template, links, pixels, out)
		}
	}
	if refused < 1000 || placed < 1000 {
		t.Fatalf("the generator is not exercising both outcomes (%d refused, %d placed)", refused, placed)
	}
}

// TestABracketInTheMiddleOfATemplateIsNotAnEnd: the tokenizer reads "a<" as text
// when a second "<" follows and starts a tag, so a text token that ends in a
// bracket is not always the end of the input, and only the last token can be cut
// short. A template that merely has "<<" in it has not stopped inside anything:
// merge tags from other tools are written <<First Name>>.
func TestABracketInTheMiddleOfATemplateIsNotAnEnd(t *testing.T) {
	for name, marked := range map[string]string{
		"a double bracket before a merge tag":   "<p>Hello <<First Name>>,</p>" + here,
		"a bracket before a tag":                "<p>a<<b>x</b></p>" + here,
		"a bracket before a comment":            "<p>a<<!-- c --></p>" + here,
		"a bracket before a bogus comment":      "<p>a<<!x></p>" + here,
		"a bracket before an end tag":           "<p>a<</p>" + here,
		"several brackets before a tag":         "<p>a<<<<b>x</b></p>" + here,
		"a bracket before the closing body tag": "<body><p>a<<b>x</b></p>" + here + "</body>",
		// Raw text may end in a bracket right before its own end tag: a control,
		// this was never refused.
		"a bracket at the end of a script": "<script>a<</script>" + here,
		"a bracket at the end of a style":  "<style>a<</style>" + here,
	} {
		t.Run(name, func(t *testing.T) {
			template, at := unmarked(t, marked)
			out, fragment, err := placement(t, template)
			if err != nil {
				t.Fatalf("a template that does not end inside anything was refused: %v", err)
			}
			if want := template[:at] + fragment + template[at:]; out != want {
				t.Errorf("the fragment is not at byte %d.\n want: %q\n got:  %q", at, want, out)
			}
			if n := countOf(anchorHrefs(t, out), messageTestLinks().Unsubscribe); n != 1 {
				t.Errorf("a client would show %d unsubscribe links, want 1.\ngot: %s", n, out)
			}
		})
	}
}

// tokenTrail is what the HTML tokenizer makes of s: one "kind:raw" entry per
// token. The end-of-input entry is left out unless it carries bytes, which is a
// tag the end cut short.
func tokenTrail(s string) []string {
	z := nethtml.NewTokenizer(strings.NewReader(s))
	var trail []string
	for {
		kind := z.Next()
		raw := string(z.Raw())
		if kind == nethtml.ErrorToken {
			if raw != "" {
				trail = append(trail, "cut:"+raw)
			}
			return trail
		}
		trail = append(trail, kind.String()+":"+raw)
	}
}

// endSwallowsATag is whether a start tag written after the last byte of template
// is read as a tag. It is not when the template ends inside a comment, a tag or a
// raw-text element: the tag becomes part of what is unfinished. That is the
// tokenizer's own behaviour asked directly (the tokens of the template with the
// tag are those of the template, plus the tag), and it shares no code with
// planHTML.
func endSwallowsATag(template string) bool {
	const tag = "<hr>"
	want := append(tokenTrail(template), "StartTag:"+tag)
	return !slices.Equal(tokenTrail(template+tag), want)
}

// TestARefusalIsExactlyAnEndThatSwallows checks the planner's judgement of where
// a template ends against what the tokenizer does with a tag written there.
//
// A template is refused as ending inside a comment or a tag when its end swallows
// a tag, and only then, with one exception made on purpose: a template that ends
// in a lone "<" is refused although a tag after it would be read as a tag, because
// it stops at the start of a tag it did not finish. A template that swallows and
// is accepted would have its footer swallowed; one that does not and is refused
// is an author told that a working template is broken, which is what
// <<First Name>> did.
//
// Two sources of templates: every sequence of up to four pieces from a small
// alphabet of the characters that make and end constructs, which reaches the
// short odd endings random templates rarely do, and the random templates of the
// property test above.
func TestARefusalIsExactlyAnEndThatSwallows(t *testing.T) {
	url := messageTestLinks().Unsubscribe
	var swallowing, refusedOpen, framed, outlook, checked int
	check := func(template string) {
		if strings.TrimSpace(template) == "" {
			return
		}
		checked++
		_, err := planHTML(template, url)
		swallows := endSwallowsATag(template)
		if swallows {
			swallowing++
		}
		switch {
		case err == errTemplateFrames:
			framed++
		case err == errTemplateOutlookOpen:
			// A different question (is a section Outlook hides still open at the
			// spot?), answered after the one this test asks.
			outlook++
		case err == errTemplateEndsOpen:
			refusedOpen++
			if !swallows && !strings.HasSuffix(template, "<") {
				t.Fatalf("template %q ends in nothing that swallows a tag, and was refused as if it did", template)
			}
		case err != nil:
			t.Fatalf("template %q: %v", template, err)
		case swallows:
			t.Fatalf("template %q ends inside something that swallows a tag, and was accepted", template)
		}
	}

	alphabet := []string{
		"<", ">", "</", "/", "!", "-", "--", "?", "=", "\"", "'", "a", " ", "[", "]",
		"<p>", "</p>", "<!--", "-->", "<script>", "</script>", "<plaintext>", "<title>", "<a ", "</body>", "<![", "<frameset>",
	}
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		check(prefix)
		if depth == 0 {
			return
		}
		for _, piece := range alphabet {
			walk(prefix+piece, depth-1)
		}
	}
	walk("", 4)

	generate := templateGenerator(0x9E3779B97F4A7C15)
	for i := 0; i < 30000; i++ {
		check(generate())
	}

	if swallowing < 5000 || refusedOpen < 5000 || framed < 1000 {
		t.Fatalf("the templates are not exercising the outcomes: %d of %d swallow, %d refused as open, %d are framesets", swallowing, checked, refusedOpen, framed)
	}
}

// TestPlanHTMLFindsTheSpotAndTheLink: planHTML on its own.
func TestPlanHTMLFindsTheSpotAndTheLink(t *testing.T) {
	const url = "https://x.test/u?token=1"
	for name, tc := range map[string]struct {
		marked string
		link   bool
	}{
		"an empty part":                  {here, false},
		"text":                           {"Hi" + here, false},
		"a closing body":                 {"<p>Hi</p>" + here + "</body>", false},
		"the last closing body":          {"</body>x" + here + "</body>y", false},
		"a closing html and no body":     {"<p>Hi</p>" + here + "</html>", false},
		"a closing body before an html":  {"<p>Hi</p>" + here + "</body></html>", false},
		"a link":                         {`<a href="https://x.test/u?token=1">u</a>` + here, true},
		"a link and a closing body":      {`<a href='https://x.test/u?token=1'>u</a>` + here + `</body>`, true},
		"a link with entities":           {`<a href="https://x.test/u?token&#61;1">u</a>` + here, true},
		"a link padded by control chars": {"<a href=\"\t https://x.test/u?token=1 \n\">u</a>" + here, true},
		"another link":                   {`<a href="https://x.test/other">u</a>` + here, false},
		"a link in a comment":            {`<!-- <a href="https://x.test/u?token=1"> -->` + here, false},
	} {
		source, at := unmarked(t, tc.marked)
		got, err := planHTML(source, url)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.insertAt != at || got.linksToUnsubscribe != tc.link {
			t.Errorf("%s: planHTML(%q) = %+v, want insertAt %d and link %v", name, source, got, at, tc.link)
		}
	}
}

// TestTheRawTextElementsAreTheTokenizersOwn: planHTML keeps its own list of the
// elements whose content the tokenizer reads as text, to know when the input
// ends inside one. A tokenizer that learned another would make the two disagree
// about what is a tag, so each name is checked against the tokenizer itself: a
// closing body tag after <name> is a tag if and only if the name is not on the
// list.
func TestTheRawTextElementsAreTheTokenizersOwn(t *testing.T) {
	for _, name := range []string{
		"a", "abbr", "address", "applet", "area", "article", "aside", "audio", "b", "base", "blockquote", "body", "br",
		"button", "canvas", "caption", "center", "code", "col", "colgroup", "data", "datalist", "dd", "details", "dialog",
		"div", "dl", "dt", "em", "embed", "fieldset", "figure", "font", "footer", "form", "frame", "h1", "head",
		"header", "hr", "html", "i", "iframe", "img", "input", "label", "legend", "li", "link", "listing", "main", "map", "mark",
		"menu", "meta", "nav", "noembed", "noframes", "noscript", "object", "ol", "optgroup", "option", "output", "p", "param",
		"picture", "plaintext", "pre", "script", "section", "select", "small", "source", "span", "strong", "style", "sub",
		"summary", "sup", "table", "tbody", "td", "template", "textarea", "tfoot", "th", "thead", "title", "tr", "track", "u",
		"ul", "video", "wbr", "xmp", "svg", "math", "marquee", "blink", "nobr", "isindex", "keygen", "bgsound", "image",
	} {
		// "<name></body>": if the tokenizer reads </body> as a tag, <name> did not
		// make what follows it text. Asked of the tokenizer itself and not of
		// planHTML, which has rules of its own about some of these names (what
		// follows <template> is not the document's).
		z := nethtml.NewTokenizer(strings.NewReader("<" + name + "></body>"))
		z.Next()
		tokenizerReadsATag := z.Next() == nethtml.EndTagToken
		if rawTextElements[name] == tokenizerReadsATag {
			t.Errorf("<%s>: the list says raw text = %v, the tokenizer says </body> is a tag = %v", name, rawTextElements[name], tokenizerReadsATag)
		}
	}
}

// BenchmarkCheckShown is what the tree check costs beside the tokenizer pass, for
// the sizes a layout makes (8, 33 and 134 kB): go test ./component/campaigns/
// -run XXX -bench CheckShown. Both run once per message. On the machine this was
// written on, the tree check takes about 0.07, 0.3 and 1.6 ms and the tokenizer
// pass 55 to 70 percent of that, against a round trip to the provider of tens of
// milliseconds, so the verdict is not cached per template revision (a cache would
// also have to show that what a recipient's name or address does to the text
// cannot change the tree).
func BenchmarkCheckShown(b *testing.B) {
	opts := RenderOptions{Links: messageTestLinks()}
	for _, blocks := range []int{10, 40, 160} {
		doc := layoutOfBlocks(blocks)
		plan, err := planHTML(doc, opts.Links.Unsubscribe)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("%dkB/planHTML", len(doc)/1024), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			for i := 0; i < b.N; i++ {
				if _, err := planHTML(doc, opts.Links.Unsubscribe); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("%dkB/checkShown", len(doc)/1024), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			for i := 0; i < b.N; i++ {
				if err := checkShown(doc, plan, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// layoutOfBlocks is a document shaped like a layout's output, n blocks long: a
// table per block, Outlook sections around its wrapper and its button.
func layoutOfBlocks(n int) string {
	var b strings.Builder
	b.WriteString(`<html xmlns:v="urn:schemas-microsoft-com:vml"><head><meta name="viewport" content="width=device-width">` +
		`<style>body{margin:0}@media (max-width:599px){.ecw{width:100%!important}}</style></head><body style="margin:0">`)
	for i := 0; i < n; i++ {
		b.WriteString(`<!--[if mso]><table align="center" width="600"><tr><td><![endif]-->`)
		b.WriteString(`<table align="center" width="600" class="ecw" style="width:600px"><tbody><tr><td style="padding:24px;font-family:Arial,sans-serif;font-size:16px;line-height:24px">`)
		b.WriteString(`<h2 style="margin:0 0 12px">Section heading</h2><p style="margin:0 0 12px">Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam &copy; &nbsp; quis nostrud.</p>`)
		b.WriteString(`<!--[if !mso]><!--><table><tbody><tr><td bgcolor="#111111"><a href="https://acme.test/shop" target="_blank" style="display:block;color:#fff">Shop now</a></td></tr></tbody></table><!--<![endif]-->`)
		b.WriteString(`<img src="https://cdn.acme.test/a.png" width="552" height="200" alt="banner" style="display:block">`)
		b.WriteString(`</td></tr></tbody></table><!--[if mso]></td></tr></table><![endif]-->`)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}
