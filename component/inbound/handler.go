package inbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
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
// endpoint is unauthenticated only in the sense that the caller holds no MemQL
// credential. It holds a shared secret instead.
type Handler struct {
	cfg    Config
	engine Engine
	logger *slog.Logger
	now    func() time.Time
	// registered answers a source this cluster REGISTERED FOR ITSELF at
	// runtime -- see SetRegisteredSource. Nil on a node that wired none, which
	// leaves resolution exactly as it was.
	registered RegisteredSource
	// largeBodies holds one token for each request holding a body over the
	// receiver's default cap, and largeBodyWait is how long such a request
	// waits for a free one before it is answered 503. largeBodyGrace,
	// largeBodyRate (bytes per second) and largeBodyHold pace the read a turn
	// covers. See readBody and pacedBody.
	largeBodies                   chan struct{}
	largeBodyWait                 time.Duration
	largeBodyGrace, largeBodyHold time.Duration
	largeBodyRate                 float64
}

// RegisteredSource resolves one source name to its policy, or (_, false) when
// the name is not one it answers for.
//
// It is the tier between the environment and the connectors, for a sender that
// is neither: not pinned by an operator at deploy time, and not a connector's
// tenant, but something the cluster itself set up while running and holds the
// secret for. The cluster's GitHub App, when a cluster owner registers it from
// the product, is the case it exists for -- GitHub generates the webhook secret
// during that registration, so no environment could have carried it.
//
// A function rather than an import, because this module sits below whatever
// knows the answer (component/identity, for GitHub) and must not reach up for
// it; app/ wires the two together (SetRegisteredSource).
type RegisteredSource func(ctx context.Context, name string) (SourceConfig, bool)

// SetRegisteredSource installs the runtime tier. Called once, from app/, before
// the route serves; a second call replaces the first, and nil removes it.
func (h *Handler) SetRegisteredSource(resolve RegisteredSource) {
	if h == nil {
		return
	}
	h.registered = resolve
}

