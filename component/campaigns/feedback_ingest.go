package campaigns

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// feedback_ingest.go -- the wire from a provider's webhook to the
// suppression list (memql#3461).
//
// # Where the seam is, and why it is here
//
// The issue asks whether the parser should be a Go integration or a DSL
// automation over the existing inbound-webhook surface. It is both, split at
// the point where each is actually better:
//
//	TRANSPORT   the existing POST /inbound/{source} receiver. It already has
//	            the deny-by-default source allowlist and the per-source HMAC
//	            this needs, and it already stages a verified row. Building a
//	            second ingress with the same properties would be building it
//	            worse.
//	TRIGGER     a DSL automation on the staged row. Ordinary graph plumbing;
//	            nothing about it wants Go.
//	PARSING     Go. A DSN is folded RFC 822 field groups inside a MIME
//	            report and an SES payload is JSON nested inside JSON inside a
//	            string. A filter expression is the wrong instrument, and one
//	            that half-worked would drop reports silently.
//
// # What authorizes a suppression, and why it is not a role
//
// campaignSuppress and campaignRecordFeedback are admin-gated because a
// caller is ASSERTING a verdict: they say this address bounced and the
// cluster believes them. This path asserts nothing. It re-reads a payload a
// third party signed with a secret the operator configured, arriving at a
// source the operator listed, in a format the operator named -- three
// deployment-level decisions, all made in env, all made by whoever would
// have held the admin role anyway.
//
// So the gate is the CONFIGURATION rather than the caller:
//
//  1. the inbound row's source must appear in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES;
//  2. the row must carry signatureVerified=true, so a source configured
//     scheme='none' can never drive a suppression;
//  3. every address and every verdict comes out of the signed body.
//
// The most a caller can do by invoking this on an arbitrary row id is cause
// a webhook the operator already trusted to be processed again, which is
// idempotent. Gating on admin instead would have meant every deployment
// minting a service-account credential for an automation to present -- real
// operator burden buying no property this does not already have.
//
// # Nothing is dropped quietly
//
// A payload that cannot be read stamps the inbound row `failed` with the
// reason and returns an error. `inboundRequestsByStatus(status: "failed")`
// is then the list of feedback this deployment could not understand. A
// payload that reads fine and carries no bounce -- a delivery receipt, an
// SNS handshake -- stamps `processed` and says what it was, because
// recording that as a failure would teach an operator to ignore failures.

func (w *Worker) handleIngestFeedback(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	requestID := strings.TrimSpace(argString(args, "inboundRequestId"))
	if requestID == "" {
		return nil, fmt.Errorf("campaigns.ingestFeedback: inboundRequestId is required")
	}

	req, found, err := w.store.InboundRequestByID(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.ingestFeedback: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.ingestFeedback: inbound request %q not found", requestID)
	}

	format := w.cfg.FeedbackFormatFor(req.Source)
	if format == "" {
		// The common case for this automation: an inbound row from some
		// other integration entirely. Not a fault, and it must not read as
		// one -- an error here would put a failure line in the log for every
		// webhook this deployment receives.
		return feedbackResult(map[string]any{
			"ingested": false,
			"source":   req.Source,
			"reason":   "the source is not listed in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES, so it is not a campaign feedback feed on this deployment",
		})
	}
	if !req.SignatureVerified {
		return nil, w.failFeedback(ctx, req, fmt.Sprintf(
			"source %q is configured as a %s feedback feed but this delivery was not signature-verified; "+
				"an unverified payload may not drive a cluster-wide suppression", req.Source, format))
	}

	parsed, err := ParseFeedback(format, req.Body)
	if err != nil {
		return nil, w.failFeedback(ctx, req, fmt.Sprintf("could not read the %s payload from %q: %v", format, req.Source, err))
	}
	if len(parsed.Reports) == 0 {
		w.markFeedbackProcessed(ctx, req)
		return feedbackResult(map[string]any{
			"ingested": true,
			"source":   req.Source,
			"format":   format,
			"reports":  0,
			"reason":   parsed.Ignored,
		})
	}

	suppressed, recorded := 0, 0
	for _, report := range parsed.Reports {
		applied, err := w.applyFeedbackReport(ctx, report)
		if err != nil {
			return nil, w.failFeedback(ctx, req, fmt.Sprintf("could not record a %s report: %v", report.Kind, err))
		}
		recorded++
		if applied {
			suppressed++
		}
	}
	w.markFeedbackProcessed(ctx, req)

	w.logger.Info("campaigns: provider feedback ingested",
		"source", req.Source, "format", format, "reports", recorded, "suppressed", suppressed)

	return feedbackResult(map[string]any{
		"ingested":   true,
		"source":     req.Source,
		"format":     format,
		"reports":    recorded,
		"suppressed": suppressed,
	})
}

