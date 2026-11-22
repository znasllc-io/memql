package memql

// The ACCOUNT that pays for one vendor call (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// approach A and decisions K3, K8, K9, K17, A8).
//
// Routing chooses a MODEL. One step follows: which account pays for this call
// at that model's vendor. There are two kinds.
//
//   - The CLUSTER'S account is the cluster's workload identity federation with
//     the vendor: at most one per vendor, the same for every record, held by
//     the process's one sign-in (ai_vendor_signin.go).
//   - A person's API KEY. It belongs to the person who added it, who may lend
//     it. The cluster's own work never spends one, and there is no cluster key.
//
// WHAT THIS FILE HOLDS. The account's name, the seam the key integration
// installs on the provider registry (VendorKeys), and the three questions the
// router asks of the registry: which accounts can pay for this person at this
// record (VendorAccountsFor, or its two halves VendorKeysAt and
// VendorAccountsWith), may this one still pay (VendorAccountUsable), and give
// me this record's client paid by it (BindVendorAccount). How a key's
// credential reaches a request is vendor_account_middleware.go.
//
// WHY A SEAM. Whether a key may pay is a question about rows: whose key it is,
// who it is lent to, whether it was refused. Lending is a machine's lending
// (component/worker.Sharing), and component/worker imports this package, so
// the answer cannot be computed here. integrations/vendorkeys computes it and
// installs it, as the fleet and app doors are installed. A node with nothing
// installed has no keys, which is the narrowing direction: every vendor call
// is then the cluster's account or nothing.
//
// TEXT CALLS ONLY (K17). A key pays for the text model calls a route choice
// governs. Speech, transcription and embeddings stay on the cluster's account:
// embeddings bind the cluster's shared memory search to one model and must not
// follow a person's key, and the audio paths are resolved outside routes.
//
// Regression tests: vendor_account_test.go; the router's half is
// component/router/accounts_test.go.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// The two kinds of account.
const (
	VendorAccountCluster = "cluster"
	VendorAccountKey     = "key"
)

// VendorAccount is what pays for one call at a vendor.
type VendorAccount struct {
	// Kind is VendorAccountCluster or VendorAccountKey.
	Kind string
	// Vendor is VendorAnthropic or VendorOpenAI.
	Vendor string
	// KeyId is the bare id of the key's row. Empty for the cluster's account.
	KeyId string
	// OwnerUserId is the bare id of the person the key belongs to. Empty for
	// the cluster's account.
	OwnerUserId string
}

// accountKeyRefPrefix starts the reference to a key.
const accountKeyRefPrefix = VendorAccountKey + ":"

// accountReferenceSeparator joins a record's name to an account's reference in
// the chain a fallback wrapper walks. A record name is a DSL identifier and
// never contains it.
const accountReferenceSeparator = "@"

// Ref is how an account is named beside a record: "cluster" or "key:<id>".
func (a VendorAccount) Ref() string {
	if a.Kind == VendorAccountKey {
		return accountKeyRefPrefix + a.KeyId
	}
	return VendorAccountCluster
}

// JoinAccountReference names a record paid by an account, the form a fallback
// wrapper's chain carries: the model and the account together (K10).
func JoinAccountReference(record string, a VendorAccount) string {
	return record + accountReferenceSeparator + a.Ref()
}

// SplitAccountReference undoes JoinAccountReference. A name that carries no
// account reference comes back whole with an empty ref.
//
// Only an exact "cluster" or a "key:<id>" after the LAST separator is a
// reference. A fleet model id or an app reference that happens to contain the
// separator is therefore left alone.
func SplitAccountReference(name string) (record, ref string) {
	i := strings.LastIndex(name, accountReferenceSeparator)
	if i <= 0 {
		return name, ""
	}
	candidate := name[i+len(accountReferenceSeparator):]
	if candidate == VendorAccountCluster ||
		(strings.HasPrefix(candidate, accountKeyRefPrefix) && len(candidate) > len(accountKeyRefPrefix)) {
		return name[:i], candidate
	}
	return name, ""
}

// VendorAccountQuery is who a call is for and how its route was chosen: the
// inputs of the paying-account rule (K8).
type VendorAccountQuery struct {
	Vendor       string
	ActingUserId string
	// Chosen: the acting person chose the route, or pinned the model
	// themselves. False is Auto, and every call nobody chose a route for; only
	// keys whose owner switched Auto on count then.
	Chosen bool
	// ForVisitor: the acting person's authority was borrowed for a visitor (a
	// shopper form, an access-code call, a deployed workflow's deployer). Such
	// a call never spends a key.
	ForVisitor bool
}

// VendorAccountPassedOver is one account that could not pay, and why. The
// reason is written on the decision record, which every signed-in person can
// read, so it never names a key.
type VendorAccountPassedOver struct {
	Account VendorAccount
	Reason  string
}

// VendorKeyVerdict is what a real call learned about a key (K11).
type VendorKeyVerdict string

