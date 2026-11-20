package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"time"
)

// sendSMTP owns the connection so cancellation interrupts every protocol phase,
// including the greeting, TLS handshake and DATA acknowledgment. Checking ctx
// before smtp.SendMail did not bound that blocking call and could leave an
// invitation's durable receipt pending long after its send deadline.
func (s *SMTPSender) sendSMTP(ctx context.Context, from, to string, body []byte) error {
	failure := func(phase string, notAccepted bool, err error) error {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			// A socket deadline can wake just before the context's timer runs.
			err = context.DeadlineExceeded
		}
		return &SendError{NotAccepted: notAccepted, Cause: err, Detail: fmt.Sprintf("smtp %s on %s: %v", phase, s.cfg.Host, err)}
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(s.cfg.Host, s.cfg.Port))
	if err != nil {
		return failure("connect", true, err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err = connection.SetDeadline(deadline); err != nil {
			return failure("deadline", true, err)
		}
	}
	client, err := smtp.NewClient(connection, s.cfg.Host)
	if err != nil {
		return failure("greeting", true, err)
	}
	defer client.Close()
	if err = client.Hello("localhost"); err != nil {
		return failure("hello", true, err)
	}
	if supported, _ := client.Extension("STARTTLS"); supported {
		// Preserve certificate/hostname validation; never downgrade a failed TLS
		// upgrade into a plaintext authenticated send.
		if err = client.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return failure("STARTTLS", true, err)
		}
	}
	if s.cfg.Username != "" {
		if supported, _ := client.Extension("AUTH"); !supported {
			return failure("AUTH", true, fmt.Errorf("server does not support AUTH"))
		}
		if err = client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return failure("AUTH", true, err)
		}
	}
	if err = client.Mail(from); err != nil {
		return failure("MAIL", true, err)
	}
	if err = client.Rcpt(to); err != nil {
		return failure("RCPT", true, err)
	}
	data, err := client.Data()
	if err != nil {
		return failure("DATA", true, err)
	}
	// Only RenderRFC5322 output reaches this sink: message/config header values
	// and extra headers were validated, and MIME boundaries are random. Body
	// content is intentional mail, not a second set of unvalidated headers.
	// codeql[go/email-injection]
	if _, err = data.Write(body); err != nil {
		return failure("body", false, err)
	}
	// From the first body write onward a lost acknowledgment is ambiguous. Do
	// not mark it NotAccepted: a caller could otherwise deliver a retry twice.
	if err = data.Close(); err != nil {
		return failure("acknowledgment", false, err)
	}
	// DATA's final 250 is acceptance. QUIT is cleanup and cannot undo it, even
	// if the relay immediately closes or cancellation wins before its reply.
	// Limit cleanup independently so an accepted send never waits on a silent
	// QUIT peer beyond one second, or beyond an earlier caller deadline.
	cleanupDeadline := time.Now().Add(time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(cleanupDeadline) {
		cleanupDeadline = deadline
	}
	if connection.SetDeadline(cleanupDeadline) == nil {
		_ = client.Quit()
	}
	return nil
}
