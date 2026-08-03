package inbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
)

// RoutePrefix is the path the receiver mounts under. A request is
// "<RoutePrefix><source>" and nothing deeper -- see splitSource.
const RoutePrefix = "/inbound/"

// Engine is the narrow slice of the MemQL engine the receiver needs. Declared
// as an interface (outbound.Engine precedent) so tests drive the handler
// without a database.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Handler is the HTTP receiver. It performs its own authorization -- the
// source allowlist plus the per-source signature check -- and fails closed on
// every path, which is why the route is declared in
// server.HandlerAuthorizedPaths() rather than server.PublicPaths(): the latter
// would make it unauthenticated on every verifier-consuming node, and this
// endpoint is unauthenticated only in the sense that the caller holds no memQL
// credential. It holds a shared secret instead.
type Handler struct {
	cfg    Config
	engine Engine
	logger *slog.Logger
	now    func() time.Time
}

// NewHandler builds the receiver. A nil logger is tolerated (discarding).
func NewHandler(cfg Config, engine Engine, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{cfg: cfg, engine: engine, logger: logger, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Enabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, ok := splitSource(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	src, known := h.cfg.Sources[name]
	if !known {
		// 404 rather than 403: an unauthenticated caller learns nothing about
		// which sources this deployment has configured. An unlisted source and
		// a misconfigured one are indistinguishable from outside, which is the
		// intent -- LoadConfig already told the operator which is which.
		http.NotFound(w, r)
		return
	}

	body, err := h.readBody(r)
	if err != nil {
		if err == errBodyTooLarge {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	// The body is stored in a string field and rendered into a MemQL string
	// literal, so it has to be text. Refusing here is honest; json.Marshal
	// would silently rewrite every invalid byte to U+FFFD and stage a row whose
	// body no longer matches what was signed.
	if !utf8.Valid(body) {
		http.Error(w, "request body is not valid UTF-8", http.StatusBadRequest)
		return
	}

	verified, err := verify(src, r.Header, body, h.now().UTC(), h.cfg.Tolerance)
	if err != nil {
		// Logged with the reason, answered without it.
		h.logger.Warn("inbound receiver: request refused", "source", name, "error", err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	dedupeKey, err := dedupeKeyFor(src, r.Header, body)
	if err != nil {
		h.logger.Warn("inbound receiver: request refused", "source", name, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	received := h.now().UTC()
	requestID := requestIDFor(name, dedupeKey)
	mutation := fmt.Sprintf(
		`mutation stageInboundRequest(requestId: %s, source: %s, medium: "webhook", body: %s, `+
			`contentType: %s, dedupeKey: %s, signatureVerified: %t, receivedAt: %s)`,
		memqlString(requestID), memqlString(name), memqlString(string(body)),
		memqlString(r.Header.Get("Content-Type")), memqlString(dedupeKey), verified,
		memqlString(received.Format(time.RFC3339)))

	if _, err := h.engine.Execute(systemActorContext(r.Context()), mutation); err != nil {
		// The delivery was valid; we simply could not record it. 503 is the
		// honest answer -- every sender in the story's list retries on 5xx, and
		// the staging mutation is idempotent by requestId, so the retry lands
		// on the same row rather than duplicating the event.
		h.logger.Error("inbound receiver: staging failed", "source", name, "error", err)
		http.Error(w, "could not stage request", http.StatusServiceUnavailable)
		return
	}

	h.logger.Info("inbound receiver: staged", "source", name, "id", requestID, "verified", verified)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": requestID, "status": "received"})
}

var errBodyTooLarge = fmt.Errorf("body exceeds the configured cap")

// readBody reads at most MaxBodyBytes, and one byte more so an over-cap body is
// DETECTED rather than silently truncated -- a truncated body would fail its
// own signature check and look like an attack.
func (h *Handler) readBody(r *http.Request) ([]byte, error) {
	limit := int64(h.cfg.MaxBodyBytes)
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// splitSource extracts the source name from the request path and rejects
// anything deeper. "/inbound/shopify" yields "shopify"; "/inbound/shopify/x"
// and "/inbound/" yield nothing, so a nested path can never be read as its
// parent's source.
func splitSource(path string) (string, bool) {
	idx := strings.Index(path, RoutePrefix)
	if idx < 0 {
		return "", false
	}
	rest := path[idx+len(RoutePrefix):]
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// dedupeKeyFor resolves the idempotency key: the sender's own when it supplies
// one, otherwise the SHA-256 of the body so a byte-identical redelivery
// collapses onto the same row.
//
// A sender-supplied key is validated, because it is the one caller-controlled
// half of the composite request id (see requestIDFor).
func dedupeKeyFor(src SourceConfig, header http.Header, body []byte) (string, error) {
	if src.DedupeHeader == "" {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	key := strings.TrimSpace(header.Get(src.DedupeHeader))
	if key == "" {
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:]), nil
	}
	if len(key) > 256 {
		return "", fmt.Errorf("dedupe header %q is longer than 256 bytes", src.DedupeHeader)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("dedupe header %q contains a control character", src.DedupeHeader)
		}
	}
	return key, nil
}

// requestIDFor derives the row id from (source, dedupeKey), which is what makes
// staging idempotent without a read-then-write race: a redelivery renders the
// same id and @createOnly on the mutation preserves the product's handling
// state.
//
// The composition must be INJECTIVE or two distinct events collapse onto one
// row -- the memql#2980 class. It is, because the parts cannot forge the
// separator: a source name is drawn from the allowlist and matched against
// sourceNamePattern (lowercase alnum, '_' and '-' only), and a dedupe key is
// either a hex digest we produced or a header value dedupeKeyFor has already
// rejected control characters in. NUL therefore appears in neither part, so the
// split at the first NUL is unique and equal concatenations force equal parts.
func requestIDFor(source, dedupeKey string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + dedupeKey))
	return "inb" + hex.EncodeToString(sum[:16])
}

// memqlString renders a Go string as a MemQL string literal.
//
// NOT fmt %q. Go's quoting emits \x00, \a and \v escapes, and the MemQL lexer
// accepts only the JSON set (" \ / b f n r t u) -- it rejects the rest outright
// with "invalid escape character". A webhook body is arbitrary third-party
// text, so %q would refuse perfectly ordinary payloads at the parser. JSON
// encoding emits exactly the escapes the lexer implements.
func memqlString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal only fails on unsupported types; a string is not one.
		return `""`
	}
	return string(b)
}

// systemInboundActor is the actor the staging mutation runs as. The request
// carries no memQL identity -- a third party signed it with a shared secret,
// which authorizes the DELIVERY and says nothing about a user -- so the write
// runs as a named system actor and is auditable as one (outbound worker
// precedent).
const systemInboundActor = "system:inbound"

func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemInboundActor,
		Claims: map[string]any{
			"sub":  systemInboundActor,
			"role": "system",
		},
	})
}
