package email

// ndr.go -- the Graph mailbox reader (design D14, memql#4824).
//
// # The gap this closes
//
// The campaign engine suppresses an address when a provider tells it the
// address is dead. On the webhook transports that report arrives as an HTTP
// POST to /inbound/{source} and the existing machinery takes over. On the
// GRAPH transport there is no webhook: a Microsoft 365 mailbox reports a
// failed delivery by mailing a `multipart/report` DSN back to the sender,
// and nothing was reading that mailbox. So on the transport this deployment
// actually sends on, suppression starved -- quietly, in the only direction
// that matters, because a list that never grows looks exactly like a list
// with nothing to add.
//
// # Why a poll and not a subscription
//
// Graph change notifications would be the push equivalent, and they need a
// publicly reachable HTTPS endpoint that answers a validation handshake plus
// a subscription this cluster renews before it lapses. That is a new
// unauthenticated HTTP surface (this repo's policy is deny-by-default and
// every exception is individually approved) plus a renewal timer whose
// failure mode is silence -- the same silence this file exists to remove. A
// poll needs no endpoint, no renewal, and fails loudly into the node log.
//
// # signatureVerified: true is honest here, and it is worth saying why
//
// The field gates campaignIngestFeedback, so setting it is not cosmetic.
// There is no third-party signature on a DSN to check: PROVENANCE IS THE
// VERIFICATION. The bytes were read out of OUR OWN mailbox, over OUR OWN
// client-credentials token, from an endpoint naming the mailbox this
// deployment sends as. Nobody can put a message on that path without already
// holding the credential -- which is a stronger statement than an HMAC over
// a body a stranger POSTed to a public URL, and the same statement the
// `scheme="none"` inbound sources make when the transport itself is the
// authentication.
//
// What it does NOT claim is that the DSN's CONTENTS are true. A DSN can be
// forged by any host that can mail our mailbox. That is a property of email
// and not of this reader; it is why the suppression a bounce produces is
// per-address and reversible rather than something that deletes a roster.
//
// # Nothing is ever marked processed on a failure
//
// A message that cannot be fetched, cannot be represented, or cannot be
// staged is left UNREAD and logged. It will be tried again on the next pass,
// which for a permanently unreadable message means a log line every interval
// -- deliberately, because the alternatives are worse in the direction that
// matters: marking it read discards a bounce and the list quietly starves
// again, and there is no third state that is honest about a message we
// neither handled nor want to see again.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/id"
)

const (
	// NDRComponent is the Dependency ComponentName.
	NDRComponent = common.ComponentName("email.ndr")

	// NDRPollSecondsEnv is how often the mailbox is read. 0 disables the
	// reader outright, which is the setting for a deployment whose bounces
	// arrive over a webhook instead.
	NDRPollSecondsEnv = "MEMQL_EMAIL_NDR_POLL_SECONDS"

	// defaultNDRPollSeconds is five minutes. A bounce is not urgent -- the
	// address is already dead and the next send to it is minutes or days
	// away -- and each pass costs a token check plus a mailbox listing
	// against a tenant that throttles.
	defaultNDRPollSeconds = 300

	// NDRSource is the `source` every staged row carries. The operator lists
	// `graph-mailbox=rfc3464` in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES to let the
	// campaigns feedback path act on it; without that entry the rows are
	// staged and ignored, which is a configuration state and not an error.
	NDRSource = "graph-mailbox"

	// NDRMedium is the `medium` every staged row carries. Distinct from
	// "webhook" because the provenance argument above is different: a
	// webhook row was signed by a third party, a mailbox row was read from
	// a mailbox only this credential can open.
	NDRMedium = "mailbox"

	// ndrProcessedCategory is stamped on a message once its row exists, so
	// a human opening the mailbox can see what the engine has taken and
	// what it has not.
	ndrProcessedCategory = "memql-processed"

	// ndrListPageSize bounds one pass. A bounded read of an unbounded set
	// is a truncation, so this is not "all the unread mail" and must not be
	// described as such -- it is a rate, and the backlog drains over
	// successive passes.
	ndrListPageSize = 25

	// ndrClaimName is the cross-replica claim namespace, in the same
	// automation_execution_claims ledger the automation guard uses.
	ndrClaimName = "emailNdrPoll"

	// systemNDRActor is the named system identity the staging write runs
	// as. There is no user behind a bounce: a mail server sent it. Named
	// rather than anonymous so the write is auditable as what it is
	// (component/inbound and component/outbound set the precedent).
	systemNDRActor = "system:email-ndr"

	// ndrStartupDelay lets the engine and the plug-in registry come up
	// before the first pass. Without it the first read happens against a
	// sender that has not resolved and an engine that cannot execute, and
	// the boot log carries two warnings that mean nothing.
	ndrStartupDelay = 20 * time.Second
)

