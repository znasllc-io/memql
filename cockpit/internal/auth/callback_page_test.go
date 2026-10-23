package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteCallbackPage_Success(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallbackPage(rec, true, "You're signed in", "memQL Cockpit has been authorized to connect on your behalf.")

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"memQL Cockpit", "You&#39;re signed in", "badge ok", "<style>"} {
		if !strings.Contains(body, want) {
			t.Errorf("success page missing %q", want)
		}
	}
}

func TestWriteCallbackPage_ErrorEscapesMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallbackPage(rec, false, "Sign-in failed", `boom <script>alert(1)</script>`)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("error message was not HTML-escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("expected escaped message in the error page")
	}
	if !strings.Contains(body, "badge err") {
		t.Error("error page should use the error badge")
	}
}
