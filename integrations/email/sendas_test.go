package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sendas_test.go -- the send-as-identity parameter (design D5) and the From
// display-name escaping it forced (design D6), memql#4821.
//
// The load-bearing assertion in the Graph cases is that the THREE places one
// identity is spent -- the sendMail URL, the payload's `from`, and the MIME
// `From:` header -- all move together. Asserting on any one of them alone
// would pass while the other two drifted, and a drift there is a message
// whose envelope and header name different mailboxes: a DMARC alignment
// failure that reaches nobody as an error.

// graphTestServer stands in for Microsoft Graph. It answers the token
// endpoint and records the sendMail request, so a test can read back the URL
// path and the body the sender actually built.
type graphTestServer struct {
	*httptest.Server
	sendPath string
	sendBody string
}

func newGraphTestServer(t *testing.T) *graphTestServer {
	t.Helper()
	g := &graphTestServer{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "/oauth2/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		g.sendPath = r.URL.Path
		g.sendBody = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(g.Close)
	return g
}

// graphSenderAgainst points a GraphSender at the test server. Graph's real
// endpoints are absolute URLs built inside the sender, so the redirection is
// done with a RoundTripper rather than by configuration -- which also keeps
// the production URL-building code under test instead of replacing it.
func graphSenderAgainst(t *testing.T, srv *graphTestServer, cfg GraphConfig) *GraphSender {
	t.Helper()
	return NewGraphSender(cfg, graphRoutedClient(srv.Server), nil)
}

// graphRoutedClient rewrites every absolute graph.microsoft.com /
// login.microsoftonline.com URL onto a test server, preserving the PATH and
// the QUERY. Both matter: the path segment is where the send-as mailbox
// lands, and the query is where the mailbox reader's $filter and $select go.
func graphRoutedClient(srv *httptest.Server) *http.Client {
	base := srv.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		target := srv.URL + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			return nil, err
		}
		out.Header = r.Header
		return base.RoundTrip(out)
	})}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func defaultGraphConfig() GraphConfig {
	return GraphConfig{
		TenantId:     "tenant-id",
		ClientId:     "client-id",
		ClientSecret: "client-secret-value",
		SenderAddr:   "no-reply@example.test",
		FromName:     "MemQL",
	}
}

func TestSendAsIsZero(t *testing.T) {
	if !(SendAs{}).IsZero() {
		t.Error("the zero value must read as zero; it is the sentinel every transactional caller passes")
	}
	// Whitespace counts as empty. A stored identity whose address is a stray
	// space is not an identity, and honouring it builds /users/%20/sendMail.
	if !(SendAs{Address: "   ", FromName: " \t"}).IsZero() {
		t.Error("a whitespace-only identity must read as zero")
	}
	if (SendAs{FromName: "Acme"}).IsZero() {
		t.Error("a display-name-only identity is NOT zero: it means the default mailbox under a different name")
	}
	if (SendAs{Address: "sales@example.test"}).IsZero() {
		t.Error("an address-only identity is not zero")
	}
}

