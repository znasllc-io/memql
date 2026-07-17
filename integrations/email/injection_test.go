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

// TestSMTPSend_ValidateRejectsInjection covers the first barrier layer:
// SMTPSender.Send calls Message.Validate() first, so an unsafe To short-
// circuits before any header is assembled and never dials.
func TestSMTPSend_ValidateRejectsInjection(t *testing.T) {
	s := NewSMTPSender(SMTPConfig{Host: "localhost", Port: "25", FromAddr: "no-reply@example.com"}, nil)
	err := s.Send(context.Background(), Message{
		To:       "guest@example.com\r\nBcc: victim@example.com",
		Subject:  "Welcome",
		TextBody: "hello",
	})
	if err == nil {
		t.Fatal("SMTPSender.Send delivered a header-injection message")
	}
	if !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("expected a header-injection rejection, got: %v", err)
	}
}

// TestSMTPSend_ClosureGuardsConfigFrom covers the SECOND barrier layer, the
// writeHeaders closure. The From header is built from SMTPConfig (FromName +
// FromAddr), which Message.Validate() does not inspect -- so a CR/LF planted
// in FromName reaches header assembly with a clean To/Subject. The closure
// must reject it before smtp.SendMail. This is the only test that exercises
// the closure (the Validate cases short-circuit before it runs).
func TestSMTPSend_ClosureGuardsConfigFrom(t *testing.T) {
	s := NewSMTPSender(SMTPConfig{
		Host:     "localhost",
		Port:     "25",
		FromAddr: "no-reply@example.com",
		FromName: "Acme\r\nBcc: victim@example.com",
	}, nil)
	err := s.Send(context.Background(), Message{
		To:       "guest@example.com",
		Subject:  "Welcome",
		TextBody: "hello",
	})
	if err == nil {
		t.Fatal("SMTPSender.Send delivered a message with an injected From header")
	}
	if !strings.Contains(err.Error(), "header injection") {
		t.Fatalf("expected a header-injection rejection from the closure, got: %v", err)
	}
}
