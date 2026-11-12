package shopify

import (
	"strings"
	"testing"
	"time"
)

// privacy_verdict_test.go -- a privacy request that keeps failing is seen
// (ruling 79).
//
// A failed job is run again at every pass. One that has been failing for more
// than a day is not going to mend by itself, and until now nothing told
// anybody: the job's row was the only place it showed. The store's verdict
// now lists it, and the watch mails the owner about a store that needs
// attention.
//
// These are the verdict's half, on what the read answers. The read itself,
// against the jobs' own rows, is compliance_runner_db_test.go.

const privacySentence = "A privacy request for this store could not be completed."

func failingSince(d time.Duration) privacyFailing {
	return privacyFailing{Count: 1, Since: verdictNow.Add(-d)}
}

// A job failed and due 25 hours ago gives Needs attention with the exact
// sentence, and no act: nothing a person presses mends it.
func TestAPrivacyRequestFailingForADayNeedsAttention(t *testing.T) {
	store := verdictStore()
	in := verdictInputOf(store, cleanRows(store))
	if v := computeVerdict(in); v.State != VerdictRecording {
		t.Fatalf("the fixture store reads %q with %v, want recording", v.State, reasonCodes(v))
	}
	in.Privacy = failingSince(25 * time.Hour)

	v := computeVerdict(in)

	if v.State != VerdictNeedsAttention || len(v.Reasons) != 1 {
		t.Fatalf("state %q with reasons %v, want needs attention for the one reason", v.State, reasonCodes(v))
	}
	r := v.Reasons[0]
	if r.Code != "privacy_request_failing" || ReasonPrivacyRequestFailing != "privacy_request_failing" {
		t.Errorf("the reason's code is %q (the constant is %q), want privacy_request_failing", r.Code, ReasonPrivacyRequestFailing)
	}
	if r.Sentence != privacySentence {
		t.Errorf("the reason's sentence is %q, want exactly %q", r.Sentence, privacySentence)
	}
	if r.Act != "" || r.ActLabel != "" || r.ActURL != "" {
		t.Errorf("the reason offers an act (%q, %q, %q); the job is tried again by itself and nothing a person presses mends it", r.Act, r.ActLabel, r.ActURL)
	}
	if r.Count != 1 || r.AtLeast {
		t.Errorf("the reason counts %d (at least: %v), want the one request", r.Count, r.AtLeast)
	}
	since := verdictNow.Add(-25 * time.Hour).Format(time.RFC3339)
	if !strings.Contains(r.Detail, "1 request") || !strings.Contains(r.Detail, since) {
		t.Errorf("the reason's detail is %q, want how many and since when (%s)", r.Detail, since)
	}
	for _, word := range []string{"gid://", "Customer", "@"} {
		if strings.Contains(r.Detail, word) || strings.Contains(r.Sentence, word) {
			t.Errorf("the reason names whom the request is about: %q / %q", r.Sentence, r.Detail)
		}
	}
}

// Several, and a page of them that was full: the detail says how many and
// that there may be more.
func TestTheReasonSaysHowManyRequestsAreFailing(t *testing.T) {
	oldest := verdictNow.Add(-72 * time.Hour)
	r, ok := privacyReason(privacyFailing{Count: 3, Since: oldest})
	if !ok || r.Count != 3 || r.AtLeast || !strings.Contains(r.Detail, "3 requests") || !strings.Contains(r.Detail, oldest.Format(time.RFC3339)) {
		t.Errorf("three failing requests give %+v (%v)", r, ok)
	}
	r, ok = privacyReason(privacyFailing{Count: 100, AtLeast: true, Since: oldest})
	if !ok || r.Count != 100 || !r.AtLeast || !strings.Contains(r.Detail, "At least 100 requests") {
		t.Errorf("a full page of failing requests gives %+v (%v), want a floor", r, ok)
	}
	if r.Sentence != privacySentence {
		t.Errorf("the sentence changes with the count: %q", r.Sentence)
	}
}

// None failing for a day gives no reason: a job that failed an hour ago is
// tried again at the next pass, and a done one is done. Which jobs count is
// the read's (privacyRequestsFailing); with none, the verdict says nothing.
func TestNoPrivacyRequestFailingGivesNoReason(t *testing.T) {
	store := verdictStore()
	in := verdictInputOf(store, cleanRows(store))
	in.Privacy = privacyFailing{}
	if v := computeVerdict(in); v.State != VerdictRecording || len(v.Reasons) != 0 {
		t.Errorf("state %q with %v, want recording and no reason", v.State, reasonCodes(v))
	}
	if _, ok := privacyReason(privacyFailing{}); ok {
		t.Error("no failing request is a reason")
	}
}

