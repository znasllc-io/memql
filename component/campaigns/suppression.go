package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/znasllc-io/memql/component/memql"
)

func organizationSuppressionID(accountID, digest string) string {
	sum := sha256.Sum256([]byte("campaigns/organization-suppression\x00" + bare(accountID) + "\x00" + digest))
	return hex.EncodeToString(sum[:])
}

// SuppressionForSend preserves cluster-wide safety blocks, then checks the
// client's opt-outs. Fresh reads matter when another replica handles the click.
func (s *Store) SuppressionForSend(ctx context.Context, accountID, digest string) (Suppression, bool, error) {
	global, found, err := s.SuppressionByDigest(ctx, digest)
	if found || err != nil || accountID == "" || digest == "" {
		return global, found, err
	}
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "suppressionForOrganization",
		arg{"suppressionId", organizationSuppressionID(accountID, digest)}, arg{"accountId", bare(accountID)}))
	if err != nil || len(rows) == 0 {
		return Suppression{}, false, err
	}
	r := rows[len(rows)-1]
	if str(r, "liftedAt") != "" {
		return Suppression{}, false, nil
	}
	return Suppression{Digest: digest, Reason: str(r, "reason"), SuppressedAt: parseTime(str(r, "suppressedAt"))}, true, nil
}

// BlockingSuppression is the row lane's suppression read (X9, G7). A message
// about one person's own request is not mail from a list, so an unsubscribe --
// from the cluster or from the organization -- does not stop it; an address that
// bounced, complained, or that an operator blocked does. It reads both rows,
// cluster first, and answers the first whose reason blocks. A lifted
// organization row blocks nothing. A failed read is an error, never "not
// suppressed".
func (s *Store) BlockingSuppression(ctx context.Context, accountID, digest string) (Suppression, bool, error) {
	if digest == "" {
		return Suppression{}, false, nil
	}
	global, found, err := s.SuppressionByDigest(ctx, digest)
	if err != nil {
		return Suppression{}, false, err
	}
	if found && blocksATransactionalMessage(global.Reason) {
		return global, true, nil
	}
	if accountID == "" {
		return Suppression{}, false, nil
	}
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "suppressionForOrganization",
		arg{"suppressionId", organizationSuppressionID(accountID, digest)}, arg{"accountId", bare(accountID)}))
	if err != nil || len(rows) == 0 {
		return Suppression{}, false, err
	}
	r := rows[len(rows)-1]
	if str(r, "liftedAt") != "" || !blocksATransactionalMessage(str(r, "reason")) {
		return Suppression{}, false, nil
	}
	return Suppression{Digest: digest, Reason: str(r, "reason"), SuppressedAt: parseTime(str(r, "suppressedAt"))}, true, nil
}

// blocksATransactionalMessage: a dead address, a complaint and an operator's
// block stop every message; an unsubscribe stops only a list's.
func blocksATransactionalMessage(reason string) bool {
	switch reason {
	case "hard_bounce", "complaint", "manual":
		return true
	}
	return false
}

// RecordOrganizationSuppression is append-only: no read-modify-write, process
// cache, or lease is involved. A retry writes the same organization/digest key.
func (s *Store) RecordOrganizationSuppression(ctx context.Context, accountID, digest, reason, domain, campaignID, note string) error {
	if accountID == "" || digest == "" {
		return errors.New("organization and email digest are required for an organization suppression")
	}
	return s.execServerOnly(ctx, call("mutation", "recordOrganizationSuppression",
		arg{"suppressionId", organizationSuppressionID(accountID, digest)}, arg{"accountId", bare(accountID)},
		arg{"emailDigest", digest}, arg{"reason", reason}, arg{"domain", domain},
		arg{"sourceCampaignId", campaignID}, arg{"note", note}))
}
