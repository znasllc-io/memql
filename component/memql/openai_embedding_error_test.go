package memql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the error-content invariant declared on OpenAIEmbeddingClient
// (memql#3186): no error returned by the embedding client may contain any byte
// of the upstream response body. The bound has to hold HERE, at the source,
// because these errors propagate unbroken into log sinks that log them verbatim
// (integrations/embedding wraps with %w, component/memql/executor_mutation logs
// the result), and the text submitted for embedding is user content.

// canaryUserContent stands in for the user text an embedding request carries.
// If the vendor ever starts echoing `input` back in an error body, this is the
// shape of what would reach the log.
const canaryUserContent = "PATIENT-NOTE-2291 my mother's maiden name is Rosalind and the door code is 8842"

// newTestEmbeddingClient returns a client pointed at srv.
func newTestEmbeddingClient(t *testing.T, srv *httptest.Server) *OpenAIEmbeddingClient {
	t.Helper()
	c := NewOpenAIEmbeddingClient("test-key", "text-embedding-3-small", 1536)
	c.baseURL = srv.URL
	c.httpClient = srv.Client()
	return c
}

// assertNoBodyLeak fails if any distinctive fragment of body shows up in msg.
func assertNoBodyLeak(t *testing.T, msg string, fragments ...string) {
	t.Helper()
	for _, frag := range fragments {
		if strings.Contains(msg, frag) {
			t.Fatalf("error leaked upstream response content %q\n  full error: %s", frag, msg)
		}
	}
}

