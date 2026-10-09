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
