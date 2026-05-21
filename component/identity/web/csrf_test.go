package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Round-trip the token in the response body so we can
		// assert on it from tests.
		_, _ = io.WriteString(w, CSRFTokenFromRequest(r))
	}
}

func mw(t *testing.T, opts CSRFOptions) http.Handler {
	t.Helper()
	return CSRFMiddleware(opts)(okHandler())
}

// ---------------------------------------------------------------------------
// GET path: cookie is minted on first request, reused on the next
// ---------------------------------------------------------------------------

func TestCSRF_GETMintsCookieAndStashesInContext(t *testing.T) {
	h := mw(t, CSRFOptions{})
	req := httptest.NewRequest(http.MethodGet, "/me/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if body == "" {
		t.Fatal("CSRF token was not stashed in the request context")
	}
	// Cookie must be set.
	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == CSRFCookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("expected memql_csrf cookie to be minted on first GET")
	}
	if found.Value != body {
		t.Errorf("cookie value %q != context token %q", found.Value, body)
	}
	if !found.HttpOnly {
		t.Error("memql_csrf cookie must be HttpOnly")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", found.SameSite)
	}
}

func TestCSRF_GETReusesExistingCookie(t *testing.T) {
	h := mw(t, CSRFOptions{})
	req := httptest.NewRequest(http.MethodGet, "/me/", nil)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "existing-token-abc"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "existing-token-abc" {
		t.Errorf("expected existing cookie to be reused; got token = %q", rec.Body.String())
	}
	// No new cookie should be set when one already exists.
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("expected no new cookie when one already exists; got %d", len(rec.Result().Cookies()))
	}
}

// ---------------------------------------------------------------------------
// POST path: token presence + match
// ---------------------------------------------------------------------------

func TestCSRF_POSTWithMatchingFormFieldAdmits(t *testing.T) {
	h := mw(t, CSRFOptions{})
	form := url.Values{}
	form.Set(CSRFFormField, "tok-123")
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "tok-123"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("matching token should admit; got status %d, body=%q", rec.Code, rec.Body.String())
	}
}

func TestCSRF_POSTWithMatchingHeaderAdmits(t *testing.T) {
	h := mw(t, CSRFOptions{})
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", nil)
	req.Header.Set(CSRFHeader, "tok-xyz")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "tok-xyz"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("matching X-CSRF-Token header should admit; got %d", rec.Code)
	}
}

func TestCSRF_POSTRejectsMissingCookie(t *testing.T) {
	h := mw(t, CSRFOptions{})
	// No cookie at all -- middleware mints one (the "fresh" case)
	// and MUST reject because the POST can't possibly carry a
	// matching token.
	form := url.Values{}
	form.Set(CSRFFormField, "anything")
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with no prior cookie must be rejected; got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CSRF") {
		t.Errorf("rejection body should mention CSRF; got %q", rec.Body.String())
	}
}

func TestCSRF_POSTRejectsMissingFormField(t *testing.T) {
	h := mw(t, CSRFOptions{})
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", nil)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "tok-123"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with cookie but no form/header token must be rejected; got %d", rec.Code)
	}
}

func TestCSRF_POSTRejectsMismatch(t *testing.T) {
	h := mw(t, CSRFOptions{})
	form := url.Values{}
	form.Set(CSRFFormField, "wrong-value")
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "right-value"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched token must be rejected; got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Exempt paths
// ---------------------------------------------------------------------------

func TestCSRF_ExemptPathBypassesGate(t *testing.T) {
	h := mw(t, CSRFOptions{ExemptPaths: []string{"/login"}})
	// No cookie, no token -- normally rejected. Exempt path admits.
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("exempt path POST must admit without token; got %d", rec.Code)
	}
	// Cookie should STILL be minted on the response so subsequent
	// non-exempt requests from the same browser have one.
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == CSRFCookieName {
			found = true
			break
		}
	}
	if !found {
		t.Error("exempt-path response should still mint a CSRF cookie for future requests")
	}
}

func TestCSRF_NonExemptPathStillGated(t *testing.T) {
	h := mw(t, CSRFOptions{ExemptPaths: []string{"/login"}})
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-exempt POST must still be gated; got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// HTTP method coverage
// ---------------------------------------------------------------------------

func TestCSRF_UnsafeMethodsAllGated(t *testing.T) {
	h := mw(t, CSRFOptions{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/me/x", nil)
			req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "tok"})
			// no token in form or header
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s should be gated; got %d", method, rec.Code)
			}
		})
	}
}

func TestCSRF_SafeMethodsBypassGate(t *testing.T) {
	h := mw(t, CSRFOptions{})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/me/x", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s should admit without token; got %d", method, rec.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Header beats form
// ---------------------------------------------------------------------------

func TestCSRF_HeaderTakesPrecedenceOverForm(t *testing.T) {
	h := mw(t, CSRFOptions{})
	form := url.Values{}
	form.Set(CSRFFormField, "wrong-form-value")
	req := httptest.NewRequest(http.MethodPost, "/me/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(CSRFHeader, "right-header-value")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "right-header-value"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("header should take precedence over form; got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Secure flag derived from option
// ---------------------------------------------------------------------------

func TestCSRF_SecureFlagHonored(t *testing.T) {
	secure := true
	h := mw(t, CSRFOptions{Secure: &secure})
	req := httptest.NewRequest(http.MethodGet, "/me/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].Secure {
		t.Errorf("Secure=true option should set Secure on cookie; got %+v", cookies)
	}
}
