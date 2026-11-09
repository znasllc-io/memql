package web

// legal_test.go covers the two legal pages, /legal/tos and /legal/privacy.
// Nothing covered them until the placeholder documents were replaced
// (2026-11-09). What is pinned here, in the order a person meets it:
//
//   - a browser that navigates to a legal page on identity's own host is
//     sent to MemQL OS, which renders identity's pages (nativeUI);
//   - the OS asks for the native representation, whose Body is MARKDOWN
//     (the OS renders it with its own markdown component) beside the
//     version and effective date the front matter declares;
//   - the front-matter contract the embedded documents and an installation's
//     override files share (documentType / version / effectiveDate), which
//     loadLegalDoc reads;
//   - the override path replaces a document, and an unreadable override falls
//     back to the embedded default LOUDLY;
//   - the HTML template renders the markdown and escapes raw HTML, for the
//     paths that still serve identity's own HTML;
//   - identity's own sign-in template links to both pages.

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

var legalVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// newLegalServer mounts a Server with no store: the legal pages and the
// sign-in page read nothing, and CountUsers answering 1 keeps /login from
// redirecting to the first-owner wizard.
func newLegalServer(t *testing.T, cfg identity.Config, logger *slog.Logger) (*Server, http.Handler) {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://identity.example.test"
	}
	if logger == nil {
		logger = slog.Default()
	}
	srv, err := NewServer(cfg, logger, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.CountUsers = func(context.Context) (int, error) { return 1, nil }
	mux := http.NewServeMux()
	srv.Mount(mux)
	return srv, mux
}

// nativeGet is the request MemQL OS makes for an identity page: the native
// media type, from the OS origin identity derives from its own base URL.
func nativeGet(t *testing.T, srv *Server, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", NativeMediaType)
	req.Header.Set("Origin", srv.nativeOrigin(req))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func browserGet(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// htmlPage renders a page's HTML representation by calling the handler
// itself, below the nativeUI middleware that would send a browser to the OS.
func htmlPage(path string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func embeddedLegal(t *testing.T, path string) []byte {
	t.Helper()
	body, err := fs.ReadFile(FS, path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return body
}

type nativeLegalPage struct {
	Page string `json:"page"`
	Data struct {
		Layout        struct{ Title string }
		Body          string
		Version       string
		EffectiveDate string
	} `json:"data"`
}

func decodeNativeLegal(t *testing.T, rec *httptest.ResponseRecorder) nativeLegalPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != NativeMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, NativeMediaType)
	}
	var page nativeLegalPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	return page
}

// TestABrowserOnIdentityIsSentToTheOSLegalPage: the sign-in page a person
// sees is MemQL OS's, and a legal link opened on identity's own host lands
// on the OS page for the same path. The OS then asks for the native
// representation below.
func TestABrowserOnIdentityIsSentToTheOSLegalPage(t *testing.T) {
	_, mux := newLegalServer(t, identity.Config{}, nil)
	for _, path := range []string{"/legal/tos", "/legal/privacy"} {
		rec := browserGet(mux, path)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET %s status = %d, want 303 to the OS; body: %s", path, rec.Code, rec.Body.String())
		}
		if got, want := rec.Header().Get("Location"), "https://os.example.test/identity"+path; got != want {
			t.Errorf("GET %s Location = %q, want %q", path, got, want)
		}
	}
}

// TestLegalPagesNativeRepresentationCarriesTheMarkdown: MemQL OS renders
// Body with its own markdown component (clients/os/src/auth/IdentityScreen.tsx,
// the legal_view page) under Layout.Title, so Body must stay markdown with the
// front matter stripped, and the version and date must ride beside it.
func TestLegalPagesNativeRepresentationCarriesTheMarkdown(t *testing.T) {
	srv, mux := newLegalServer(t, identity.Config{}, nil)
	for _, tc := range []struct {
		path, title string
		doc         legalDocument
	}{
		{"/legal/tos", "Terms of Service", srv.tos},
		{"/legal/privacy", "Privacy Notice", srv.privacy},
	} {
		t.Run(tc.path, func(t *testing.T) {
			page := decodeNativeLegal(t, nativeGet(t, srv, mux, tc.path))
			if page.Page != "legal_view" {
				t.Errorf("page = %q, want legal_view", page.Page)
			}
			if page.Data.Layout.Title != tc.title {
				t.Errorf("Layout.Title = %q, want %q (the OS shows it above the document)", page.Data.Layout.Title, tc.title)
			}
			body := page.Data.Body
			if !strings.Contains(body, "## 1.") {
				t.Errorf("Body lost its markdown headings; the OS renders markdown, not HTML: %q", body[:min(len(body), 120)])
			}
			if strings.Contains(body, "<h2>") {
				t.Errorf("Body was rendered to HTML on the native path; the OS would escape it")
			}
			if strings.Contains(body, "documentType:") || strings.HasPrefix(body, "---") {
				t.Errorf("front matter leaked into the native Body")
			}
			if strings.HasPrefix(strings.TrimSpace(body), "# ") {
				t.Errorf("the document opens with its own title heading, which the OS and the HTML page both show already")
			}
			if page.Data.Version != tc.doc.Version || page.Data.EffectiveDate != tc.doc.EffectiveDate {
				t.Errorf("native version/effectiveDate = %q/%q, want %q/%q",
					page.Data.Version, page.Data.EffectiveDate, tc.doc.Version, tc.doc.EffectiveDate)
			}
		})
	}
}

