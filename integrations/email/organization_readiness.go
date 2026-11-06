package email

import (
	"context"
	"strings"
)

// CheckSender checks locally known delivery prerequisites without sending a
// message. It does not promise delivery or replace the send-time check.
func CheckSender(ctx context.Context, sender Sender, as SendAs) error {
	if sender == nil {
		return permanentSendRefusal("email delivery is unavailable")
	}
	if checker, ok := sender.(interface {
		CheckSender(context.Context, SendAs) error
	}); ok {
		return checker.CheckSender(ctx, as)
	}
	return nil
}

// Contacts is implemented by a Sender that can say whom a person should write to
// about the email an organization sends: the address its own email connection
// names (Azure's: the connection's reply address, else its default sender).
type Contacts interface {
	Contact(ctx context.Context, accountID string) (string, error)
}

// Contact is the address a person can write to about an organization's email,
// from the organization's own email connection (Contacts), resolved as
// CheckSender resolves the organization's sender. "" when the organization has no
// connection of its own that names one, or the transport cannot say. It is never
// the installation's own sender: that mailbox speaks for no organization.
func Contact(ctx context.Context, sender Sender, accountID string) (string, error) {
	if sender == nil {
		return "", nil
	}
	if contacts, ok := sender.(Contacts); ok {
		return contacts.Contact(ctx, accountID)
	}
	return "", nil
}

// Contact asks the organization's own connection, never the node's transport.
func (l *LazySender) Contact(ctx context.Context, accountID string) (string, error) {
	account := strings.TrimSpace(accountID)
	if l == nil || l.organization == nil || account == "" {
		return "", nil
	}
	sender, err := l.organization(ctx, account)
	if err != nil || sender == nil {
		return "", err
	}
	return Contact(ctx, sender, account)
}

func (l *LazySender) CheckSender(ctx context.Context, as SendAs) error {
	if l == nil {
		return permanentSendRefusal("email delivery is unavailable")
	}
	if _, capture := l.envResolved.(*CaptureSender); capture {
		return nil
	}
	if l.organization != nil {
		account := strings.TrimSpace(as.AccountID)
		if account == "" {
			account = "self"
		}
		sender, err := l.organization(ctx, account)
		if err != nil {
			return err
		}
		if sender != nil {
			return CheckSender(ctx, sender, as)
		}
		if account != "self" {
			return permanentSendRefusal("connect this organization's email domain before sending")
		}
	}
	sender := l.Resolve(ctx)
	if _, logOnly := sender.(*LogSender); logOnly {
		return permanentSendRefusal("email delivery is not configured")
	}
	return CheckSender(ctx, sender, as)
}
