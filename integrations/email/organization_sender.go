package email

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

type azureConnection struct {
	SenderIdentityID string    `json:"senderIdentityId"`
	Status           string    `json:"status"`
	AccountID        string    `json:"accountId"`
	Plan             azurePlan `json:"plan"`
	ReplyTo          string    `json:"replyTo"`
	Config           ACSConfig `json:"config"`
}

// Preparing the operator's first ACS connection must not interrupt the
// currently configured transactional transport halfway through DNS setup.
// This exception never applies to a client or an explicit disconnect.
func (c azureConnection) retainsOperatorTransport() bool {
	if c.AccountID != "self" || c.Config.AccountID != "" {
		return false
	}
	switch c.Status {
	case "planned", "provisioning", "dns", "verified":
		return true
	}
	return false
}

// Resolve afresh on every send: a disconnect or credential rotation on one
// replica takes effect on the next message handled by every other replica.
// An explicitly selected organization never falls back to the operator's mail.
func (a *azureSetup) sender(ctx context.Context, account string) (Sender, error) {
	account = memql.BareShortId(strings.TrimSpace(account))
	var connection azureConnection
	found, _, err := a.store.read(ctx, "organization:"+account, &connection)
	if err != nil {
		return nil, permanentSendRefusal("could not read this organization's email connection")
	}
	if !found {
		if account == "self" {
			return nil, nil
		}
		return nil, permanentSendRefusal("connect this organization's email domain before sending")
	}
	if account == "self" && connection.retainsOperatorTransport() {
		return nil, nil
	}
	if connection.Status != "ready" || connection.AccountID != account || connection.Config.AccountID != account {
		return nil, permanentSendRefusal("this organization's email connection is not ready")
	}
	q := strings.Replace(renderConfigCall("clientAccountById", map[string]string{"accountId": account}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(emailStateContext(ctx), q)
	if err != nil {
		return nil, permanentSendRefusal("could not verify this organization's email connection")
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 || rows[0]["status"] != "active" {
		return nil, permanentSendRefusal("the sending organization is inactive")
	}
	sender, err := NewACSSender(connection.Config)
	if err != nil {
		return nil, permanentSendRefusal("this organization's Azure email connection is invalid; connect again")
	}
	sender.operations = &storedACSOperations{connection: a.store}
	return sender, nil
}