// It joins the store's other reasons, last, and a store that is starting
// needs attention for it too.
func TestTheFailingPrivacyRequestJoinsTheOtherReasons(t *testing.T) {
	store := verdictStore()
	rows := cleanRows(store)
	setRow(rows, "customer", func(r map[string]any) { r["lastError"] = "shopify admin: HTTP 401" })
	in := verdictInputOf(store, rows)
	in.Privacy = failingSince(30 * time.Hour)
	v := computeVerdict(in)
	codes := reasonCodes(v)
	if v.State != VerdictNeedsAttention || len(codes) < 2 || codes[len(codes)-1] != ReasonPrivacyRequestFailing {
		t.Errorf("state %q with %v, want the privacy reason listed after the store's other reasons", v.State, codes)
	}

	starting := verdictStore()
	startingRows := cleanRows(starting)
	for i := range startingRows {
		startingRows[i]["backfillStatus"] = "running"
	}
	in = verdictInputOf(starting, startingRows)
	if v := computeVerdict(in); v.State != VerdictStarting {
		t.Fatalf("the fixture store reads %q, want starting", v.State)
	}
	in.Privacy = failingSince(30 * time.Hour)
	if v := computeVerdict(in); v.State != VerdictNeedsAttention || strings.Join(reasonCodes(v), ",") != ReasonPrivacyRequestFailing {
		t.Errorf("a starting store with a failing privacy request reads %q with %v, want needs attention", v.State, reasonCodes(v))
	}
}

// A store that is paused or not connected keeps its state: nobody is recording
// it and that is what its word says. The reason is listed on it all the same.
// A shop's erasure is by its nature a job of a store that is uninstalled, so
// without this the one request most likely to be failing would be seen
// nowhere.
func TestAStoreThatIsPausedOrNotConnectedKeepsItsStateAndListsTheReason(t *testing.T) {
	for name, edit := range map[string]func(*Store){
		VerdictPaused:       func(s *Store) { s.Status = StatusPaused },
		VerdictNotConnected: func(s *Store) { s.UninstalledAt = "2026-10-01T00:00:00Z" },
	} {
		t.Run(name, func(t *testing.T) {
			store := verdictStore()
			edit(&store)
			in := verdictInputOf(store, cleanRows(store))
			if v := computeVerdict(in); v.State != name || len(v.Reasons) != 0 {
				t.Fatalf("the fixture store reads %q with %v", v.State, reasonCodes(v))
			}
			in.Privacy = failingSince(25 * time.Hour)
			v := computeVerdict(in)
			if v.State != name {
				t.Errorf("the store reads %q, want it to stay %q", v.State, name)
			}
			if len(v.Reasons) != 1 || v.Reasons[0].Code != ReasonPrivacyRequestFailing || v.Reasons[0].Sentence != privacySentence {
				t.Errorf("the store lists %v, want the one privacy reason", v.Reasons)
			}
		})
	}
}

// The watch mails the owner about it like any other reason: its sentence is
// one of the mail's lines, and nothing of the mail's copy changes.
func TestTheWatchMailsTheOwnerAboutAFailingPrivacyRequest(t *testing.T) {
	store := verdictStore()
	in := verdictInputOf(store, cleanRows(store))
	in.Privacy = failingSince(25 * time.Hour)
	v := computeVerdict(in)

	wait := 30 * time.Minute
	plan := planWatch(watchedBefore(v), v, wait, verdictNow)
	if plan.Alert {
		t.Fatal("the owner is mailed before the store has needed attention for the wait")
	}
	if strings.Join(plan.Record.Reasons, ",") != ReasonPrivacyRequestFailing {
		t.Errorf("the watch records the reasons %v", plan.Record.Reasons)
	}
	recorded := plan.Record.payload()
	plan = planWatch(recorded, v, wait, verdictNow.Add(wait+time.Minute))
	if !plan.Alert {
		t.Fatal("a store that has needed attention for the wait, for a failing privacy request alone, is not mailed about")
	}
	subject, body := alertEmail(v, plan.Record.Episode, "https://os.example.test")
	if subject != store.Domain+" needs attention" {
		t.Errorf("subject %q", subject)
	}
	if !strings.Contains(body, "\n"+privacySentence+"\n") {
		t.Errorf("the mail does not carry the reason's sentence as one of its lines:\n%s", body)
	}
	if !strings.Contains(body, "This email carries no order and nothing about a customer.") || strings.Contains(body, "gid://") {
		t.Errorf("the mail's copy changed, or it names a customer:\n%s", body)
	}
}

