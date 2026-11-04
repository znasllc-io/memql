package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// send_order.go -- THE THIRD SHAPE OF AUTOMATED MAIL: what an ORDER entitles.
//
// The program fixes two shapes (send_to_recipient.go): the transactional
// outbox, for this cluster's own people, and the marketing lane, for people
// already on a list. A message an order entitles -- the request a store sends
// the buyer of an order, about that order -- is neither: the buyer is on no
// list, and a store that asked only its subscribers would be asking only the
// customers it expects to be happy.
//
// # An order, never an address
//
// The route takes (storeId, orderGid) and reads the buyer's address off the
// mirrored order ITSELF; no argument names a recipient, so nothing can point
// it at a stranger. The message is built for that one buyer as a recipient
// that exists only for this send -- no recipient row is written, so the buyer
// lands on no list a campaign could reach -- and the caller's fields fill the
// template's {{fields.<key>}} tags (the product, the link, its expiry).
//
// # Every lawful-mail property of the marketing lane, but no subscription
//
// It runs through the same send as the marketing lane's primitive: the
// organization's suppression list at the point of send, the RFC 8058
// one-click pair (whose unsubscribe suppresses the address for the
// organization), the organization's sender, the reviewed template, pacing,
// and the delivery ledger -- idempotent per requestId, the same durable
// receipt a single send keeps. What it does not ask is a subscription: the
// order is the reason for the message.
//
// # For trusted jobs only
//
// A client that could call this could mail the buyer of any order its store
// mirrors, so a call without internal origin is refused before anything is
// read. The reviews pack's request job calls it from a trusted automation,
// borrowing the store's merchant, whose organization the template and the
// sender must belong to.

// handleSendOrderMessage answers campaignSendOrderMessage.
func (w *Worker) handleSendOrderMessage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("campaigns.sendOrderMessage: a message an order entitles is sent by a trusted job, never by a client")
	}
	storeID := strings.TrimSpace(argString(args, "storeId"))
	orderGid := strings.TrimSpace(argString(args, "orderGid"))
	if storeID == "" || orderGid == "" || strings.TrimSpace(argString(args, "templateId")) == "" || strings.TrimSpace(argString(args, "requestId")) == "" {
		return nil, errors.New("campaigns.sendOrderMessage: storeId, orderGid, templateId and requestId are required")
	}
	owner := strings.TrimSpace(callerUserID(ctx))
	if owner == "" {
		return nil, errors.New("campaigns.sendOrderMessage: no caller identity; the job borrows the store's merchant")
	}
	// THE ORGANIZATION IS THE STORE'S, CHECKED HERE, NEVER TAKEN. It scopes
	// the suppression list consulted and the unsubscribe the buyer is handed,
	// so the send runs only under the organization whose site SERVES the
	// store; the template, the sender and the caller's authority are then held
	// to that organization by the send itself. A store previewed by another
	// organization's site is not that organization's store.
	account := strings.TrimSpace(argString(args, "accountId"))
	if account == "" {
		return nil, errors.New("campaigns.sendOrderMessage: accountId is required: the send runs under the store's own organization")
	}
	served, err := w.store.StoreServedBy(w.systemActorContext(ctx), storeID, account)
	if err != nil {
		return nil, undecided(fmt.Errorf("campaigns.sendOrderMessage: %w", err))
	}
	if !served {
		return nil, fmt.Errorf("campaigns.sendOrderMessage: store %q is not served by a site of organization %q", storeID, account)
	}
	email, found, err := w.store.OrderBuyerEmail(w.systemActorContext(ctx), storeID, orderGid)
	if err != nil {
		return nil, undecided(fmt.Errorf("campaigns.sendOrderMessage: %w", err))
	}
	if !found {
		return nil, fmt.Errorf("campaigns.sendOrderMessage: order %q is not mirrored on store %q", orderGid, storeID)
	}
	if EmailDigest(email) == "" {
		return nil, fmt.Errorf("campaigns.sendOrderMessage: order %q carries no address to write to", orderGid)
	}
	recipient := Recipient{
		ID:        orderRecipientID(storeID, orderGid),
		AccountID: account,
		Email:     email,
		Fields:    orderFields(args["fields"]),
	}
	// THE SEND IS THE MARKETING LANE'S, with the recipient this order
	// entitles in place of a list member. Only the arguments the send reads
	// go on: nothing a caller put beside the order can name another address.
	send := map[string]any{
		"templateId":  argString(args, "templateId"),
		"recipientId": recipient.ID,
		"requestId":   argString(args, "requestId"),
	}
	if id := strings.TrimSpace(argString(args, "senderIdentityId")); id != "" {
		send["senderIdentityId"] = id
	}
	return w.sendToRecipientOnce(ctx, send, singleSend{deferWhenLimited: true, orderRecipient: &recipient})
}

// orderRecipientID is the stable id of the recipient one order entitles: per
// store and order, a bare slug. It names no row.
func orderRecipientID(storeID, orderGid string) string {
	sum := sha256.Sum256([]byte(storeID + "\x00" + orderGid))
	return "order-" + hex.EncodeToString(sum[:])[:24]
}

// orderFields keeps the caller's fields that are text, for {{fields.<key>}}.
func orderFields(v any) map[string]any {
	in, _ := v.(map[string]any)
	out := map[string]any{}
	for k, val := range in {
		k = strings.TrimSpace(k)
		if s, ok := val.(string); ok && k != "" {
			out[k] = s
		}
	}
	return out
}

// OrderBuyerEmail reads the checkout address of one mirrored order. Orders
// are cluster-owner tier, so the caller passes a system identity.
func (s *Store) OrderBuyerEmail(ctx context.Context, storeID, orderGid string) (string, bool, error) {
	rows, err := s.rows(ctx, call("query", "shopifyOrderByGid", arg{"gid", orderGid}, arg{"storeId", storeID}))
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	if memql.BareShortId(strings.TrimSpace(str(rows[0], "storeId"))) != memql.BareShortId(storeID) && strings.TrimSpace(str(rows[0], "storeId")) != "" {
		return "", false, nil
	}
	return strings.TrimSpace(str(rows[0], "email")), true, nil
}

// StoreServedBy reports whether a site of the organization SERVES the store:
// its serving binding names it. A preview binding does not make a store an
// organization's. The binding holds whichever spelling of the store id its
// writer used, so both are asked.
func (s *Store) StoreServedBy(ctx context.Context, storeID, account string) (bool, error) {
	bareStore := memql.BareShortId(strings.TrimSpace(storeID))
	rows, err := s.rows(ctx, call("query", "sitesBoundToStore", arg{"storeIds", []string{bareStore, "v1:shopify:store:" + bareStore}}))
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		binding, _ := row["binding"].(map[string]any)
		if memql.BareShortId(strings.TrimSpace(str(binding, "storeId"))) != bareStore {
			continue
		}
		if sameOrganization(str(row, "accountId"), account) {
			return true, nil
		}
	}
	return false, nil
}