// TestEmbeddedLegalDocumentsKeepTheFrontMatterContract pins the shape
// loadLegalDoc reads: a leading `---` block carrying documentType, a
// three-part version and an ISO date, followed by the document. The version
// is what a user sees change when the operator publishes a new document, so
// the embedded defaults must carry a real one, and a rewritten document must
// not keep the placeholder's.
func TestEmbeddedLegalDocumentsKeepTheFrontMatterContract(t *testing.T) {
	for _, tc := range []struct{ path, documentType string }{
		{"legal/tos.md", "tos"},
		{"legal/privacy.md", "privacy"},
	} {
		t.Run(tc.documentType, func(t *testing.T) {
			body := embeddedLegal(t, tc.path)
			if got := frontMatterValue(body, "documentType", ""); got != tc.documentType {
				t.Errorf("documentType = %q, want %q", got, tc.documentType)
			}
			doc := newLegalDocument(body)
			if !legalVersionRe.MatchString(doc.Version) {
				t.Errorf("version = %q, want MAJOR.MINOR.PATCH", doc.Version)
			}
			if doc.Version == "1.0.0" {
				t.Errorf("version is still the placeholder's 1.0.0; a rewritten document bumps it, " +
					"because the version is what tells a user the document changed")
			}
			if _, err := time.Parse("2006-01-02", doc.EffectiveDate); err != nil {
				t.Errorf("effectiveDate = %q, want YYYY-MM-DD: %v", doc.EffectiveDate, err)
			}
			text := stripFrontMatter(string(body))
			if strings.Contains(text, "documentType:") || strings.HasPrefix(text, "---") {
				t.Errorf("stripFrontMatter left front matter in the body: %q", text[:min(len(text), 80)])
			}
			for _, placeholder := range []string{"bundled with the identity service", "starting point", "OVERRIDE_PATH"} {
				if strings.Contains(text, placeholder) {
					t.Errorf("%s still carries placeholder text %q; operator instructions belong in "+
						"docs/public/operate/auth/legal-pages.md, not in what a user reads", tc.path, placeholder)
				}
			}
		})
	}
}

// TestEngineLegalDocumentsNameNoOperator: the engine default is
// operator-neutral and says "the operator" throughout. An installation names
// itself through the override files (docs/public/operate/auth/legal-pages.md);
// naming one operator here would make every other installation's legal pages
// claim to be theirs.
func TestEngineLegalDocumentsNameNoOperator(t *testing.T) {
	for _, path := range []string{"legal/tos.md", "legal/privacy.md"} {
		lower := strings.ToLower(string(embeddedLegal(t, path)))
		for _, name := range []string{"znas", "tucson"} {
			if strings.Contains(lower, name) {
				t.Errorf("%s names %q; the engine default refers to the operator, and an installation "+
					"names itself in its override file", path, name)
			}
		}
		if !strings.Contains(lower, "the operator") {
			t.Errorf("%s never says \"the operator\"", path)
		}
	}
}

