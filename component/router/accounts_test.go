package router

// accounts_test.go -- which ACCOUNT pays for a vendor call, and what the
// decision record says about it (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K8, K9, K10, K14, K15, K17 and R7, safety rules S8 and S9).
//
// What a vendor's answer means is classified in component/memql
// (vendor_account_errors_test.go reads each status and body). A vendor here
// returns the classified failure, and these tests pin what the router does
// with each kind.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// vendorCalls records every call the stand-in vendors were asked to make, as
// "<record>@<account>", in the order they were made.
type vendorCalls struct {
	mu   sync.Mutex
	made []string
	// fail is the failure an attempt returns, by "<record>@<account>".
	fail map[string]error
}

func (c *vendorCalls) attempt(record string, account memql.VendorAccount) error {
	name := memql.JoinAccountReference(record, account)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.made = append(c.made, name)
	return c.fail[name]
}

func (c *vendorCalls) attempts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.made...)
}

// accountVendor stands in for one vendor text record. It serves every text
// surface, and it can be bound to an account the way the real clients are
// (memql.VendorAccountClient): the bound copy says who paid.
type accountVendor struct {
	record string
	calls  *vendorCalls
	paidBy memql.VendorAccount
}

func (v *accountVendor) WithVendorAccount(account memql.VendorAccount, _ memql.VendorKeys) any {
	bound := *v
	bound.paidBy = account
	return &bound
}

func (v *accountVendor) VendorOrigin() string { return "" }

func (v *accountVendor) Call(context.Context, string) (any, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return nil, err
	}
	return "answer", nil
}

func (v *accountVendor) CallChat(context.Context, []common.ChatMessage) (string, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return "", err
	}
	return "answer", nil
}

func (v *accountVendor) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return "", err
	}
	return `{"ok":true}`, nil
}

func (v *accountVendor) CallChatWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return nil, err
	}
	return &common.ToolCallingChatResult{AssistantText: "answer"}, nil
}

func (v *accountVendor) CallChatStreamWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return nil, err
	}
	ch := make(chan common.StreamToolChunk, 1)
	ch <- common.StreamToolChunk{Content: "answer", Done: true}
	close(ch)
	return ch, nil
}

func (v *accountVendor) CallChatStream(context.Context, []common.ChatMessage) (<-chan common.StreamChunk, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return nil, err
	}
	ch := make(chan common.StreamChunk, 1)
	ch <- common.StreamChunk{Content: "answer", Done: true}
	close(ch)
	return ch, nil
}

func (v *accountVendor) CallVision(context.Context, string, []common.VisionContent) (string, error) {
	if err := v.calls.attempt(v.record, v.paidBy); err != nil {
		return "", err
	}
	return "a picture", nil
}

// lentKey is one key the stand-in seam knows about.
type lentKey struct {
	account memql.VendorAccount
	// auto: its owner switched Auto on, so it counts for a call nobody chose
	// a route for.
	auto bool
	// passedOver is K9's reason when the key cannot pay.
	passedOver string
	// label and fingerprint are what a person sees of the key. The router is
	// never told either; they are here so a test can say so.
	label, fingerprint string
}

// accountKeys stands in for integrations/vendorkeys.
type accountKeys struct {
	mu      sync.Mutex
	keys    []lentKey
	queries []memql.VendorAccountQuery
	readErr error
}

func (k *accountKeys) asked() []memql.VendorAccountQuery {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]memql.VendorAccountQuery(nil), k.queries...)
}

func (k *accountKeys) remove(keyId string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	kept := k.keys[:0]
	for _, key := range k.keys {
		if key.account.KeyId != keyId {
			kept = append(kept, key)
		}
	}
	k.keys = kept
}

func (k *accountKeys) KeysFor(_ context.Context, q memql.VendorAccountQuery) ([]memql.VendorAccount, []memql.VendorAccountPassedOver, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.queries = append(k.queries, q)
	if k.readErr != nil {
		return nil, nil, k.readErr
	}
	var usable []memql.VendorAccount
	var passedOver []memql.VendorAccountPassedOver
	for _, key := range k.keys {
		switch {
		case key.account.Vendor != q.Vendor:
		case key.passedOver != "":
			passedOver = append(passedOver, memql.VendorAccountPassedOver{Account: key.account, Reason: key.passedOver})
		case q.Chosen || key.auto:
			usable = append(usable, key.account)
		}
	}
	return usable, passedOver, nil
}

func (k *accountKeys) Usable(ctx context.Context, q memql.VendorAccountQuery, keyId string) (memql.VendorAccount, string, bool) {
	usable, _, err := k.KeysFor(ctx, q)
	if err != nil {
		return memql.VendorAccount{}, "API keys could not be read", false
	}
	for _, account := range usable {
		if account.KeyId == keyId {
			return account, "", true
		}
	}
	return memql.VendorAccount{}, "not a key this person can use", false
}

func (k *accountKeys) Secret(context.Context, memql.VendorAccount) (string, error) {
	return "", errors.New("the stand-in vendor takes no secret")
}

func (k *accountKeys) Observe(context.Context, memql.VendorAccount, memql.VendorKeyVerdict, time.Time, string) {
}

// accountRecord is one vendor record of a test's registry.
type accountRecord struct {
	name, providerType, model string
	// noClusterAccount: the cluster has no account at this record's vendor.
	// The registry keeps the record's client and marks it unavailable, which
	// is what the loader does (unified_kinds_loader.go).
	noClusterAccount bool
}

var (
	claudeRecord = accountRecord{name: "claude", providerType: "anthropic", model: "claude-sonnet"}
	gptRecord    = accountRecord{name: "gpt", providerType: "openai", model: "gpt-mini"}
)

func (r accountRecord) withoutClusterAccount() accountRecord {
	r.noClusterAccount = true
	return r
}

func anthropicKey(id, owner string) memql.VendorAccount {
	return memql.VendorAccount{Kind: memql.VendorAccountKey, Vendor: memql.VendorAnthropic, KeyId: id, OwnerUserId: owner}
}