// When a failing privacy request is the ONLY reason a store needs attention,
// the mail does not say the store is not being recorded: it is recording
// everything, and that first line would be false. With any other reason
// present the first line is as it was, and the subject is the same either way.
func TestTheMailForAFailingPrivacyRequestAloneDoesNotSayTheStoreIsNotRecorded(t *testing.T) {
	const episode = "2026-11-10T07:30:00Z"
	const since = "Tue, 10 Nov 2026 07:30:00 UTC"
	store := verdictStore()

	in := verdictInputOf(store, cleanRows(store))
	in.Privacy = failingSince(25 * time.Hour)
	alone := computeVerdict(in)
	if strings.Join(reasonCodes(alone), ",") != ReasonPrivacyRequestFailing {
		t.Fatalf("the fixture store lists %v, want the privacy reason alone", reasonCodes(alone))
	}
	subject, body := alertEmail(alone, episode, "https://os.example.test")
	first, _, _ := strings.Cut(body, "\n")
	if want := store.Domain + " needs attention in MemQL, since " + since + "."; first != want {
		t.Errorf("with the privacy reason alone the mail begins\n  %q\nwant\n  %q", first, want)
	}
	if subject != store.Domain+" needs attention" {
		t.Errorf("subject %q", subject)
	}
	if !strings.Contains(body, "\n"+privacySentence+"\n") {
		t.Errorf("the mail lost the reason's sentence:\n%s", body)
	}

	rows := cleanRows(store)
	setRow(rows, "customer", func(r map[string]any) { r["lastError"] = "shopify admin: HTTP 401" })
	in = verdictInputOf(store, rows)
	in.Privacy = failingSince(25 * time.Hour)
	both := computeVerdict(in)
	if len(both.Reasons) < 2 {
		t.Fatalf("the fixture store lists %v, want another reason beside the privacy one", reasonCodes(both))
	}
	subject, body = alertEmail(both, episode, "https://os.example.test")
	first, _, _ = strings.Cut(body, "\n")
	if want := "MemQL is not recording everything from " + store.Domain + ", since " + since + "."; first != want {
		t.Errorf("with another reason beside it the mail begins\n  %q\nwant it as it was:\n  %q", first, want)
	}
	if subject != store.Domain+" needs attention" {
		t.Errorf("subject %q", subject)
	}
}

// The recovery after an episode whose only reason was a failing privacy
// request does not say the store "is being recorded again": it was recorded
// all along. It says the store no longer needs attention. An episode that had
// any other reason at any pass is recovered as it always was.
func TestTheRecoveryAfterAPrivacyOnlyEpisodeSaysItNoLongerNeedsAttention(t *testing.T) {
	store := verdictStore()
	wait := 30 * time.Minute
	clean := computeVerdict(verdictInputOf(store, cleanRows(store)))
	onlyPrivacy := func() Verdict {
		in := verdictInputOf(store, cleanRows(store))
		in.Privacy = failingSince(25 * time.Hour)
		return computeVerdict(in)
	}
	withAnother := func() Verdict {
		rows := cleanRows(store)
		setRow(rows, "customer", func(r map[string]any) { r["lastError"] = "shopify admin: HTTP 401" })
		in := verdictInputOf(store, rows)
		in.Privacy = failingSince(25 * time.Hour)
		return computeVerdict(in)
	}
	// episode walks a store through the passes of one episode and answers the
	// recovery mail staged when it records again.
	episode := func(passes ...Verdict) (string, string, string) {
		t.Helper()
		recorded := watchedBefore(clean)
		at := verdictNow
		var start string
		for i, v := range passes {
			plan := planWatch(recorded, v, wait, at)
			if i == 0 {
				start = plan.Record.Episode
			}
			if plan.Alert {
				plan.Record.alertStaged(plan.Record.Episode)
			}
			recorded = plan.Record.payload()
			at = at.Add(wait + time.Minute)
		}
		plan := planWatch(recorded, clean, wait, at)
		if plan.Recover == "" {
			t.Fatalf("no recovery was planned after %d pass(es)", len(passes))
		}
		subject, body := recoveryEmailOf(plan.Record, store.Domain, plan.Recover, at)
		return subject, body, "from " + stampRFC1123(start) + " to " + at.UTC().Format(time.RFC1123) + "."
	}

	subject, body, span := episode(onlyPrivacy(), onlyPrivacy())
	if subject != store.Domain+" no longer needs attention" {
		t.Errorf("the recovery of a privacy-only episode has the subject %q", subject)
	}
	if want := store.Domain + " no longer needs attention in MemQL. It needed attention " + span; body != want {
		t.Errorf("the recovery of a privacy-only episode reads\n  %q\nwant\n  %q", body, want)
	}

	for name, passes := range map[string][]Verdict{
		"another reason beside it":          {withAnother(), withAnother()},
		"another reason at an earlier pass": {withAnother(), onlyPrivacy(), onlyPrivacy()},
	} {
		subject, body, span := episode(passes...)
		if subject != store.Domain+" is being recorded again" || body != "MemQL is recording "+store.Domain+" in full again. It needed attention "+span {
			t.Errorf("%s: the recovery reads %q / %q, want it as it was", name, subject, body)
		}
	}
}