// NewHandler builds the receiver. A nil logger is tolerated (discarding).
func NewHandler(cfg Config, engine Engine, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{cfg: cfg, engine: engine, logger: logger, now: time.Now,
		largeBodies: make(chan struct{}, largeBodyReads), largeBodyWait: defaultLargeBodyWait,
		largeBodyGrace: defaultLargeBodyGrace, largeBodyRate: defaultLargeBodyRate, largeBodyHold: defaultLargeBodyHold}
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
	src, known := h.resolveSource(r.Context(), name)
	if !known {
		// 404 rather than 403: an unauthenticated caller learns nothing about
		// which sources this deployment has configured. An unlisted source and
		// a misconfigured one are indistinguishable from outside, which is the
		// intent -- LoadConfig already told the operator which is which.
		http.NotFound(w, r)
		return
	}

	// A source that signs is refused without its signature header BEFORE its
	// body is read. Such a request cannot pass verify whatever its body says,
	// so reading it -- and, for a body over the default cap, giving it one of
	// the few large-body turns -- would only let an unsigned crowd spend them.
	// The same 401 verify answers for an absent signature.
	if src.Scheme != SchemeNone && strings.TrimSpace(r.Header.Get(src.SignatureHeader)) == "" {
		h.logger.Warn("inbound receiver: request refused", "source", name,
			"error", "the signature header "+src.SignatureHeader+" is absent")
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	body, release, err := h.readBody(w, r, src)
	switch {
	case err == errBodyTooLarge:
		// LOGGED, because nothing else says it. The 413 goes back to the
		// sender and stages no row, so before this line the only trace of a
		// refused delivery was the sender's own delivery history: a GitHub push
		// over the cap opened no run, and no log in the cluster said why. The
		// length is the caller's DECLARED one (-1 when it sent none); the body
		// itself is never logged.
		args := []any{"source", name, "limit", h.bodyLimit(src), "declaredLength", r.ContentLength}
		if src.verifiedBy == memqlsync.VerifiedByEnv {
			args = append(args, "setting", "MEMQL_INBOUND_SOURCE_"+envSuffix(name)+"_MAX_BODY_BYTES")
		} else {
			args = append(args, "tier", src.verifiedBy)
		}
		h.logger.Warn("inbound receiver: request refused, body over the cap", args...)
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	case err == errLargeBodiesBusy:
		// 503, because the same delivery succeeds once a turn is free -- but
		// logged, because not every sender retries a 5xx on its own (GitHub
		// does not), and a refused delivery is then only redelivered by hand.
		h.logger.Warn("inbound receiver: request refused, too many large bodies being read",
			"source", name, "declaredLength", r.ContentLength, "turns", cap(h.largeBodies))
		http.Error(w, "receiver busy, try again", http.StatusServiceUnavailable)
		return
	case err == errBodyTooSlow:
		// The read a turn covers was cut for falling behind its pace (see
		// pacedBody), and the turn is free again. Logged for the same reason
		// as the two above: the sender is the only other party that knows.
		h.logger.Warn("inbound receiver: request refused, body arrived too slowly",
			"source", name, "declaredLength", r.ContentLength,
			"minBytesPerSecond", int64(h.largeBodyRate), "grace", h.largeBodyGrace, "hold", h.largeBodyHold)
		http.Error(w, "request body arrived too slowly", http.StatusRequestTimeout)
		return
	case err != nil:
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	defer release()
	// The body is stored in a string field and rendered into a MemQL string
	// literal, so it has to be text. Refusing here is honest; json.Marshal
	// would silently rewrite every invalid byte to U+FFFD and stage a row whose
	// body no longer matches what was signed.
	if !utf8.Valid(body) {
		http.Error(w, "request body is not valid UTF-8", http.StatusBadRequest)
		return
	}
	// U+0000 is valid UTF-8, so the gate above passes it -- and the staged row
	// then cannot exist. The body lands in a JSONB column (`payload JSONB NOT
	// NULL`), and PostgreSQL's jsonb type cannot represent U+0000. memqlString
	// renders the byte as a unicode escape, which the MemQL lexer decodes
	// correctly -- so the statement PARSES, and the failure lands one layer
	// further down, where Postgres refuses the value with "unsupported Unicode
	// escape sequence" (memql#3098). That is exactly why this package's existing
	// lexer round-trip test is blind to the class: it asserts on the DECODED
	// value, and decoding is the part that works.
	//
	// The consequence is worse than a dropped field. The insert fails, the
	// handler answers 503, and staging is idempotent by requestId -- so the
	// sender retries the same body forever against a request that can never
	// succeed. An infinite retry loop, not a stuck row.
	//
	// REFUSED rather than substituted, and that is the opposite of what
	// component/outbound does for the same byte (memql#3035). The reasoning does
	// not transfer: outbound mangles a byte in `lastError`, which is diagnostic
	// text where a replacement character beats a permanently stuck row. A
	// webhook body is what the caller SENT -- it is signature-verified material,
	// and silently rewriting it would stage a row whose body no longer matches
	// what was signed. That is the same argument the utf8.Valid gate above
	// already makes, so refusing here keeps the two consistent.
	//
	// 400 rather than 503: this request cannot be made to succeed by retrying,
	// and 4xx is what stops the sender from trying.
	if bytes.IndexByte(body, 0) >= 0 {
		http.Error(w, "request body contains a NUL byte", http.StatusBadRequest)
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
		// Logged with the reason, answered without it -- the same split every
		// other arm in this handler makes. This was the ONE arm that echoed
		// err.Error() back to the caller, which contradicts the "the caller
		// learns nothing" invariant this feature's own security notes claim,
		// on an endpoint that is unauthenticated by construction. Caught by
		// the author during the memql#2957 handoff; CodeQL did not flag it.
		h.logger.Warn("inbound receiver: request refused", "source", name, "error", err)
		http.Error(w, "invalid dedupe header", http.StatusBadRequest)
		return
	}

	headers, err := deliveryHeaders(src, r.Header)
	if err != nil {
		http.Error(w, "invalid delivery metadata", http.StatusBadRequest)
		return
	}
	received := h.now().UTC()
	requestID := requestIDFor(name, identityKeyFor(src, r.Header, body))
	mutation := fmt.Sprintf(
		`mutation stageInboundRequest(requestId: %s, source: %s, medium: "webhook", body: %s, `+
			`contentType: %s, headersJson: %s, dedupeKey: %s, signatureVerified: %t, verifiedBy: %s, receivedAt: %s)`,
		memqlString(requestID), memqlString(name), memqlString(string(body)),
		memqlString(r.Header.Get("Content-Type")), memqlString(headers), memqlString(dedupeKey), verified,
		memqlString(src.verifiedBy), memqlString(received.Format(time.RFC3339Nano)))

	if _, err := h.engine.Execute(systemActorContext(r.Context()), mutation); err != nil {
		// The delivery was valid; we simply could not record it. 503 is the
		// honest answer -- every sender in the story's list retries on 5xx, and
		// the staging mutation is idempotent by requestId, so the retry lands
		// on the same row rather than duplicating the event.
		//
		// The engine ERROR is deliberately NOT logged here, and the reason is
		// not squeamishness (CodeQL go/clear-text-logging, memql#2957). The
		// mutation is built by string interpolation and embeds the webhook
		// BODY, so any engine error that quotes the offending source -- and the
		// parser does, e.g. `unexpected token after expression: %q` -- copies
		// third-party payload bytes into our application log. A webhook body
		// routinely carries PII and bearer tokens.
		//
		// requestID is safe to log and is the correlation handle: it is
		// sha256(source, signed-payload digest) truncated, so it identifies the row without
		// revealing anything the caller sent. An operator who needs the engine's
		// own reason has it in the engine's server-side log for the same
		// request.
		//
		// memqlString JSON-encodes the body precisely so it always renders a
		// valid literal, so reaching a parse error takes an engine bug rather
		// than a crafted payload -- but "needs a bug" is not "cannot happen",
		// and this endpoint is unauthenticated by construction.
		h.logger.Error("inbound receiver: staging failed", "source", name, "id", requestID)
		http.Error(w, "could not stage request", http.StatusServiceUnavailable)
		return
	}

	h.logger.Info("inbound receiver: staged", "source", name, "id", requestID, "verified", verified)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": requestID, "status": "received"})
}

// resolveSource finds the verification policy for a source segment: the
// env-configured allowlist first, then the bound connectors.
//
// A CONNECTOR source is one the environment could not have named. The env
// tier is resolved once at boot from MEMQL_INBOUND_SOURCE_<NAME>_*, which
// works for a sender an operator configures with the deployment and cannot
// work for a multi-tenant connector: one Shopify connector serves many
// stores, each with its own webhook secret, and a store is added by an
// operator at runtime. The secret for `shopify-<storeId>` therefore lives on
// the row that describes the store, and the connector is what can read it.
//
// ENV WINS, deliberately. An operator who has pinned a source in the
// environment has made a statement about it, and neither a source the
// cluster registers later nor a connector that claims the same name may
// silently take it over -- that would move which secret verifies a live
// sender, with nothing in the environment changed to say so.
//
// The one exception is a refusal, not a takeover: a pinned name the
// dispatcher would hand to a connector is answered 404 rather than verified
// by the env secret -- see the first branch below (memql#5707 follow-up).
func (h *Handler) resolveSource(ctx context.Context, name string) (SourceConfig, bool) {
	if src, ok := h.cfg.Sources[name]; ok {
		// A name a bound connector CLAIMS is never an env source. The
		// dispatcher routes a staged row by its source name alone, through
		// memqlsync.ConnectorForSource -- SourceClaimed below gives the same
		// answer, so the two cannot disagree -- and the connector reads a row under a name it
		// claims as verified by ITS secret for that tenant: its own name as
		// signed by the app secret, a tenant's `shopify-<storeId>` as signed
		// by that store's webhook secret, with the store bound off the NAME.
		// A body the env secret verified must never be handed to it on that
		// premise; for a custom-app store it would queue a privacy purge
		// from a body the operator's secret signed.
		//
		// A bound connector's own name is refused whether or not it currently
		// claims it: an app with a client id but no sealed secret claims
		// nothing, and that is exactly the state in which a stray row would
		// be bound by client id. Refused, not handed to the connector, so
		// the verifying secret moves in neither direction and the operator
		// has an ERROR naming the variable to rename or remove (memql#5707
		// review and follow-up).
		//
		// FAILS CLOSED. SourceClaimed asks the claim only, never the
		// tenant's secret, and reports a connector that could not read its
		// tenants as an error rather than "not mine" -- which is the answer
		// that would admit the pin. Refused with the same 404, so an
		// unauthenticated caller learns nothing either way.
		c, claimed, err := memqlsync.SourceClaimed(ctx, name)
		if err != nil {
			h.logger.Error("inbound receiver: could not tell whether a connector claims an env source, refusing with 404",
				"source", name, "env", "MEMQL_INBOUND_SOURCE_"+envSuffix(name)+"_*", "error", err)
			return SourceConfig{}, false
		}
		if claimed {
			h.logger.Error("inbound receiver: env source names a source a bound connector claims, refusing with 404",
				"source", name, "connector", c.Name(), "env", "MEMQL_INBOUND_SOURCE_"+envSuffix(name)+"_*")
			return SourceConfig{}, false
		}
		src.verifiedBy = memqlsync.VerifiedByEnv
		return src, true
	}
	// THE REGISTERED TIER, after the environment and before the connectors.
	// After, for the reason above: an operator's statement about a name is not
	// something a row written from a browser takes over. Before, because a
	// source the cluster registered for itself is a more specific claim on a
	// name than a connector's prefix match.
	//
	// THE SAME FAIL-CLOSED RULE as the connector tier below, applied here
	// rather than trusted to the resolver: a policy that names a verifying
	// scheme and carries no secret is DROPPED, never admitted unverified.
	if h.registered != nil {
		if src, ok := h.registered(ctx, name); ok {
			if src.Scheme != SchemeNone && src.Secret == "" {
				h.logger.Error("inbound receiver: registered source has no resolvable secret, refusing with 404", "source", name)
				return SourceConfig{}, false
			}
			src.Name = name
			src.verifiedBy = memqlsync.VerifiedByRegistered
			return src, true
		}
	}
	src, ok := memqlsync.SourceFor(ctx, name)
	if !ok {
		return SourceConfig{}, false
	}
	// A connector that answers with no secret and no explicit "none" scheme
	// is DROPPED rather than admitted: an unresolvable credential reference
	// (a deleted globalSecret row, a store half-configured) would otherwise
	// turn into an unverified source, which is the one outcome the whole
	// deny-by-default design exists to prevent.
	if src.Scheme != SchemeNone && src.Secret == "" {
		h.logger.Error("inbound receiver: connector source has no resolvable secret, refusing with 404",
			"source", name, "secretRef", src.SecretRef)
		return SourceConfig{}, false
	}
	return SourceConfig{
		Name:            name,
		Secret:          src.Secret,
		Scheme:          src.Scheme,
		SignatureHeader: src.SignatureHeader,
		SignaturePrefix: src.SignaturePrefix,
		DedupeHeader:    src.DedupeHeader,
		ForwardHeaders:  src.ForwardHeaders,
		verifiedBy:      memqlsync.VerifiedByConnector,
	}, true
}

var (
	errBodyTooLarge    = fmt.Errorf("body exceeds the configured cap")
	errLargeBodiesBusy = fmt.Errorf("every turn for a body over the default cap is taken")
	errBodyTooSlow     = fmt.Errorf("a body holding a large-body turn fell behind its pace")
)

const (
	// largeBodyReads is how many requests on one node may hold a body over the
	// receiver's default cap at once. Two, because the senders with a raised
	// cap send their large deliveries rarely and one at a time -- a push of a
	// long branch -- while each turn may hold a body as large as its source's
	// cap (25 MiB for GitHub's) before any signature has been checked.
	largeBodyReads = 2
	// defaultLargeBodyWait is how long a large request waits for a turn. Short,
	// because a sender times its delivery out on its own (GitHub after ten
	// seconds) and an answer it can see beats a connection it gives up on.
	defaultLargeBodyWait = 3 * time.Second

	// The pace a read holding a turn must keep (pacedBody). Without one, the
	// cheap way to starve every large delivery is to hold both turns with
	// bodies that trickle a byte at a time for as long as the server lets a
	// request read. With it, a turn is held only by bytes that keep arriving
	// -- at least defaultLargeBodyRate per second once defaultLargeBodyGrace has
	// passed, and the whole body within defaultLargeBodyHold -- so holding the
	// turns costs real, sustained bandwidth. Ten seconds is also when GitHub
	// gives up on a delivery itself, and a real one arrives far faster than
	// 1 MiB/s.
	defaultLargeBodyGrace = time.Second
	defaultLargeBodyRate  = 1 << 20
	defaultLargeBodyHold  = 10 * time.Second
)

// bodyLimit is the cap a request to src is read under: the source's own when
// it has one, the receiver's otherwise.
func (h *Handler) bodyLimit(src SourceConfig) int64 {
	if src.MaxBodyBytes > 0 {
		return int64(src.MaxBodyBytes)
	}
	return int64(h.cfg.MaxBodyBytes)
}

// readBody reads at most the source's cap, and one byte more so an over-cap
// body is DETECTED rather than silently truncated -- a truncated body would
// fail its own signature check and look like an attack. A body whose declared
// length is already over the cap is refused before a byte of it is read.
//
// A BODY OVER THE RECEIVER'S DEFAULT TAKES A TURN. The default cap bounds what
// an unauthenticated caller can make the node hold before the signature check
// runs, and a source's raised cap must not quietly give that back: anyone can
// post 25 MiB to GitHub's source, signed or not. So a request that may carry
// more than the default -- one that declares more, or declares nothing --
// holds one of largeBodyReads turns from before the read until the request is
// answered (the returned release, which the caller defers), and one that
// cannot get a turn within largeBodyWait is refused with errLargeBodiesBusy. A
// request declaring no more than the default never waits, so a crowd of large
// requests cannot hold up ordinary deliveries.
//
// The read a turn covers is PACED (pacedBody): a body that stops arriving loses
// the turn with errBodyTooSlow. That needs the connection's read deadline, so it
// is in force wherever the ResponseWriter offers one -- every real server -- and
// absent only where it cannot be (a test's recorder), where the server's own
// ReadTimeout remains the bound it always was.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, src SourceConfig) ([]byte, func(), error) {
	release := func() {}
	limit := h.bodyLimit(src)
	if r.ContentLength > limit {
		return nil, release, errBodyTooLarge
	}
	var from io.Reader = r.Body
	var paced *pacedBody
	if def := int64(h.cfg.MaxBodyBytes); limit > def && (r.ContentLength < 0 || r.ContentLength > def) {
		if !h.takeLargeBodyTurn(r.Context()) {
			return nil, release, errLargeBodiesBusy
		}
		release = func() { <-h.largeBodies }
		rc := http.NewResponseController(w)
		start := time.Now()
		if rc.SetReadDeadline(start.Add(h.largeBodyGrace)) == nil {
			paced = &pacedBody{body: r.Body, rc: rc, start: start,
				grace: h.largeBodyGrace, hold: h.largeBodyHold, rate: h.largeBodyRate}
			from = paced
		}
	}
	body, err := io.ReadAll(io.LimitReader(from, limit+1))
	if err == nil && int64(len(body)) > limit {
		err = errBodyTooLarge
	}
	if err != nil {
		release()
		// The deadline that cut a slow read is LEFT as it is. Answering, the
		// server tries to drain what is left of an unread body before it writes
		// the response; an expired deadline is what makes that drain fail at
		// once, so the answer goes out and the connection closes instead of
		// waiting on the trickle that just lost its turn.
		if isTimeout(err) {
			err = errBodyTooSlow
		}
		return nil, func() {}, err
	}
	if paced != nil {
		// The body is in, so the pace is lifted. Left set, it could cut the
		// server's background read on this connection -- and that read failing
		// cancels the request's context -- while the row is being staged.
		_ = paced.rc.SetReadDeadline(time.Time{})
	}
	return body, release, nil
}

// pacedBody keeps a body holding a large-body turn arriving. Before each read it
// moves the connection's read deadline to the latest instant at which the bytes
// read so far still meet rate once grace has passed -- start + grace + n/rate --
// and never past start + hold. A sender that keeps up never meets the deadline;
// one that trickles meets it within a moment of the grace, and the read fails
// with a timeout that readBody turns into errBodyTooSlow.
type pacedBody struct {
	body        io.Reader
	rc          *http.ResponseController
	start       time.Time
	grace, hold time.Duration
	rate        float64
	n           int64
}

func (p *pacedBody) Read(b []byte) (int, error) {
	due := p.start.Add(p.grace + time.Duration(float64(p.n)/p.rate*float64(time.Second)))
	if last := p.start.Add(p.hold); due.After(last) {
		due = last
	}
	if err := p.rc.SetReadDeadline(due); err != nil {
		return 0, err
	}
	n, err := p.body.Read(b)
	p.n += int64(n)
	return n, err
}

// isTimeout reports a read cut by a deadline.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// takeLargeBodyTurn waits up to largeBodyWait for a turn, or until the request
// is gone. A nil largeBodies (a Handler not built by NewHandler) never yields
// one, which fails closed: such a request is refused rather than unbounded.
func (h *Handler) takeLargeBodyTurn(ctx context.Context) bool {
	timer := time.NewTimer(h.largeBodyWait)
	defer timer.Stop()
	select {
	case h.largeBodies <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
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

// identityKeyFor produces the half of the row id that stands for "which
// delivery is this", and it is derived from SIGNED material only.
//
// This is a security boundary, not a naming choice (memql#2957 landing review).
// The vendor signs the BODY -- and the timestamp too, where a TimestampHeader
// is configured -- but it does not sign our DedupeHeader, because that header
// is our configuration and not part of its scheme. Deriving the row id from an
// unsigned header therefore let ONE captured, still-valid request mint
// unbounded distinct rows: the signature stays valid because the body is
// unchanged, while varying the unsigned header varies the id. Storage
// amplification from a single replay, with no forged signature required.
//
// So identity is the digest of exactly what was signed, and NOTHING else. The
// sender's dedupe key is still recorded on the row -- a product automation can
// read it -- but it does not participate in the id.
//
// Folding it in as a subordinate distinguisher was considered and is wrong: if
// the key contributes to the id at all, varying it still varies the id, and one
// captured signature still mints unbounded rows. Any unsigned input in an
// identity is attacker-multipliable; the only sound identity is signed
// material.
//
// The cost, stated because it is a real behaviour change for a configured
// source: two deliveries whose signed bytes are IDENTICAL now collapse onto one
// row, where the sender's key would previously have separated them. Where a
// sender re-delivers the same event that is exactly the wanted behaviour and
// what this feature promises. Where a sender legitimately emits two distinct
// events with byte-identical payloads, it distinguishes them with a timestamp
// header -- which IS signed, and which is already how every timestamped webhook
// scheme in the wild avoids the same ambiguity. Configure TimestampHeader for
// such a source; without one, byte-identical is indistinguishable to anybody,
// including the receiver.
//
// For an unverified source (scheme "none") there is no signed material and no
// signature to replay, so the body digest stands in: identity is content, which
// is the strongest statement available when nothing is authenticated.
func identityKeyFor(src SourceConfig, header http.Header, body []byte) string {
	signed := body
	if src.Scheme != SchemeNone && src.TimestampHeader != "" {
		// Mirrors verify(): "<timestamp>.<body>" is what the HMAC covered, so
		// it is what identity must be taken over.
		ts := strings.TrimSpace(header.Get(src.TimestampHeader))
		signed = append(append([]byte(ts), '.'), body...)
	}
	sum := sha256.Sum256(signed)
	return hex.EncodeToString(sum[:])
}

// requestIDFor derives the row id from (source, identityKey), which is what makes
// staging idempotent without a read-then-write race: a redelivery renders the
// same id and @createOnly on the mutation preserves the product's handling
// state.
//
// The composition must be INJECTIVE or two distinct events collapse onto one
// row -- the memql#2980 class. It is, because the parts cannot forge the
// separator: a source name is drawn from the allowlist and matched against
// sourceNamePattern (lowercase alnum, '_' and '-' only), and a dedupe key is
// always a hex digest we produced (identityKeyFor). NUL therefore appears in
// neither part, so the split at the first NUL is unique and equal concatenations
// force equal parts.
func requestIDFor(source, identityKey string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + identityKey))
	return "inb" + hex.EncodeToString(sum[:16])
}

// memqlString renders a Go string as a MemQL string literal.
//
// It is now a one-line alias for langparser.QuoteString, which lives beside the
// lexer whose escape set it targets. This function had a byte-identical body,
// and memql#3035 -- the same defect reached through the outbound worker's
// lastError -- is what having two definitions of one escape set costs. The
// alias is kept rather than the six call sites rewritten because the name reads
// well at each of them and the indirection is free.
//
// One behavioural note, because "alias" reads as "nothing changed" and that is
// not quite true here: QuoteString turns HTML escaping OFF, which the body this
// replaced left on. So the bytes this emits for <, > and & differ from before
// -- raw now, \u003c / \u003e / \u0026 then. The value is unaffected: the lexer
// decodes both spellings to the identical string, which is why
// handler_test.go's TestRenderedLiteralsParseBackThroughTheRealLexer passes
// either way -- it asserts on the decoded value, so it is blind to this change
// in both directions. What changed is the size and legibility of the staged
// statement, not what gets staged.
func memqlString(s string) string { return langparser.QuoteString(s) }

// systemInboundActor is the actor the staging mutation runs as. The request
// carries no MemQL identity -- a third party signed it with a shared secret,
// which authorizes the DELIVERY and says nothing about a user -- so the write
// runs as a named system actor and is auditable as one (outbound worker
// precedent).
const systemInboundActor = "system:inbound"

// The write also carries INTERNAL ORIGIN, which stageInboundRequest requires
// (@serverOnly): the row's `source` and `headersJson` drive a connector all
// the way to a privacy purge, so the row's provenance must be the receiver's
// and never a client's. The stamp is earned in ServeHTTP before the one
// Execute: the source allowlist and the signature check precede it
// (memql#5707 review).
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemInboundActor,
		Claims: map[string]any{
			"sub":  systemInboundActor,
			"role": "system",
		},
	}))
}