// The people of these tests. Alice acts; Bob lent her a key.
const (
	alice = "alice"
	bob   = "bob"
)

var (
	aliceOwnKey = lentKey{account: anthropicKey("k-own", alice), label: "Alice personal", fingerprint: "sk-ant...wxyz"}
	bobLentKey  = lentKey{account: anthropicKey("k-lent", bob), label: "Team key", fingerprint: "sk-ant...abcd"}
)

func switchedOnForAuto(k lentKey) lentKey {
	k.auto = true
	return k
}

func passedOverBecause(k lentKey, reason string) lentKey {
	k.passedOver = reason
	return k
}

// accountFixture is a router over stand-in vendors, a stand-in key seam and a
// ledger that keeps what was written.
type accountFixture struct {
	router    *Router
	providers *memql.ProviderRegistry
	calls     *vendorCalls
	keys      *accountKeys
	ledger    *countingLedger
	// closedTo are the people the cluster's own account does not pay for.
	// Nobody, unless a test says so: most tests here are about the order of
	// accounts, and the cluster's account is one of them.
	closedTo map[string]bool
}

// clusterAccountClosedTo makes the cluster's own account not pay for these
// people, as it is for anybody its owner did not allow.
func (f *accountFixture) clusterAccountClosedTo(people ...string) {
	for _, p := range people {
		f.closedTo[p] = true
	}
}

// The route these tests choose, and the chain Auto's default rule walks. Both
// name the same chain: what differs is who chose.
const (
	accountChosenRoute = "chosenRoute"
	accountAutoPolicy  = "autoChain"
)

func newAccountFixture(t *testing.T, chain []string, keys []lentKey, records ...accountRecord) *accountFixture {
	t.Helper()
	f := &accountFixture{
		providers: memql.NewProviderRegistryForTest(),
		calls:     &vendorCalls{fail: map[string]error{}},
		keys:      &accountKeys{keys: keys},
		ledger:    &countingLedger{writes: make(chan string, 16)},
		closedTo:  map[string]bool{},
	}
	f.providers.SetClusterAccountGate(func(_ context.Context, userId string) bool { return !f.closedTo[userId] })
	for _, rec := range records {
		vendor := memql.VendorAnthropic
		if rec.providerType == "openai" {
			vendor = memql.VendorOpenAI
		}
		f.providers.RegisterWithParamsForTest(rec.name, rec.providerType, rec.model,
			map[string]any{"contextWindow": 200000, "inputCostPerMillion": 3.0, "outputCostPerMillion": 15.0},
			&accountVendor{record: rec.name, calls: f.calls, paidBy: memql.VendorAccount{Kind: memql.VendorAccountCluster, Vendor: vendor}})
		if rec.noClusterAccount {
			entry, _ := f.providers.Entry(rec.name)
			entry.Available = false
		}
	}
	f.providers.SetVendorKeys(f.keys)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		accountAutoPolicy:  chain,
		accountChosenRoute: chain,
	})
	f.router = New(f.providers, policies, testRules(t, defaultRule(accountAutoPolicy)), f.ledger, nil)
	return f
}

func personCtx(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleWriter})
}

// autoRequest is a call nobody chose a route for.
func autoRequest(modality airoute.Modality) ResolveRequest {
	return ResolveRequest{Level: airoute.LevelStrong, Modality: modality, UserId: alice}
}

// chosenRequest is a call under a route its acting person chose.
func chosenRequest(modality airoute.Modality) ResolveRequest {
	req := autoRequest(modality)
	req.Route, req.RouteBy = accountChosenRoute, alice
	return req
}

func helloMessages() []common.ChatMessage {
	return []common.ChatMessage{{Role: "user", Content: "hello"}}
}

// accountOf reads who the decision says paid.
func accountOf(d airoute.Decision) [3]string {
	return [3]string{d.AccountKind, d.AccountKeyId, d.AccountKeyOwnerUserId}
}

func rowAccount(args map[string]any) [3]string {
	kind, _ := args["accountKind"].(string)
	id, _ := args["accountKeyId"].(string)
	owner, _ := args["accountKeyOwnerUserId"].(string)
	return [3]string{kind, id, owner}
}

// consideredLine finds the walk's line for one record paid by one account.
func consideredLine(considered []airoute.ConsideredEntry, entry, account string) (string, bool) {
	for i := len(considered) - 1; i >= 0; i-- {
		if considered[i].Entry == entry && considered[i].Account == account {
			return considered[i].Reason, true
		}
	}
	return "", false
}

// K8 line 4: a route the person chose is paid by their own key, then by keys
// lent to them, then by the cluster's account -- one chain entry per account,
// for the same model.
func TestAChosenRouteIsPaidByThePersonsKeyThenLentKeysThenTheCluster(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord)

	_, resolved, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude@key:k-own", "claude@key:k-lent", "claude@cluster"}
	if !reflect.DeepEqual(resolved.Chain, want) {
		t.Fatalf("chain = %v, want %v", resolved.Chain, want)
	}
	if resolved.ProviderName != "claude" {
		t.Errorf("provider = %q: the record keeps its own name whoever pays", resolved.ProviderName)
	}
	if got := accountOf(resolved.Decision); got != [3]string{memql.VendorAccountKey, "k-own", alice} {
		t.Errorf("the decision says %v paid, want alice's own key", got)
	}
	if reason, ok := consideredLine(resolved.Decision.Considered, "claude", "key:k-own"); !ok || reason != "selected" {
		t.Errorf("the walk's own line for the winner is %q (found %v), want selected with its account: %+v",
			reason, ok, resolved.Decision.Considered)
	}
}