// NDREngine is the narrow slice of the MemQL engine the reader needs. An
// interface for the same reason component/inbound declares one: the staging
// path is worth testing without a database, and a fake that records the
// rendered call is the only way to see what would be written.
type NDREngine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// NDRClaimer is the cross-replica claim gate (automations.ClusterExecutionGuard
// satisfies it). Nil is correct for a single-replica deployment and MUST be
// wired in the mesh: two replicas reading one mailbox stage every bounce
// twice, and while the derived row id collapses the duplicate, both replicas
// would also race to mark the message read and one would find it already
// gone.
type NDRClaimer interface {
	ClaimWithTTL(ctx context.Context, name, dedupKey string, ttl time.Duration) bool
}

// NDRPollInterval reads the configured interval. Zero means disabled.
//
// A malformed value falls back to the DEFAULT rather than to disabled: a typo
// in a number should not silently turn a compliance-relevant feed off, and
// the warning says so out loud at the one place that can see the raw value.
func NDRPollInterval(logger *slog.Logger) time.Duration {
	reader := env.NewEnvReader("")
	raw, ok := reader.String(NDRPollSecondsEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultNDRPollSeconds * time.Second
	}
	seconds, err := reader.OptionalInt(NDRPollSecondsEnv)
	if err != nil || seconds == nil {
		if logger != nil {
			logger.Warn("email ndr: "+NDRPollSecondsEnv+" is not an integer; falling back to the default interval",
				"value", raw, "defaultSeconds", defaultNDRPollSeconds, "error", err)
		}
		return defaultNDRPollSeconds * time.Second
	}
	if *seconds <= 0 {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}

// NDRPoller reads the sending mailbox and stages what it finds.
type NDRPoller struct {
	engine  NDREngine
	claimer NDRClaimer
	resolve func() Sender
	logger  *slog.Logger

	interval         time.Duration
	campaignsEnabled bool
	now              func() time.Time

	cancel    context.CancelFunc
	running   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
	readyCh   chan struct{}
	doneCh    chan struct{}
	mu        sync.Mutex

	stagedTotal  atomic.Int64
	skippedTotal atomic.Int64
	failedTotal  atomic.Int64
}

// NewNDRPoller constructs the reader.
//
// resolveSender is called at POLL time rather than at construction, mirroring
// the campaigns worker and the outbound email transport: on a booting node
// the plug-in registry is not yet populated, and a sender captured here would
// be nil forever.
//
// campaignsEnabled is passed IN rather than read from the environment,
// because the answer belongs to component/campaigns and this package must not
// grow an edge to it to ask. The wiring site already holds the campaigns
// config; a second env parse here is a second definition of the same switch.
func NewNDRPoller(engine NDREngine, claimer NDRClaimer, resolveSender func() Sender, campaignsEnabled bool, logger *slog.Logger) *NDRPoller {
	if logger == nil {
		logger = slog.Default()
	}
	return &NDRPoller{
		engine:           engine,
		claimer:          claimer,
		resolve:          resolveSender,
		logger:           logger.With("component", string(NDRComponent)),
		interval:         NDRPollInterval(logger),
		campaignsEnabled: campaignsEnabled,
		now:              time.Now,
		readyCh:          make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

// Start launches the poll loop. Idempotent; a no-op when disabled or unwired.
//
// It does NOT check that the resolved sender is Graph, and that omission is
// deliberate: at Start the plug-in registry may not be populated, so the
// answer here would be "no" on every node and the reader would never run.
// The check happens per pass, where it is answerable.
func (p *NDRPoller) Start(_ context.Context) {
	p.startOnce.Do(func() {
		defer close(p.readyCh)
		if !p.campaignsEnabled {
			p.logger.Info("email ndr: not starting -- campaigns are disabled on this node, so nothing consumes a bounce")
			close(p.doneCh)
			return
		}
		if p.interval <= 0 {
			p.logger.Info("email ndr: disabled by configuration",
				"env", NDRPollSecondsEnv,
				"note", "bounces must arrive some other way, or the suppression list will not grow")
			close(p.doneCh)
			return
		}
		if p.engine == nil {
			p.logger.Warn("email ndr: no engine wired; not starting")
			close(p.doneCh)
			return
		}
		if p.claimer == nil {
			// A warning rather than a refusal. Single-replica is a real
			// deployment and correct without a guard; at two replicas the
			// duplicate work is bounded (the row id collapses) but the
			// mark-read race is not, and an operator should know.
			p.logger.Warn("email ndr: no cluster execution guard wired, so every replica will poll the same mailbox")
		}
		runCtx, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.cancel = cancel
		p.mu.Unlock()
		p.running.Store(true)
		go p.loop(runCtx)
	})
}

// Stop cancels the loop.
func (p *NDRPoller) Stop(_ context.Context) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		cancel := p.cancel
		p.cancel = nil
		p.mu.Unlock()
		if cancel != nil {
			cancel()
			<-p.doneCh
		}
		p.running.Store(false)
	})
}