const (
	// VendorKeyGood: the vendor served a call paid by the key.
	VendorKeyGood VendorKeyVerdict = "good"
	// VendorKeyRefused: the vendor does not accept the key.
	VendorKeyRefused VendorKeyVerdict = "refused"
	// VendorKeyCredit: the key's account is out of credit or at a spend limit.
	VendorKeyCredit VendorKeyVerdict = "credit"
)

// VendorKeys is the seam the key integration installs. Every method decides
// from rows read at the time of the call: nothing a replica remembers decides
// whether a key may pay.
type VendorKeys interface {
	// KeysFor returns the keys that can pay for this person at this vendor, in
	// the order they are tried, and the ones passed over with their reasons.
	KeysFor(ctx context.Context, q VendorAccountQuery) (usable []VendorAccount, passedOver []VendorAccountPassedOver, err error)
	// Usable judges one key for one person again at the time of the call. The
	// reason is for a record; an unknown key, a removed one and one this
	// person may not use are told apart by nothing a caller can see.
	Usable(ctx context.Context, q VendorAccountQuery, keyId string) (account VendorAccount, reason string, ok bool)
	// Secret unseals a key's value for one request. The caller holds it for
	// that request and nowhere else.
	Secret(ctx context.Context, a VendorAccount) (string, error)
	// Observe records what a real call learned about a key. at is the time of
	// the evidence; older evidence never replaces newer.
	Observe(ctx context.Context, a VendorAccount, verdict VendorKeyVerdict, at time.Time, detail string)
}

// VendorAccountClient is what a vendor's text provider implements so that one
// call can be paid by one account. Exported because the router's tests, in
// another package, stand a vendor in.
type VendorAccountClient interface {
	// WithVendorAccount returns this provider paying with the given key. The
	// receiver is left as it was.
	WithVendorAccount(account VendorAccount, keys VendorKeys) any
	// VendorOrigin is the address this provider's client dials, learned when
	// the provider was built and before any request. "" means unknown; the
	// request itself is then what refuses a key at another address.
	VendorOrigin() string
}

// clusterAccountless is a vendor client built on a cluster that has no account
// at its vendor. It serves a call paid by a key and refuses any other.
type clusterAccountless interface {
	// ClusterAccountMissing is why the cluster's account cannot pay, or nil.
	ClusterAccountMissing() error
}

// SetVendorKeys installs the key seam. Nil removes it.
func (r *ProviderRegistry) SetVendorKeys(k VendorKeys) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vendorKeys = k
}

// VendorKeysInstalled reports whether this node can pay a call with a key.
func (r *ProviderRegistry) VendorKeysInstalled() bool {
	return r.installedVendorKeys() != nil
}

func (r *ProviderRegistry) installedVendorKeys() VendorKeys {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.vendorKeys
}

// VendorOfEntry names the vendor whose accounts can pay for a record, or ""
// when the record takes no account of a person's: any record that is not a
// text model of Anthropic or OpenAI (K3, K17).
func VendorOfEntry(e *ProviderConfigEntry) string {
	if e == nil || e.Config.Base || e.Client == nil {
		return ""
	}
	if _, ok := e.Client.(VendorAccountClient); !ok {
		return ""
	}
	if e.Config.ResolvedModality() != ModalityText {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(e.Config.Type)) {
	case "anthropic", "anthropicchat":
		return VendorAnthropic
	case "openai", "openaichat":
		return VendorOpenAI
	}
	return ""
}

// keysServe reports whether a key may pay for this query at all, before any
// row is read: never for the cluster's own work, and never for a visitor.
func keysServe(ctx context.Context, q VendorAccountQuery) bool {
	return !q.ForVisitor && !auth.ActsForNoPerson(ctx, q.ActingUserId)
}

// keysRefusedAt is why a key cannot be used with this record's client, or ""
// when it can. A key is sent to the vendor's own address and nowhere else, so
// a cluster that sends the vendor's calls somewhere else uses no key (K3). The
// address is the one the client learned when it was built. The request checks
// again (vendor_account_middleware.go), so a key is never sent on the strength
// of this answer alone.
func keysRefusedAt(e *ProviderConfigEntry, vendor string) string {
	client, ok := e.Client.(VendorAccountClient)
	if !ok {
		return ""
	}
	origin := client.VendorOrigin()
	if origin == "" || strings.EqualFold(origin, vendorKeyOrigins[vendor]) {
		return ""
	}
	return fmt.Sprintf("this cluster sends %s calls to another address, so an API key is not used", vendorDisplayName(vendor))
}

// VendorKeyAnswer is what the seam said about one person's keys at one vendor.
type VendorKeyAnswer struct {
	// Usable are the keys that can pay, in the order they are tried.
	Usable []VendorAccount
	// PassedOver are the keys that cannot, each with its reason.
	PassedOver []VendorAccountPassedOver
}

