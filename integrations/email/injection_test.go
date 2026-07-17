package email

import (
	"context"
	"strings"
	"testing"
)

// injection_test.go covers the RFC 5322 header-injection barrier
// (CodeQL go/email-injection, alert #414). To and Subject are written
// verbatim into email headers, so a CR/LF/control char in either must
// be rejected before a Send -- otherwise a caller who controls the
// recipient or subject could inject extra headers or recipients.

func TestMessageValidate_RejectsHeaderInjection(t *testing.T) {
	base := Message{
		To:       "guest@example.com",
		Subject:  "Welcome",
		TextBody: "hello",
	}
	cases := map[string]Message{
		"CRLF in To": func() Message {
			m := base
			m.To = "guest@example.com\r\nBcc: victim@example.com"
			return m
		}(),
		"LF in To": func() Message {
			m := base
			m.To = "guest@example.com\nBcc: victim@example.com"
			return m
		}(),
		"CR in Subject": func() Message {
			m := base
			m.Subject = "Welcome\rX-Injected: 1"
			return m
		}(),
		"LF in Subject": func() Message {
			m := base
			m.Subject = "Welcome\nX-Injected: 1"
			return m
		}(),
		"NUL in To": func() Message {
			m := base
			m.To = "guest@example.com\x00"
			return m
		}(),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate accepted a header-injection payload: %q / %q", m.To, m.Subject)
			}
		})
	}
}

func TestMessageValidate_AllowsNewlinesInBodies(t *testing.T) {
	// Bodies live after the header/body boundary and legitimately span
	// multiple lines -- the barrier must not touch them.
	m := Message{
		To:       "guest@example.com",
		Subject:  "Welcome",
		TextBody: "line one\r\nline two\nline three",
		HTMLBody: "<p>one</p>\n<p>two</p>",
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected legitimate multi-line bodies: %v", err)
	}
}

// stubHeaderSink lets us drive SMTPSender.Send far enough to assemble the
// header block without a live relay: Send validates and builds the body,
// then aborts at ctx cancellation before dialing. A cancelled context
// returns the ctx error; a header-injection payload returns earlier, at
// validation -- so we distinguish the two by error identity.
func TestSMTPSend_RejectsInjectionBeforeDial(t *testing.T) {
	s := NewSMTPSender(SMTPConfig{Host: "localhost", Port: "25", FromAddr: "no-reply@example.com"}, nil)
	m := Message{
		To:       "guest@example.com\r\nBcc: victim@example.com",
		Subject:  "Welcome",
		TextBody: "hello",
	}
	err := s.Send(context.Background(), m)
	if err == nil {
		t.Fatal("SMTPSender.Send delivered a header-injection message")
	}
	if !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("expected a header-injection rejection, got: %v", err)
	}
}
