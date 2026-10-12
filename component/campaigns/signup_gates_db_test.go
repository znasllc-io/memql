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

// barrierKey tags a signup's context so a fake engine can tell the signups apart.
type barrierKey struct{}

// meeting holds every party at a point until all of them have reached it, the way
// concurrent signups reach the same step together: what the test needs is that all
// of them have read the same state before any of them writes.
type meeting struct {
	parties int
	mu      sync.Mutex
	arrived map[any]bool
	release chan struct{}
}

func newMeeting(parties int) *meeting {
	return &meeting{parties: parties, arrived: map[any]bool{}, release: make(chan struct{})}
}

// wait returns once every party has reached it, or when ctx ends. A party that
// comes a second time passes at once.
func (m *meeting) wait(ctx context.Context) error {
	who := ctx.Value(barrierKey{})
	m.mu.Lock()
	first := !m.arrived[who]
	if first {
		m.arrived[who] = true
		if len(m.arrived) == m.parties {
			close(m.release)
		}
	}
	m.mu.Unlock()
	if !first {
		return nil
	}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("the other signups never arrived")
	}
}

// meetingEngine makes each signup wait, at the first query whose text starts with
// prefix, for all the others. after, if set, runs once the first party has been
// let through the point (not under any lock the signup holds).
type meetingEngine struct {
	Engine
	prefix  string
	meeting *meeting
}

func (e meetingEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, e.prefix) {
		if err := e.meeting.wait(ctx); err != nil {
			return nil, err
		}
	}
	return e.Engine.Execute(ctx, q)
}

// signupsTogether runs the signups at once and returns their errors in order.
func signupsTogether(worker *Worker, owner context.Context, forms []map[string]any) []error {
	errs := make([]error, len(forms))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, form := range forms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.WithValue(owner, barrierKey{}, i), 60*time.Second)
			defer cancel()
			<-start
			_, errs[i] = worker.handleSubscribe(ctx, form, 0)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// TestConcurrentSignupsAtTheCapAddOnlyUpToTheCap: the audience's ceiling is checked
// before the locks as a fast refusal, and N signups that run side by side all pass
// it on the same count. The check that holds is the one made again under the
// audience's lock, immediately before the add.
func TestConcurrentSignupsAtTheCapAddOnlyUpToTheCap(t *testing.T) {
	n := newDigestNewsletter(t, "gate-cap") // the audience holds one member
	n.worker.cfg.MaxAudience = 3
	const visitors = 5

	// Every signup reads the audience's size before any of them writes.
	actual := n.worker.store.engine
	n.worker.store.engine = meetingEngine{Engine: actual, prefix: "query audienceRosterSize", meeting: newMeeting(visitors)}
	defer func() { n.worker.store.engine = actual }()

	forms := make([]map[string]any, visitors)
	for i := range forms {
		forms[i] = n.formFor(fmt.Sprintf("visitor%d@example.test", i))
	}
	errs := signupsTogether(n.worker, n.owner, forms)

	taken, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			taken++
		case strings.Contains(err.Error(), "cannot accept more subscribers"):
			refused++
		default:
			t.Errorf("signup %d: %v", i, err)
		}
	}
	n.worker.store.engine = actual
	if roster := n.roster(t); len(roster) != 3 {
		t.Errorf("the audience holds %d members after %d signups at a cap of 3 that started with 1, want 3: %d taken, %d refused", len(roster), visitors, taken, refused)
	}
	if taken != 2 || refused != visitors-2 {
		t.Errorf("%d signups were taken and %d refused, want 2 and %d: only the room that was there", taken, refused, visitors-2)
	}
}

