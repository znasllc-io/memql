package email

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// The verified mailbox becomes the same ordinary Campaigns sender record the
// rest of the app already selects. There is no second Azure-specific list.
func (a *azureSetup) saveSenderIdentity(ctx context.Context, connection azureConnection) (string, error) {
	identityID := emailStateID("sender:" + connection.AccountID + ":" + connection.Config.Default)
	q := strings.Replace(renderConfigCall("senderIdentityById", map[string]string{"senderIdentityId": identityID}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(memql.ContextWithFreshRead(ctx), q)
	if err != nil {
		return "", fmt.Errorf("could not read the organization's sending identity")
	}
	rows := memql.MaterializeRows(result)
	values := map[string]string{"senderIdentityId": identityID, "accountId": connection.AccountID, "address": connection.Config.Default, "fromName": connection.Config.Senders[connection.Config.Default], "replyTo": connection.ReplyTo}
	mutation := "createSenderIdentity"
	if len(rows) > 0 {
		row := rows[0]
		if memql.BareShortId(argString(row, "accountId")) != connection.AccountID || argString(row, "address") != connection.Config.Default {
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