// TestOverridePathReplacesTheEmbeddedDocument: an installation's file, with
// the same front matter, is what both representations serve, version and
// date included, and the other document stays the embedded default.
func TestOverridePathReplacesTheEmbeddedDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tos.md")
	override := "---\ndocumentType: tos\nversion: 9.9.9\neffectiveDate: 2031-01-02\n---\n\n" +
		"## 1. Who we are\n\nThe operator is Example Operator Inc.\n"
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, mux := newLegalServer(t, identity.Config{TosOverridePath: path}, nil)
	if srv.tos.Version != "9.9.9" || srv.tos.EffectiveDate != "2031-01-02" {
		t.Errorf("override front matter read as %q/%q, want 9.9.9/2031-01-02", srv.tos.Version, srv.tos.EffectiveDate)
	}
	page := decodeNativeLegal(t, nativeGet(t, srv, mux, "/legal/tos"))
	if page.Data.Version != "9.9.9" || !strings.Contains(page.Data.Body, "Example Operator Inc.") {
		t.Errorf("the native page serves version %q with body %q; want the override", page.Data.Version, page.Data.Body)
	}
	embedded := newLegalDocument(embeddedLegal(t, "legal/privacy.md"))
	if srv.privacy.Version != embedded.Version {
		t.Errorf("privacy version = %q, want the embedded %q; one override must not move the other document", srv.privacy.Version, embedded.Version)
	}
}

// TestAnUnreadableOverrideFallsBackToTheEmbeddedDocumentAndWarns: the
// fallback keeps an installation booting with legal pages, and the WARN is
// what tells an operator that their mount is wrong, because the page itself
// looks fine under their brand.
func TestAnUnreadableOverrideFallsBackToTheEmbeddedDocumentAndWarns(t *testing.T) {
	var log bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.md")
	srv, _ := newLegalServer(t, identity.Config{PrivacyOverridePath: missing}, slog.New(slog.NewTextHandler(&log, nil)))
	embedded := newLegalDocument(embeddedLegal(t, "legal/privacy.md"))
	if srv.privacy.Version != embedded.Version {
		t.Errorf("privacy version = %q, want the embedded %q", srv.privacy.Version, embedded.Version)
	}
	if !strings.Contains(log.String(), "legal override unreadable") || !strings.Contains(log.String(), missing) {
		t.Errorf("an unreadable override must be logged with its path; log:\n%s", log.String())
	}
}

// TestLegalHTMLRendersTheMarkdownAndEscapesRawHTML: the HTML representation
// used to inject the markdown source into a template styled for rendered
// HTML, so a heading reached the browser as a literal "## " line. It now
// renders, and raw HTML in a document (an override is operator-supplied, but
// the page is served before anyone signs in) does not pass through.
func TestLegalHTMLRendersTheMarkdownAndEscapesRawHTML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.md")
	override := "---\ndocumentType: privacy\nversion: 3.1.4\neffectiveDate: 2031-03-04\n---\n\n" +
		"## 1. What we keep\n\nThe operator keeps little.\n\n- your email address\n\n<script>alert(1)</script>\n"
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _ := newLegalServer(t, identity.Config{PrivacyOverridePath: path}, nil)
	for _, tc := range []struct {
		path, title string
		handler     http.HandlerFunc
		doc         legalDocument
	}{
		{"/legal/tos", "Terms of Service", srv.handleLegalTOS, srv.tos},
		{"/legal/privacy", "Privacy Notice", srv.handleLegalPrivacy, srv.privacy},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := htmlPage(tc.path, tc.handler)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			html := rec.Body.String()
			for _, want := range []string{
				"<h2>", "<li>", "<p>",
				">" + tc.title + "</h1>",
				"Version " + tc.doc.Version,
				", effective " + tc.doc.EffectiveDate,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("the page lacks %q", want)
				}
			}
			for _, leak := range []string{"\n## ", "\n- ", "documentType:", "effectiveDate:", "<script>alert(1)</script>"} {
				if strings.Contains(html, leak) {
					t.Errorf("the page carries %q: unrendered markdown, front matter or raw HTML passed through", leak)
				}
			}
		})
	}
}

// TestIdentitySignInTemplateLinksToBothLegalPages: identity's own sign-in
// template names both documents with the titles the pages carry. The sign-in
// page a person sees is MemQL OS's (clients/os/src/auth/SignInPage.tsx, covered
// by clients/os/test/auth/signIn.test.tsx), which links the same two paths.
func TestIdentitySignInTemplateLinksToBothLegalPages(t *testing.T) {
	srv, _ := newLegalServer(t, identity.Config{}, nil)
	rec := htmlPage("/login", srv.handleLoginGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	html := rec.Body.String()
	for _, want := range []string{`href="/legal/tos">Terms of Service</a>`, `href="/legal/privacy">Privacy Notice</a>`} {
		if !strings.Contains(html, want) {
			t.Errorf("the sign-in template lacks the link %s", want)
		}
	}
}
