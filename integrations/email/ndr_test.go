package email

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ndr_test.go -- the Graph mailbox reader (design D14, memql#4824).
//
// The fixture is a REAL RFC 3464 delivery status notification, not a
// hand-shaped stand-in, because everything downstream of this reader keys on
// the DSN's own fields: `Final-Recipient` is what the campaigns parser scans
// for and `Status:` is the class digit it classifies by. A fixture that
// merely LOOKED like a report would let this file pass while staging
// something the next component cannot read.
//
// The rendered mutation is put through the REAL LEXER AND PARSER rather than
// matched as a string. This repo has a documented history of suites that
// record call strings and never parse them -- which hides exactly the bug
// this rendering can have, since the body is arbitrary third-party text
// pasted into a MemQL source literal.

// realDSN is a delivery status notification in the shape Exchange Online
// returns: a three-part multipart/report whose middle part is the
// machine-readable delivery-status.
var realDSN = strings.ReplaceAll(`From: postmaster@example.test
To: no-reply@example.test
Subject: Undeliverable: Spring sale
Date: Tue, 01 Sep 2026 09:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="dsnboundary"

--dsnboundary
Content-Type: text/plain; charset=utf-8

Delivery has failed to these recipients or groups:

dead@example.invalid
The email address you entered couldn't be found.

--dsnboundary
Content-Type: message/delivery-status

Reporting-MTA: dns; mx.example.test

Final-Recipient: rfc822; dead@example.invalid
Action: failed
Status: 5.1.1
Diagnostic-Code: smtp; 550 5.1.1 <dead@example.invalid> User unknown

--dsnboundary
Content-Type: message/rfc822

Subject: Spring sale
To: dead@example.invalid

Hello -- 20% off everything.
--dsnboundary--
`, "\n", "\r\n")

// ordinaryMail is what a human sends to a no-reply mailbox. The reader must
// leave it alone: unread, uncategorized, unstaged.
var ordinaryMail = strings.ReplaceAll(`From: person@example.test
To: no-reply@example.test
Subject: is anyone there
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

Please call me back.
`, "\n", "\r\n")

// --- the fake mailbox ---------------------------------------------------

type ndrTestServer struct {
	*httptest.Server

	mu sync.Mutex
	// messages the inbox lists, in order.
	unread []ndrMessage
	// bodies keyed by message id. A missing id makes $value answer 500,
	// which is how the fetch-failure case is driven.
	bodies map[string]string
	// patched records every id marked processed, with the body of the PATCH.
	patched map[string]string
	// listCalls / fetchCalls count the round trips a pass actually made, so
	// a test can assert that a gated pass made NONE.
	listCalls  int
	fetchCalls int
}

func newNDRTestServer(t *testing.T) *ndrTestServer {
	t.Helper()
	s := &ndrTestServer{bodies: map[string]string{}, patched: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`))

		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox/messages"):
			s.listCalls++
			// The filter is the whole point of the listing; asserting it
			// here rather than in a test keeps every case honest about it.
			if got := r.URL.Query().Get("$filter"); got != "isRead eq false" {
				t.Errorf("the inbox listing did not filter on unread: $filter=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": s.unread})

		case strings.HasSuffix(r.URL.Path, "/$value"):
			s.fetchCalls++
			id := messageIDFromPath(strings.TrimSuffix(r.URL.Path, "/$value"))
			body, ok := s.bodies[id]
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"ErrorItemNotFound"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(body))

		case r.Method == http.MethodPatch:
			id := messageIDFromPath(r.URL.Path)
			buf, _ := io.ReadAll(r.Body)
			s.patched[id] = string(buf)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))

		default:
			t.Errorf("unexpected request to the fake mailbox: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func messageIDFromPath(path string) string {
	idx := strings.LastIndex(path, "/messages/")
	if idx < 0 {
		return ""
	}
	return path[idx+len("/messages/"):]
}

func (s *ndrTestServer) hold(id, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unread = append(s.unread, ndrMessage{ID: id, Subject: "x", ReceivedDateTime: "2026-09-01T09:00:00Z"})
	if body != "" {
		s.bodies[id] = body
	}
}

func (s *ndrTestServer) wasPatched(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.patched[id]
	return v, ok
}

// --- the fake engine ----------------------------------------------------

type recordingEngine struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (e *recordingEngine) Execute(_ context.Context, query string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, query)
	return nil, e.err
}

func (e *recordingEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type stubClaimer struct {
	allow bool
	keys  []string
}

func (c *stubClaimer) ClaimWithTTL(_ context.Context, _, dedupKey string, _ time.Duration) bool {
	c.keys = append(c.keys, dedupKey)
	return c.allow
}

// pollerAgainst wires a poller to a fake mailbox and a recording engine, with
// the production Graph sender in between -- the URL building, the token
// cache and the HTTP verbs are all the real ones.
func pollerAgainst(t *testing.T, srv *ndrTestServer, engine NDREngine, claimer NDRClaimer) *NDRPoller {
	t.Helper()
	t.Setenv(NDRPollSecondsEnv, "60")
	sender := NewGraphSender(GraphConfig{
		TenantId: "tenant", ClientId: "client", ClientSecret: "secret-value",
		SenderAddr: "no-reply@example.test", FromName: "MemQL",
	}, graphRoutedClient(srv.Server), nil)
	return NewNDRPoller(engine, claimer, func() Sender { return sender }, true, nil)
}

// --- what the reader stages ---------------------------------------------

func TestNDRStagesARealDeliveryReport(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())

	calls := engine.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one staging call, got %d: %v", len(calls), calls)
	}
	args := parseStagedCall(t, calls[0])

	if args["source"] != NDRSource {
		t.Errorf("source = %q, want %q -- this is the value the operator lists in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES", args["source"], NDRSource)
	}
	if args["medium"] != NDRMedium {
		t.Errorf("medium = %q, want %q", args["medium"], NDRMedium)
	}
	if args["signatureVerified"] != "true" {
		t.Errorf("signatureVerified = %q, want true -- the field is what gates campaignIngestFeedback", args["signatureVerified"])
	}
	if args["dedupeKey"] != "AAMkAD-message-1" {
		t.Errorf("dedupeKey = %q, want the Graph message id", args["dedupeKey"])
	}
	if args["requestId"] != ndrRequestID("AAMkAD-message-1") {
		t.Errorf("requestId = %q, want the derived id %q", args["requestId"], ndrRequestID("AAMkAD-message-1"))
	}
	if args["receivedAt"] != "2026-09-01T09:00:00Z" {
		t.Errorf("receivedAt = %q, want the mailbox's own timestamp", args["receivedAt"])
	}
	if !strings.HasPrefix(args["contentType"], "multipart/report") {
		t.Errorf("contentType = %q, want the message's own multipart/report header", args["contentType"])
	}

	// The staged body must be the MIME BYTE FOR BYTE. The campaigns DSN
	// parser scans it for Final-Recipient / Status, so any rewrite here
	// changes a verdict about somebody's address into a verdict about our
	// transformation of it.
	if args["body"] != realDSN {
		t.Errorf("the staged body is not the fetched MIME.\n  got %d bytes\n want %d bytes", len(args["body"]), len(realDSN))
	}
	for _, field := range []string{"Final-Recipient: rfc822; dead@example.invalid", "Status: 5.1.1", "Action: failed"} {
		if !strings.Contains(args["body"], field) {
			t.Errorf("the staged body lost %q, which is what the feedback parser keys on", field)
		}
	}

	// ...and only THEN is the message stamped.
	patch, ok := srv.wasPatched("AAMkAD-message-1")
	if !ok {
		t.Fatal("a staged message was not marked processed, so it will be fetched again forever")
	}
	if !strings.Contains(patch, `"isRead":true`) || !strings.Contains(patch, ndrProcessedCategory) {
		t.Errorf("the PATCH must set BOTH isRead and the category: %s", patch)
	}
	if p.Staged() != 1 {
		t.Errorf("staged counter = %d, want 1", p.Staged())
	}
}

// parseStagedCall drives the REAL lexer and parser over a rendered mutation
// and returns its named arguments.
//
// Both halves are load-bearing. The PARSER proves the statement is
// syntactically a call the engine would accept; the LEXER's decoded literals
// prove the values survived quoting -- which is where a %q-rendered control
// byte would be lost, since the MemQL lexer implements only the JSON escape
// set and rejects Go's \x00, \a and \v outright.
func parseStagedCall(t *testing.T, call string) map[string]string {
	t.Helper()
	tokens, err := langparser.NewLexer(call).Tokenize()
	if err != nil {
		t.Fatalf("the MemQL lexer refused the rendered mutation, so this would stage nothing at runtime: %v\n%s", err, call)
	}
	if _, err := langparser.NewParser(tokens).Parse(); err != nil {
		t.Fatalf("the MemQL parser refused the rendered mutation: %v\n%s", err, call)
	}

	args := map[string]string{}
	for i := 0; i+2 < len(tokens); i++ {
		if tokens[i].Type != langparser.TokenIdentifier || tokens[i+1].Type != langparser.TokenColon {
			continue
		}
		args[tokens[i].Literal] = tokens[i+2].Literal
	}
	return args
}

// --- identity and redelivery --------------------------------------------

func TestNDRRequestIDIsStableAndInjective(t *testing.T) {
	// Stable: a message re-read because the PATCH failed last pass renders
	// the same row id, and @createOnly then preserves whatever the product
	// has already done with it.
	if ndrRequestID("AAMkAD-1") != ndrRequestID("AAMkAD-1") {
		t.Fatal("the derived id is not stable across a redelivery")
	}
	// Injective: two messages collapsing onto one row is a bounce that, as
	// far as the graph is concerned, never happened.
	if ndrRequestID("AAMkAD-1") == ndrRequestID("AAMkAD-2") {
		t.Fatal("two distinct message ids produced one row id")
	}
	if !strings.HasPrefix(ndrRequestID("x"), "inb") {
		t.Errorf("the id must carry the inbound prefix: %q", ndrRequestID("x"))
	}
}

func TestNDRRedeliveryStagesOntoTheSameRow(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())
	// The same message, offered again -- what happens when a PATCH failed,
	// or when a second replica reads the mailbox before the first stamps it.
	p.PollOnce(context.Background())

	calls := engine.recorded()
	if len(calls) != 2 {
		t.Fatalf("expected two staging calls, got %d", len(calls))
	}
	first, second := parseStagedCall(t, calls[0]), parseStagedCall(t, calls[1])
	if first["requestId"] != second["requestId"] {
		t.Errorf("a redelivery rendered a DIFFERENT row id (%q vs %q), so it would duplicate rather than collapse",
			first["requestId"], second["requestId"])
	}
}

// --- every failure leaves the message unread -----------------------------

func TestNDRFetchFailureLeavesTheMessageUnread(t *testing.T) {
	srv := newNDRTestServer(t)
	// Listed, but no body registered: $value answers 500.
	srv.hold("AAMkAD-broken", "")
	engine := &recordingEngine{}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())

	if calls := engine.recorded(); len(calls) != 0 {
		t.Errorf("a message that could not be fetched was staged anyway: %v", calls)
	}
	if _, ok := srv.wasPatched("AAMkAD-broken"); ok {
		t.Error("a message that could not be fetched was marked processed -- that discards a bounce permanently")
	}
	if p.Failed() != 1 {
		t.Errorf("failed counter = %d, want 1", p.Failed())
	}
}

func TestNDRStagingFailureLeavesTheMessageUnread(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{err: errors.New("engine not ready")}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())

	if _, ok := srv.wasPatched("AAMkAD-message-1"); ok {
		t.Error("a message whose staging failed was marked processed, so the row will never exist and nothing will look again")
	}
	if p.Staged() != 0 || p.Failed() != 1 {
		t.Errorf("counters: staged=%d failed=%d, want 0 / 1", p.Staged(), p.Failed())
	}
}

func TestNDRRefusesBytesItCannotStore(t *testing.T) {
	srv := newNDRTestServer(t)
	// A NUL inside the report. Valid UTF-8, and impossible in a JSONB
	// column -- the class that made the webhook path retry forever against a
	// row that could never exist (memql#3098).
	srv.hold("AAMkAD-nul", strings.Replace(realDSN, "Hello --", "Hello \x00--", 1))
	engine := &recordingEngine{}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())

	if calls := engine.recorded(); len(calls) != 0 {
		t.Errorf("a body carrying a NUL was staged: %v", calls)
	}
	if _, ok := srv.wasPatched("AAMkAD-nul"); ok {
		t.Error("an unstageable message was marked processed")
	}
}

// --- what it leaves alone ------------------------------------------------

func TestNDRLeavesOrdinaryMailUntouched(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-human", ordinaryMail)
	engine := &recordingEngine{}
	p := pollerAgainst(t, srv, engine, nil)

	p.PollOnce(context.Background())

	if calls := engine.recorded(); len(calls) != 0 {
		t.Errorf("a person's message was staged as a delivery report: %v", calls)
	}
	if _, ok := srv.wasPatched("AAMkAD-human"); ok {
		t.Error("a person's message was marked read; to them that is indistinguishable from somebody having opened it")
	}
	if p.Skipped() != 1 {
		t.Errorf("skipped counter = %d, want 1", p.Skipped())
	}
}

func TestNDRDoesNothingWithoutAGraphSender(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{}
	t.Setenv(NDRPollSecondsEnv, "60")
	p := NewNDRPoller(engine, nil, func() Sender { return NewLogSender(nil) }, true, nil)

	p.PollOnce(context.Background())

	if calls := engine.recorded(); len(calls) != 0 {
		t.Errorf("a non-Graph node read a mailbox it does not have: %v", calls)
	}
	if srv.listCalls != 0 {
		t.Errorf("a non-Graph node made %d mailbox requests", srv.listCalls)
	}
}

// TestNDRResolvesThroughALazySender is the case a node that READS but never
// SENDS lands in: the lazy wrapper's resolution has never run, and asking it
// is the only way to learn this node's sender is Graph.
func TestNDRResolvesThroughALazySender(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{}
	t.Setenv(NDRPollSecondsEnv, "60")

	graph := NewGraphSender(GraphConfig{
		TenantId: "tenant", ClientId: "client", ClientSecret: "secret-value",
		SenderAddr: "no-reply@example.test",
	}, graphRoutedClient(srv.Server), nil)
	lazy := NewLazySender(graph, nil, nil, nil)

	p := NewNDRPoller(engine, nil, func() Sender { return lazy }, true, nil)
	p.PollOnce(context.Background())

	if len(engine.recorded()) != 1 {
		t.Error("the reader could not see through the lazy wrapper, so a node that never sends would never read either")
	}
}

func TestNDRClaimGatesTheWholePass(t *testing.T) {
	srv := newNDRTestServer(t)
	srv.hold("AAMkAD-message-1", realDSN)
	engine := &recordingEngine{}
	claimer := &stubClaimer{allow: false}
	p := pollerAgainst(t, srv, engine, claimer)

	p.PollOnce(context.Background())

	if srv.listCalls != 0 {
		t.Errorf("a replica that lost the claim listed the mailbox anyway (%d times); the listing is the racy part", srv.listCalls)
	}
	if len(claimer.keys) != 1 {
		t.Fatalf("expected exactly one claim attempt, got %v", claimer.keys)
	}
	// The key buckets by interval, so two replicas ticking seconds apart
	// still collide. A wall-clock key would let both through.
	if !strings.HasPrefix(claimer.keys[0], "no-reply@example.test@") {
		t.Errorf("claim key %q does not name the mailbox", claimer.keys[0])
	}
}

// --- configuration -------------------------------------------------------

func TestNDRPollIntervalReadsTheEnvironment(t *testing.T) {
	t.Run("unset is the default", func(t *testing.T) {
		t.Setenv(NDRPollSecondsEnv, "")
		if got := NDRPollInterval(nil); got != defaultNDRPollSeconds*time.Second {
			t.Errorf("interval = %v, want %v", got, defaultNDRPollSeconds*time.Second)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Setenv(NDRPollSecondsEnv, "0")
		if got := NDRPollInterval(nil); got != 0 {
			t.Errorf("interval = %v, want 0 (disabled)", got)
		}
	})
	t.Run("a value is honoured", func(t *testing.T) {
		t.Setenv(NDRPollSecondsEnv, "45")
		if got := NDRPollInterval(nil); got != 45*time.Second {
			t.Errorf("interval = %v, want 45s", got)
		}
	})
	t.Run("a typo falls back to the default, not to off", func(t *testing.T) {
		// The direction matters: silently disabling a compliance-relevant
		// feed because somebody typed "5m" is the failure this whole file
		// exists to remove.
		t.Setenv(NDRPollSecondsEnv, "5m")
		if got := NDRPollInterval(nil); got != defaultNDRPollSeconds*time.Second {
			t.Errorf("interval = %v, want the default", got)
		}
	})
}

func TestNDRPollerDoesNotStartWhenCampaignsAreOff(t *testing.T) {
	t.Setenv(NDRPollSecondsEnv, "60")
	p := NewNDRPoller(&recordingEngine{}, nil, func() Sender { return nil }, false, nil)
	p.Start(context.Background())
	select {
	case <-p.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Start never signalled ready")
	}
	if p.IsRunning() {
		t.Error("the reader started on a node where nothing consumes a bounce")
	}
	p.Stop(context.Background())
}

func TestNDRPollerDoesNotStartWhenDisabled(t *testing.T) {
	t.Setenv(NDRPollSecondsEnv, "0")
	p := NewNDRPoller(&recordingEngine{}, nil, func() Sender { return nil }, true, nil)
	p.Start(context.Background())
	select {
	case <-p.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Start never signalled ready")
	}
	if p.IsRunning() {
		t.Error("the reader started with the poll interval set to 0")
	}
	p.Stop(context.Background())
}

// TestNDRRenderedLiteralsSurviveTheRealLexer is the negative control for the
// quoting choice: every one of these inputs is something a DSN can carry, and
// Go's %q spells three of them with escapes the MemQL lexer rejects outright.
func TestNDRRenderedLiteralsSurviveTheRealLexer(t *testing.T) {
	for _, body := range []string{
		realDSN,
		"quotes \" and backslashes \\ and both \\\"",
		"control \x01\x07\x0b bytes",
		"unicode: café — \U0001F600",
		"html-ish <b>&</b>",
		"",
	} {
		call := renderNDRStageMutation("inb-test", body, "multipart/report", "msg-1", time.Unix(0, 0).UTC())
		args := parseStagedCall(t, call)
		if args["body"] != body {
			t.Errorf("the body did not round-trip through the lexer.\n  in:  %q\n  out: %q", body, args["body"])
		}
	}
}
