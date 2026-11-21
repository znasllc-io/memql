package providers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func digestMatches(body []byte, signature, secret string) bool {
	actual, err := hex.DecodeString(signature)
	if err != nil || len(actual) != sha256.Size || secret == "" {
		return false
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hmac.Equal(h.Sum(nil), actual)
}
func fresh(timestamp string, now time.Time) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	delta := now.Unix() - seconds
	return delta >= -300 && delta <= 300
}
func VerifyStripeWebhook(body []byte, header, secret string, now time.Time) (Webhook, error) {
	if len(body) > maxResponseBytes {
		return Webhook{}, ErrInvalidSignature
	}
	var timestamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		pair := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(pair) != 2 {
			continue
		}
		switch pair[0] {
		case "t":
			if timestamp != "" {
				return Webhook{}, ErrInvalidSignature
			}
			timestamp = pair[1]
		case "v1":
			signatures = append(signatures, pair[1])
		}
	}
	if !fresh(timestamp, now) {
		return Webhook{}, ErrInvalidSignature
	}
	signed := append([]byte(timestamp+"."), body...)
	valid := false
	for _, signature := range signatures {
		if digestMatches(signed, signature, secret) {
			valid = true
			break
		}
	}
	if !valid {
		return Webhook{}, ErrInvalidSignature
	}
	var v struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Livemode bool   `json:"livemode"`
		Created  int64  `json:"created"`
		Data     struct {
			Object struct {
				ID            string            `json:"id"`
				Status        string            `json:"status"`
				Metadata      map[string]string `json:"metadata"`
				Reference     string            `json:"client_reference_id"`
				PaymentIntent json.RawMessage   `json:"payment_intent"`
			} `json:"object"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &v) != nil || v.ID == "" || v.Data.Object.ID == "" {
		return Webhook{}, ErrInvalidSignature
	}
	if v.Livemode {
		return Webhook{}, ErrWrongEnvironment
	}
	account := v.Data.Object.Metadata["account_id"]
	if account == "" {
		account = v.Data.Object.Reference
	}
	objectID := v.Data.Object.ID
	if v.Type == "charge.refunded" || strings.HasPrefix(v.Type, "charge.dispute.") {
		// A charge/dispute is a trigger to reconcile the durable PaymentIntent,
		// never a direct debit/credit from the webhook's potentially stale state.
		var reference string
		if json.Unmarshal(v.Data.Object.PaymentIntent, &reference) != nil {
			var expanded struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(v.Data.Object.PaymentIntent, &expanded) == nil {
				reference = expanded.ID
			}
		}
		if !strings.HasPrefix(reference, "pi_") || !validIdentifier(reference) {
			return Webhook{}, ErrInvalidSignature
		}
		objectID = reference
	}
	return Webhook{ID: v.ID, Type: v.Type, ObjectID: objectID, Status: v.Data.Object.Status, AccountID: account, OperationID: v.Data.Object.Metadata["operation_id"], CreatedAt: time.Unix(v.Created, 0).UTC()}, nil
}
func VerifyDiditWebhook(body []byte, headers http.Header, secret string, now time.Time) (Webhook, error) {
	if len(body) > maxResponseBytes || !fresh(headers.Get("X-Timestamp"), now) {
		return Webhook{}, ErrInvalidSignature
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return Webhook{}, ErrInvalidSignature
	}
	// encoding/json sorts map keys recursively and normalizes whole-valued
	// numbers; HTML escaping must be off for Didit's Unicode-preserving V2 form.
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(parsed) != nil {
		return Webhook{}, ErrInvalidSignature
	}
	normalized := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	valid := false
	for _, name := range []string{"X-Signature-V2", "X-Signature-V2-Previous"} {
		if digestMatches(normalized, headers.Get(name), secret) {
			valid = true
		}
	}
	// Exact raw-body verification also covers the complete payload. The deprecated
	// Simple signature is deliberately not accepted because it omits the decision.
	if !valid {
		for _, name := range []string{"X-Signature", "X-Signature-Previous"} {
			if digestMatches(body, headers.Get(name), secret) {
				valid = true
			}
		}
	}
	if !valid {
		return Webhook{}, ErrInvalidSignature
	}
	var v struct {
		ID          string            `json:"event_id"`
		Type        string            `json:"webhook_type"`
		SessionID   string            `json:"session_id"`
		Environment string            `json:"environment"`
		Status      string            `json:"status"`
		AccountID   string            `json:"vendor_data"`
		Metadata    map[string]string `json:"metadata"`
		Created     int64             `json:"created_at"`
		Timestamp   int64             `json:"timestamp"`
	}
	if json.Unmarshal(body, &v) != nil || v.ID == "" || v.SessionID == "" {
		return Webhook{}, ErrInvalidSignature
	}
	// Test events sent from a live console must never grant adult status. Sandbox
	// environment and the locally persisted sandbox session are both required.
	if v.Environment != "sandbox" || headers.Get("X-Didit-Test-Webhook") == "true" {
		return Webhook{}, ErrWrongEnvironment
	}
	timestamp, _ := strconv.ParseInt(headers.Get("X-Timestamp"), 10, 64)
	if v.Timestamp != timestamp {
		return Webhook{}, ErrInvalidSignature
	}
	return Webhook{ID: v.ID, Type: v.Type, ObjectID: v.SessionID, Status: v.Status, AccountID: v.AccountID, OperationID: v.Metadata["operation_id"], CreatedAt: time.Unix(v.Created, 0).UTC()}, nil
}
