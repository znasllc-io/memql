package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// A refused /authorize reached through MemQL OS (the native representation)
// must say WHY, not "Bad Request". The OS renders the `error` field verbatim,
// so before this an editor signing in with the wrong client_id met a bare
// status text in the browser and nothing anywhere named the redirect URI.

func nativeAuthorize(t *testing.T, mux *http.ServeMux, client, redirect string) *httptest.ResponseRecorder {
	t.Helper()
	return nativeGET(mux, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             client,
		"redirect_uri":          redirect,
		"state":                 "s",
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}))
}

func decodeNative(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not native JSON: %v (%s)", err, w.Body.String())
	}
	return body
}

func TestNativeAuthorizeRefusalCarriesHeadingAndMessage(t *testing.T) {
	_, mux := nativeTestServer(t)
	w := nativeAuthorize(t, mux, identity.BuiltinClientVSCode, "https://elsewhere.example.test/callback")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, NativeMediaType) {
		t.Fatalf("content type = %q, want the native media type", ct)
	}
	body := decodeNative(t, w)
	if body["heading"] != "Invalid redirect URI" {
		t.Fatalf("heading = %q", body["heading"])
	}
	if !strings.HasPrefix(body["error"], "Invalid redirect URI. ") || !strings.Contains(body["error"], "not registered") {
		t.Fatalf("error = %q, want the heading and the message, not a status text", body["error"])
	}
	if body["error"] == http.StatusText(http.StatusBadRequest) {
		t.Fatal("the refusal still reads as a bare status text")
	}
}

func TestNativeAuthorizeUnknownClientIsNamed(t *testing.T) {
	_, mux := nativeTestServer(t)
	w := nativeAuthorize(t, mux, "not-a-client", "http://127.0.0.1:54321/callback")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if body := decodeNative(t, w); body["heading"] != "Unknown client" {
		t.Fatalf("heading = %q, want Unknown client", body["heading"])
	}
}

func TestNativeAuthorizeAcceptsTheEditorClient(t *testing.T) {
	// The pre-validation the VS Code extension makes before opening a browser
	// depends on exactly this: the built-in editor client on a loopback
	// callback, any port, is NOT a refusal.
	_, mux := nativeTestServer(t)
	w := nativeAuthorize(t, mux, identity.BuiltinClientVSCode, "http://127.0.0.1:54321/callback")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("an accepted request carried an error: %s", w.Body.String())
	}
}

func TestRefusalHeadersNeverReachTheWire(t *testing.T) {
	_, mux := nativeTestServer(t)
	for _, w := range []*httptest.ResponseRecorder{
		nativeAuthorize(t, mux, "not-a-client", "http://127.0.0.1:1/callback"),
		func() *httptest.ResponseRecorder {
			// The plain browser path, which answers with the HTML page itself.
			r := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{"client_id": "not-a-client"}), nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			return rec
		}(),
	} {
		if w.Header().Get(refusalHeadingHeader) != "" || w.Header().Get(refusalMessageHeader) != "" {
			t.Fatalf("an internal refusal header leaked: %v", w.Header())
		}
	}
}

func TestNativeRefusalBodyShapes(t *testing.T) {
	cases := []struct {
		heading, message, want string
	}{
		{"Invalid redirect URI", "Not registered.", "Invalid redirect URI. Not registered."},
		{"Unknown client.", "Cannot continue.", "Unknown client. Cannot continue."},
		{"Only a heading", "", "Only a heading"},
		{"", "Only a message.", "Only a message."},
		{"", "", "Bad Request"},
	}
	for _, tc := range cases {
		if got := nativeRefusalBody(http.StatusBadRequest, tc.heading, tc.message)["error"]; got != tc.want {
			t.Errorf("nativeRefusalBody(%q, %q) = %q, want %q", tc.heading, tc.message, got, tc.want)
		}
	}
}

func TestAuthorizeRefusalIsLoggedWithTheClient(t *testing.T) {
	var buf bytes.Buffer
	s := newAuthorizeTestServer(t)
	s.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          "http://127.0.0.1:54321/cockpit/callback",
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)
	s.handleAuthorize(httptest.NewRecorder(), req)

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("no single log record: %v (%s)", err, buf.String())
	}
	if record["level"] != "INFO" || record["msg"] != "authorize refused" {
		t.Fatalf("record = %v", record)
	}
	if record["client_id"] != clientID || record["reason"] != "redirect_uri not registered for client_id" {
		t.Fatalf("record does not name the client and the reason: %v", record)
	}
}

func TestAuthorizeAcceptedRequestLogsNoRefusal(t *testing.T) {
	var buf bytes.Buffer
	s := newAuthorizeTestServer(t)
	s.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)
	s.handleAuthorize(httptest.NewRecorder(), req)
	if strings.Contains(buf.String(), "authorize refused") {
		t.Fatalf("an accepted request was logged as refused: %s", buf.String())
	}
}