// The cluster that has no account at a vendor still serves a person who holds
// a key there: this is what an API key is for.
func TestAKeyServesAModelTheClusterHasNoAccountFor(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord.withoutClusterAccount())
	ctx := personCtx(alice)

	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatalf("a model the person holds a key for was refused: %v", err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own"}) {
		t.Fatalf("calls = %v, want the one call paid by the person's key", made)
	}
	args := f.ledger.args(t)
	if got := rowAccount(args); got != [3]string{memql.VendorAccountKey, "k-own", alice} || args["outcome"] != "ok" {
		t.Errorf("row = outcome %v paid by %v, want ok paid by alice's key", args["outcome"], got)
	}
	// The cluster's account is on the walk as the account that could not pay.
	if reason, ok := consideredArgLine(args, "claude", memql.VendorAccountCluster); !ok || !strings.Contains(reason, "not set up for Anthropic") {
		t.Errorf("the row does not say why the cluster's account did not pay: %q (found %v)", reason, ok)
	}
}

func consideredArgLine(args map[string]any, entry, account string) (string, bool) {
	list, _ := args["considered"].([]any)
	for i := len(list) - 1; i >= 0; i-- {
		m, _ := list[i].(map[string]any)
		got, _ := m["account"].(string)
		if m["entry"] == entry && got == account {
			reason, _ := m["reason"].(string)
			return reason, true
		}
	}
	return "", false
}

// K8 lines 4 and 5: under Auto only keys whose owner switched Auto on count.
// The router does not decide that; it tells the seam how the route was chosen.
func TestAutoCountsOnlyKeysSwitchedOn(t *testing.T) {
	pinned := func(by string) ResolveRequest {
		req := autoRequest(airoute.ModalityChat)
		req.ExplicitProvider, req.PinnedBy = "claude", by
		return req
	}
	routedBy := func(by string) ResolveRequest {
		req := chosenRequest(airoute.ModalityChat)
		req.RouteBy = by
		return req
	}
	for _, tc := range []struct {
		name   string
		req    ResolveRequest
		chosen bool
		chain  []string
	}{
		{"Auto", autoRequest(airoute.ModalityChat), false, []string{"claude@key:k-lent", "claude@cluster"}},
		{"a route the acting person chose", routedBy(alice), true, []string{"claude@key:k-own", "claude@key:k-lent", "claude@cluster"}},
		{"a route somebody else chose", routedBy(bob), false, []string{"claude@key:k-lent", "claude@cluster"}},
		{"a model the acting person pinned", pinned(alice), true, []string{"claude@key:k-own", "claude@key:k-lent", "claude@cluster"}},
		{"a model somebody else pinned", pinned(bob), false, []string{"claude@key:k-lent", "claude@cluster"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey, switchedOnForAuto(bobLentKey)}, claudeRecord)
			_, resolved, err := f.router.resolveChat(personCtx(alice), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolved.Chain, tc.chain) {
				t.Errorf("chain = %v, want %v", resolved.Chain, tc.chain)
			}
			asked := f.keys.asked()
			if len(asked) == 0 {
				t.Fatal("the key seam was never asked")
			}
			for _, q := range asked {
				if q.Chosen != tc.chosen || q.ActingUserId != alice || q.Vendor != memql.VendorAnthropic || q.ForVisitor {
					t.Errorf("the seam was asked %+v, want chosen=%v for alice at Anthropic", q, tc.chosen)
				}
			}
		})
	}
}

// One walk asks the seam once per vendor, however many of the vendor's records
// the chain names: every ask is a read of rows.
func TestAWalkAsksForAPersonsKeysOncePerVendor(t *testing.T) {
	second := accountRecord{name: "claudeBig", providerType: "anthropic", model: "claude-opus"}
	f := newAccountFixture(t, []string{"claude", "claudeBig", "gpt"}, []lentKey{aliceOwnKey}, claudeRecord, second, gptRecord)

	_, resolved, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude@key:k-own", "claude@cluster", "claudeBig@key:k-own", "claudeBig@cluster", "gpt@cluster"}
	if !reflect.DeepEqual(resolved.Chain, want) {
		t.Fatalf("chain = %v, want %v", resolved.Chain, want)
	}
	perVendor := map[string]int{}
	for _, q := range f.keys.asked() {
		perVendor[q.Vendor]++
	}
	if perVendor[memql.VendorAnthropic] != 1 || perVendor[memql.VendorOpenAI] != 1 {
		t.Errorf("the seam was asked %v times per vendor, want once each", perVendor)
	}
}

// K8 line 1: work with no acting person is the cluster's own, and the
// cluster's own work never spends a key. The seam is not even asked.
func TestWorkWithNoPersonIsPaidByTheClusterOnly(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{switchedOnForAuto(aliceOwnKey)}, claudeRecord)
	ctx := sweepContext()
	req := ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat}

	got, err := f.router.ResolveFor(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@cluster"}) {
		t.Errorf("calls = %v, want the cluster's account", made)
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("the key seam was asked %+v for work no person acts in", asked)
	}
	if got := rowAccount(f.ledger.args(t)); got != [3]string{memql.VendorAccountCluster, "", ""} {
		t.Errorf("the row says %v paid, want the cluster's account", got)
	}

	// And with no cluster account there is nothing to pay with: the model is
	// unavailable, whatever keys exist.
	bare := newAccountFixture(t, []string{"claude"}, []lentKey{switchedOnForAuto(aliceOwnKey)}, claudeRecord.withoutClusterAccount())
	_, err = bare.router.ResolveFor(ctx, req)
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("err = %v, want every door shut", err)
	}
	if len(bare.keys.asked()) != 0 {
		t.Error("the key seam was asked for work no person acts in")
	}
}

// K8 line 2: a call made for a visitor under a person's borrowed authority is
// never paid by that person's key.
func TestAVisitorsCallIsNeverPaidByAKey(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{switchedOnForAuto(aliceOwnKey)}, claudeRecord)
	ctx := auth.ContextWithVisitorBorrow(personCtx(alice))

	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@cluster"}) {
		t.Errorf("calls = %v, want the cluster's account", made)
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("the key seam was asked %+v for a visitor's call", asked)
	}
}

