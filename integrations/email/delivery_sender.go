package email

import "context"

// ResolveDeliverySender resolves the exact configured identity before a caller
// reserves its durable send receipt. Log-only is never a delivery transport, even
// on a local installation: accepting it would both claim a send and log secrets
// such as call invitation links. Explicit capture remains available and identifiable
// through CapturesMessages. The resolved sender stays fixed for this one attempt.
func ResolveDeliverySender(ctx context.Context, sender Sender, as SendAs) (Sender, error) {
	if sender == nil {
		return nil, permanentSendRefusal("email delivery is not configured")
	}
	if lazy, ok := sender.(*LazySender); ok {
		resolved, err := lazy.senderFor(ctx, as)
		if err != nil {
			return nil, err
		}
		sender = resolved
	}
	if _, logOnly := sender.(*LogSender); sender == nil || logOnly {
		return nil, permanentSendRefusal("email delivery is not configured")
	}
	return sender, nil
}
