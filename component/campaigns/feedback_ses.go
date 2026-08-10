package campaigns

import (
	"encoding/json"
	"fmt"
	"strings"
)

// feedback_ses.go -- Amazon SES feedback over SNS (memql#3461).
//
// The second format exists for one reason a DSN cannot cover: a COMPLAINT.
// A complaint is not a delivery failure -- nothing bounced, a person pressed
// "this is spam" -- so there is no DSN to send and no status code to read.
// It is also the single most damaging signal to a sending reputation, which
// makes "we cannot receive one" a poor place to stop.
//
// # Two envelopes, then two payload shapes
//
// SNS wraps the SES message in `{"Type":"Notification","Message":"<json>"}`,
// where Message is JSON *as a string*. A raw SES event posted directly (an
// EventBridge relay, a test) has no wrapper. Both are accepted.
//
// Inside, SES has two spellings of the same thing: the older SNS
// notification uses `notificationType`, the newer event publishing uses
// `eventType`. Same nested `bounce` / `complaint` objects.
//
// # The handshake is not a failure
//
// An SNS topic subscription begins with a `SubscriptionConfirmation`
// message. It is well-formed, it carries no feedback, and it needs the
// operator to visit a URL -- so it is reported as Ignored with that said out
// loud, rather than as a parse error. Calling it an error would put a red
// mark on the one message that means the wiring is working.
//
// # Classification is SES's own field
//
//	bounceType Permanent                  -> hard_bounce
//	bounceType Transient / Undetermined   -> soft_bounce
//	a complaint                           -> complaint
//
// `Undetermined` is treated as transient on purpose. SES says it could not
// tell; suppressing an address permanently on "could not tell" loses real
// subscribers, and the per-recipient retry budget already bounds the cost of
// being wrong in this direction.

type snsEnvelope struct {
	Type             string `json:"Type"`
	Message          string `json:"Message"`
	SubscribeURL     string `json:"SubscribeURL"`
	TopicArn         string `json:"TopicArn"`
	NotificationType string `json:"notificationType"`
}

type sesEvent struct {
	NotificationType string `json:"notificationType"`
	EventType        string `json:"eventType"`
	Bounce           *struct {
		BounceType        string `json:"bounceType"`
		BounceSubType     string `json:"bounceSubType"`
		BouncedRecipients []struct {
			EmailAddress   string `json:"emailAddress"`
			Status         string `json:"status"`
			DiagnosticCode string `json:"diagnosticCode"`
			Action         string `json:"action"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint *struct {
		ComplaintFeedbackType string `json:"complaintFeedbackType"`
		ComplainedRecipients  []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"complainedRecipients"`
	} `json:"complaint"`
	Mail *struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		Tags map[string][]string `json:"tags"`
	} `json:"mail"`
}

// parseSESFeedback reads one SES feedback payload, with or without its SNS
// wrapper.
func parseSESFeedback(body string) (FeedbackParse, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return FeedbackParse{}, fmt.Errorf("the payload is empty")
	}

	var envelope snsEnvelope
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return FeedbackParse{}, fmt.Errorf("the payload is not JSON: %w", err)
	}
	switch envelope.Type {
	case "SubscriptionConfirmation":
		return FeedbackParse{Ignored: "SNS subscription confirmation: visit the SubscribeURL in the message to activate the feedback topic. No feedback was carried"}, nil
	case "UnsubscribeConfirmation":
		return FeedbackParse{Ignored: "SNS unsubscribe confirmation: this deployment is no longer subscribed to the feedback topic"}, nil
	}

	payload := trimmed
	if envelope.Type == "Notification" || (envelope.Message != "" && envelope.NotificationType == "") {
		// SNS carries the SES message as JSON inside a JSON string.
		if strings.TrimSpace(envelope.Message) == "" {
			return FeedbackParse{}, fmt.Errorf("the SNS notification carried an empty Message")
		}
		payload = envelope.Message
	}

	var event sesEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return FeedbackParse{}, fmt.Errorf("the SES message is not JSON: %w", err)
	}

	kind := strings.ToLower(strings.TrimSpace(firstNonEmpty(event.NotificationType, event.EventType)))
	campaignID := sesCampaignID(event)

	switch kind {
	case "bounce":
		return sesBounceReports(event, campaignID)
	case "complaint":
		return sesComplaintReports(event, campaignID)
	case "delivery", "send", "open", "click", "rendering failure", "delivery delay", "subscription":
		return FeedbackParse{Ignored: "SES " + kind + " event: not a bounce or a complaint"}, nil
	case "":
		return FeedbackParse{}, fmt.Errorf("the SES message states no notificationType or eventType, so there is nothing to classify")
	default:
		return FeedbackParse{}, fmt.Errorf("SES event type %q is not one this parser recognises", kind)
	}
}

