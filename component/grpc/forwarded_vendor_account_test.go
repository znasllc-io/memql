package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// forwarded_vendor_account_test.go -- which account pays for a chat that
// crossed a node boundary (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// mesh rule M6 and safety rule S8).
//
// A chat a person sends reaches a bff and is forwarded to an agent, which makes
// the model call. Which account pays is decided THERE, on the receiving node,
// from that node's own rows, for the person the forward says is calling. So
// two things have to hold on the hop, and each fails silently when it does
// not: the forward must carry WHO is calling in a form the receiver binds as
// the acting person (or the receiver finds no keys and quietly pays with the
// cluster's account, or with nothing), and it must carry NOTHING about an
// account, least of all a key (or a key has left the node that unsealed it).
//
// It drives the production producer and receiver halves over the same two
// adapters the router and the handler use, as forwarded_call_account_test.go
// does for the organization of a call.

// forwardedVendor stands in for a vendor text record on the receiving node.
type forwardedVendor struct{ paidBy memqlengine.VendorAccount }

func (v *forwardedVendor) WithVendorAccount(account memqlengine.VendorAccount, _ memqlengine.VendorKeys) any {
	return &forwardedVendor{paidBy: account}
}
func (v *forwardedVendor) VendorOrigin() string                      { return "" }
func (v *forwardedVendor) Call(context.Context, string) (any, error) { return "answer", nil }
func (v *forwardedVendor) CallChat(context.Context, []common.ChatMessage) (string, error) {
	return "answer", nil
}

// receivingNodeKeys is the receiving node's key seam: everybody's keys, and
// what it was asked.
type receivingNodeKeys struct {
	byOwner map[string]string
	asked   []memqlengine.VendorAccountQuery
}

func (k *receivingNodeKeys) KeysFor(_ context.Context, q memqlengine.VendorAccountQuery) ([]memqlengine.VendorAccount, []memqlengine.VendorAccountPassedOver, error) {
	k.asked = append(k.asked, q)
	keyId, ok := k.byOwner[memqlengine.BareShortId(q.ActingUserId)]
	if !ok {
		return nil, nil, nil
	}
	return []memqlengine.VendorAccount{{
		Kind: memqlengine.VendorAccountKey, Vendor: q.Vendor, KeyId: keyId, OwnerUserId: memqlengine.BareShortId(q.ActingUserId),
	}}, nil, nil
}

func (k *receivingNodeKeys) Usable(ctx context.Context, q memqlengine.VendorAccountQuery, keyId string) (memqlengine.VendorAccount, string, bool) {
	usable, _, _ := k.KeysFor(ctx, q)
	for _, account := range usable {
		if account.KeyId == keyId {
			return account, "", true
		}
	}
	return memqlengine.VendorAccount{}, "not usable", false
}

func (k *receivingNodeKeys) Secret(context.Context, memqlengine.VendorAccount) (string, error) {
	return "", nil
}

func (k *receivingNodeKeys) Observe(context.Context, memqlengine.VendorAccount, memqlengine.VendorKeyVerdict, time.Time, string) {
}

