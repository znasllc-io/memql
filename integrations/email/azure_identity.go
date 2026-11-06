package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// azureSenderIdentityID is the Campaigns sender record a domain writes for one
// of its addresses when the organization has none: the same id however often
// that sender is added.
func azureSenderIdentityID(account, address string) string {
	return emailStateID("sender:" + account + ":" + address)
}

// senderRecords reads the organization's Campaigns sender records, newest first.
func (a *azureSetup) senderRecords(ctx context.Context, account string) ([]map[string]any, error) {
	q := strings.Replace(renderConfigCall("senderIdentitiesForAccount", map[string]string{"accountId": account}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(memql.ContextWithFreshRead(ctx), q)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(result), nil
}

// azureSenderRecord is the record that speaks for one address of the domain:
// the one the domain wrote, else the newest one still in use. Records arrive
// newest first. Nil when the organization has no record in use for it.
func azureSenderRecord(records []map[string]any, account, address string) map[string]any {
	address = strings.ToLower(strings.TrimSpace(address))
	var newest map[string]any
	for _, row := range records {
		if argString(row, "status") == "disabled" || memql.BareShortId(argString(row, "accountId")) != account || strings.ToLower(strings.TrimSpace(argString(row, "address"))) != address {
			continue
		}
		if memql.BareShortId(argString(row, "id")) == azureSenderIdentityID(account, address) {
			return row
		}
		if newest == nil {
			newest = row
		}
	}
	return newest
}

// The verified mailbox becomes the same ordinary Campaigns sender record the
// rest of the app already selects. There is no second Azure-specific list.
//
// It records the sender just added to the domain, default or not, and TAKES
// OVER a record the organization already has for the address -- one recorded
// by hand before Azure had it -- rather than writing a second: automations and
// campaigns name that record, and every send uses its display name, so it is
// the one that must carry the name Azure holds. A record recorded and then
// archived is left as it is.
func (a *azureSetup) saveSenderIdentity(ctx context.Context, account string, sender azureSender) (string, error) {
	records, err := a.senderRecords(ctx, account)
	if err != nil {
		return "", fmt.Errorf("could not read the organization's sending identity")
	}
	identityID := azureSenderIdentityID(account, sender.Address)
	row := azureSenderRecord(records, account, sender.Address)
	if row != nil {
		identityID = memql.BareShortId(argString(row, "id"))
	} else {
		for _, record := range records {
			if memql.BareShortId(argString(record, "id")) == identityID {
				row = record
			}
		}
	}
	values := map[string]string{"senderIdentityId": identityID, "accountId": account, "address": sender.Address, "fromName": sender.DisplayName, "replyTo": sender.ReplyTo}
	mutation := "createSenderIdentity"
	if row != nil {
		if memql.BareShortId(argString(row, "accountId")) != account || !strings.EqualFold(argString(row, "address"), sender.Address) {
			return "", fmt.Errorf("this sending identity belongs to another organization")
		}
		if argString(row, "address") == sender.Address && argString(row, "fromName") == values["fromName"] && argString(row, "replyTo") == values["replyTo"] {
			return identityID, nil
		}
		mutation = "updateSenderIdentity"
	}
	if _, err = a.store.engine.Execute(ctx, renderConfigCall(mutation, values)); err != nil {
		return "", fmt.Errorf("could not save the organization's sending identity; check your organization permissions and try again")
	}
	return identityID, nil
}

// senderReplies says where replies to each sender the domain has registered go,
// for a page that lists them. Nil when the sender records cannot be read, so the
// page says nothing rather than something untrue.
func (a *azureSetup) senderReplies(ctx context.Context, connection azureConnection) map[string]string {
	if len(connection.Config.Senders) == 0 {
		return nil
	}
	records, err := a.senderRecords(ctx, connection.AccountID)
	if err != nil {
		slog.Warn("email: the sending domain's sender records could not be read, so its page shows no reply addresses", "error", err)
		return nil
	}
	return azureReplies(connection, records)
}

// azureReplies answers senderReplies from the organization's sender records. A
// sender's replies go to the Reply-To on the record that speaks for it, else to
// the connection's, which every message that names none carries.
func azureReplies(connection azureConnection, records []map[string]any) map[string]string {
	replies := make(map[string]string, len(connection.Config.Senders))
	for address := range connection.Config.Senders {
		replies[address] = connection.ReplyTo
		if own := strings.TrimSpace(argString(azureSenderRecord(records, connection.AccountID, address), "replyTo")); own != "" {
			replies[address] = own
		}
	}
	return replies
}
