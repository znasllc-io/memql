package campaigns

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
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

func TestSendingReadinessReportsCaptureWithoutSending(t *testing.T) {
	for _, test := range []struct {
		name    string
		sender  email.Sender
		capture bool
	}{
		{"capture", &email.CaptureSender{}, true},
		{"external", &organizationReadySender{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newTestWorker(t, organizationSendEngine(), test.sender)
			rows, err := w.handleSendingReadiness(importCtx(), map[string]any{"accountId": "client-a", "senderIdentityId": "sender"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var reply struct{ Ready, Capture bool }
			if err := json.Unmarshal(rows[0].Payload, &reply); err != nil {
				t.Fatal(err)
			}
			if !reply.Ready || reply.Capture != test.capture {
				t.Fatalf("incorrect transport readiness: %+v", reply)
			}
		})
	}
}

// Readiness names the Azure refusal of a sender that a person fixes on the
// sender or its domain, from the same check a real send makes, so a page can
// flag the sender for anyone who may send.
func TestSendingReadinessNamesTheSenderRefusal(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	for _, test := range []struct {
		name    string
		senders map[string]string // what Azure has registered for client-a
		refusal string
	}{
		{"registered", map[string]string{"hello@client-a.test": "Client A"}, ""},
		{"case", map[string]string{"hello@client-a.test": "Client A"}, ""},
		{"unregistered", map[string]string{"news@client-a.test": "Client A"}, "unregistered"},
		{"renamed", map[string]string{"hello@client-a.test": "Client A News"}, "renamed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := ""
			for address := range test.senders {
				primary = address
			}
			sender, err := email.NewACSSender(email.ACSConfig{AccountID: "client-a", Endpoint: "https://client-a.communication.azure.com", AccessKey: key, Senders: test.senders, Default: primary})
			if err != nil {
				t.Fatal(err)
			}
			e := organizationSendEngine()
			if test.name == "case" {
				e.senderIdentities["sender"]["address"] = "Hello@Client-A.test"
			}
			w := newTestWorker(t, e, sender)
			rows, err := w.handleSendingReadiness(importCtx(), map[string]any{"accountId": "client-a", "senderIdentityId": "sender"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Ready         bool   `json:"ready"`
				Reason        string `json:"reason"`
				SenderRefusal string `json:"senderRefusal"`
			}
			if err := json.Unmarshal(rows[0].Payload, &reply); err != nil {
				t.Fatal(err)
			}
			if reply.SenderRefusal != test.refusal || reply.Ready != (test.refusal == "") || (!reply.Ready && reply.Reason == "") {
				t.Fatalf("%+v", reply)
			}
		})
	}
}
