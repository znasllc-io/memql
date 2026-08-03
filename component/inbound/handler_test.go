package inbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
)

// fakeEngine records what the handler executed instead of running it, so the
// staging call can be inspected without a database.
type fakeEngine struct {
	calls []string
	err   error
}

func (f *fakeEngine) Execute(_ context.Context, q string) (any, error) {
	f.calls = append(f.calls, q)
	return nil, f.err
}

func testHandler(t *testing.T, eng Engine, src SourceConfig) *Handler {
	t.Helper()
	src.Name = "acme"
	h := NewHandler(Config{
		Enabled:      true,
		MaxBodyBytes: 1024,
		Tolerance:    5 * time.Minute,
		Sources:      map[string]SourceConfig{"acme": src},
	}, eng, quietLogger())
	h.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return h
}

func signedRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(body))))
	return r
}

func hexSource() SourceConfig {
	return SourceConfig{Scheme: SchemeHMACSHA256Hex, Secret: testSecret, SignatureHeader: "X-Sig"}
}

// The happy path: a well-signed request is staged and acknowledged.
func TestHandlerStagesAVerifiedRequest(t *testing.T) {
	eng := &fakeEngine{}
	rec := httptest.NewRecorder()
	testHandler(t, eng, hexSource()).ServeHTTP(rec, signedRequest(t, `{"event":"order.created"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("a well-signed request was answered %d, want 202: %s", rec.Code, rec.Body)
	}
	if len(eng.calls) != 1 {
		t.Fatalf("expected exactly one staging call, got %d: %v", len(eng.calls), eng.calls)
	}
	call := eng.calls[0]
	for _, want := range []string{
		"mutation stageInboundRequest(",
		`source: "acme"`,
		`medium: "webhook"`,
		"signatureVerified: true",
		`{\"event\":\"order.created\"}`,
	} {
		if !strings.Contains(call, want) {
			t.Errorf("staging call is missing %q:\n%s", want, call)
		}
	}
}

// Every path that must NOT reach the graph. A single table, because the
// property under test is the same for all of them: nothing is staged.
func TestHandlerRefusesWithoutStaging(t *testing.T) {
	body := `{"a":1}`
	goodSig := hex.EncodeToString(sign(testSecret, []byte(body)))

	for _, tc := range []struct {
		name     string
		build    func(t *testing.T) *http.Request
		src      SourceConfig
		wantCode int
	}{
		{
			name: "unknown source",
			build: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/inbound/nope", strings.NewReader(body))
			},
			src:      hexSource(),
			wantCode: http.StatusNotFound,
		},
		{
			// A nested path must not be read as its parent's source, or
			// /inbound/acme/../../x becomes acme's endpoint.
			name: "nested path below a known source",
			build: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/inbound/acme/extra", strings.NewReader(body))
			},
			src:      hexSource(),
			wantCode: http.StatusNotFound,
		},
		{
			name: "bare prefix with no source",
			build: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/inbound/", strings.NewReader(body))
			},
			src:      hexSource(),
			wantCode: http.StatusNotFound,
		},
		{
			name:     "GET instead of POST",
			build:    func(t *testing.T) *http.Request { return httptest.NewRequest(http.MethodGet, "/inbound/acme", nil) },
			src:      hexSource(),
			wantCode: http.StatusMethodNotAllowed,
		},
		{
			name: "bad signature",
			build: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(body))
				r.Header.Set("X-Sig", hex.EncodeToString(sign("wrong", []byte(body))))
				return r
			},
			src:      hexSource(),
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "no signature at all",
			build: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(body))
			},
			src:      hexSource(),
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "body over the cap",
			build: func(t *testing.T) *http.Request {
				big := strings.Repeat("x", 2048)
				r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(big))
				r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(big))))
				return r
			},
			src:      hexSource(),
			wantCode: http.StatusRequestEntityTooLarge,
		},
		{
			name: "body is not valid UTF-8",
			build: func(t *testing.T) *http.Request {
				raw := string([]byte{0xff, 0xfe, 0x00})
				r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(raw))
				r.Header.Set("X-Sig", hex.EncodeToString(sign(testSecret, []byte(raw))))
				return r
			},
			src:      hexSource(),
			wantCode: http.StatusBadRequest,
		},
		{
			name: "dedupe header carries a control character",
			build: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(body))
				r.Header.Set("X-Sig", goodSig)
				// Set directly: http.Header.Set would not stop this, and the
				// point is that the HANDLER does.
				r.Header["X-Dedupe"] = []string{"a\x00b"}
				return r
			},
			src: SourceConfig{
				Scheme: SchemeHMACSHA256Hex, Secret: testSecret,
				SignatureHeader: "X-Sig", DedupeHeader: "X-Dedupe",
			},
			wantCode: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{}
			rec := httptest.NewRecorder()
			testHandler(t, eng, tc.src).ServeHTTP(rec, tc.build(t))
			if rec.Code != tc.wantCode {
				t.Errorf("answered %d, want %d: %s", rec.Code, tc.wantCode, rec.Body)
			}
			if len(eng.calls) != 0 {
				t.Errorf("a refused request still reached the graph -- that is the whole failure "+
					"this endpoint exists to prevent:\n%v", eng.calls)
			}
		})
	}
}