// K9: when no account can pay, the model is unavailable for this person, the
// walk says why on the record's own line, and the route moves on.
func TestNoAccountMeansTheModelIsPassedOverWithItsReason(t *testing.T) {
	const refused = "an API key was refused by the vendor"
	keys := []lentKey{passedOverBecause(aliceOwnKey, refused)}
	f := newAccountFixture(t, []string{"claude", "gpt"}, keys, claudeRecord.withoutClusterAccount(), gptRecord)

	_, resolved, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProviderName != "gpt" || !reflect.DeepEqual(resolved.Chain, []string{"gpt@cluster"}) {
		t.Fatalf("served by %q over %v, want the next source on the route", resolved.ProviderName, resolved.Chain)
	}
	reason, ok := consideredLine(resolved.Decision.Considered, "claude", "")
	if !ok {
		t.Fatalf("the walk does not name the model it passed over: %+v", resolved.Decision.Considered)
	}
	for _, want := range []string{refused, "the cluster's account is not set up for Anthropic"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the model's line is %q, want it to say %q", reason, want)
		}
	}

	// With nothing behind it the call is refused, and the refusal's own list
	// carries the same reason.
	alone := newAccountFixture(t, []string{"claude"}, keys, claudeRecord.withoutClusterAccount())
	_, _, err = alone.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(refusal.Doors) != 1 || refusal.Doors[0].Name != "claude" || !strings.Contains(refusal.Doors[0].Reason, refused) {
		t.Errorf("the refusal's doors are %+v, want claude with its reason", refusal.Doors)
	}
}

// A list of keys that cannot be READ never fails a call the cluster's account
// can serve.
func TestKeysThatCannotBeReadLeaveTheClustersAccount(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
	f.keys.readErr = errors.New("database is restarting")

	_, resolved, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved.Chain, []string{"claude@cluster"}) {
		t.Errorf("chain = %v, want the cluster's account alone", resolved.Chain)
	}
	if got := accountOf(resolved.Decision); got != [3]string{memql.VendorAccountCluster, "", ""} {
		t.Errorf("the decision says %v paid, want the cluster's account", got)
	}
}

// accountFailure is a failure about the account that paid, of one kind. The
// table that turns a vendor's status and body into the kind is
// component/memql's.
func accountFailure(kind string) error {
	return &memql.VendorAccountError{Vendor: memql.VendorAnthropic, Kind: kind, Reason: "the vendor's words, which may quote sk-ant-secret"}
}

// K10: a failure about the account is followed by the SAME model on the next
// account, and each attempt's row names the account it tried.
func TestAccountFailureMovesToTheNextAccountForTheSameModel(t *testing.T) {
	for _, kind := range memql.VendorAccountFailureKinds() {
		t.Run(kind, func(t *testing.T) {
			f := newAccountFixture(t, []string{"claude", "gpt"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord, gptRecord)
			f.calls.fail["claude@key:k-own"] = accountFailure(kind)
			ctx := personCtx(alice)

			got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
				t.Fatalf("the call failed with a second account standing behind the first: %v", err)
			}
			if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own", "claude@key:k-lent"}) {
				t.Fatalf("calls = %v, want the same model on the next account", made)
			}

			failed, retried, served := f.ledger.args(t), f.ledger.args(t), f.ledger.args(t)
			if failed["outcome"] != "error" || rowAccount(failed) != [3]string{memql.VendorAccountKey, "k-own", alice} {
				t.Errorf("first row = outcome %v paid by %v, want the failed attempt on alice's key", failed["outcome"], rowAccount(failed))
			}
			if retried["outcome"] != "fallback_used" || rowAccount(retried) != [3]string{memql.VendorAccountKey, "k-own", alice} {
				t.Errorf("fallback row = outcome %v paid by %v, want it to name the account that failed", retried["outcome"], rowAccount(retried))
			}
			if served["outcome"] != "ok" || served["providerName"] != "claude" ||
				rowAccount(served) != [3]string{memql.VendorAccountKey, "k-lent", bob} {
				t.Errorf("served row = outcome %v provider %v paid by %v, want claude paid by the lent key",
					served["outcome"], served["providerName"], rowAccount(served))
			}
			line, ok := consideredArgLine(served, "claude", "key:k-own")
			if !ok || !strings.HasPrefix(line, failedWhenCalledPrefix+"account_"+kind) {
				t.Errorf("the served row's line for the failed account is %q (found %v), want its failure", line, ok)
			}
			if strings.Contains(line, "sk-ant-secret") {
				t.Errorf("the walk quotes the vendor's words: %q", line)
			}
			if line, ok := consideredArgLine(served, "claude", "key:k-lent"); !ok || line != "selected from fallback chain" {
				t.Errorf("the served row's own line is %q (found %v)", line, ok)
			}
		})
	}
}

// Every kind of account failure has the router's own words on the walk.
func TestEveryAccountFailureKindHasWords(t *testing.T) {
	kinds := memql.VendorAccountFailureKinds()
	if len(kinds) == 0 {
		t.Fatal("no account failure kinds are declared")
	}
	for _, kind := range kinds {
		code := "account_" + kind
		if failureCode(accountFailure(kind)) != code {
			t.Errorf("failureCode = %q, want %q", failureCode(accountFailure(kind)), code)
		}
		if failureWords[code] == "" {
			t.Errorf("the code %q has no words: a reader of the walk would see the code alone", code)
		}
	}
}

// K10: any other failure follows today's fallback, which is the next MODEL.
// The record's other accounts are not tried: a rate limit or an outage is not
// about who paid.
func TestAnotherFailureMovesToTheNextModel(t *testing.T) {
	for name, failure := range map[string]error{
		"a rate limit": errors.New("429 Too Many Requests: rate limit exceeded"),
		"an outage":    errors.New("500 Internal Server Error"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAccountFixture(t, []string{"claude", "gpt"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord, gptRecord)
			f.calls.fail["claude@key:k-own"] = failure
			ctx := personCtx(alice)

			got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
				t.Fatal(err)
			}
			if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own", "gpt@cluster"}) {
				t.Fatalf("calls = %v, want the next model and none of claude's other accounts", made)
			}
		})
	}
}