// TestEmbedBatchNon200DoesNotLeakResponseBody is the acceptance-criterion pin:
// an httptest server returns a non-200 with a body, and the body must be absent
// from err.Error() while the status code survives.
func TestEmbedBatchNon200DoesNotLeakResponseBody(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantAbsent []string
		wantPresen []string
	}{
		{
			// The worst case the issue describes: the vendor echoes the
			// submitted input back inside its error envelope.
			name:   "vendor echoes submitted input in error.message",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"'$.input' is invalid: ` + canaryUserContent +
				`","type":"invalid_request_error","param":"input","code":"invalid_value"}}`,
			wantAbsent: []string{canaryUserContent, "PATIENT-NOTE-2291", "8842", "is invalid"},
			wantPresen: []string{"400", "invalid_request_error", "invalid_value"},
		},
		{
			// A well-formed envelope with no echo: classification survives,
			// the human-readable message still does not.
			name:       "well-formed envelope keeps type and code only",
			status:     http.StatusUnauthorized,
			body:       `{"error":{"message":"Incorrect API key provided: sk-proj-ABCDEF. You can find your API key at ...","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`,
			wantAbsent: []string{"sk-proj-ABCDEF", "Incorrect API key", "You can find"},
			wantPresen: []string{"401", "invalid_api_key"},
		},
		{
			// Not the vendor at all -- a proxy/WAF interstitial. Nothing about
			// it is an enum, so nothing at all survives but the status.
			name:       "non-JSON interstitial body is dropped entirely",
			status:     http.StatusBadGateway,
			body:       "<html><body>Request blocked. Payload: " + canaryUserContent + "</body></html>",
			wantAbsent: []string{canaryUserContent, "Request blocked", "<html>"},
			wantPresen: []string{"502"},
		},
		{
			// A misbehaving upstream stuffs content into a field we expected to
			// be an enum. The shape check rejects it.
			name:       "content smuggled through error.type is rejected",
			status:     http.StatusForbidden,
			body:       `{"error":{"type":"` + canaryUserContent + `","code":"rate_limit_exceeded"}}`,
			wantAbsent: []string{canaryUserContent, "Rosalind", "8842"},
			wantPresen: []string{"403", "rate_limit_exceeded"},
		},
		{
			// `"code": null` must not cost us the sibling `type` field.
			name:       "null code does not discard type",
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"message":"Rate limit reached for ` + canaryUserContent + `","type":"insufficient_quota","code":null}}`,
			wantAbsent: []string{canaryUserContent, "Rate limit reached"},
			wantPresen: []string{"429", "insufficient_quota"},
		},
		{
			// An unbounded body must not produce an unbounded error.
			name:       "huge body does not produce a huge error",
			status:     http.StatusInternalServerError,
			body:       strings.Repeat(canaryUserContent, 500),
			wantAbsent: []string{canaryUserContent},
			wantPresen: []string{"500"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestEmbeddingClient(t, srv)
			_, err := c.EmbedBatch(context.Background(), []string{canaryUserContent})
			if err == nil {
				t.Fatalf("expected an error for status %d, got nil", tc.status)
			}
			msg := err.Error()

			assertNoBodyLeak(t, msg, tc.wantAbsent...)
			for _, want := range tc.wantPresen {
				if !strings.Contains(msg, want) {
					t.Fatalf("expected error to retain %q, got: %s", want, msg)
				}
			}
			if len(msg) > 256 {
				t.Fatalf("error is not bounded (%d bytes): %s", len(msg), msg)
			}
		})
	}
}

// TestEmbedNon200DoesNotLeakResponseBody covers the single-text entry point --
// the one integrations/embedding actually calls -- so the bound is pinned on the
// path that reaches the executor_mutation log sinks, not only on EmbedBatch.
func TestEmbedNon200DoesNotLeakResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"input too long: ` + canaryUserContent + `","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}))
	defer srv.Close()

	c := newTestEmbeddingClient(t, srv)
	_, err := c.Embed(context.Background(), canaryUserContent)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	assertNoBodyLeak(t, err.Error(), canaryUserContent, "input too long")
	if !strings.Contains(err.Error(), "context_length_exceeded") {
		t.Fatalf("expected the vendor error code to survive, got: %s", err.Error())
	}
}

// TestEmbedBatchParseErrorDoesNotLeakResponseBody pins the sibling `parse
// embedding response` path. encoding/json errors render body-derived fragments
// (the offending byte for *json.SyntaxError, a value fragment for
// *json.UnmarshalTypeError), so that path was hardened alongside the status
// path rather than left on %w.
func TestEmbedBatchParseErrorDoesNotLeakResponseBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// Syntax error: %w would render `invalid character 'P' ...`.
			name: "malformed body",
			body: canaryUserContent,
		},
		{
			// Type error: %w would render the offending value fragment.
			name: "wrong type for data",
			body: `{"data":"` + canaryUserContent + `"}`,
		},
		{
			// A numeric literal is rendered verbatim by UnmarshalTypeError
			// ("number 99999999999999999999999999") when it overflows.
			name: "overflowing numeric literal",
			body: `{"data":[{"index":99999999999999999999999999,"embedding":[]}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestEmbeddingClient(t, srv)
			_, err := c.EmbedBatch(context.Background(), []string{canaryUserContent})
			if err == nil {
				t.Fatal("expected a parse error, got nil")
			}
			msg := err.Error()
			assertNoBodyLeak(t, msg, canaryUserContent, "Rosalind", "8842", "99999999999999999999999999")
			if !strings.Contains(msg, "parse embedding response") {
				t.Fatalf("expected the parse-stage prefix, got: %s", msg)
			}
		})
	}
}

// TestErrorClassificationTokenShape pins the enum shape check directly.
func TestErrorClassificationTokenShape(t *testing.T) {
	accepted := []any{"invalid_api_key", "rate_limit_exceeded", "server_error", "model.not-found", "429"}
	for _, v := range accepted {
		if _, ok := errorClassificationToken(v); !ok {
			t.Errorf("expected %v to be accepted as a classification token", v)
		}
	}

	rejected := []any{
		nil,                      // absent
		"",                       // empty
		float64(429),             // numeric code
		true,                     // nonsense
		map[string]any{"a": "b"}, // nonsense
		"has spaces in it",       // prose
		"quote\"injection",       // quoting
		"newline\ninjection",     // log-line injection
		strings.Repeat("a", 49),  // over budget
		canaryUserContent,        // smuggled user content
	}
	for _, v := range rejected {
		if tok, ok := errorClassificationToken(v); ok {
			t.Errorf("expected %#v to be rejected, got token %q", v, tok)
		}
	}
}
