package world

import (
	"strings"
	"testing"
)

func mailbox() *World {
	return New(Config{Addresses: []string{"ops@example.test"}})
}

func TestADuplicateIsACCEPTEDANDRECORDED(t *testing.T) {
	// The rule here is the OPPOSITE of a production outbox's, and it is the
	// whole reason this package exists. A world that REFUSED a duplicate
	// would make "zero duplicated side effects" true by construction and
	// prove nothing about the platform; the world's job is to notice.
	w := mailbox()
	for i := 0; i < 2; i++ {
		if err := w.Send("ops@example.test", "the same body", "run:step:1"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if got := w.Count("mailbox"); got != 2 {
		t.Fatalf("Count = %d, want 2 -- a count that quietly excluded duplicates would make `count: 1` pass on a world that delivered twice", got)
	}
	if got := w.Duplicates(); got != 1 {
		t.Fatalf("Duplicates = %d, want 1", got)
	}
	if !strings.Contains(w.DuplicateDetail(), "ops@example.test") {
		t.Errorf("DuplicateDetail = %q, want it to name what repeated", w.DuplicateDetail())
	}
}

func TestTheDuplicateKeyIsTheDELIVERYAndNotTheCallersIdempotencyKey(t *testing.T) {
	// Keying on the caller's idempotency key would make the world agree with
	// the platform by construction -- and the whole question is whether the
	// platform's key did its job.
	w := mailbox()
	_ = w.Send("ops@example.test", "same body", "run:step:1")
	_ = w.Send("ops@example.test", "same body", "run:step:2") // a DIFFERENT key
	if got := w.Duplicates(); got != 1 {
		t.Fatalf("Duplicates = %d, want 1: two identical deliveries under different keys are still two deliveries", got)
	}
	if !strings.Contains(w.DuplicateDetail(), "run:step:2") {
		t.Errorf("the detail does not name the key the duplicate was made under: %q", w.DuplicateDetail())
	}
}

func TestADifferentBodyIsNotADuplicate(t *testing.T) {
	w := mailbox()
	_ = w.Send("ops@example.test", "first", "k1")
	_ = w.Send("ops@example.test", "second", "k2")
	if got := w.Duplicates(); got != 0 {
		t.Fatalf("Duplicates = %d, want 0", got)
	}
}

func TestAnUnknownAddressIsAnErrorAndNotASilentDrop(t *testing.T) {
	// A scenario that mails somewhere the world does not know about would
	// otherwise report a clean run having delivered nothing.
	w := mailbox()
	err := w.Send("nobody@example.test", "body", "k")
	if err == nil {
		t.Fatal("Send accepted an address the world does not know")
	}
	if !strings.Contains(err.Error(), "ops@example.test") {
		t.Errorf("the error does not say what it DOES accept: %v", err)
	}
	if w.Count("mailbox") != 0 {
		t.Error("a refused delivery was counted")
	}
}

func TestAnUnknownScriptIsAnErrorNamingWhatItKnows(t *testing.T) {
	w := New(Config{Scripts: map[string]string{"reconcile.sh": "ok\n"}})
	if _, err := w.RunScript("nope.sh", "body", "k"); err == nil || !strings.Contains(err.Error(), "reconcile.sh") {
		t.Fatalf("RunScript = %v, want an error naming the scripts it has", err)
	}
}

func TestTheMachineRecordsTheHashOfWhatItWasAskedToRun(t *testing.T) {
	// The fleet scenario's whole assertion.
	w := New(Config{Scripts: map[string]string{"reconcile.sh": "ok\n"}})
	if _, err := w.RunScript("reconcile.sh", "the exact body", "k"); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	hashes := w.ScriptHashes()
	if len(hashes) != 1 {
		t.Fatalf("ScriptHashes = %v", hashes)
	}
	if hashes[0] != Digest("the exact body") {
		t.Errorf("the recorded hash is not the hash of what was asked: %q", hashes[0])
	}
	if hashes[0] == Digest("a different body") {
		t.Error("two different bodies digest the same")
	}
}

func TestAnUnknownCounterIsReportedRatherThanReadAsZero(t *testing.T) {
	// A typo'd counter that read zero would make `count: 0` pass forever,
	// which is the durability family's headline passing for the wrong reason.
	w := mailbox()
	if _, ok := w.Counter("mailbox.snet"); ok {
		t.Fatal("Counter answered for a misspelled name")
	}
	if _, ok := w.Counter("nonsense"); ok {
		t.Fatal("Counter answered for a name with no facet")
	}
	if n, ok := w.Counter("mailbox.sent"); !ok || n != 0 {
		t.Fatalf("Counter(mailbox.sent) = %d, %v", n, ok)
	}
}

func TestEveryKnownCounterSpellingActuallyAnswers(t *testing.T) {
	w := New(Config{
		Addresses: []string{"ops@example.test"},
		Scripts:   map[string]string{"s.sh": "ok"},
		Routes:    map[string]string{"POST /x": "{}"},
	})
	for _, spec := range KnownCounters() {
		if _, ok := w.Counter(spec); !ok {
			t.Errorf("KnownCounters lists %q and Counter does not answer for it", spec)
		}
	}
}
