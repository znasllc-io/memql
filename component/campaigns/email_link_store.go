package campaigns

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// NudgeWaitingEnrollments re-checks waiting confirmations and recovers a late
// confirmation after its timeout. The page releases its choice gate first;
// recovery takes journey -> recipient -> enrollment, the drain's lock order.
func (s *Store) NudgeWaitingEnrollments(ctx context.Context, recipientID string, at time.Time, coordinate emailEnrollmentGate) error {
	ctx = auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx))
	var candidates []map[string]any
	cursor := ""
	for {
		rows, next, err := s.rowsPage(ctx, cursor, call("query", "waitingEnrollmentsForRecipient", arg{"recipientId", bare(recipientID)}))
		if err != nil {
			return err
		}
		candidates = append(candidates, rows...)
		if next == "" {
			break
		}
		if next == cursor {
			return fmt.Errorf("confirmation enrollment scan repeated its cursor")
		}
		cursor = next
	}
	for _, candidate := range candidates {
		if err := s.nudgeEnrollment(ctx, recipientID, at, candidate, coordinate); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) nudgeEnrollment(ctx context.Context, recipientID string, at time.Time, candidate map[string]any, coordinate emailEnrollmentGate) error {
	journeyID, enrollmentID := bare(str(candidate, "journeyId")), bare(str(candidate, "id"))
	hold, err := coordinate(context.WithoutCancel(ctx), journeyID, bare(recipientID), enrollmentID)
	if err != nil {
		return err
	}
	defer hold.Release()
	phase, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	read := func(name, key, id string) (map[string]any, error) {
		rows, err := s.rows(phase, call("query", name, arg{key, id}))
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[0], nil
	}
	row, err := read("journeyEnrollmentById", "enrollmentId", enrollmentID)
	if err != nil || row == nil {
		return err
	}
	waiting := str(row, "status") == "waiting"
	if !waiting && !(str(row, "status") == "exited" && str(row, "exitReason") == "not confirmed") {
		return nil
	}
	source, err := read("journeyById", "journeyId", journeyID)
	if err != nil || source == nil {
		return err
	}
	journey, err := journeyFromRow(source)
	if err != nil {
		return err
	}
	if journey.Status == "archived" || (!waiting && journey.Status != "active") {
		return nil
	}
	recipient, found, err := s.RecipientByID(ownerActorContext(phase, journey.OwnerUserID), recipientID)
	if err != nil {
		return err
	}
	if !found || recipient.SubscriptionStatus != "subscribed" || recipient.JoinedAt.IsZero() ||
		bare(str(row, "recipientId")) != bare(recipientID) || bare(str(row, "journeyId")) != journey.ID ||
		bare(str(row, "ownerUserId")) != journey.OwnerUserID || !sameOrganization(str(row, "accountId"), journey.AccountID) ||
		!sameOrganization(recipient.AccountID, journey.AccountID) || EmailDigest(recipient.Email) != str(row, "emailDigest") ||
		journeyEnrollmentID(journey.ID, recipientID, recipient.JoinedAt) != enrollmentID {
		return nil
	}
	if _, blocked, err := s.SuppressionForSend(phase, journey.AccountID, EmailDigest(recipient.Email)); err != nil || blocked {
		return err
	}
	version := memql.VersionTimeAfter(templateTime(row["createdAt"]), at).Format(time.RFC3339Nano)
	mutation := "nudgeJourneyEnrollment"
	args := []arg{{"enrollmentId", enrollmentID}, {"nextAt", at.UTC().Format(time.RFC3339Nano)}, {"versionTime", version}}
	if !waiting {
		// A stable Confirm step id advances to its following step. If editing
		// removed it, its former index names the next surviving step instead.
		index := max(0, min(integer(row, "stepIndex"), len(journey.Steps)))
		for i, step := range journey.Steps {
			if step.ID == str(row, "stepId") {
				if step.Kind != "confirm" {
					return nil
				}
				index = i + 1
				break
			}
		}
		nextID, status, completed := "", "active", ""
		if index < len(journey.Steps) {
			nextID = journey.Steps[index].ID
		} else {
			status = "completed"
			completed = at.UTC().Format(time.RFC3339Nano)
		}
		mutation = "advanceJourneyEnrollment"
		args = append(args, arg{"stepId", nextID}, arg{"stepIndex", index}, arg{"status", status}, arg{"exitReason", ""}, arg{"completedAt", completed}, arg{"attempts", 0}, arg{"lastError", ""})
	}
	if err := hold.Verify(phase); err != nil {
		return err
	}
	return detachedWrite(phase, func(writeCtx context.Context) error {
		return s.execServerOnly(writeCtx, call("mutation", mutation, args...))
	})
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
