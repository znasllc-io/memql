package email

import (
	"context"
	"testing"
)

func TestResolveDeliverySenderRefusesLoggingAndHonorsOrganization(t *testing.T) {
	ctx := context.Background()
	for _, sender := range []Sender{nil, NewLogSender(nil), NewLazySender(NewLogSender(nil), nil, nil, nil)} {
		if resolved, err := ResolveDeliverySender(ctx, sender, SendAs{}); err == nil || resolved != nil {
			t.Fatal("a non-delivery transport was accepted")
		}
	}
	env, organization := &stubSender{}, &stubSender{}
	lazy := NewLazySender(env, nil, nil, nil)
	lazy.organization = func(_ context.Context, account string) (Sender, error) {
		if account != "self" {
			t.Fatalf("unexpected default organization %q", account)
		}
		return organization, nil
	}
	if resolved, err := ResolveDeliverySender(ctx, lazy, SendAs{}); err != nil || resolved != organization {
		t.Fatalf("default organization bypassed: %v", err)
	}
	if env.count.Load() != 0 || organization.count.Load() != 0 {
		t.Fatal("resolving a sender sent a message")
	}
	capture := &CaptureSender{}
	lazy.envResolved = capture
	if resolved, err := ResolveDeliverySender(ctx, lazy, SendAs{}); err != nil || resolved != capture || !CapturesMessages(resolved) {
		t.Fatalf("explicit capture did not take priority: %v", err)
	}
}
