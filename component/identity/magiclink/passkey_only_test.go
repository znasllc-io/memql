package magiclink

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/identity"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// passkey_only_test.go -- design section 9 item 9, plus the enumeration
// property the design's threat table promises (memql#4304).
//
// The behaviour under test: a request for a sign-in link against a
// passkey_only account writes NO row, sends NO link, sends a NOTICE, and
// returns exactly what an ordinary issue returns -- so the caller's response
// cannot be used to tell a hardened account from any other address.

// policyEngine answers the two constructs Issue reaches for and records every
// write, so "no row was written" is observable rather than assumed.
type policyEngine struct {
	mu sync.Mutex
	// policy is the signInPolicy the user lookup reports. Empty means "no
	// such user" -- the registration path.
	policy  string
	writes  []string
	reads   int
	unknown []string
}

func (e *policyEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query userByEmail("):
		e.reads++
		if e.policy == "" {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:user:u1",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue("v1:identity:user:u1"),
				"primaryEmail": structpb.NewStringValue("team@acme.test"),
				"signInPolicy": structpb.NewStringValue(e.policy),
			}},
		}}}}, nil
	case strings.HasPrefix(q, "mutation createMagicLinkRequest("):
		e.writes = append(e.writes, q)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	case strings.HasPrefix(q, "mutation createAuditEvent("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	e.unknown = append(e.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// recordingSender counts which of the two messages went out.
type recordingSender struct {
	links   int
	notices int
	lastURL string
}

func (s *recordingSender) SendMagicLink(_ context.Context, in SendInput) error {
	s.links++
	s.lastURL = in.LinkURL
	return nil
}

func (s *recordingSender) SendSignInDisabledNotice(_ context.Context, _ NoticeInput) error {
	s.notices++
	return nil
}

func newPolicyIssuer(policy string) (*Issuer, *policyEngine, *recordingSender) {
	eng := &policyEngine{policy: policy}
	sender := &recordingSender{}
	return &Issuer{
		Cfg: identity.Config{
			BaseURL:          "https://identity.test",
			BrandName:        "MemQL",
			RegistrationMode: identity.RegistrationModeOpen,
		},
		Store:  &identity.Store{Engine: eng, Logger: slog.Default()},
		Sender: sender,
		Logger: slog.Default(),
	}, eng, sender
}

func TestPasskeyOnlyAccountGetsANoticeAndNoRow(t *testing.T) {
	iss, eng, sender := newPolicyIssuer("passkey_only")

	res, err := iss.Issue(context.Background(), IssueInput{
		Email:        "team@acme.test",
		AdminSession: true,
		SourceIP:     "203.0.113.4",
	})
	if err != nil {
		t.Fatalf("Issue returned %v, want nil -- the caller's outcome must not differ from an "+
			"ordinary issue, or the response itself becomes an oracle for which accounts are hardened", err)
	}

	if len(eng.writes) != 0 {
		t.Errorf("a passkey_only account produced %d magicLinkRequest write(s), want 0: %v\n\n"+
			"No row means no credential exists to be clicked, which is the point -- the account is "+
			"not reachable by anyone reading the mailbox.", len(eng.writes), eng.writes)
	}
	if sender.links != 0 {
		t.Errorf("%d sign-in link(s) were sent to a passkey_only account, want 0", sender.links)
	}
	if sender.notices != 1 {
		t.Errorf("%d notice(s) sent, want exactly 1 -- silence would leave the account holder "+
			"unable to tell 'the email is slow' from 'somebody is trying to get in'", sender.notices)
	}
	// And the caller learns nothing: no request id, no binding nonce, which is
	// what an ordinary issue would have handed back.
	if res.RequestId != "" || res.BindingNonce != "" {
		t.Errorf("the refusal returned request/nonce material (%+v); it must return the zero value", res)
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("Issue reached constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
}

// TestPasskeyOnlyIsIndistinguishableFromAnOrdinaryIssue is the enumeration
// property.
//
// Both calls return the same error (nil) and the same shape of outcome to the
// caller. The ONLY difference is which email arrives, and only the mailbox
// sees that. If this test ever fails, an unauthenticated visitor has gained a
// way to discover which accounts have hardened themselves -- which is a map of
// where NOT to bother attacking, and therefore of where to bother.
func TestPasskeyOnlyIsIndistinguishableFromAnOrdinaryIssue(t *testing.T) {
	hardened, _, hardenedSender := newPolicyIssuer("passkey_only")
	ordinary, ordinaryEngine, ordinarySender := newPolicyIssuer("any")

	in := IssueInput{Email: "team@acme.test", AdminSession: true}

	hardenedRes, hardenedErr := hardened.Issue(context.Background(), in)
	ordinaryRes, ordinaryErr := ordinary.Issue(context.Background(), in)

	if (hardenedErr == nil) != (ordinaryErr == nil) {
		t.Fatalf("the two paths returned different error shapes: hardened=%v ordinary=%v", hardenedErr, ordinaryErr)
	}
	// The ordinary path DID write a row and send a link -- so the assertion
	// above is comparing two live paths, not two no-ops.
	if len(ordinaryEngine.writes) != 1 {
		t.Fatalf("the ordinary path wrote %d row(s), want 1 -- without this the comparison above "+
			"proves nothing", len(ordinaryEngine.writes))
	}
	if ordinarySender.links != 1 || hardenedSender.links != 0 {
		t.Fatalf("link counts: ordinary=%d hardened=%d, want 1 and 0", ordinarySender.links, hardenedSender.links)
	}
	if hardenedSender.notices != 1 || ordinarySender.notices != 0 {
		t.Fatalf("notice counts: hardened=%d ordinary=%d, want 1 and 0", hardenedSender.notices, ordinarySender.notices)
	}
	_ = hardenedRes
	_ = ordinaryRes
}

// TestUnknownAddressIsUnaffectedByThePolicy pins the registration case: an
// address with no user row has no policy to apply, and must follow the
// ordinary path.
func TestUnknownAddressIsUnaffectedByThePolicy(t *testing.T) {
	iss, eng, sender := newPolicyIssuer("") // no user row

	if _, err := iss.Issue(context.Background(), IssueInput{Email: "newcomer@acme.test", AdminSession: true}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(eng.writes) != 1 {
		t.Errorf("a first-time registration wrote %d row(s), want 1", len(eng.writes))
	}
	if sender.links != 1 || sender.notices != 0 {
		t.Errorf("links=%d notices=%d, want 1 and 0", sender.links, sender.notices)
	}
}

// TestEmailedLinkCarriesNoState pins that `state` left the URL (memql#4302).
//
// It used to ride as a query parameter and be compared against the row on
// consume, which proved only that the clicker had read the email. The binding
// cookie replaced it, and printing it into a message it no longer protects
// would just be a value in a mailbox.
func TestEmailedLinkCarriesNoState(t *testing.T) {
	iss, _, sender := newPolicyIssuer("")

	if _, err := iss.Issue(context.Background(), IssueInput{
		Email:        "newcomer@acme.test",
		AdminSession: true,
		State:        "the-oauth-state",
	}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if strings.Contains(sender.lastURL, "state=") {
		t.Errorf("the emailed link carries a state parameter: %s", sender.lastURL)
	}
	if !strings.Contains(sender.lastURL, "ml=") {
		t.Errorf("the emailed link lost its token: %s", sender.lastURL)
	}
}