// The kill switch takes the receiver out entirely, even for a source that is
// otherwise perfectly configured.
func TestHandlerKillSwitchRefusesEverything(t *testing.T) {
	eng := &fakeEngine{}
	h := testHandler(t, eng, hexSource())
	h.cfg.Enabled = false
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(t, `{"a":1}`))
	if rec.Code != http.StatusNotFound || len(eng.calls) != 0 {
		t.Errorf("MEMQL_INBOUND_ENABLED=false must refuse: code=%d calls=%v", rec.Code, eng.calls)
	}
}

// A staging failure is answered 5xx, because every sender in the story's list
// retries on 5xx and the delivery itself was valid. Answering 2xx would drop
// the event silently.
func TestHandlerAnswers5xxWhenStagingFails(t *testing.T) {
	eng := &fakeEngine{err: context.DeadlineExceeded}
	rec := httptest.NewRecorder()
	testHandler(t, eng, hexSource()).ServeHTTP(rec, signedRequest(t, `{"a":1}`))
	if rec.Code < 500 {
		t.Errorf("a staging failure was answered %d; the sender would treat that as delivered "+
			"and never retry, so the event is lost", rec.Code)
	}
}

// Idempotency is structural: the id is derived from (source, dedupeKey), so a
// redelivery renders the same id and @createOnly protects the product's
// handling state. If the id ever became time- or nonce-derived, a redelivery
// would stage a second row and the product would process the event twice.
func TestRedeliveryRendersTheSameRowId(t *testing.T) {
	eng := &fakeEngine{}
	h := testHandler(t, eng, hexSource())
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), signedRequest(t, `{"a":1}`))
	}
	if len(eng.calls) != 2 {
		t.Fatalf("expected two staging calls, got %d", len(eng.calls))
	}
	id := func(call string) string {
		const k = "requestId: "
		rest := call[strings.Index(call, k)+len(k):]
		return rest[:strings.Index(rest, ",")]
	}
	if a, b := id(eng.calls[0]), id(eng.calls[1]); a != b {
		t.Errorf("a redelivery of the same event rendered a different row id (%s vs %s), so it "+
			"would be staged twice and processed twice", a, b)
	}
}

// Two DIFFERENT events must not collapse onto one row -- the memql#2980
// injectivity class. The separator is what carries this, so the cases are
// chosen to attack it.
func TestRequestIdCompositionIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, pair := range [][2]string{
		{"acme", "x"},
		{"acme", "y"},
		{"acme-x", ""},
		{"acme", "\x00x"}, // unreachable through the handler; pinned anyway
		{"a", "cmex"},
		{"ac", "mex"},
	} {
		id := requestIDFor(pair[0], pair[1])
		key := pair[0] + "|" + pair[1]
		if prev, dup := seen[id]; dup {
			t.Errorf("(%s) and (%s) collapse onto the same row id %s -- two distinct events would "+
				"share one row", prev, key, id)
		}
		seen[id] = key
	}
}

