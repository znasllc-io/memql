package router

// accounts.go -- which ACCOUNT pays for a vendor call (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K8, K9, K10, K15, K17 and R7).
//
// Routing chooses a MODEL. For a vendor's text model one step follows: who
// pays. There are two kinds of account, the cluster's own (its federation with
// the vendor) and a person's API key, and component/memql answers which of
// them can pay for a given person at a given record (vendor_account.go). This
// file is the router's half: where that question is asked, and how the answer
// travels.
//
// WHERE IT IS ASKED. In the walk, for a vendor text record, AFTER every gate
// that stands in front of a vendor today: a run's no-paid-providers ceiling,
// the cost ceiling, the context floor and the modality check (R7). A key is a
// paid provider; nothing that stops the cluster's account lets a key through.
// A record with no account that can pay is passed over like a shut door, with
// the reasons on its line (K9), and the route moves on.
//
// HOW IT TRAVELS. The chain a fallback wrapper walks names the model and the
// account together: "claude@key:<id>", then "claude@cluster". That is K10: a
// failure about the account is followed by the same model on the next account,
// before the route moves to the next model. Any other failure skips the
// record's remaining accounts (accountHop), which is today's fallback. Every
// other entry of the chain is spelled as it always was.
//
// JUDGED AGAIN AT THE CALL. A wrapper resolves each chain entry by name when
// the call is made (providerLookup), and a reference to a key is checked
// against the rows again there. A key removed, paused or refused since the
// resolution is not spent, and a reference this person may not use resolves
// nothing, exactly as a made-up one does (S9).
//
// WHICH SURFACES. The text surfaces a route choice governs: chat, streaming
// chat, both tool surfaces and structured output (K17). Vision, embeddings,
// speech, transcription and images stay on the cluster's account. Vision is
// among them because it is resolved without a wrapper and writes no decision
// record of its own, so a key-paid picture would be spend with no row saying
// who paid (K15).
//
// Regression tests: accounts_test.go.

import (
	"context"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// accountsApply reports whether calls on this surface are paid by an account a
// person can hold.
func accountsApply(mod providerModality) bool {
	switch mod {
	case modalityStreamTools, modalityChat, modalityStreamChat, modalityTools, modalityStructured:
		return true
	}
	return false
}

// clusterAccountGated reports whether a vendor call on this surface must ask,
// by itself, whether the cluster's own account pays for the person it is for
// (owner decision 2026-11-22; component/memql vendor_account_cluster.go).
//
// A vendor's text model on a text surface asks through its accounts
// (accountRecord): the cluster's account is one of the accounts composed for
// the record, and is left out for a person it is closed to. EVERY OTHER vendor
// candidate is the cluster's account or nothing, so the walk asks here: a
// picture, speech, transcription and an image, which take no key of a
// person's, and equally a record on a text surface whose client cannot be paid
// by a key. The rule must not depend on what kind of client a record has.
//
// EMBEDDINGS DO NOT ASK: the memory space is the cluster's, one model for
// everybody, and a person whose writes were not embedded would be missing from
// everybody's search.
func clusterAccountGated(mod providerModality) bool {
	return mod != modalityEmbedding
}

// accountQuery is who a call is for and how its route was chosen, in the words
// the paying-account rule takes (K8).
func (r *Router) accountQuery(req ResolveRequest, vendor string) memql.VendorAccountQuery {
	return memql.VendorAccountQuery{
		Vendor:       vendor,
		ActingUserId: strings.TrimSpace(req.UserId),
		Chosen:       chosenByActor(req),
		ForVisitor:   req.ForVisitor,
	}
}

// accountMemo keeps what the key seam answered during ONE resolution.
//
// A route can name many records of one vendor (`federation:strongest` expands
// to all of them), and every one of them asks which of this person's keys can
// pay. The answer is the same for all of them, and each ask is a read of rows,
// so one resolution asks once per vendor. It is never kept past the
// resolution: the call itself judges its account again.
type accountMemo struct {
	mu   sync.Mutex
	keys map[string]memql.VendorKeyAnswer
}

type accountMemoKey struct{}

// withAccountMemo starts a resolution's memo. Always a fresh one: an answer
// read for an earlier resolution must not decide this one.
func withAccountMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, accountMemoKey{}, &accountMemo{keys: map[string]memql.VendorKeyAnswer{}})
}