// Dependency surface.
func (p *NDRPoller) IsRunning() bool                     { return p.running.Load() }
func (p *NDRPoller) Order() int                          { return 12 }
func (p *NDRPoller) ComponentName() common.ComponentName { return NDRComponent }
func (p *NDRPoller) Ready() <-chan struct{}              { return p.readyCh }

// Counters, for a test and for anyone reading a status surface later.
func (p *NDRPoller) Staged() int64  { return p.stagedTotal.Load() }
func (p *NDRPoller) Skipped() int64 { return p.skippedTotal.Load() }
func (p *NDRPoller) Failed() int64  { return p.failedTotal.Load() }

func (p *NDRPoller) loop(ctx context.Context) {
	defer close(p.doneCh)
	p.logger.Info("email ndr: started", "pollInterval", p.interval.String(), "batchSize", ndrListPageSize)
	startup := time.NewTimer(ndrStartupDelay)
	defer startup.Stop()
	select {
	case <-ctx.Done():
		return
	case <-startup.C:
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	p.PollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			p.logger.Info("email ndr: stopped")
			return
		case <-ticker.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce runs one pass. Exported so a test drives it synchronously rather
// than racing the ticker -- the campaigns worker's DrainOnce precedent, and
// the reason this file has a real fixture test at all.
func (p *NDRPoller) PollOnce(ctx context.Context) {
	g := p.graphSender(ctx)
	if g == nil {
		// Not an error and not a warning. A node sending over SMTP, or one
		// with no sender configured, legitimately has no mailbox to read;
		// saying so at Debug keeps the reason findable without a line every
		// five minutes on every node in the cluster.
		p.logger.Debug("email ndr: this node's sender is not Microsoft Graph, so there is no mailbox to read")
		return
	}
	mailbox := strings.TrimSpace(g.cfg.SenderAddr)
	if mailbox == "" {
		p.logger.Debug("email ndr: the Graph sender names no mailbox")
		return
	}
	if !p.claimPass(ctx, mailbox) {
		return
	}

	token, err := g.getToken(ctx)
	if err != nil {
		p.logger.Warn("email ndr: could not acquire a Graph token; the mailbox was not read this pass", "error", err)
		return
	}
	ids, err := p.listUnread(ctx, g, mailbox, token)
	if err != nil {
		p.logger.Warn("email ndr: could not list the mailbox", "mailbox", mailbox, "error", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	p.logger.Debug("email ndr: unread messages to examine", "mailbox", mailbox, "count", len(ids))
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		p.processMessage(ctx, g, mailbox, token, id)
	}
}

// graphSender resolves this node's sender and reports it only when it is
// Graph.
//
// A LazySender is unwrapped through its own Resolve, which runs the same
// one-time resolution a send would. Not doing so would leave a node that
// reads but never sends behind an unresolved wrapper forever, concluding it
// had no Graph sender when it plainly does.
func (p *NDRPoller) graphSender(ctx context.Context) *GraphSender {
	if p == nil || p.resolve == nil {
		return nil
	}
	sender := p.resolve()
	if lazy, ok := sender.(*LazySender); ok && lazy != nil {
		sender = lazy.Resolve(ctx)
	}
	g, _ := sender.(*GraphSender)
	return g
}

// claimPass takes the cross-replica claim for this interval window.
//
// The key is (mailbox, interval bucket) rather than a per-message key: the
// unit of work is the PASS, because the listing itself is the expensive and
// racy part -- two replicas that both list will both try to mark the same
// messages read, and the loser's PATCH 404s on a message the winner has
// already moved. Bucketing by the interval means two replicas whose tickers
// are seconds apart still collide, which a wall-clock key would not.
func (p *NDRPoller) claimPass(ctx context.Context, mailbox string) bool {
	if p.claimer == nil {
		return true
	}
	bucket := p.now().UTC().Truncate(p.interval).Unix()
	key := fmt.Sprintf("%s@%d", strings.ToLower(mailbox), bucket)
	if p.claimer.ClaimWithTTL(ctx, ndrClaimName, key, p.interval) {
		return true
	}
	p.logger.Debug("email ndr: another replica holds this pass", "mailbox", mailbox)
	return false
}

// ndrMessage is the sliver of a Graph message object this reader selects.
type ndrMessage struct {
	ID               string `json:"id"`
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
}

// listUnread returns the ids of the mailbox's unread messages, bounded.
//
// It filters on `isRead eq false` and NOT on the content type, because Graph
// cannot filter on it -- the media type lives in the MIME, and the message
// resource does not expose it in a form $filter can read. So the report test
// happens after the fetch, in processMessage. The cost is that a non-report
// sitting unread is re-examined every pass; the premise that makes that
// acceptable is that this is a NO-REPLY SENDING MAILBOX, whose unread mail is
// expected to be machine-generated reports. The alternative -- marking a
// stranger's message read to stop looking at it -- hides a human's mail, and
// that is the failure this refuses to have.
func (p *NDRPoller) listUnread(ctx context.Context, g *GraphSender, mailbox, token string) ([]ndrMessage, error) {
	query := url.Values{}
	query.Set("$filter", "isRead eq false")
	query.Set("$select", "id,subject,receivedDateTime")
	query.Set("$top", fmt.Sprintf("%d", ndrListPageSize))
	// Encode() spells a space as '+', which is form encoding rather than
	// URI encoding. OData reads the filter as a path-style expression and a
	// literal '+' inside it is a '+', not a space.
	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/mailFolders/inbox/messages?%s",
		url.PathEscape(mailbox), strings.ReplaceAll(query.Encode(), "+", "%20"))

	_, body, err := g.get(ctx, endpoint, token)
	if err != nil {
		return nil, err
	}
	var page struct {
		Value []ndrMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("graph: parsing the message list: %w", err)
	}
	return page.Value, nil
}

// processMessage handles exactly one message: fetch, classify, stage, stamp.
//
// Every early return leaves the message UNREAD. That is the invariant the
// whole file is arranged around -- see the header.
func (p *NDRPoller) processMessage(ctx context.Context, g *GraphSender, mailbox, token string, msg ndrMessage) {
	base := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/messages/%s",
		url.PathEscape(mailbox), url.PathEscape(msg.ID))

	_, raw, err := g.get(ctx, base+"/$value", token)
	if err != nil {
		p.failedTotal.Add(1)
		// The SUBJECT is not logged. A DSN's subject quotes the original
		// message's, which for a campaign is operator-authored and for
		// anything else is a stranger's mail; the id is the correlation
		// handle and carries nothing.
		p.logger.Warn("email ndr: could not fetch a message; leaving it unread for the next pass",
			"mailbox", mailbox, "messageId", msg.ID, "error", err)
		return
	}

	contentType, isReport := reportContentType(raw)
	if !isReport {
		p.skippedTotal.Add(1)
		p.logger.Debug("email ndr: not a delivery report; leaving it untouched",
			"mailbox", mailbox, "messageId", msg.ID, "contentType", contentType)
		return
	}

	// The same two gates component/inbound applies to a webhook body, for
	// the same reason: the body lands in a JSONB column, and PostgreSQL's
	// jsonb cannot represent U+0000 while invalid UTF-8 cannot be rendered
	// as a MemQL string literal without silently becoming U+FFFD. A message
	// failing either is left unread rather than mangled -- staging a body
	// that is not what arrived would make the DSN parser's verdict a
	// verdict about our rewrite.
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		p.failedTotal.Add(1)
		p.logger.Error("email ndr: a delivery report contains bytes that cannot be stored as text; leaving it unread",
			"mailbox", mailbox, "messageId", msg.ID, "bytes", len(raw))
		return
	}

	requestID := ndrRequestID(msg.ID)
	received := p.messageReceivedAt(msg)
	mutation := renderNDRStageMutation(requestID, string(raw), contentType, msg.ID, received)
	if _, err := p.engine.Execute(ndrActorContext(ctx), mutation); err != nil {
		p.failedTotal.Add(1)
		// The engine error is deliberately NOT logged, exactly as in
		// component/inbound: the mutation embeds the whole message, and the
		// parser quotes offending source in its errors -- which would copy a
		// bounced recipient's address and the original message's body into
		// the application log. The request id is the correlation handle.
		p.logger.Error("email ndr: staging failed; leaving the message unread",
			"mailbox", mailbox, "messageId", msg.ID, "id", requestID)
		return
	}
	p.stagedTotal.Add(1)
	p.logger.Info("email ndr: staged a delivery report",
		"mailbox", mailbox, "id", requestID, "source", NDRSource)

	// Marking read is the LAST step and its failure is not a staging
	// failure. The row exists; a redelivery of this message renders the same
	// derived id and @createOnly preserves the product-side handling state,
	// so the worst case is one wasted fetch next pass.
	if err := p.markProcessed(ctx, g, base, token); err != nil {
		p.logger.Warn("email ndr: staged the report but could not mark the message processed; it will be fetched again next pass",
			"mailbox", mailbox, "messageId", msg.ID, "id", requestID, "error", err)
	}
}

