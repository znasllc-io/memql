// Package providers implements bounded, server-only payment and age-verification
// protocol operations. The caller owns admission, durable operation identities,
// reservations and retries; these adapters never retry an uncertain write.
package providers

import (
	"context"
	"errors"
	"time"
)

type Mode string

const (
	Disabled Mode = "disabled"
	Mock     Mode = "mock"
	Sandbox  Mode = "sandbox"
)

var (
	ErrDisabled         = errors.New("provider disabled")
	ErrMockOperation    = errors.New("local mock lifecycle is owned by the calls service")
	ErrOutcomeUnknown   = errors.New("provider outcome unknown; reconcile before retry")
	ErrInvalidSignature = errors.New("invalid provider webhook signature")
	ErrWrongEnvironment = errors.New("provider environment is not sandbox")
)

type CheckoutRequest struct {
	OperationID string
	AccountID   string
	AmountCents int64
	ReturnURL   string
	CancelURL   string
	SaveMethod  bool
	CustomerID  string
}
type Checkout struct {
	ID              string
	URL             string
	Status          string
	PaymentStatus   string
	AccountID       string
	OperationID     string
	AmountCents     int64
	Currency        string
	PaymentID       string
	CustomerID      string
	PaymentMethodID string
	Paid            bool
	Refunded        bool
	Disputed        bool
}
type ChargeRequest struct {
	OperationID     string
	AccountID       string
	AmountCents     int64
	CustomerID      string
	PaymentMethodID string
}
type Payment struct {
	ID              string
	Status          string
	AccountID       string
	OperationID     string
	AmountCents     int64
	Currency        string
	CustomerID      string
	PaymentMethodID string
	Paid            bool
	Refunded        bool
	Disputed        bool
	RequiresAction  bool
}
type PaymentProvider interface {
	CreateCheckout(context.Context, CheckoutRequest) (Checkout, error)
	ReadCheckout(context.Context, string) (Checkout, error)
	ChargeOffSession(context.Context, ChargeRequest) (Payment, error)
	ReadPayment(context.Context, string) (Payment, error)
}
type VerificationRequest struct {
	OperationID string
	AccountID   string
	CallbackURL string
	// Scenario is a Didit sandbox scenario, not a production result override.
	Scenario string
}
type Verification struct {
	ID              string
	URL             string
	Status          string
	AccountID       string
	OperationID     string
	WorkflowID      string
	WorkflowVersion int
	Adult18         bool
	Underage        bool
	Method          string
	VerifiedAt      time.Time
}
type VerificationProvider interface {
	CreateVerification(context.Context, VerificationRequest) (Verification, error)
	ReadVerification(context.Context, string) (Verification, error)
}

// Webhook is deliberately a reference, not a verification or credit decision.
// Consumers fetch the authoritative object and match its stored operation and
// account before applying it. No identity documents or card details escape here.
type Webhook struct {
	ID          string
	Type        string
	ObjectID    string
	Status      string
	AccountID   string
	OperationID string
	CreatedAt   time.Time
}
