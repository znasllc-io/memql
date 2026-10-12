package campaigns

import (
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

const (
	wantEndsOpen = "This template's HTML ends inside a comment or a tag, so its unsubscribe link cannot be placed. Fix the HTML and save it again."
	wantFrames   = "This template uses frames, which email cannot show."

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
// Not in the pieces, on purpose: <svg>, <math>, <template>, <select> and
// <frameset>. A footer left inside one of those when the template stops short is
// shown as something other than HTML (or not shown), and the tokenizer cannot
// see that; planHTML says so, and does not refuse it. (A frameset is refused, for
// a different reason; the test of where a refusal comes from adds it to its own
// alphabet.)
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
// (templatePieces) through both. A template is refused with one of the two
// sentences, or the message is the template with the fragment spliced in once,
// and a parser sees the unsubscribe link as one link in the body.
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
			if err.Error() != wantEndsOpen && err.Error() != wantFrames {
				t.Fatalf("template %q was refused with something that is not one of the two sentences: %v", template, err)
			}
			continue
		}
		placed++
		if strings.Count(out, fragment) != 1 || strings.Replace(out, fragment, "", 1) != template {
			t.Fatalf("template %q: taking the fragment out of %q does not give the template back", template, out)
		}
		if n := countOf(anchorHrefs(t, out), url); n != 1 {
			t.Fatalf("template %q was accepted, but a parser finds %d unsubscribe links in %q", template, n, out)
		}
		// The planner reads its own output as a template with a link and no fault.
		again, perr := planHTML(out, url)
		if perr != nil || !again.linksToUnsubscribe {
			t.Fatalf("template %q: the planner does not read %q as a template with its link (%+v, %v)", template, out, again, perr)
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
	var swallowing, refusedOpen, framed, checked int
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
	const url = "https://x.test/u?token=1"
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
		// "<name></body>" then a second, unmistakable closing tag: if </body> is a tag
		// the spot is before it. If <name> made it text, the spot is not there (the
		// input then ends inside the element, a refusal, or the end tag closes it).
		source := "<" + name + "></body>"
		plan, err := planHTML(source, url)
		tokenizerReadsATag := err == nil && plan.insertAt == len("<"+name+">")
		if rawTextElements[name] == tokenizerReadsATag {
			t.Errorf("<%s>: the list says raw text = %v, the tokenizer says </body> is a tag = %v", name, rawTextElements[name], tokenizerReadsATag)
		}
	}
}