func TestAForwardedChatIsPaidByTheRightKey(t *testing.T) {
	// THE PRODUCER: a bff session for alice forwards her chat.
	sess := callAccountSession(context.Background())
	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal: %v", err)
	}
	envelope, err := proto.Marshal(&memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AiChat{AiChat: &memqlv1.AiChatMsg{
			RequestId: "r1", Messages: []*memqlv1.AiChatMessage{{Role: "user", Content: "hello"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	forward := &nodev1.AiForwardRequest{
		RequestId:     "r1",
		Auth:          principal.Claims,
		MemqlEnvelope: envelope,
		Authority:     forwardedAuthorityToProto(principal.Authority, "bff-1", "bff"),
	}

	// NOTHING ABOUT AN ACCOUNT CROSSES THE HOP. Asserted on the message TYPES,
	// so a field added later for convenience fails here: the forward, the
	// authority it carries, and the chat inside it.
	for _, message := range []protoreflect.MessageDescriptor{
		forward.ProtoReflect().Descriptor(),
		forward.GetAuthority().ProtoReflect().Descriptor(),
		(&memqlv1.AiChatMsg{}).ProtoReflect().Descriptor(),
		(&memqlv1.AiChatMessage{}).ProtoReflect().Descriptor(),
	} {
		fields := message.Fields()
		for i := 0; i < fields.Len(); i++ {
			name := strings.ToLower(string(fields.Get(i).Name()))
			for _, word := range []string{"api_key", "apikey", "vendor_key", "vendorkey", "secret", "account_kind", "account_key", "paid_by"} {
				if strings.Contains(name, word) {
					t.Errorf("%s.%s: a forwarded call carries who is calling, and the receiving node decides who pays from its own rows. "+
						"A field that names a key or an account takes that decision, or the key, off the node that holds it.", message.FullName(), fields.Get(i).Name())
				}
			}
		}
	}
	for claim := range forward.GetAuth() {
		if strings.Contains(strings.ToLower(claim), "key") || strings.Contains(strings.ToLower(claim), "secret") {
			t.Errorf("the forwarded claims carry %q", claim)
		}
	}

	// THE RECEIVER: an agent binds the forward, and holds nothing of the
	// originating call but what the wire carried.
	received := forwardedAuthorityFromProto(forward.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(received, time.Now())
	if err != nil {
		t.Fatalf("the receiver refused a well-formed assertion: %v", err)
	}
	remote := bindForwardedContext(context.Background(), forward.GetAuth(), access, received)

	// Its own registry, its own keys. Bob holds a key here too, and it is the
	// earlier one in every list: a receiver that answered for anybody but the
	// forwarded person would pick it.
	keys := &receivingNodeKeys{byOwner: map[string]string{"alice": "k-alice", "bob": "k-bob"}}
	providers := memqlengine.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("claude", "anthropic", "claude-sonnet", map[string]any{"contextWindow": 200000},
		&forwardedVendor{paidBy: memqlengine.VendorAccount{Kind: memqlengine.VendorAccountCluster, Vendor: memqlengine.VendorAnthropic}})
	entry, _ := providers.Entry("claude")
	entry.Available = false // this cluster has no account at the vendor: a key pays, or nothing does
	providers.SetVendorKeys(keys)

	rules := memqlengine.NewRuleRegistry()
	if err := rules.Register(&memqlengine.RuleConfig{
		Name: memqlengine.DefaultRuleName, When: memqlengine.RuleWhen{Present: map[string]bool{}},
		Policy: "vendor", OnUnavailable: memqlengine.OnUnavailableDegrade, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	r := router.New(providers, memqlengine.NewPolicyRegistryForTest(map[string][]string{"vendor": {"claude"}}), rules, nil, nil)

	// The agent's model call names no user: the acting person is whoever the
	// forward bound.
	resolved, err := r.ResolveFor(remote, airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat})
	if err != nil {
		t.Fatalf("the forwarded chat found no account on the receiving node: %v\n\n"+
			"The forward must bind the caller as the acting person, or the receiver asks for nobody's keys.", err)
	}
	decision := resolved.Resolution.Decision
	if decision.AccountKind != memqlengine.VendorAccountKey || decision.AccountKeyId != "k-alice" || decision.AccountKeyOwnerUserId != "alice" {
		t.Errorf("the forwarded chat is paid by %s %q of %q, want alice's key",
			decision.AccountKind, decision.AccountKeyId, decision.AccountKeyOwnerUserId)
	}
	if len(keys.asked) == 0 {
		t.Fatal("the receiving node never asked for the caller's keys")
	}
	for _, q := range keys.asked {
		if memqlengine.BareShortId(q.ActingUserId) != "alice" || q.ForVisitor {
			t.Errorf("the receiving node asked for %+v, want the forwarded person and nobody else", q)
		}
	}
}
