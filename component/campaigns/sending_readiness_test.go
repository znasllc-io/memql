package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/integrations/email"
)

type organizationReadySender struct {
	recordingSender
}

func (s *organizationReadySender) CheckSender(_ context.Context, as email.SendAs) error {
	if as.AccountID != "client-a" || as.Address != "hello@client-a.test" {
		return fmt.Errorf("organization email connection is unavailable")
	}
	return nil
}

func TestSendingReadinessUsesSelectedOrganizationAndNeverSends(t *testing.T) {
	sender := &organizationReadySender{}
	w := newTestWorker(t, organizationSendEngine(), sender)
	for _, test := range []struct {
		account, identity string
		ready             bool
	}{
		{"client-a", "sender", true},
		{"self", "", false},           // operator unconfigured does not block client
		{"client-b", "sender", false}, // readable sender is not this org's
		{"client-a", "", false},       // client never uses operator default
	} {
		rows, err := w.handleSendingReadiness(importCtx(), map[string]any{"accountId": test.account, "senderIdentityId": test.identity}, 0)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rows[0].Payload, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Ready != test.ready || (!reply.Ready && reply.Reason == "") {
			t.Fatalf("%s/%s: %+v", test.account, test.identity, reply)
		}
	}
	w.cfg.UnsubscribeSecret = ""
	rows, err := w.handleSendingReadiness(importCtx(), map[string]any{"accountId": "client-a", "senderIdentityId": "sender"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	_ = json.Unmarshal(rows[0].Payload, &reply)
	if reply["ready"] != false {
		t.Fatal("missing unsubscribe setup accepted")
	}
	if sender.count() != 0 {
		t.Fatal("readiness sent email")
	}
	reader := newTestWorker(t, &readOnlyOrganizationEngine{fakeEngine: organizationSendEngine()}, sender)
	if _, err := reader.handleSendingReadiness(importCtx(), map[string]any{"accountId": "client-a", "senderIdentityId": "sender"}, 0); err == nil {
		t.Fatal("reader gained sending authority")
	}
}
