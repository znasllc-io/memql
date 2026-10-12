package campaigns

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// signup_gates_db_test.go -- what a public signup holds while it works
// (memql#6022, F9 fix round 2), against a REAL engine and a REAL Postgres.
//
// A signup serialises its writes with two advisory locks, and each lock is a
// transaction that holds a connection of the pod's pool (4 connections in the
// deployed configuration) until the signup is done. What the signup does while it
// holds them therefore decides what the rest of the pod can do: a slow read under
// the locks stalls every other signup of the storefront, and two signups that each
// hold a connection and wait for another are a deadlock the pool cannot get out of.

// formFor is the form this storefront's visitor posts for an address.
func (n *digestNewsletter) formFor(address string) map[string]any {
	form := make(map[string]any, len(n.args))
	for k, v := range n.args {
		form[k] = v
	}
	form["email"], form["submissionId"] = address, "submission-"+address
	return form
}

// signedUp reports whether the storefront holds a signup for the address.
func (n *digestNewsletter) signedUp(t *testing.T, address string) bool {
	t.Helper()
	row, err := n.worker.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, NormalizeEmail(address)))
	if err != nil {
		t.Fatal(err)
	}
	return row != nil
}

// slowMailbox holds one signup's lookup by digest until the test lets it go, the
// way a slow database holds it.
type slowMailbox struct {
	Engine
	digest  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *slowMailbox) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query recipientInAudienceByDigest") && strings.Contains(q, e.digest) {
		e.once.Do(func() { close(e.entered) })
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.Engine.Execute(ctx, q)
}

// TestASlowMailboxReadDoesNotBlockASecondSignupOnTheSameStorefront: the mailbox
// reads -- by digest, the page of rows without one, and the roster fallback -- cost
// seconds in a large cluster, and they decide nothing a lock has to protect. Run
// under the storefront's locks they hold a pool connection for all of it and make
// every other signup of the storefront wait: a second signup failed after 10 s with
// a raw "i/o timeout" while the first was still reading.
func TestASlowMailboxReadDoesNotBlockASecondSignupOnTheSameStorefront(t *testing.T) {
	n := newEmptyNewsletter(t, "gate-slow")
	actual := n.worker.store.engine
	slow := &slowMailbox{Engine: actual, digest: EmailDigest("slow@example.test"), entered: make(chan struct{}), release: make(chan struct{})}
	n.worker.store.engine = slow
	defer func() { n.worker.store.engine = actual }()

	slowDone := make(chan error, 1)
	go func() {
		_, err := n.worker.handleSubscribe(n.owner, n.formFor("slow@example.test"), 0)
		slowDone <- err
	}()
	select {
	case <-slow.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first signup never reached its mailbox read")
	}

	// The first signup is inside its mailbox read. A second one on the same
	// storefront must not wait for it.
	ctx, cancel := context.WithTimeout(n.owner, 5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := n.worker.handleSubscribe(ctx, n.formFor("fast@example.test"), 0)
	if err != nil {
		t.Errorf("a second signup on the same storefront failed after %s while the first was reading its mailbox: %v", time.Since(started).Round(time.Millisecond), err)
	}

	close(slow.release)
	select {
	case err := <-slowDone:
		if err != nil {
			t.Errorf("the first signup, once its read finished: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first signup did not finish after its read was released")
	}
	for _, address := range []string{"slow@example.test", "fast@example.test"} {
		if !n.signedUp(t, address) {
			t.Errorf("no signup was recorded for %s", address)
		}
	}
}

// hugeAudience answers the reads a signup makes about a mailbox as a very large
// audience would: a full page of rows without a digest, none of them the
// mailbox, and a roster far past what a signup will walk.
type hugeAudience struct {
	Engine
	size  int
	pages *int
}

func (e hugeAudience) Execute(ctx context.Context, q string) (any, error) {
	switch {
	case strings.HasPrefix(q, "query recipientsWithoutDigestInAudience"):
		rows := make([]any, 0, digestLessWindow)
		for i := 0; i < digestLessWindow; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("v1:campaigns:recipient:stranger-%02d", i), "email": fmt.Sprintf("stranger%02d@example.test", i), "subscriptionStatus": "subscribed"})
		}
		return map[string]any{"output": rows}, nil
	case strings.HasPrefix(q, "query audienceRosterSize"):
		return map[string]any{"output": []any{map[string]any{"count": e.size}}}, nil
	case strings.HasPrefix(q, "query audienceRosterForSend"):
		*e.pages++
		return map[string]any{
			"output": []any{map[string]any{"id": "v1:campaigns:recipient:someone", "email": "someone@example.test", "subscriptionStatus": "subscribed"}},
			"cursor": "more",
		}, nil
	}
	return e.Engine.Execute(ctx, q)
}

// TestAnAudienceOverTheScanLimitIsAnsweredAtOnce: a roster walk that can only end
// in "cannot tell" is seconds a page spent on nothing. The audience's size is one
// count, and an audience bigger than the walk will read is answered from that.
func TestAnAudienceOverTheScanLimitIsAnsweredAtOnce(t *testing.T) {
	n := newEmptyNewsletter(t, "gate-huge")
	limit := mailboxRosterScanPages
	mailboxRosterScanPages = 3
	actual := n.worker.store.engine
	pages := 0
	n.worker.store.engine = hugeAudience{Engine: actual, size: 10 * mailboxRosterScanPages * rosterPageRows, pages: &pages}
	n.args["email"] = consentMailbox
	started := time.Now()
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)
	elapsed := time.Since(started)
	n.worker.store.engine = actual
	mailboxRosterScanPages = limit

	if err == nil || !strings.Contains(err.Error(), "try again later") {
		t.Fatalf("the form answered %v; want it to say it cannot take the subscription for now", err)
	}
	if pages != 0 {
		t.Errorf("the roster was walked for %d pages although the audience is past what the walk reads", pages)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the answer took %s", elapsed)
	}
	n.assertTheFormDidNothing(t, 0)
}