// The streaming tool surface follows the same order. A vendor's refusal of a
// streaming call is returned before the stream (vendor_stream_preflight_test.go
// in component/memql), which is what lets the wrapper move on.
func TestAStreamingCallFollowsTheSameAccountOrder(t *testing.T) {
	f := newAccountFixture(t, []string{"claude", "gpt"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord, gptRecord)
	f.calls.fail["claude@key:k-own"] = accountFailure("credit")
	f.calls.fail["claude@key:k-lent"] = errors.New("529 overloaded")
	ctx := personCtx(alice)

	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityStreamingTools))
	if err != nil {
		t.Fatal(err)
	}
	if err := invokeDecisionClient(ctx, got.Client, airoute.ModalityStreamingTools); err != nil {
		t.Fatal(err)
	}
	want := []string{"claude@key:k-own", "claude@key:k-lent", "gpt@cluster"}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, want) {
		t.Fatalf("calls = %v, want %v", made, want)
	}
}

// Every text surface a route governs takes an account; the others stay on the
// cluster's account (K17).
func TestEveryTextSurfaceTakesAnAccount(t *testing.T) {
	for _, modality := range []airoute.Modality{
		airoute.ModalityChat, airoute.ModalityTools, airoute.ModalityStreamingChat,
		airoute.ModalityStreamingTools, airoute.ModalityStructured,
	} {
		t.Run(string(modality), func(t *testing.T) {
			f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord.withoutClusterAccount())
			ctx := personCtx(alice)
			got, err := f.router.ResolveFor(ctx, chosenRequest(modality))
			if err != nil {
				t.Fatal(err)
			}
			if modality == airoute.ModalityStructured {
				_, err = callStructured(t, ctx, got.Client)
			} else {
				err = invokeDecisionClient(ctx, got.Client, modality)
			}
			if err != nil {
				t.Fatal(err)
			}
			if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own"}) {
				t.Errorf("calls = %v, want the one call paid by the key", made)
			}
			if got := rowAccount(f.ledger.args(t)); got != [3]string{memql.VendorAccountKey, "k-own", alice} {
				t.Errorf("the row says %v paid, want alice's key", got)
			}
		})
	}
}

func TestOtherSurfacesStayOnTheClustersAccount(t *testing.T) {
	// With a cluster account the picture goes to the cluster's account, and
	// the seam is not asked.
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
	req := chosenRequest(airoute.ModalityVision)
	_, resolved, err := f.router.resolveDirect(personCtx(alice), req, modalityVision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved.Chain, []string{"claude"}) || accountOf(resolved.Decision) != [3]string{} {
		t.Errorf("chain %v paid by %v, want the record alone and no account on the decision", resolved.Chain, accountOf(resolved.Decision))
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("the key seam was asked %+v for a picture", asked)
	}

	// Without one, a key does not stand in.
	bare := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord.withoutClusterAccount())
	if _, _, err := bare.router.resolveDirect(personCtx(alice), req, modalityVision); err == nil {
		t.Error("a picture was served by a key: only text model calls take one")
	}
}

// K15: the row says who paid by the account's kind, the key's id and its
// owner's id. The key's name and fingerprint are not the router's to know.
func TestTheRecordNamesTheAccountAndNeverTheKeysName(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord)
	ctx := personCtx(alice)

	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	var query string
	select {
	case query = <-f.ledger.writes:
	case <-time.After(3 * time.Second):
		t.Fatal("the ledger never wrote its record")
	}
	for _, private := range []string{aliceOwnKey.label, aliceOwnKey.fingerprint, bobLentKey.label, bobLentKey.fingerprint} {
		if strings.Contains(query, private) {
			t.Errorf("the row carries %q", private)
		}
	}
	// The row as the engine will read it.
	f.ledger.writes <- query
	args := f.ledger.args(t)
	if got := rowAccount(args); got != [3]string{memql.VendorAccountKey, "k-own", alice} {
		t.Errorf("the row says %v paid, want the account's kind, the key's id and its owner's id", got)
	}
}

// The mutation's accountKind is a two-value enum: a row for a source that
// takes no account must leave it out, or the whole row is refused.
func TestARowWithNoAccountLeavesItsKindOut(t *testing.T) {
	args := buildRouterCallArgs(CallRecord{Outcome: "ok"}, "call-1")
	if _, sent := args["accountKind"]; sent {
		t.Errorf("accountKind = %v on a row with no account", args["accountKind"])
	}
	if args["accountKeyId"] != "" || args["accountKeyOwnerUserId"] != "" {
		t.Errorf("key fields = %v / %v, want both empty", args["accountKeyId"], args["accountKeyOwnerUserId"])
	}
	args = buildRouterCallArgs(CallRecord{Outcome: "ok", AccountKind: memql.VendorAccountCluster}, "call-2")
	if args["accountKind"] != memql.VendorAccountCluster {
		t.Errorf("accountKind = %v, want the cluster's account", args["accountKind"])
	}
	args = buildRouterCallArgs(CallRecord{Outcome: "ok", AccountKind: "somebody"}, "call-3")
	if _, sent := args["accountKind"]; sent {
		t.Errorf("an unknown kind %v was sent to a two-value enum", args["accountKind"])
	}
}

// R7: a run that forbids paid providers stops a key exactly as it stops the
// cluster's account, before any account is considered.
func TestPaidProvidersForbiddenStillStopsAKey(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
	req := chosenRequest(airoute.ModalityChat)
	req.NoPaidProviders = true

	_, _, err := f.router.resolveChat(personCtx(alice), req)
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("accounts were considered (%+v) for a source the run forbids", asked)
	}
	// The wrapper asks again when the call is made.
	if _, _, ok := f.router.providerLookup(personCtx(alice), req, "claude@key:k-own", modalityChat); ok {
		t.Error("a key-paid attempt escaped the run's ceiling at call time")
	}
}