// TestTwoConcurrentSignupsForTheSameNewAddressMakeOneMember: both read "nobody has
// this address" before either writes. The audience gets one member and one signup,
// and the signup names that member -- the second signup finds the first's signup
// under the lock and is accepted without a second add.
func TestTwoConcurrentSignupsForTheSameNewAddressMakeOneMember(t *testing.T) {
	n := newEmptyNewsletter(t, "gate-same")
	actual := n.worker.store.engine
	n.worker.store.engine = meetingEngine{Engine: actual, prefix: "query recipientsWithoutDigestInAudience", meeting: newMeeting(2)}
	defer func() { n.worker.store.engine = actual }()

	first, second := n.formFor("same@example.test"), n.formFor("Same@Example.test")
	first["submissionId"], second["submissionId"] = "first", "second"
	errs := signupsTogether(n.worker, n.owner, []map[string]any{first, second})
	n.worker.store.engine = actual

	for i, err := range errs {
		if err != nil {
			t.Errorf("signup %d: %v", i, err)
		}
	}
	roster := n.roster(t)
	if len(roster) != 1 {
		t.Fatalf("the audience holds %d members for one address, want 1", len(roster))
	}
	row, err := n.worker.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, "same@example.test"))
	if err != nil || row == nil {
		t.Fatalf("no signup was recorded: %v %v", row, err)
	}
	if got := bare(str(row, "recipientId")); got != roster[0].ID {
		t.Errorf("the signup names recipient %q, want the member there is, %q", got, roster[0].ID)
	}
}

// addsMeanwhile adds a recipient for the mailbox through another path -- an
// operator, an import, another form's signup -- at the moment a signup finishes its
// last read before the locks: after it has decided the mailbox is new.
type addsMeanwhile struct {
	Engine
	add  func()
	once sync.Once
}

func (e *addsMeanwhile) Execute(ctx context.Context, q string) (any, error) {
	res, err := e.Engine.Execute(ctx, q)
	if strings.HasPrefix(q, "query recipientsWithoutDigestInAudience") {
		e.once.Do(e.add)
	}
	return res, err
}

// TestAMailboxAddedAfterTheReadIsAdoptedNotAddedAgain: "this mailbox is new" was
// decided before the locks. Under the audience's lock the keyed read by digest is
// made again, so a row that appeared in between is the member and the signup does
// not add a second row for the same person -- who would then be on every campaign
// twice.
func TestAMailboxAddedAfterTheReadIsAdoptedNotAddedAgain(t *testing.T) {
	n := newEmptyNewsletter(t, "gate-meanwhile")
	manual := n.suffix + "-manual"
	actual := n.worker.store.engine
	n.worker.store.engine = &addsMeanwhile{Engine: actual, add: func() {
		if err := n.store.AddRecipient(n.owner, manual, n.audience, " Subscriber@Example.TEST ", "", "manual", n.org, nil); err != nil {
			t.Error(err)
		}
	}}
	defer func() { n.worker.store.engine = actual }()

	n.args["email"] = consentMailbox
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	n.worker.store.engine = actual

	roster := n.roster(t)
	if len(roster) != 1 || roster[0].ID != manual {
		t.Errorf("the audience holds %v, want only the row that was added meanwhile (%s): the signup added a second row for the same mailbox", ids(roster), manual)
	}
	signup := n.signupRow(t)
	if signup == nil {
		t.Fatal("no signup was recorded")
	}
	if got := bare(str(signup, "recipientId")); got != manual {
		t.Errorf("the signup names recipient %q, want the member there is, %q", got, manual)
	}
}

// TestAMailboxThatOptedOutMeanwhileIsNotWelcomed: the same window, with the row that
// appeared in between already opted out. Re-applying the verdict under the lock
// refuses the signup and writes nothing.
func TestAMailboxThatOptedOutMeanwhileIsNotWelcomed(t *testing.T) {
	n := newEmptyNewsletter(t, "gate-meanwhile-out")
	manual := n.suffix + "-manual"
	actual := n.worker.store.engine
	n.worker.store.engine = &addsMeanwhile{Engine: actual, add: func() {
		if err := n.store.AddRecipient(n.owner, manual, n.audience, " Subscriber@Example.TEST ", "", "manual", n.org, nil); err != nil {
			t.Error(err)
			return
		}
		if err := n.store.SetRecipientSubscription(n.owner, manual, "unsubscribed", time.Now()); err != nil {
			t.Error(err)
		}
	}}
	defer func() { n.worker.store.engine = actual }()

	n.args["email"] = consentMailbox
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	n.worker.store.engine = actual
	n.assertTheFormDidNothing(t, 1)
}