// applyFeedbackReport is the same decision handleRecordFeedback documents,
// reached from a signed payload instead of an admin's argument: a hard
// bounce or a complaint suppresses the address cluster-wide and leaves the
// audience membership alone; a soft bounce does neither.
//
// Returns whether the address was suppressed.
func (w *Worker) applyFeedbackReport(ctx context.Context, report FeedbackReport) (bool, error) {
	digest := EmailDigest(report.Email)
	if digest == "" {
		return false, fmt.Errorf("%q is not a usable email address", redactAddress(report.Email))
	}
	switch report.Kind {
	case "soft_bounce":
		// Transient. Suppressing on one is how a sender loses a real
		// subscriber to a full mailbox; MEMQL_CAMPAIGNS_MAX_ATTEMPTS bounds
		// the cost of not suppressing.
		return false, nil
	case "hard_bounce", "complaint":
	default:
		return false, fmt.Errorf("report kind %q is not hard_bounce / soft_bounce / complaint", report.Kind)
	}
	if err := w.store.RecordSuppression(
		w.systemActorContext(ctx),
		digest,
		report.Kind,
		EmailDomain(report.Email),
		memql.BareShortId(report.CampaignID),
		report.Note,
	); err != nil {
		return false, err
	}
	return true, nil
}

// failFeedback stamps the inbound row and returns the error to the caller,
// so the same fact is both queryable and loud.
func (w *Worker) failFeedback(ctx context.Context, req InboundRequest, reason string) error {
	if err := w.store.SetInboundStatus(ctx, req.ID, "failed", reason); err != nil {
		w.logger.Warn("campaigns: could not stamp an unreadable feedback payload as failed", "request", req.ID, "error", err)
	}
	w.logger.Warn("campaigns: provider feedback could not be ingested", "request", req.ID, "source", req.Source, "reason", reason)
	return fmt.Errorf("campaigns.ingestFeedback: %s", reason)
}

func (w *Worker) markFeedbackProcessed(ctx context.Context, req InboundRequest) {
	if err := w.store.SetInboundStatus(ctx, req.ID, "processed", ""); err != nil {
		w.logger.Warn("campaigns: could not stamp a processed feedback payload", "request", req.ID, "error", err)
	}
}

func feedbackResult(payload map[string]any) ([]memorynodes.MemoryNode, error) {
	return resultNode("campaignFeedbackIngest", payload)
}

// InboundRequest is the slice of v1:platform:inboundRequest this path reads.
type InboundRequest struct {
	ID                string
	Source            string
	Body              string
	SignatureVerified bool
}

// InboundRequestByID reads one staged webhook delivery.
func (s *Store) InboundRequestByID(ctx context.Context, requestID string) (InboundRequest, bool, error) {
	rows, err := s.rows(ctx, call("query", "inboundRequestById", arg{"requestId", requestID}))
	if err != nil || len(rows) == 0 {
		return InboundRequest{}, false, err
	}
	r := rows[0]
	return InboundRequest{
		ID:                bare(str(r, "id")),
		Source:            str(r, "source"),
		Body:              str(r, "body"),
		SignatureVerified: boolean(r, "signatureVerified"),
	}, true, nil
}

// SetInboundStatus stamps the product-side handling transition the inbound
// receiver deliberately never writes past `received`.
func (s *Store) SetInboundStatus(ctx context.Context, requestID, status, lastError string) error {
	args := []arg{
		{"requestId", requestID},
		{"status", status},
		{"processedAt", time.Now().UTC().Format(time.RFC3339)},
	}
	// Always sent, including empty: a row moving from `failed` to
	// `processed` on a redelivery must not keep the earlier reason.
	args = append(args, arg{"lastError", truncateError(lastError)})
	return s.exec(ctx, call("mutation", "updateInboundRequestStatus", args...))
}