// R7: the cost ceiling is asked before a vendor hop that follows a local
// source, before any account is considered.
func TestTheCostCeilingIsAskedBeforeAnyAccount(t *testing.T) {
	f := newAccountFixture(t, []string{"fleet:absent", "claude"}, []lentKey{switchedOnForAuto(aliceOwnKey)}, claudeRecord)
	f.providers.SetFleetInference(&stubFleetInference{})
	f.router.ceilingCheck = func(context.Context) (string, bool) { return "the ceiling of this test", true }

	_, _, err := f.router.resolveChat(personCtx(alice), autoRequest(airoute.ModalityChat))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalCeilingReached {
		t.Fatalf("err = %v, want the ceiling's refusal", err)
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("accounts were considered (%+v) past a reached ceiling", asked)
	}
}

// K14: a key's spend is metered spend, costed as the cluster's is.
func TestAKeyPaidCallIsMeteredSpend(t *testing.T) {
	served := func(keys []lentKey) map[string]any {
		f := newAccountFixture(t, []string{"claude"}, keys, claudeRecord)
		ctx := personCtx(alice)
		got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
			t.Fatal(err)
		}
		return f.ledger.args(t)
	}
	byKey, byCluster := served([]lentKey{aliceOwnKey}), served(nil)

	if rowAccount(byKey)[0] != memql.VendorAccountKey || rowAccount(byCluster)[0] != memql.VendorAccountCluster {
		t.Fatalf("the two rows are paid by %v and %v", rowAccount(byKey), rowAccount(byCluster))
	}
	if byKey["billing"] != BillingMetered {
		t.Errorf("billing = %v, want metered: a key's spend counts against every ceiling", byKey["billing"])
	}
	if byKey["pricingConfigured"] != true {
		t.Errorf("pricingConfigured = %v on a key-paid row", byKey["pricingConfigured"])
	}
	for _, field := range []string{"inputCost", "outputCost", "totalCost", "inputTokens", "outputTokens"} {
		if !reflect.DeepEqual(byKey[field], byCluster[field]) {
			t.Errorf("%s = %v paid by a key and %v paid by the cluster", field, byKey[field], byCluster[field])
		}
	}
	if cost, _ := byKey["totalCost"].(float64); cost <= 0 {
		t.Errorf("totalCost = %v, want the call costed", byKey["totalCost"])
	}
}

// S8: a key never reaches a machine or an app. A local model and an app
// session carry no account on the route or on the row, with keys installed.
func TestLocalAndAppSourcesNeverGetAnAccount(t *testing.T) {
	// The app's call passes the process-wide guard on local calls, so it runs
	// in a process of its own, as the other tests that reach a local door do.
	if observerDecisionChild(t) {
		return
	}
	keys := []lentKey{aliceOwnKey}

	app := newAccountFixture(t, []string{memql.AppReferencePrefix + "claude-code", "claude"}, keys, claudeRecord)
	app.providers.SetFleetInference(&stubFleetInference{})
	app.providers.SetAppInference(&stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}})
	ctx := personCtx(alice)
	got, err := app.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	if got.Resolution.Decision.Door != DoorApp || accountOf(got.Resolution.Decision) != [3]string{} {
		t.Errorf("door %q paid by %v, want the app with no account", got.Resolution.Decision.Door, accountOf(got.Resolution.Decision))
	}
	args := app.ledger.args(t)
	if _, sent := args["accountKind"]; sent || args["accountKeyId"] != "" || args["accountKeyOwnerUserId"] != "" {
		t.Errorf("the app's row names an account: %v", rowAccount(args))
	}
	if made := app.calls.attempts(); len(made) != 0 {
		t.Errorf("the vendor was called (%v) with the app open", made)
	}
	_, resolved, err := app.router.resolveChat(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{memql.AppReferencePrefix + "claude-code", "claude@key:k-own", "claude@cluster"}
	if !reflect.DeepEqual(resolved.Chain, want) {
		t.Errorf("chain = %v, want %v", resolved.Chain, want)
	}

	local := newAccountFixture(t, []string{memql.FleetReferencePrefix + "qwen", "claude"}, keys, claudeRecord)
	local.providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("qwen", true)}})
	_, resolved, err = local.router.resolveChat(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{memql.FleetReferencePrefix + "qwen", "claude@key:k-own", "claude@cluster"}
	if resolved.Decision.Door != DoorLocal || accountOf(resolved.Decision) != [3]string{} || !reflect.DeepEqual(resolved.Chain, want) {
		t.Errorf("door %q paid by %v over %v, want the local model with no account and %v",
			resolved.Decision.Door, accountOf(resolved.Decision), resolved.Chain, want)
	}
}

// S9: use is decided per call from the rows. A key that stopped being usable
// between the resolution and the call is not spent, and a reference this
// person may not use resolves nothing, exactly as a made-up one does.
func TestAnAccountIsJudgedAgainWhenTheCallIsMade(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord)
	ctx := personCtx(alice)
	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	f.keys.remove("k-own")
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
		t.Fatal(err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-lent"}) {
		t.Fatalf("calls = %v, want the removed key never tried", made)
	}
	if got := rowAccount(f.ledger.args(t)); got != [3]string{memql.VendorAccountKey, "k-lent", bob} {
		t.Errorf("the row says %v paid, want the lent key", got)
	}

	req := chosenRequest(airoute.ModalityChat)
	for _, name := range []string{"claude@key:made-up", "claude@key:", "claude@key:k-own"} {
		if _, _, ok := f.router.providerLookup(ctx, req, name, modalityChat); ok {
			t.Errorf("%q resolved an account", name)
		}
	}
	// Bob's key is lent to alice and to nobody else.
	stranger := chosenRequest(airoute.ModalityChat)
	stranger.UserId, stranger.RouteBy = "", ""
	if _, _, ok := f.router.providerLookup(sweepContext(), stranger, "claude@key:k-lent", modalityChat); ok {
		t.Error("a key paid for a call no person acts in")
	}
}