func TestGraphZeroSendAsUsesTheConfiguredDefault(t *testing.T) {
	srv := newGraphTestServer(t)
	sender := graphSenderAgainst(t, srv, defaultGraphConfig())

	if err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if !strings.Contains(srv.sendPath, "no-reply@example.test") {
		t.Errorf("sendMail path = %q, want the configured default mailbox", srv.sendPath)
	}
	var payload struct {
		Message struct {
			From struct {
				EmailAddress struct {
					Address string `json:"address"`
					Name    string `json:"name"`
				} `json:"emailAddress"`
			} `json:"from"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(srv.sendBody), &payload); err != nil {
		t.Fatalf("payload: %v (body %q)", err, srv.sendBody)
	}
	if got := payload.Message.From.EmailAddress.Address; got != "no-reply@example.test" {
		t.Errorf("payload from address = %q, want the configured default", got)
	}
	if got := payload.Message.From.EmailAddress.Name; got != "MemQL" {
		t.Errorf("payload from name = %q, want the configured default", got)
	}
}

func TestGraphSendAsMovesUrlAndFromTogether(t *testing.T) {
	srv := newGraphTestServer(t)
	sender := graphSenderAgainst(t, srv, defaultGraphConfig())

	if err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{Address: "sales@example.test", FromName: "Acme Sales"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The URL segment. Graph resolves it against the token's tenant and
	// stamps the envelope sender from it.
	if !strings.Contains(srv.sendPath, "sales@example.test") {
		t.Errorf("sendMail path = %q, want the identity mailbox", srv.sendPath)
	}
	if strings.Contains(srv.sendPath, "no-reply@example.test") {
		t.Errorf("sendMail path still names the DEFAULT mailbox: %q", srv.sendPath)
	}
	// ...and the header, which must agree with it.
	if !strings.Contains(srv.sendBody, `"address":"sales@example.test"`) {
		t.Errorf("payload from address did not follow the URL: %s", srv.sendBody)
	}
	if !strings.Contains(srv.sendBody, `"name":"Acme Sales"`) {
		t.Errorf("payload from name did not follow the identity: %s", srv.sendBody)
	}
}

// TestGraphSendAsHalvesFallBackIndependently pins the fold. A campaign that
// overrides only `fromName` must keep the deployment's mailbox -- resolving
// the pair as a unit would silently blank the address and build
// /users//sendMail.
func TestGraphSendAsHalvesFallBackIndependently(t *testing.T) {
	srv := newGraphTestServer(t)
	sender := graphSenderAgainst(t, srv, defaultGraphConfig())

	if err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{FromName: "Spring Sale"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(srv.sendPath, "no-reply@example.test") {
		t.Errorf("a name-only identity moved the mailbox: path %q", srv.sendPath)
	}
	if !strings.Contains(srv.sendBody, `"name":"Spring Sale"`) {
		t.Errorf("a name-only identity did not reach the From name: %s", srv.sendBody)
	}

	if err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{Address: "sales@example.test"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(srv.sendBody, `"name":"MemQL"`) {
		t.Errorf("an address-only identity went nameless instead of taking the default display name: %s", srv.sendBody)
	}
}

// TestGraphSendAsReachesTheMimePath covers the OTHER Graph request form. A
// message with extra headers (every campaign message, because of RFC 8058)
// goes out as base64 MIME, and the From header there is composed by a
// different line of code than the structured payload's.
func TestGraphSendAsReachesTheMimePath(t *testing.T) {
	srv := newGraphTestServer(t)
	sender := graphSenderAgainst(t, srv, defaultGraphConfig())

	if err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
		Headers: map[string]string{"List-Unsubscribe": "<https://example.test/u?t=x>"},
	}, SendAs{Address: "sales@example.test", FromName: "Acme Sales"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(srv.sendPath, "sales@example.test") {
		t.Errorf("MIME send did not move the URL: %q", srv.sendPath)
	}
	raw, err := base64.StdEncoding.DecodeString(srv.sendBody)
	if err != nil {
		t.Fatalf("the MIME form must be base64: %v", err)
	}
	if !strings.Contains(string(raw), "From: Acme Sales <sales@example.test>\r\n") {
		t.Errorf("the rendered From header did not follow the identity:\n%s", raw)
	}
}

func TestGraphRefusesAnIdentityThatCannotGoOnAHeader(t *testing.T) {
	srv := newGraphTestServer(t)
	sender := graphSenderAgainst(t, srv, defaultGraphConfig())

	// The structured payload never passes through RenderRFC5322, so without
	// SendAs.Validate this reaches Graph as a JSON-escaped control byte.
	err := sender.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{Address: "sales@example.test", FromName: "Acme\r\nBcc: victim@example.test"})
	if err == nil {
		t.Fatal("a CRLF in the display name was accepted")
	}
	if !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("want a header-injection refusal, got %v", err)
	}
	if srv.sendPath != "" {
		t.Error("the refusal happened AFTER a request reached the provider")
	}
}

// --- the SMTP side ------------------------------------------------------

func TestSMTPRefusesANonDefaultIdentityPermanently(t *testing.T) {
	// Host deliberately unreachable: a correct implementation refuses before
	// it dials, so this test never touches the network.
	s := NewSMTPSender(SMTPConfig{
		Host: "127.0.0.1", Port: "1", FromAddr: "no-reply@example.test", FromName: "MemQL",
	}, nil)

	err := s.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{Address: "sales@example.test", FromName: "Acme Sales"})
	if err == nil {
		t.Fatal("SMTP accepted a send as a mailbox its AUTH is not bound to")
	}
	var se *SendError
	if !errors.As(err, &se) || !se.Permanent {
		// Retryable would park the campaign forever: no amount of waiting
		// turns an SMTP relay into a multi-mailbox one.
		t.Fatalf("the refusal must be a PERMANENT SendError, got %#v (%v)", se, err)
	}
	for _, want := range []string{"sales@example.test", "no-reply@example.test", DefaultGraphEnvKeys().SenderAddr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// TestSMTPAcceptsADisplayNameOnlyIdentity is the other half of the rule. Only
// the ADDRESS is bound by AUTH; a different From phrase changes nothing about
// authentication, so refusing it would block a campaign for no reason.
func TestSMTPAcceptsADisplayNameOnlyIdentity(t *testing.T) {
	s := NewSMTPSender(SMTPConfig{
		Host: "127.0.0.1", Port: "1", FromAddr: "no-reply@example.test", FromName: "MemQL",
	}, nil)
	err := s.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{FromName: "Spring Sale"})
	var se *SendError
	if errors.As(err, &se) && se.Permanent {
		t.Fatalf("a display-name-only identity was refused as if it were a mailbox change: %v", err)
	}
	// It fails at the DIAL, which is the proof it got past the identity gate.
	if err == nil {
		t.Fatal("expected a connection failure against the unreachable relay")
	}
}

// TestSMTPCaseInsensitiveIdentityMatch: a mailbox address is case-insensitive
// in its domain and, in practice, in its local part too for every provider
// this ships against. Refusing "Sales@..." against "sales@..." would fail a
// send that is asking for exactly the configured mailbox.
func TestSMTPCaseInsensitiveIdentityMatch(t *testing.T) {
	s := NewSMTPSender(SMTPConfig{
		Host: "127.0.0.1", Port: "1", FromAddr: "no-reply@example.test",
	}, nil)
	err := s.Send(context.Background(), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	}, SendAs{Address: "NO-REPLY@Example.Test"})
	var se *SendError
	if errors.As(err, &se) && se.Permanent {
		t.Fatalf("the configured mailbox was refused on case alone: %v", err)
	}
}

// --- D6: the display name on the wire ------------------------------------

func TestFromHeaderQuotesWhatRFC5322Requires(t *testing.T) {
	const addr = "no-reply@example.test"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain stays bare", "MemQL", "MemQL <" + addr + ">"},
		{"an atext-only phrase stays bare", "MemQL Support Team", "MemQL Support Team <" + addr + ">"},
		// The comma is the case that motivated D6: unquoted, a strict parser
		// reads `Acme, Inc. <a@b>` as TWO addresses.
		{"comma", "Acme, Inc.", `"Acme, Inc." <` + addr + ">"},
		{"colon", "Support: Billing", `"Support: Billing" <` + addr + ">"},
		{"semicolon", "A;B", `"A;B" <` + addr + ">"},
		{"angle brackets", "<Acme>", `"<Acme>" <` + addr + ">"},
		{"quote is escaped", `The "Best" Co`, `"The \"Best\" Co" <` + addr + ">"},
		{"backslash is escaped", `Acme\Sales`, `"Acme\\Sales" <` + addr + ">"},
		{"non-ascii", "Café MemQL", `"Café MemQL" <` + addr + ">"},
		{"empty name yields a bare address", "", addr},
		{"whitespace-only name yields a bare address", "   ", addr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromHeader(addr, tc.in); got != tc.want {
				t.Errorf("FromHeader(%q, %q) = %q, want %q", addr, tc.in, got, tc.want)
			}
		})
	}
}

// TestFromHeaderQuotingSurvivesTheRenderer closes the loop: the quoted value
// has to be what actually lands on the header line, not something the
// renderer re-escapes or refuses.
func TestFromHeaderQuotingSurvivesTheRenderer(t *testing.T) {
	raw, err := RenderRFC5322(FromHeader("no-reply@example.test", "Acme, Inc."), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(raw), "From: \"Acme, Inc.\" <no-reply@example.test>\r\n") {
		t.Errorf("the quoted display name did not reach the wire:\n%s", raw)
	}
}

// TestFromHeaderStillCannotSmuggleAHeader: quoting must not have turned a
// control byte into something the renderer now accepts. The refusal stays
// where it was -- at serialization -- and this is the negative control for it.
func TestFromHeaderStillCannotSmuggleAHeader(t *testing.T) {
	_, err := RenderRFC5322(FromHeader("no-reply@example.test", "Acme\r\nBcc: victim@example.test"), Message{
		To: "person@example.test", Subject: "Hi", TextBody: "body",
	})
	if err == nil {
		t.Fatal("a CRLF in the display name rendered; quoting must not neutralize the injection barrier")
	}
	if !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("want a header-injection refusal, got %v", err)
	}
}
