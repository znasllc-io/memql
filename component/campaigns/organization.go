package campaigns

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// Authorization to read two organizations never authorizes mailing one with
// the other's resources. Legacy private, unattributed rows may still work
// together; attributing either side requires attributing both to the same org.
func sameOrganization(accountID, resourceAccountID string) bool {
	return bare(accountID) == bare(resourceAccountID)
}

func (w *Worker) validateCampaignOrganization(ctx context.Context, campaign Campaign, tmpl Template) error {
	if !sameOrganization(campaign.AccountID, tmpl.AccountID) {
		return fmt.Errorf("campaign and template must belong to the same organization")
	}
	if campaign.AccountID != "" && !sameOrganization(campaign.AccountID, w.store.AudienceAccountID(ctx, campaign.AudienceID)) {
		return fmt.Errorf("campaign audience must be readable and belong to the same organization")
	}
	return nil
}

// errNoSendAuthority is requireSendAuthority's refusal. One value, so a caller
// that answers a refusal by what it is (the row lane's code for it,
// send_row.go) reads it without reading its text.
var errNoSendAuthority = errors.New("campaign send requires write permission in this organization")

// A send is an action even when it writes no campaign row (test copies and
// single-recipient sends). Re-resolve current permissions before delivery so
// an organization reader cannot mail simply because the inputs were readable.
func (w *Worker) requireSendAuthority(ctx context.Context, account string) error {
	if w.store == nil || w.store.engine == nil || !w.store.engine.OrganizationCapable(ctx, account, auth.VerbUpdate, auth.ResourceData) {
		return errNoSendAuthority
	}
	return nil
}
