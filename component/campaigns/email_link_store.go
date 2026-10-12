package campaigns

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// NudgeWaitingEnrollments makes every waiting automation re-check this recipient.
// Collect before writing: the query sorts by version time, which each nudge moves.
func (s *Store) NudgeWaitingEnrollments(ctx context.Context, recipientID string, at time.Time) error {
	ctx = auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx))
	var waiting []map[string]any
	cursor := ""
	for {
		rows, next, err := s.rowsPage(ctx, cursor, call("query", "waitingEnrollmentsForRecipient", arg{"recipientId", bare(recipientID)}))
		if err != nil {
			return err
		}
		waiting = append(waiting, rows...)
		if next == "" {
			break
		}
		cursor = next
	}
	for _, row := range waiting {
		if err := s.execServerOnly(ctx, call("mutation", "nudgeJourneyEnrollment", arg{"enrollmentId", bare(str(row, "id"))}, arg{"nextAt", at.UTC().Format(time.RFC3339Nano)}, arg{"versionTime", memql.VersionTimeAfter(templateTime(row["createdAt"]), at).Format(time.RFC3339Nano)})); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) consentExists(ctx context.Context, eventID string) (bool, error) {
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "newsletterConsentById", arg{"eventId", eventID}))
	return len(rows) > 0, err
}

func (s *Store) organizationSuppression(ctx context.Context, accountID, digest string) (map[string]any, error) {
	if accountID == "" || digest == "" {
		return nil, nil
	}
	rows, err := s.rows(memql.ContextWithFreshRead(ctx), call("query", "suppressionForOrganization", arg{"suppressionId", organizationSuppressionID(accountID, digest)}, arg{"accountId", bare(accountID)}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[len(rows)-1], nil
}

// LiftOrganizationSuppression can lift only this organization's own opt-out.
// Global bounce/complaint rows remain authoritative and are never altered here.
func (s *Store) LiftOrganizationSuppression(ctx context.Context, accountID, digest string, at time.Time) error {
	row, err := s.organizationSuppression(ctx, accountID, digest)
	if err != nil {
		return err
	}
	if row == nil || str(row, "reason") != "unsubscribed" || str(row, "liftedAt") != "" {
		return nil
	}
	return s.execServerOnly(ctx, call("mutation", "liftOrganizationSuppression", arg{"suppressionId", organizationSuppressionID(accountID, digest)}, arg{"accountId", bare(accountID)}, arg{"liftedAt", at.UTC().Format(time.RFC3339Nano)}, arg{"versionTime", memql.VersionTimeAfter(templateTime(row["createdAt"]), at).Format(time.RFC3339Nano)}))
}

// TemplateAtRevision reads the stored version named by a view token, under the
// sender's current authority. A later edit must never change an old message.
func (s *Store) TemplateAtRevision(ctx context.Context, templateID, revision string) (Template, bool, error) {
	rows, err := s.rows(ctx, call("query", "templateAtRevision", arg{"templateId", bare(templateID)}, arg{"revision", revision}))
	if err != nil || len(rows) == 0 {
		return Template{}, false, err
	}
	r := rows[0]
	return Template{Revision: templateTime(r["createdAt"]).Format(time.RFC3339Nano), Name: str(r, "name"), AccountID: bare(str(r, "accountId")), ID: bare(str(r, "id")), Subject: str(r, "subject"), TextBody: str(r, "textBody"), HTMLBody: str(r, "htmlBody"), Status: str(r, "status")}, true, nil
}