// markProcessed stamps the message read and categorized.
//
// Both in one PATCH, because they answer different questions and must not
// drift apart: `isRead` is what stops the next pass looking at it, and the
// category is what tells a HUMAN opening the mailbox that the engine took
// this one. A message that were read but uncategorized would look, to a
// person, like somebody had opened it.
func (p *NDRPoller) markProcessed(ctx context.Context, g *GraphSender, endpoint, token string) error {
	body, err := json.Marshal(map[string]any{
		"isRead":     true,
		"categories": []string{ndrProcessedCategory},
	})
	if err != nil {
		return err
	}
	_, _, err = g.patch(ctx, endpoint, token, body)
	return err
}

// messageReceivedAt prefers the mailbox's own timestamp and falls back to
// now. An absent or unparseable receivedDateTime is a Graph quirk, not a
// reason to refuse a bounce, and "when we read it" is a defensible answer
// that is never more than one poll interval out.
func (p *NDRPoller) messageReceivedAt(msg ndrMessage) time.Time {
	if ts := strings.TrimSpace(msg.ReceivedDateTime); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			return parsed.UTC()
		}
	}
	return p.now().UTC()
}

// reportContentType returns a message's top-level Content-Type and whether it
// is a delivery report.
//
// Decided from the MIME we are ABOUT TO STAGE rather than from Graph's
// `internetMessageHeaders` projection of it. Those are two sources of truth
// about one byte range, and reading the one we store is the version that
// cannot disagree with itself.
//
// `multipart/report` covers RFC 3464 delivery status notifications and RFC
// 3798 disposition notifications alike; which of those a body actually is,
// and what it means, is component/campaigns' feedback parser's job. This
// function's only question is "is this machine-generated mail about a message
// we sent", and the media type is exactly that question.
func reportContentType(raw []byte) (string, bool) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// A header block RFC 5322 cannot read is not a report we can stage
		// as one. Left untouched rather than guessed at.
		return "", false
	}
	header := parsed.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return header, false
	}
	return header, strings.EqualFold(strings.TrimSpace(mediaType), "multipart/report")
}