// accountsFor answers which accounts can pay for this request at one vendor
// text record, in the order they are tried, and which were passed over. Both
// are nil for an entry that takes no account.
func (r *Router) accountsFor(ctx context.Context, req ResolveRequest, entry *memql.ProviderConfigEntry) ([]memql.VendorAccount, []memql.VendorAccountPassedOver) {
	if r == nil || r.providers == nil {
		return nil, nil
	}
	vendor := memql.VendorOfEntry(entry)
	if vendor == "" {
		return nil, nil
	}
	q := r.accountQuery(req, vendor)
	memo, _ := ctx.Value(accountMemoKey{}).(*accountMemo)
	if memo == nil {
		return r.providers.VendorAccountsWith(entry, r.providers.VendorKeysAt(ctx, q))
	}
	memo.mu.Lock()
	answer, known := memo.keys[vendor]
	memo.mu.Unlock()
	if !known {
		answer = r.providers.VendorKeysAt(ctx, q)
		memo.mu.Lock()
		memo.keys[vendor] = answer
		memo.mu.Unlock()
	}
	return r.providers.VendorAccountsWith(entry, answer)
}

// accountRecord reports whether this candidate is a vendor text record on a
// surface an account pays for.
func (r *Router) accountRecord(mod providerModality, cand candidate, entry *memql.ProviderConfigEntry) bool {
	return cand.Door == DoorFederation && accountsApply(mod) && memql.VendorOfEntry(entry) != ""
}

// chainEntriesFor is how one candidate is spelled in the chain a fallback
// wrapper walks: a vendor text record once per account that can pay, in order,
// and anything else by its own name.
//
// A vendor record with no account is left out. The wrapper could only skip it.
func (r *Router) chainEntriesFor(ctx context.Context, req ResolveRequest, mod providerModality, cand candidate) []string {
	if cand.Door != DoorFederation || !accountsApply(mod) || r == nil || r.providers == nil {
		return []string{cand.Name}
	}
	entry, ok := r.providers.EntryForUser(ctx, req.UserId, cand.Name)
	if !ok || !r.accountRecord(mod, cand, entry) {
		return []string{cand.Name}
	}
	accounts, _ := r.accountsFor(ctx, req, entry)
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, memql.JoinAccountReference(cand.Name, account))
	}
	return names
}

// noAccountReason is the line for a model no account can pay for: every
// account that was considered, and why each could not (K9). The reasons are
// the registry's and the key seam's, which never name a key.
func noAccountReason(passedOver []memql.VendorAccountPassedOver) string {
	const lead = "no account can pay for this call"
	seen := map[string]bool{}
	reasons := make([]string, 0, len(passedOver))
	for _, p := range passedOver {
		reason := strings.TrimSpace(p.Reason)
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return lead
	}
	return lead + ": " + strings.Join(reasons, "; ")
}