// VendorKeysAt asks the seam which keys can pay for this person at q.Vendor.
//
// It is the half of VendorAccountsFor that READS ROWS, split out so that one
// walk of a route asks once per vendor however many of the vendor's records
// the route names (component/router/accounts.go).
//
// A call with no acting person, or one made for a visitor, has no keys and the
// seam is not asked. A read that FAILS passes keys over and never fails the
// call: the cluster's account may still serve it, and a call that could have
// been served must not be refused because a list of keys could not be read.
func (r *ProviderRegistry) VendorKeysAt(ctx context.Context, q VendorAccountQuery) VendorKeyAnswer {
	keys := r.installedVendorKeys()
	if keys == nil || q.Vendor == "" || !keysServe(ctx, q) {
		return VendorKeyAnswer{}
	}
	usable, passedOver, err := keys.KeysFor(ctx, q)
	if err != nil {
		return VendorKeyAnswer{PassedOver: []VendorAccountPassedOver{{
			Account: VendorAccount{Kind: VendorAccountKey, Vendor: q.Vendor},
			Reason:  "API keys could not be read",
		}}}
	}
	return VendorKeyAnswer{Usable: usable, PassedOver: passedOver}
}

// VendorAccountsWith composes a record's accounts from an answer already read:
// the keys the seam offers, then the cluster's account (K8), with every account
// that cannot pay beside its reason (K9). It reads nothing.
//
// The answer must be the one for this record's vendor; a key of another vendor
// is left out rather than offered.
func (r *ProviderRegistry) VendorAccountsWith(e *ProviderConfigEntry, keys VendorKeyAnswer) ([]VendorAccount, []VendorAccountPassedOver) {
	vendor := VendorOfEntry(e)
	if vendor == "" {
		return nil, nil
	}
	var accounts []VendorAccount
	passedOver := make([]VendorAccountPassedOver, 0, len(keys.PassedOver)+1)
	for _, skipped := range keys.PassedOver {
		if skipped.Account.Vendor == vendor {
			passedOver = append(passedOver, skipped)
		}
	}
	refusal := keysRefusedAt(e, vendor)
	for _, key := range keys.Usable {
		switch {
		case key.Vendor != vendor:
		case refusal != "":
			passedOver = append(passedOver, VendorAccountPassedOver{Account: key, Reason: refusal})
		default:
			accounts = append(accounts, key)
		}
	}

	cluster := VendorAccount{Kind: VendorAccountCluster, Vendor: vendor}
	if e.Available {
		accounts = append(accounts, cluster)
	} else {
		passedOver = append(passedOver, VendorAccountPassedOver{
			Account: cluster,
			Reason:  fmt.Sprintf("the cluster's account is not set up for %s", vendorDisplayName(vendor)),
		})
	}
	return accounts, passedOver
}

// VendorAccountsFor answers which accounts can pay for this person at this
// record, in order, and which were passed over (K8, K9).
//
// The order: the keys the seam offers (the person's own default, then keys
// lent to them), then the cluster's account. A call with no acting person, or
// one made for a visitor, has the cluster's account and nothing else.
func (r *ProviderRegistry) VendorAccountsFor(ctx context.Context, q VendorAccountQuery, e *ProviderConfigEntry) ([]VendorAccount, []VendorAccountPassedOver) {
	vendor := VendorOfEntry(e)
	if vendor == "" {
		return nil, nil
	}
	q.Vendor = vendor
	return r.VendorAccountsWith(e, r.VendorKeysAt(ctx, q))
}

// VendorAccountUsable judges one account reference again at the time of the
// call. A reference this person may not use resolves nothing, exactly as a
// made-up one does (S9).
func (r *ProviderRegistry) VendorAccountUsable(ctx context.Context, q VendorAccountQuery, e *ProviderConfigEntry, ref string) (VendorAccount, bool) {
	vendor := VendorOfEntry(e)
	if vendor == "" {
		return VendorAccount{}, false
	}
	q.Vendor = vendor
	if ref == VendorAccountCluster {
		return VendorAccount{Kind: VendorAccountCluster, Vendor: vendor}, e.Available
	}
	keyId := strings.TrimPrefix(ref, accountKeyRefPrefix)
	if keyId == ref || keyId == "" {
		return VendorAccount{}, false
	}
	keys := r.installedVendorKeys()
	if keys == nil || !keysServe(ctx, q) || keysRefusedAt(e, vendor) != "" {
		return VendorAccount{}, false
	}
	account, _, ok := keys.Usable(ctx, q, keyId)
	if !ok || account.Vendor != vendor {
		return VendorAccount{}, false
	}
	return account, true
}

// BindVendorAccount returns a record's client paying with one account.
func (r *ProviderRegistry) BindVendorAccount(e *ProviderConfigEntry, a VendorAccount) (any, bool) {
	if e == nil || e.Client == nil {
		return nil, false
	}
	if a.Kind == VendorAccountCluster {
		return e.Client, e.Available
	}
	client, ok := e.Client.(VendorAccountClient)
	keys := r.installedVendorKeys()
	if !ok || keys == nil || a.Kind != VendorAccountKey || a.KeyId == "" {
		return nil, false
	}
	return client.WithVendorAccount(a, keys), true
}