// With no key seam installed a vendor call is the cluster's account or
// nothing, which is every node before the key integration is wired.
func TestWithoutAKeySeamAVendorCallIsTheClustersAccount(t *testing.T) {
	f := newAccountFixture(t, []string{"claude", "gpt"}, nil, claudeRecord.withoutClusterAccount(), gptRecord)
	f.providers.SetVendorKeys(nil)

	_, resolved, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved.Chain, []string{"gpt@cluster"}) || accountOf(resolved.Decision) != [3]string{memql.VendorAccountCluster, "", ""} {
		t.Errorf("chain %v paid by %v, want gpt on the cluster's account", resolved.Chain, accountOf(resolved.Decision))
	}
	if reason, ok := consideredLine(resolved.Decision.Considered, "claude", ""); !ok || !strings.Contains(reason, "not set up for Anthropic") {
		t.Errorf("claude's line is %q (found %v)", reason, ok)
	}
}

// OWNER DECISION 2026-11-22 (D4): the record of a call a person's API key paid
// for says who paid and why it failed in the router's own words, and never
// carries the vendor's sentence. That sentence is about the key owner's
// account at the vendor (it can name their organization, their plan, a part of
// their key), and this row is read by other people: the caller the key was
// lent to, and everybody who reads the decisions list. The key's owner reads
// the vendor's words on the key's own page.
func TestAKeyPaidFailureKeepsTheRoutersWordsAndNeverTheVendors(t *testing.T) {
	const vendorsWords = "the vendor's words"
	for _, kind := range memql.VendorAccountFailureKinds() {
		t.Run("about the account/"+kind, func(t *testing.T) {
			f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord)
			f.calls.fail["claude@key:k-own"] = accountFailure(kind)
			ctx := personCtx(alice)
			got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err != nil {
				t.Fatal(err)
			}
			want := "account_" + kind + ": " + failureWords["account_"+kind]
			for _, outcome := range []string{"error", "fallback_used"} {
				row := f.ledger.args(t)
				if row["outcome"] != outcome || rowAccount(row) != [3]string{memql.VendorAccountKey, "k-own", alice} {
					t.Fatalf("row = outcome %v paid by %v, want the %s row of alice's key", row["outcome"], rowAccount(row), outcome)
				}
				if message := fmt.Sprint(row["errorMessage"]); message != want {
					t.Errorf("the %s row's errorMessage = %q, want the router's own words %q", outcome, message, want)
				}
				if message := fmt.Sprint(row["errorMessage"]); strings.Contains(message, vendorsWords) || strings.Contains(message, "sk-ant-secret") {
					t.Errorf("the %s row quotes the vendor: %q", outcome, message)
				}
			}
		})
	}

	// A failure that says nothing about the account is the vendor's sentence
	// all the same, and it can name the key owner's organization.
	t.Run("about something else", func(t *testing.T) {
		f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
		f.calls.fail["claude@key:k-own"] = errors.New("429 rate limit reached for organization org-alices-company on tokens per minute")
		ctx := personCtx(alice)
		got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err == nil {
			t.Fatal("the call was served, so no failed row is being measured")
		}
		row := f.ledger.args(t)
		if row["outcome"] != "error" || rowAccount(row) != [3]string{memql.VendorAccountKey, "k-own", alice} {
			t.Fatalf("row = outcome %v paid by %v, want the failed attempt on alice's key", row["outcome"], rowAccount(row))
		}
		if message := fmt.Sprint(row["errorMessage"]); message != "rate_limit: rate limited" {
			t.Errorf("errorMessage = %q, want the router's own words for a rate limit", message)
		}
	})

	// THE CLUSTER'S OWN ACCOUNT keeps the error's text, as it always did: the
	// decision is about a person's key.
	t.Run("the cluster's account keeps its text", func(t *testing.T) {
		f := newAccountFixture(t, []string{"claude"}, nil, claudeRecord)
		f.calls.fail["claude@cluster"] = errors.New("500 internal server error: upstream said so")
		ctx := personCtx(alice)
		got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err == nil {
			t.Fatal("the call was served, so no failed row is being measured")
		}
		row := f.ledger.args(t)
		if message := fmt.Sprint(row["errorMessage"]); !strings.Contains(message, "upstream said so") {
			t.Errorf("errorMessage = %q, want the error's own text for the cluster's account", message)
		}
	})
}

// D4 again, on the one other place a failed attempt's error is written down:
// the refusal a wrapper answers when the cost ceiling is reached before its
// next vendor hop. That refusal names the source that just failed and why, to
// the caller and on the decision. When a person's key paid for the failed
// attempt, the why is the router's words.
func TestACeilingRefusalAfterAKeyFailedNeverQuotesTheVendor(t *testing.T) {
	f := newAccountFixture(t, []string{"fleet:absent", "claude"}, []lentKey{aliceOwnKey, bobLentKey}, claudeRecord)
	f.providers.SetFleetInference(&stubFleetInference{})
	f.calls.fail["claude@key:k-own"] = accountFailure("refused")
	// Reached only once a vendor attempt has been made: the resolution passes,
	// and the hop to the next account is the one that is refused.
	f.router.ceilingCheck = func(context.Context) (string, bool) {
		return "the ceiling of this test", len(f.calls.attempts()) > 0
	}
	ctx := personCtx(alice)

	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	_, err = got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages())
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalCeilingReached {
		t.Fatalf("err = %v, want the ceiling's refusal after the first account failed", err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own"}) {
		t.Fatalf("calls = %v, want one attempt on alice's key and none past the ceiling", made)
	}
	said := false
	for _, door := range refusal.Doors {
		if strings.Contains(door.Reason, "the vendor's words") || strings.Contains(door.Reason, "sk-ant-secret") {
			t.Errorf("the refusal quotes the vendor to the caller: %q", door.Reason)
		}
		if door.Name == "claude" && door.Reason == FailureReason(accountFailure("refused")) {
			said = true
		}
	}
	if !said {
		t.Errorf("the refusal does not say why the key-paid attempt failed in the router's words: %+v", refusal.Doors)
	}
	for _, line := range refusal.Decision.Considered {
		if strings.Contains(line.Reason, "the vendor's words") || strings.Contains(line.Reason, "sk-ant-secret") {
			t.Errorf("the decision quotes the vendor: %q", line.Reason)
		}
	}
}

