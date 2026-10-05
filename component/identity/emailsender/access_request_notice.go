package emailsender

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/integrations/email"
)

func (s *EngineEmailSender) SendAccessRequestNotice(ctx context.Context, recipient string) error {
	sender := s.resolveSender()
	if sender == nil {
		return errors.New("access request notification: email is unavailable")
	}
	return sender.Send(ctx, email.Message{To: recipient, Subject: "Cluster access requests need review",
		TextBody: fmt.Sprintf("New access requests are waiting in %s. Open MemQL OS, then Users → Access requests to approve or reject them.\n", s.Cfg.BrandName),
	}, email.SendAs{})
}