// accountAtCall decides, when a call is made, which account pays for one chain
// entry. paid is false for an entry that takes no account; ok is false when
// the entry cannot be served at all.
//
// A PLAIN NAME is the record on the cluster's own account, as it always was:
// it needs the record to be available. A REFERENCE names its account, and a
// key is judged again from the rows (S9). A reference on a surface no account
// pays for resolves nothing.
//
// A PLAIN NAME OF A VENDOR RECORD ASKS WHO THE CLUSTER'S ACCOUNT PAYS FOR
// (clusterAccountGated). The walk asks that of every vendor candidate it
// considers, but a plain name reaches here without having been walked: every
// fallback wrapper resolves the rest of its chain by name after the source it
// chose fails when called (audio.go, fallback.go, fallback_structured.go,
// fallback_stream_chat.go), and a consented fallback resolves one name
// directly. With the question asked only in the walk, a person the account is
// closed to was refused a vendor's speech when no local model was online and
// served it, on the cluster's account, when one was online and failed; the
// same went for a vendor behind an app that failed. Embeddings do not ask,
// here as in the walk. The cluster's own work is always paid.
//
// A record passed over here leaves no line of its own on the decision: the
// lookup has no channel for a reason, as for a record that is unavailable.
// The person is told what failed in front of it.
func (r *Router) accountAtCall(
	ctx context.Context,
	req ResolveRequest,
	entry *memql.ProviderConfigEntry,
	ref string,
	mod providerModality,
) (account memql.VendorAccount, paid, ok bool) {
	if ref == "" {
		if !entry.Available {
			return memql.VendorAccount{}, false, false
		}
		if doorFor(entry.Config.Name) == DoorFederation && clusterAccountGated(mod) &&
			r.providers.ClusterAccountClosedTo(ctx, req.UserId) != "" {
			return memql.VendorAccount{}, false, false
		}
		if vendor := memql.VendorOfEntry(entry); vendor != "" && accountsApply(mod) {
			return memql.VendorAccount{Kind: memql.VendorAccountCluster, Vendor: vendor}, true, true
		}
		return memql.VendorAccount{}, false, true
	}
	if !accountsApply(mod) {
		return memql.VendorAccount{}, false, false
	}
	account, usable := r.providers.VendorAccountUsable(ctx, r.accountQuery(req, ""), entry, ref)
	return account, usable, usable
}

// stampAccount writes who pays onto a decision.
func stampAccount(d *airoute.Decision, account memql.VendorAccount) {
	d.AccountKind = account.Kind
	d.AccountKeyId, d.AccountKeyOwnerUserId = "", ""
	if account.Kind == memql.VendorAccountKey {
		d.AccountKeyId = account.KeyId
		d.AccountKeyOwnerUserId = account.OwnerUserId
	}
}

// accountRef is the reference of the account a decision says paid: "cluster",
// "key:<id>", or "" for a source that takes none. It is what a line of the
// walk carries (airoute.ConsideredEntry.Account).
func accountRef(d airoute.Decision) string {
	switch d.AccountKind {
	case memql.VendorAccountCluster:
		return memql.VendorAccountCluster
	case memql.VendorAccountKey:
		return memql.VendorAccount{Kind: memql.VendorAccountKey, KeyId: d.AccountKeyId}.Ref()
	}
	return ""
}

// paidByAKey reports whether a decision says a person's API key paid. What a
// row and a refusal may say about such a call's failure is narrower than for
// any other source (failure_reason.go, recordedError).
func paidByAKey(d airoute.Decision) bool {
	return d.AccountKind == memql.VendorAccountKey
}

// accountHop is K10's second half, for one walk of a wrapper's chain: after a
// failure that is NOT about the account, the record's remaining accounts are
// passed over and the route moves to the next model.
//
// The first half needs no state. A failure that is about the account simply
// lets the loop reach the next entry, which is the same record on the next
// account.
type accountHop struct {
	skip map[string]bool
}

// passOver reports whether this chain entry is another account of a record
// that already failed for a reason no account would change.
func (h *accountHop) passOver(name string) bool {
	if len(h.skip) == 0 {
		return false
	}
	record, ref := splitChainEntry(name)
	return ref != "" && h.skip[record]
}

// failed notes one failed attempt.
func (h *accountHop) failed(name string, err error) {
	record, ref := splitChainEntry(name)
	if ref == "" {
		return
	}
	if _, aboutAccount := memql.VendorAccountFailure(err); aboutAccount {
		return
	}
	if h.skip == nil {
		h.skip = map[string]bool{}
	}
	h.skip[record] = true
}

// splitChainEntry reads a chain entry as a record and the account that pays.
//
// Only a vendor entry is ever split. A fleet model id and an app reference are
// names somebody else chose, and one that happened to end like a reference
// must still resolve as itself.
func splitChainEntry(name string) (record, ref string) {
	if doorFor(name) != DoorFederation {
		return name, ""
	}
	return memql.SplitAccountReference(name)
}