// OWNER DECISION 2026-11-22: the cluster's own vendor account is not a pool
// for everybody. It pays for the cluster's own work, and for a person's calls
// only when that person may use it (component/memql vendor_account_cluster.go).
// These are the router's half: what a walk does for a person it is closed to.
// bobsChosenRequest is chosenRequest for bob.
func bobsChosenRequest(modality airoute.Modality) ResolveRequest {
	req := chosenRequest(modality)
	req.UserId, req.RouteBy = bob, bob
	return req
}

func TestTheClustersAccountDoesNotPayForAPersonItIsClosedTo(t *testing.T) {
	// No key and a closed account: the vendor's model is passed over for her,
	// with the reason on its line, and the route says nothing can serve.
	f := newAccountFixture(t, []string{"claude"}, nil, claudeRecord)
	f.clusterAccountClosedTo(alice)
	_, _, err := f.router.resolveChat(personCtx(alice), chosenRequest(airoute.ModalityChat))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal: nothing can pay for her", err)
	}
	said := false
	for _, door := range refusal.Doors {
		if door.Name == "claude" && strings.Contains(door.Reason, memql.ClusterAccountClosedReason) {
			said = true
		}
	}
	if !said {
		t.Errorf("the refusal does not say the cluster's account is closed to her: %+v", refusal.Doors)
	}
	if made := f.calls.attempts(); len(made) != 0 {
		t.Fatalf("the vendor was called (%v) on an account that does not pay for her", made)
	}

	// Somebody the account is open to is served by it, on the same cluster.
	got, err := f.router.ResolveFor(personCtx(bob), bobsChosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(personCtx(bob), helloMessages()); err != nil {
		t.Fatal(err)
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@cluster"}) {
		t.Fatalf("calls = %v, want the cluster's account for a person who may use it", made)
	}
}

func TestAPersonTheClustersAccountIsClosedToPaysWithTheirKeyAndNothingElse(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
	f.clusterAccountClosedTo(alice)
	ctx := personCtx(alice)
	_, resolved, err := f.router.resolveChat(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude@key:k-own"}; !reflect.DeepEqual(resolved.Chain, want) {
		t.Fatalf("chain = %v, want %v: the cluster's account is not behind her key", resolved.Chain, want)
	}
	// The record says the cluster's account was passed over, and why.
	line, ok := consideredLine(resolved.Decision.Considered, "claude", memql.VendorAccountCluster)
	if !ok || line != memql.ClusterAccountClosedReason {
		t.Errorf("the decision's line for the cluster's account is %q (found %v)", line, ok)
	}

	// Her key fails for a reason about the account: there is no next account.
	f.calls.fail["claude@key:k-own"] = accountFailure("credit")
	got, err := f.router.ResolveFor(ctx, chosenRequest(airoute.ModalityChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(common.ChatAIProvider).CallChat(ctx, helloMessages()); err == nil {
		t.Fatal("the call was served after her only account failed")
	}
	if made := f.calls.attempts(); !reflect.DeepEqual(made, []string{"claude@key:k-own"}) {
		t.Fatalf("calls = %v, want her key alone", made)
	}
}

// A wrapper resolves "record@cluster" by name when the call is made, and the
// account is judged again there: a permission taken away since the resolution
// is not spent.
func TestTheClustersAccountIsJudgedAgainWhenTheCallIsMade(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, nil, claudeRecord)
	ctx := personCtx(alice)
	req := chosenRequest(airoute.ModalityChat)
	if _, _, ok := f.router.providerLookup(ctx, req, "claude@cluster", modalityChat); !ok {
		t.Fatal("the cluster's account did not resolve for a person who may use it")
	}
	f.clusterAccountClosedTo(alice)
	if _, _, ok := f.router.providerLookup(ctx, req, "claude@cluster", modalityChat); ok {
		t.Fatal("the cluster's account was spent at the call after it was closed to her")
	}
}

// The surfaces that take no key of a person's -- a picture, speech,
// transcription, an image -- are the cluster's account or nothing, so the same
// gate stands in front of them. Embeddings are the exception: the memory space
// is the cluster's, one model for everybody.
func TestTheVendorSurfacesWithNoKeyAskTheSameGate(t *testing.T) {
	f := newAccountFixture(t, []string{"claude"}, []lentKey{aliceOwnKey}, claudeRecord)
	f.clusterAccountClosedTo(alice)
	req := chosenRequest(airoute.ModalityVision)
	_, _, err := f.router.resolveDirect(personCtx(alice), req, modalityVision)
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal: a picture is the cluster's account or nothing, and it is closed to her", err)
	}
	said := false
	for _, door := range refusal.Doors {
		if strings.Contains(door.Reason, memql.ClusterAccountClosedReason) {
			said = true
		}
	}
	if !said {
		t.Errorf("the refusal does not say why: %+v", refusal.Doors)
	}
	if asked := f.keys.asked(); len(asked) != 0 {
		t.Errorf("the key seam was asked %+v for a picture", asked)
	}

	// The cluster's own work is served as it always was.
	own := autoRequest(airoute.ModalityVision)
	own.UserId = ""
	if _, _, err := f.router.resolveDirect(context.Background(), own, modalityVision); err != nil {
		t.Fatalf("the cluster's own picture was refused: %v", err)
	}
}

func TestEmbeddingsStayOnTheClustersAccountForEverybody(t *testing.T) {
	if clusterAccountGated(modalityEmbedding) {
		t.Fatal("embeddings ask the gate: a person whose writes were not embedded would be missing from everybody's search")
	}
	for _, mod := range []providerModality{modalityVision, modalitySpeech, modalityTranscribe, modalityImage} {
		if !clusterAccountGated(mod) {
			t.Errorf("modality %v is paid by the cluster's account for a person without asking whether they may use it", mod)
		}
	}
	// The text surfaces ask through their accounts, not through this.
	for _, mod := range []providerModality{modalityStreamTools, modalityChat, modalityStreamChat, modalityTools, modalityStructured} {
		if clusterAccountGated(mod) {
			t.Errorf("modality %v asks twice", mod)
		}
	}
}
