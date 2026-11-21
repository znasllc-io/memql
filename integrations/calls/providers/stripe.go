package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const StripeAPIVersion = "2026-11-18.endive"
const stripeBase = "https://api.stripe.com/v1"

type StripeConfig struct {
	Mode          Mode
	SecretKey     string
	WebhookSecret string
	Client        *http.Client
}
type Stripe struct {
	providerHTTP
	secret        string
	webhookSecret string
}

func NewStripe(c StripeConfig) (*Stripe, error) {
	h, err := newHTTP(c.Mode, c.Client)
	if err != nil {
		return nil, err
	}
	if h.mode == Sandbox && !(strings.HasPrefix(c.SecretKey, "sk_test_") || strings.HasPrefix(c.SecretKey, "rk_test_")) {
		return nil, ErrWrongEnvironment
	}
	return &Stripe{providerHTTP: h, secret: c.SecretKey, webhookSecret: c.WebhookSecret}, nil
}
func (s *Stripe) headers(operation string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + s.secret}, "Stripe-Version": {StripeAPIVersion}, "Content-Type": {"application/x-www-form-urlencoded"}}
	if operation != "" {
		h.Set("Idempotency-Key", operation)
	}
	return h
}

type stripePayment struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Amount         int64             `json:"amount"`
	AmountReceived int64             `json:"amount_received"`
	Currency       string            `json:"currency"`
	Livemode       bool              `json:"livemode"`
	Customer       string            `json:"customer"`
	PaymentMethod  string            `json:"payment_method"`
	Metadata       map[string]string `json:"metadata"`
	LatestCharge   *struct {
		Refunded       bool  `json:"refunded"`
		Disputed       bool  `json:"disputed"`
		AmountRefunded int64 `json:"amount_refunded"`
	} `json:"latest_charge"`
}
type stripeCheckout struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Status        string            `json:"status"`
	PaymentStatus string            `json:"payment_status"`
	Amount        int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
	Livemode      bool              `json:"livemode"`
	Customer      string            `json:"customer"`
	Reference     string            `json:"client_reference_id"`
	Metadata      map[string]string `json:"metadata"`
	PaymentIntent *stripePayment    `json:"payment_intent"`
}

