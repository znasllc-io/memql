package email

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// azureSenderIdentityID is the Campaigns sender record a domain writes for one
// of its addresses: the same id however often that sender is added.
func azureSenderIdentityID(account, address string) string {
	return emailStateID("sender:" + account + ":" + address)
}

// The verified mailbox becomes the same ordinary Campaigns sender record the
// rest of the app already selects. There is no second Azure-specific list.
// It records the sender just added to the domain, default or not.
func (a *azureSetup) saveSenderIdentity(ctx context.Context, account string, sender azureSender) (string, error) {
	identityID := azureSenderIdentityID(account, sender.Address)
	q := strings.Replace(renderConfigCall("senderIdentityById", map[string]string{"senderIdentityId": identityID}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(memql.ContextWithFreshRead(ctx), q)
	if err != nil {
		return "", fmt.Errorf("could not read the organization's sending identity")
	}
	rows := memql.MaterializeRows(result)
	values := map[string]string{"senderIdentityId": identityID, "accountId": account, "address": sender.Address, "fromName": sender.DisplayName, "replyTo": sender.ReplyTo}
	mutation := "createSenderIdentity"
	if len(rows) > 0 {
		row := rows[0]
		if memql.BareShortId(argString(row, "accountId")) != account || argString(row, "address") != sender.Address {
			return "", fmt.Errorf("this sending identity belongs to another organization")
		}
		if argString(row, "fromName") == values["fromName"] && argString(row, "replyTo") == values["replyTo"] {
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
	q := strings.Replace(renderConfigCall("senderIdentitiesForAccount", map[string]string{"accountId": connection.AccountID}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(memql.ContextWithFreshRead(ctx), q)
	if err != nil {
		return nil
	}
	return azureReplies(connection, memql.MaterializeRows(result))
}

// azureReplies answers senderReplies from the organization's sender records. A
// sender's replies go to the Reply-To on its record, else to the connection's,
// which every message that names none carries. The record the domain wrote for
// an address speaks for it; failing that, the newest record still in use.
func azureReplies(connection azureConnection, records []map[string]any) map[string]string {
	own := map[string]string{}
	for _, row := range records {
		address := strings.ToLower(strings.TrimSpace(argString(row, "address")))
		if _, seen := own[address]; argString(row, "status") == "disabled" || seen && memql.BareShortId(argString(row, "id")) != azureSenderIdentityID(connection.AccountID, address) {
			continue
		}
		own[address] = strings.TrimSpace(argString(row, "replyTo"))
	}
	replies := make(map[string]string, len(connection.Config.Senders))
	for address := range connection.Config.Senders {
		replies[address] = connection.ReplyTo
		if own[address] != "" {
			replies[address] = own[address]
		}
	}
	return replies
}