func sesBounceReports(event sesEvent, campaignID string) (FeedbackParse, error) {
	if event.Bounce == nil {
		return FeedbackParse{}, fmt.Errorf("the SES message says it is a bounce but carries no `bounce` object")
	}
	kind, ok := sesBounceKind(event.Bounce.BounceType)
	if !ok {
		return FeedbackParse{}, fmt.Errorf(
			"SES bounceType %q is not Permanent, Transient or Undetermined, so its severity is not stated", event.Bounce.BounceType)
	}
	note := strings.TrimSpace("bounceType=" + event.Bounce.BounceType + " subType=" + event.Bounce.BounceSubType)

	var out FeedbackParse
	for _, r := range event.Bounce.BouncedRecipients {
		addr := normalizeReportedAddress(r.EmailAddress)
		if addr == "" {
			continue
		}
		detail := note
		if r.Status != "" {
			detail += " status=" + r.Status
		}
		out.Reports = append(out.Reports, FeedbackReport{
			Email: addr, Kind: kind, CampaignID: campaignID,
			Note: redactAddressesIn(collapseWhitespace(detail)),
		})
	}
	if len(out.Reports) == 0 {
		return FeedbackParse{}, fmt.Errorf("the SES bounce named no address this can read")
	}
	return out, nil
}

func sesComplaintReports(event sesEvent, campaignID string) (FeedbackParse, error) {
	if event.Complaint == nil {
		return FeedbackParse{}, fmt.Errorf("the SES message says it is a complaint but carries no `complaint` object")
	}
	note := "complaint"
	if t := strings.TrimSpace(event.Complaint.ComplaintFeedbackType); t != "" {
		note += " feedbackType=" + t
	}
	var out FeedbackParse
	for _, r := range event.Complaint.ComplainedRecipients {
		addr := normalizeReportedAddress(r.EmailAddress)
		if addr == "" {
			continue
		}
		out.Reports = append(out.Reports, FeedbackReport{
			Email: addr, Kind: "complaint", CampaignID: campaignID, Note: note,
		})
	}
	if len(out.Reports) == 0 {
		return FeedbackParse{}, fmt.Errorf("the SES complaint named no address this can read")
	}
	return out, nil
}

// sesBounceKind maps SES's own severity onto the domain's vocabulary.
//
// Undetermined is TRANSIENT. SES is saying it could not tell, and a
// permanent suppression on "could not tell" costs a real subscriber -- while
// being wrong the other way costs at most MEMQL_CAMPAIGNS_MAX_ATTEMPTS more
// tries at a dead address.
func sesBounceKind(bounceType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(bounceType)) {
	case "permanent":
		return "hard_bounce", true
	case "transient", "undetermined":
		return "soft_bounce", true
	default:
		return "", false
	}
}

// sesCampaignID recovers the send from the original message SES echoes back
// -- the X-Campaign-Id header the campaign sender stamps, or an SES message
// tag of the same name. Best-effort: attribution is for the review, not for
// the suppression.
func sesCampaignID(event sesEvent) string {
	if event.Mail == nil {
		return ""
	}
	for _, h := range event.Mail.Headers {
		if strings.EqualFold(strings.TrimSpace(h.Name), headerCampaignID) {
			return strings.TrimSpace(h.Value)
		}
	}
	for name, values := range event.Mail.Tags {
		if strings.EqualFold(strings.TrimSpace(name), headerCampaignID) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