func checkoutResult(v stripeCheckout) (Checkout, error) {
	if v.Livemode {
		return Checkout{}, ErrWrongEnvironment
	}
	if !strings.HasPrefix(v.ID, "cs_test_") {
		return Checkout{}, errors.New("invalid sandbox checkout")
	}
	result := Checkout{ID: v.ID, URL: v.URL, Status: v.Status, PaymentStatus: v.PaymentStatus, AmountCents: v.Amount, Currency: v.Currency, AccountID: v.Reference, OperationID: v.Metadata["operation_id"], CustomerID: v.Customer, Paid: v.PaymentStatus == "paid"}
	if v.PaymentIntent != nil {
		payment, err := paymentResult(*v.PaymentIntent)
		if err != nil {
			return Checkout{}, err
		}
		result.PaymentID = payment.ID
		result.PaymentMethodID = payment.PaymentMethodID
		result.Refunded = payment.Refunded
		result.Disputed = payment.Disputed
	}
	return result, nil
}
func paymentResult(v stripePayment) (Payment, error) {
	if v.Livemode {
		return Payment{}, ErrWrongEnvironment
	}
	if !strings.HasPrefix(v.ID, "pi_") {
		return Payment{}, errors.New("invalid payment result")
	}
	return Payment{ID: v.ID, Status: v.Status, AccountID: v.Metadata["account_id"], OperationID: v.Metadata["operation_id"], AmountCents: v.Amount, Currency: v.Currency, CustomerID: v.Customer, PaymentMethodID: v.PaymentMethod, Paid: v.Status == "succeeded" && v.AmountReceived == v.Amount, RequiresAction: v.Status == "requires_action", Refunded: v.LatestCharge != nil && (v.LatestCharge.Refunded || v.LatestCharge.AmountRefunded > 0), Disputed: v.LatestCharge != nil && v.LatestCharge.Disputed}, nil
}
func (s *Stripe) CreateCheckout(ctx context.Context, r CheckoutRequest) (Checkout, error) {
	if err := s.enabled(); err != nil {
		return Checkout{}, err
	}
	if !validIdentifier(r.OperationID) || !validIdentifier(r.AccountID) || r.AmountCents < 1000 || r.AmountCents > 50000 || !validReturnURL(r.ReturnURL) || !validReturnURL(r.CancelURL) {
		return Checkout{}, errors.New("invalid checkout request")
	}
	if r.CustomerID != "" && !strings.HasPrefix(r.CustomerID, "cus_") {
		return Checkout{}, errors.New("invalid customer")
	}
	form := url.Values{"mode": {"payment"}, "success_url": {r.ReturnURL}, "cancel_url": {r.CancelURL}, "client_reference_id": {r.AccountID}, "metadata[operation_id]": {r.OperationID}, "metadata[account_id]": {r.AccountID}, "line_items[0][quantity]": {"1"}, "line_items[0][price_data][currency]": {"usd"}, "line_items[0][price_data][unit_amount]": {strconv.FormatInt(r.AmountCents, 10)}, "line_items[0][price_data][product_data][name]": {"Callkey hosting credit"}, "payment_method_types[0]": {"card"}, "payment_intent_data[metadata][operation_id]": {r.OperationID}, "payment_intent_data[metadata][account_id]": {r.AccountID}, "expand[0]": {"payment_intent.latest_charge"}}
	if r.CustomerID != "" {
		form.Set("customer", r.CustomerID)
	} else if r.SaveMethod {
		form.Set("customer_creation", "always")
	}
	// The service has already recorded an explicit, capped off-session mandate.
	// Saving is opt-in; payment alone never creates a standing charging authority.
	if r.SaveMethod {
		form.Set("payment_intent_data[setup_future_usage]", "off_session")
	}
	var v stripeCheckout
	_, err := s.request(ctx, http.MethodPost, stripeBase+"/checkout/sessions", strings.NewReader(form.Encode()), s.headers(r.OperationID), &v)
	if err != nil {
		return Checkout{}, err
	}
	return checkoutResult(v)
}
func (s *Stripe) ReadCheckout(ctx context.Context, id string) (Checkout, error) {
	if !validIdentifier(id) || !strings.HasPrefix(id, "cs_test_") {
		return Checkout{}, errors.New("invalid sandbox checkout id")
	}
	var v stripeCheckout
	_, err := s.request(ctx, http.MethodGet, stripeBase+"/checkout/sessions/"+url.PathEscape(id)+"?expand[]=payment_intent.latest_charge", nil, s.headers(""), &v)
	if err != nil {
		return Checkout{}, err
	}
	return checkoutResult(v)
}
func (s *Stripe) ChargeOffSession(ctx context.Context, r ChargeRequest) (Payment, error) {
	if err := s.enabled(); err != nil {
		return Payment{}, err
	}
	if !validIdentifier(r.OperationID) || !validIdentifier(r.AccountID) || r.AmountCents < 1000 || r.AmountCents > 50000 || !validIdentifier(r.CustomerID) || !strings.HasPrefix(r.CustomerID, "cus_") || !validIdentifier(r.PaymentMethodID) || !strings.HasPrefix(r.PaymentMethodID, "pm_") {
		return Payment{}, errors.New("invalid automatic top-up")
	}
	form := url.Values{"amount": {strconv.FormatInt(r.AmountCents, 10)}, "currency": {"usd"}, "customer": {r.CustomerID}, "payment_method": {r.PaymentMethodID}, "off_session": {"true"}, "confirm": {"true"}, "payment_method_types[0]": {"card"}, "metadata[account_id]": {r.AccountID}, "metadata[operation_id]": {r.OperationID}, "expand[0]": {"latest_charge"}}
	var v stripePayment
	_, err := s.request(ctx, http.MethodPost, stripeBase+"/payment_intents", strings.NewReader(form.Encode()), s.headers(r.OperationID), &v)
	if err != nil {
		// An off-session authentication challenge is commonly HTTP 402. Keep
		// the existing intent so the service can pause for the owner rather
		// than manufacture a second charge with a new operation identity.
		var rejection *rejectedResponse
		if errors.As(err, &rejection) && rejection.status == http.StatusPaymentRequired {
			var envelope struct {
				Error struct {
					PaymentIntent *stripePayment `json:"payment_intent"`
				} `json:"error"`
			}
			if json.Unmarshal(rejection.body, &envelope) == nil && envelope.Error.PaymentIntent != nil {
				result, parseErr := paymentResult(*envelope.Error.PaymentIntent)
				if parseErr == nil && !result.Paid {
					return result, nil
				}
			}
		}
		return Payment{}, err
	}
	return paymentResult(v)
}
func (s *Stripe) ReadPayment(ctx context.Context, id string) (Payment, error) {
	if !validIdentifier(id) || !strings.HasPrefix(id, "pi_") {
		return Payment{}, errors.New("invalid payment id")
	}
	var v stripePayment
	_, err := s.request(ctx, http.MethodGet, stripeBase+"/payment_intents/"+url.PathEscape(id)+"?expand[]=latest_charge", nil, s.headers(""), &v)
	if err != nil {
		return Payment{}, err
	}
	return paymentResult(v)
}
func (s *Stripe) VerifyWebhook(body []byte, header string, now time.Time) (Webhook, error) {
	return VerifyStripeWebhook(body, header, s.webhookSecret, now)
}
