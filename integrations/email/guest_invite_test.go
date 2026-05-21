package email

import (
	"context"
	"strings"
	"testing"
	"time"
)

// captureSender drops every Send into a slice so tests can assert on
// the rendered body.
type captureSender struct {
	sent []Message
}

func (c *captureSender) Send(_ context.Context, m Message) error {
	c.sent = append(c.sent, m)
	return nil
}

func TestSendGuestInvite_InitialSendOmitsResendNotice(t *testing.T) {
	cap := &captureSender{}
	err := SendGuestInvite(context.Background(), cap, GuestInviteParams{
		To:          "guest@example.com",
		GuestName:   "Alex",
		InviterName: "Jose",
		SpaceName:   "Engineering",
		JoinURL:     "https://example.com/join/abc",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		// Resend left false -- the first-send shape.
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(cap.sent) != 1 {
		t.Fatalf("expected 1 send, got %d", len(cap.sent))
	}
	m := cap.sent[0]
	if strings.HasPrefix(m.Subject, "Updated link:") {
		t.Errorf("first-send subject should not carry 'Updated link:' prefix; got %q", m.Subject)
	}
	if strings.Contains(m.TextBody, "replaces any prior invitation") {
		t.Errorf("first-send body should not carry the resend banner; got: %s", m.TextBody)
	}
}

func TestSendGuestInvite_ResendCarriesReplacementCopy(t *testing.T) {
	cap := &captureSender{}
	err := SendGuestInvite(context.Background(), cap, GuestInviteParams{
		To:          "guest@example.com",
		GuestName:   "Alex",
		InviterName: "Jose",
		SpaceName:   "Engineering",
		JoinURL:     "https://example.com/join/xyz",
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		Resend:      true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	m := cap.sent[0]
	if !strings.HasPrefix(m.Subject, "Updated link:") {
		t.Errorf("resend subject should carry 'Updated link:' prefix; got %q", m.Subject)
	}
	if !strings.Contains(m.TextBody, "replaces any prior invitation") {
		t.Errorf("resend text body missing the 'this link replaces any prior' banner; got: %s", m.TextBody)
	}
	if !strings.Contains(m.HTMLBody, "Updated link inside.") {
		t.Errorf("resend HTML body missing the highlight box; got: %s", m.HTMLBody)
	}
	// The new URL is present.
	if !strings.Contains(m.TextBody, "https://example.com/join/xyz") {
		t.Errorf("resend body missing the new URL; got: %s", m.TextBody)
	}
}
