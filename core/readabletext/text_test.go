package readabletext

import (
	"strings"
	"testing"
)

func TestArticleProseSurvivesNavigationAndKeepsSources(t *testing.T) {
	body := `<html><head><script>private script</script></head><body><header>site masthead</header><nav>` + strings.Repeat("menu item ", 10000) + `</nav><main><article><header><h1>Water research</h1><p>Author name</p></header><p>Removal <strong>efficiency</strong> &ge; 90&#37;.</p><p>Evidence: <a href="https://example.org/paper?a=1&amp;b=2">the study</a>.</p><footer>Funding and limitations</footer><div hidden>hidden text</div><span aria-hidden="true">hidden icon</span></article></main><footer>site footer</footer></body></html>`
	got := Extract(body, "text/html; charset=utf-8")
	for _, want := range []string{"Water research", "Author name", "Removal efficiency ≥ 90%.", "the study (https://example.org/paper?a=1&b=2)", "Funding and limitations"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, unwanted := range []string{"menu item", "masthead", "site footer", "hidden", "script"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, got)
		}
	}
	if len(got) > 300 {
		t.Fatalf("navigation consumed the excerpt: %d bytes", len(got))
	}
}

func TestStructureWhitespaceAndMalformedHTML(t *testing.T) {
	got := Extract("<main>\n  <h1>Results</h1>\n\n  <p>First <em>word</em>, next.</p><p>Second.</p><table><tr><th>Dose<th>Removal<tr><td>5<td>90%</table><pre>x = 1\n  y = 2</pre><p><a href='/paper'>Source</a> <a href='javascript:bad()'>Bad link</a></main>", "text/html")
	for _, want := range []string{"Results\nFirst word, next.\nSecond.", "Dose | Removal |", "5 | 90% |", "x = 1\n  y = 2", "Source (/paper)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "javascript:") || strings.Contains(got, "\n\n\n") {
		t.Fatalf("unexpected markup or layout whitespace: %q", got)
	}
}

func TestArticleHeaderMenusDoNotConsumeResearchExcerpt(t *testing.T) {
	// The live MediaWiki layout nests its language selector in main > header;
	// dropping only nav or the site's outer header leaves hundreds of links.
	body := `<main><header><h1>Article title</h1><div class="vector-dropdown mw-portlet mw-portlet-lang"><ul>` +
		strings.Repeat(`<li><a href="https://example.org/translated">Language menu</a></li>`, 1000) +
		`</ul></div><p>Author byline</p></header><div class="vector-menu mw-portlet mw-portlet-views">Edit page</div>` +
		`<div role="toolbar">Page tools</div><div role="menu">Account menu</div><div role="menubar">Site menu</div>` +
		`<div class="mw-body-content"><p>Article evidence <a href="https://example.org/study">primary source</a>.</p>` +
		`<ul class="references"><li>Reference <a hreflang="fr" href="https://example.org/french-study">French study</a></li></ul>` +
		`<p class="about-mw-portlet">A discussion of menu design.</p></div></main>`
	got := Extract(body, "text/html")
	for _, want := range []string{"Article title", "Author byline", "Article evidence primary source (https://example.org/study)", "French study (https://example.org/french-study)", "A discussion of menu design."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, unwanted := range []string{"Language menu", "Edit page", "Page tools", "Account menu", "Site menu"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in research excerpt", unwanted)
		}
	}
	if len(got) > 300 {
		t.Fatalf("menus consumed the excerpt: %d bytes", len(got))
	}
}

func TestAllSearchResultsAndUnmarkedPagesRemainReadable(t *testing.T) {
	for _, body := range []string{
		`<article>One <a href="/one">source</a></article><article>Two <a href="/two">source</a></article>`,
		`<body><p>One <a href="/one">source</a></p><p>Two <a href="/two">source</a></p></body>`,
	} {
		got := Extract(body, "text/html")
		if !strings.Contains(got, "One source (/one)") || !strings.Contains(got, "Two source (/two)") {
			t.Fatalf("discarded search evidence: %q", got)
		}
	}
}

func TestNonHTMLIsNotReinterpretedAsMarkup(t *testing.T) {
	for _, typ := range []string{"application/json", "text/plain", "application/xml"} {
		body := " {\"comparison\":\"x < 3 && y > 1\",\"code\":\"<main>\"} "
		if got := Extract(body, typ); got != strings.TrimSpace(body) {
			t.Fatalf("%s changed structured evidence: %q", typ, got)
		}
	}
}
