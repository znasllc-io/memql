package identity

import (
	"context"
	"time"
)

// signin_notice.go -- the new-sign-in notification seam (memql#4305).
//
// # Why this exists
//
// It is the cheapest detection control in the magic-link hardening design,
// and the only one that reaches the account holder AFTER somebody else has
// already won. Device binding stops a colleague from riding your link; it
// does nothing about a colleague who requests their own link to the shared
// address they can also read. Nothing stops that, and the design says so.
// What visibility does is make it a fact somebody notices: a message lands in
// the mailbox saying a session was created, when, from where, and with what.
//
// On a shared mailbox that lands in front of everyone. That is the point.
//
// # No action link, deliberately
//
// The obvious next line -- "wasn't you? click here to revoke" -- is refused
// in the design (section 7.1). An unauthenticated revoke link mailed to a
// shared mailbox is a denial-of-service handle: anyone who can read the
// mailbox can sign everybody out, repeatedly, and the message is delivered
// to them by us. The copy tells the reader to sign in and revoke from their
// profile page instead, which costs one step and cannot be weaponised.

// SignInNotice describes one newly created session, for the message.
type SignInNotice struct {
	// SessionId is the v1:identity:authSession row. Carried for the audit
	// row, never rendered into the email.
	SessionId string
	// UserId / Email name the account. Email is the delivery address.
	UserId string
	Email  string
	// Source is the authSession source ("bff_exchange", "oidc_cookie",
	// "device_code") -- what KIND of sign-in this was.
	Source string
	// ClientLabel is the best-effort User-Agent captured at issuance.
	ClientLabel string
	// SourceIP is where the sign-in came from.
	SourceIP string
	// At is the issuance instant.
	At time.Time
	// BrandName is the cluster's own name, so the message says which system
	// somebody signed in to.
	BrandName string
}

// SignInNotifier delivers SignInNotice. Implemented by the identity
// service's email sender; nil anywhere it is not wired, and every caller
// treats a nil notifier and a delivery failure identically -- LOGGED, NEVER
// FATAL. A mail outage must not stop people signing in.
type SignInNotifier interface {
	SendNewSignIn(ctx context.Context, in SignInNotice) error
}