// ndrRequestID derives the staged row's id from the Graph message id.
//
// Two properties, and both are load-bearing.
//
// INJECTIVE, because two distinct messages collapsing onto one row is a
// bounce that, as far as the graph is concerned, never happened. core/id's
// map form is what buys it: the map is serialized with sorted keys and JSON
// quoting, so the two fields cannot run together whatever they contain --
// which is a stronger argument than component/inbound's hand-composed
// `source + "\x00" + key`, whose injectivity rests on a separate proof that
// neither part can carry a NUL.
//
// STABLE across a redelivery, because Graph returns the same message id every
// time. A message that was staged but whose mark-read failed re-renders the
// same row id on the next pass, and @createOnly then preserves whatever the
// product has already done with it instead of resetting it to 'received'.
//
// Truncated to 32 hex characters so a mailbox row looks like every other
// inboundRequest in `inboundRequestsByStatus` -- the same shape
// component/inbound's requestIDFor produces, and the same 128-bit birthday
// bound it already accepts.
func ndrRequestID(messageID string) string {
	derived := string(id.New().MustFromMap(map[string]any{
		"source":    NDRSource,
		"messageId": messageID,
	}))
	if len(derived) > 32 {
		derived = derived[:32]
	}
	return "inb" + derived
}

// renderNDRStageMutation builds the staging call.
//
// EVERY string goes through langparser.QuoteString rather than %q. The two
// diverge on control bytes that Go's quoting spells with its own escapes and
// the MemQL lexer does not understand, and a DSN carries arbitrary
// third-party text -- so this is the one place the difference is guaranteed
// to be reachable rather than theoretical.
func renderNDRStageMutation(requestID, body, contentType, dedupeKey string, received time.Time) string {
	return fmt.Sprintf(
		`mutation stageInboundRequest(requestId: %s, source: %s, medium: %s, body: %s, `+
			`contentType: %s, dedupeKey: %s, signatureVerified: %t, receivedAt: %s)`,
		langparser.QuoteString(requestID),
		langparser.QuoteString(NDRSource),
		langparser.QuoteString(NDRMedium),
		langparser.QuoteString(body),
		langparser.QuoteString(contentType),
		langparser.QuoteString(dedupeKey),
		true,
		langparser.QuoteString(received.UTC().Format(time.RFC3339)))
}

// ndrActorContext runs the staging write as a named system actor. There is no
// user behind a bounce -- a mail server sent it -- and the write is auditable
// as the reader rather than as nobody.
func ndrActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: systemNDRActor,
		Claims: map[string]any{
			"sub":  systemNDRActor,
			"role": "system",
		},
	})
}
