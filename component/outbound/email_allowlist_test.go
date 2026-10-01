package outbound

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestEmailAllowlistConfigLoadsExactAddressesAndDomains(t *testing.T) {
	t.Setenv("MEMQL_OUTBOUND_EMAIL_ALLOWLIST", " Desk@Example.COM , , ops.example.org, ")
	got := LoadConfig().EmailAllowlist
	want := []string{"desk@example.com", "ops.example.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("email allowlist = %v, want %v", got, want)
	}
	for target, wantAllowed := range map[string]bool{
		"desk@example.com":           true,
		"other@example.com":          false,
		"desk+tag@example.com":       false,
		"desk@sub.example.com":       false,
		"any@ops.example.org":        true,
		"any@sub.ops.example.org":    true,
		"any@notops.example.org":     false,
		"any@ops.example.org.evil":   false,
		"one@two@ops.example.org":    false,
		"Name <any@ops.example.org>": false,
	} {
		if allowed := emailAllowed(target, got); allowed != wantAllowed {
			t.Errorf("loaded policy admits %q = %v, want %v", target, allowed, wantAllowed)
		}
	}
	t.Setenv("MEMQL_OUTBOUND_EMAIL_ALLOWLIST", " , ")
	if got := LoadConfig().EmailAllowlist; len(got) != 0 {
		t.Fatalf("empty configured allowlist must disable email, got %v", got)
	}
}

func TestExactMailboxConfigPreservesNonASCIISpelling(t *testing.T) {
	t.Setenv("MEMQL_OUTBOUND_EMAIL_ALLOWLIST", " DEſK@Example.COM, DESK@Example.COM ")
	got := LoadConfig().EmailAllowlist
	want := []string{"deſk@example.com", "desK@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("exact mailbox bytes changed: got %v, want %v", got, want)
	}
	if emailAllowed("desk@example.com", got) {
		t.Fatal("Unicode configuration must not authorize the ASCII mailbox")
	}
	for _, target := range want {
		if !emailAllowed(target, got) {
			t.Fatalf("explicitly configured UTF-8 mailbox %q must still match literally", target)
		}
	}
}

func TestDrainExactMailboxPolicyPrecedesEmailTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   string
		target  string
		allowed bool
	}{
		{name: "exact", target: "desk@example.com", allowed: true},
		{name: "case insensitive", target: "DESK@EXAMPLE.COM", allowed: true},
		{name: "Unicode long-s is distinct", target: "deſk@example.com"},
		{name: "Unicode Kelvin sign is distinct", target: "desK@example.com"},
		{name: "Unicode configured address stays distinct", entry: "desK@example.com", target: "desk@example.com"},
		{name: "other mailbox", target: "other@example.com"},
		{name: "plus alias", target: "desk+tag@example.com"},
		{name: "subdomain", target: "desk@sub.example.com"},
		{name: "domain suffix attack", target: "desk@example.com.evil"},
		{name: "address list", target: "desk@example.com,other@example.com"},
		{name: "display name", target: "Desk <desk@example.com>"},
		{name: "comment", target: "desk@example.com (Desk)"},
		{name: "leading whitespace", target: " desk@example.com"},
		{name: "header injection", target: "desk@example.com\r\nBcc:other@example.com"},
		{name: "multiple at signs", target: "desk@@example.com"},
		{name: "missing local part", target: "@example.com"},
		{name: "missing domain", target: "desk@"},
		{name: "malformed entry", entry: "desk@@example.com", target: "desk@example.com"},
		{name: "display-name entry", entry: "Desk <desk@example.com>", target: "desk@example.com"},
		{name: "list entry", entry: "desk@example.com,other@example.com", target: "desk@example.com"},
		{name: "wildcard is not expansion", entry: "*@example.com", target: "desk@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry
			if entry == "" {
				entry = "desk@example.com"
			}
			row := pendingRow("v1:platform:outboundRequest:exact-mailbox")
			row["medium"], row["target"] = "email", tc.target
			eng := &fakeEngine{pending: []map[string]any{row}}
			transport := &fakeTransport{}
			worker := newTestWorker(eng, transport, nil)
			worker.cfg.EmailAllowlist = []string{entry}

			worker.drainOnce(context.Background())

			stamps := eng.stamped()
			if tc.allowed {
				if transport.count() != 1 || len(stamps) != 2 || !strings.Contains(stamps[1], `status: "sent"`) {
					t.Fatalf("admitted mailbox must deliver once and stamp sent: deliveries=%d stamps=%v", transport.count(), stamps)
				}
				return
			}
			if transport.count() != 0 {
				t.Fatal("refused mailbox must never reach the email transport")
			}
			if len(stamps) != 1 || !strings.Contains(stamps[0], `status: "failed"`) || !strings.Contains(stamps[0], "allowlist") {
				t.Fatalf("refused mailbox must fail before sending: %v", stamps)
			}
		})
	}
}