// The dedupe key falls back to the body digest, so a sender with no idempotency
// header still gets redelivery collapse for a byte-identical retry -- and does
// NOT get it for a different event.
func TestDedupeKeyFallsBackToTheBodyDigest(t *testing.T) {
	src := hexSource()
	body := []byte(`{"a":1}`)
	got, err := dedupeKeyFor(src, http.Header{}, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sum := sha256.Sum256(body)
	if got != hex.EncodeToString(sum[:]) {
		t.Errorf("dedupe key is not the body digest: %s", got)
	}
	other, _ := dedupeKeyFor(src, http.Header{}, []byte(`{"a":2}`))
	if other == got {
		t.Error("two different bodies produced the same dedupe key, so distinct events would " +
			"collapse onto one row")
	}
}

// TestRenderedLiteralsParseBackThroughTheRealLexer is the reason memqlString
// is not fmt %q.
//
// The staged body is arbitrary third-party text pasted into a MemQL source
// string, and the MemQL lexer implements only the JSON escape set
// (" \ / b f n r t u) -- it REJECTS anything else outright. Go's %q emits
// \x00, \a and \v, so an ordinary payload carrying a control character would
// have failed to parse at the engine, after the sender had already been
// told 202. This drives the real lexer over each rendered literal and checks
// the value survives the round trip.
func TestRenderedLiteralsParseBackThroughTheRealLexer(t *testing.T) {
	for _, in := range []string{
		`{"event":"order.created"}`,
		"quotes \" and backslashes \\ and both \\\"",
		"newline\ntab\treturn\r",
		"control \x00\x01\x07\x0b bytes",
		"unicode: caf\u00e9 \u2014 \U0001F600",
		"html-ish <b>&</b>",
		"trailing backslash \\",
		"",
	} {
		lex := parser.NewLexer(memqlString(in))
		tok, err := lex.NextToken()
		if err != nil {
			t.Errorf("the MemQL lexer refused a rendered literal, so a payload like this would "+
				"be accepted at the edge and then fail to stage.\n  input:   %q\n  rendered: %s\n  error: %v",
				in, memqlString(in), err)
			continue
		}
		if tok.Literal != in {
			t.Errorf("literal did not round-trip.\n  input:  %q\n  parsed: %q", in, tok.Literal)
		}
	}
}

// TestHandlerDedupeFailureLeaksNothingToTheCaller pins the invariant this
// feature's security notes state and that every OTHER arm of ServeHTTP already
// kept: the caller learns nothing.
//
// The dedupe arm was the one exception -- it answered `http.Error(w,
// err.Error(), ...)`, echoing handler-internal error text to a caller that is
// unauthenticated by construction. CodeQL did not flag it; the author found it
// while handing the PR off (memql#2957), and it is fixed on the branch.
//
// Failing-first: restore `err.Error()` in the dedupe arm and the exact-match
// assertion fails, because the internal text names the header and its limit.
func TestHandlerDedupeFailureLeaksNothingToTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"over the length limit", strings.Repeat("a", 300)},
		{"contains a control character", "abc\x01def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := hexSource()
			src.DedupeHeader = "X-Dedupe"

			body := `{"event":"order.created"}`
			r := signedRequest(t, body)
			r.Header.Set("X-Dedupe", tc.value)

			eng := &fakeEngine{}
			rec := httptest.NewRecorder()
			testHandler(t, eng, src).ServeHTTP(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", rec.Code, rec.Body)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != "invalid dedupe header" {
				t.Errorf("the 400 body must be a fixed string that tells the caller nothing.\n"+
					"  got:  %q\n  want: %q\n"+
					"Echoing err.Error() here hands an unauthenticated caller the handler's "+
					"internal diagnostics, which is what the 401 arm above deliberately avoids.",
					got, "invalid dedupe header")
			}
			// The header VALUE must never come back either, whatever the wording.
			if strings.Contains(rec.Body.String(), tc.value) {
				t.Errorf("the response echoed the caller's own header value back:\n%s", rec.Body.String())
			}
			if len(eng.calls) != 0 {
				t.Errorf("a refused request must not stage a row, got %d calls", len(eng.calls))
			}
		})
	}
}
