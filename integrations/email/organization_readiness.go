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
