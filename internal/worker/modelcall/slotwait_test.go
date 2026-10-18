package modelcall

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// slotwait_test.go pins the one thing this machine says about contention.
//
// Whether a background call starves a fast one is a question about this
// machine's traffic, and the only answer anybody will have is the log:
// before this, a call that waited ninety seconds for a slot and was then
// refused left a debug line, which the default level never prints.

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAWaitedSlotIsLoggedWithItsLevelAndPurpose. The level and purpose are
// the only things on the wire that say which kind of work held the slot
// and which was kept waiting, so they are what the line carries.
func TestAWaitedSlotIsLoggedWithItsLevelAndPurpose(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: 20 * time.Millisecond,
	})

	holder := startWithIdle("holder", 30)
	holder.Level, holder.Purpose = "strong", "planner"
	m.Start(context.Background(), newRecorder(), holder)
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 30)
	waiter.Level, waiter.Purpose = "fast", "decide"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	time.Sleep(120 * time.Millisecond) // past the floor, with the holder still holding
	release()
	awaitRequest(t, arrived)
	if end := rec.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("the waiting call ended %q", end.GetErrorCode())
	}

	out := logs.String()
	for _, want := range []string{
		`"msg":"model call waited for a concurrency slot"`,
		`"request_id":"waiter"`,
		`"call_level":"fast"`,
		`"purpose":"decide"`,
		`"model":"m"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %s:\n%s", want, out)
		}
	}
	assertCallLevelSplit(t, logLine(t, out, "model call waited for a concurrency slot", "waiter"), "INFO", "fast")
	if strings.Contains(out, `"request_id":"holder"`) {
		t.Errorf("a call that took a free slot must not be logged as having waited:\n%s", out)
	}
}

// TestARefusedWaitIsALoudLine. The refusal is a FAILED call -- the engine
// does not reroute it -- so it is the line an owner most needs, and it was
// a debug line.
func TestARefusedWaitIsALoudLine(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory: inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:    func(string) string { return "" },
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 1)
	waiter.Level = "fast"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	if end := rec.wait(t); end.GetErrorCode() != CodeConcurrencyExceeded {
		t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeConcurrencyExceeded)
	}
	out := logs.String()
	for _, want := range []string{
		`"level":"WARN"`,
		`"msg":"model call refused after waiting for a concurrency slot"`,
		`"request_id":"waiter"`,
		`"error_code":"model_concurrency_exceeded"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %s:\n%s", want, out)
		}
	}
	assertCallLevelSplit(t, logLine(t, out, "model call refused after waiting for a concurrency slot", "waiter"), "WARN", "fast")
}

// TestACallThatDoesNotWaitSaysNothing. Every call logging would bury the
// ones that matter.
func TestACallThatDoesNotWaitSaysNothing(t *testing.T) {
	srv, _ := ollamaChatStub(t, []string{"hi"}, false)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory: inventoryWith(ollamaModel(srv.URL, "llama3.1:8b", models.Attributes{MaxConcurrent: 2})),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:    func(string) string { return "" },
	})
	rec := newRecorder()
	m.Start(context.Background(), rec, start("r1", "llama3.1:8b", KindChat))
	rec.wait(t)
	if out := logs.String(); strings.Contains(out, "concurrency slot") {
		t.Errorf("a call that took a free slot logged about slots:\n%s", out)
	}
}

// assertEndLogged checks the line a wait leaves when it ends without a grant:
// the sentence, the call's level and purpose, and the code the call ended with.
func assertEndLogged(t *testing.T, out, errorCode string) {
	t.Helper()
	for _, want := range []string{
		`"level":"WARN"`,
		`"msg":"model call ended while waiting for a concurrency slot"`,
		`"request_id":"waiter"`,
		`"call_level":"fast"`,
		`"purpose":"decide"`,
		`"error_code":"` + errorCode + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %s:\n%s", want, out)
		}
	}
	assertCallLevelSplit(t, logLine(t, out, "model call ended while waiting for a concurrency slot", "waiter"), "WARN", "fast")
}

// logLine returns the one JSON line that logs msg for requestID. It fails the
// test unless exactly one line does.
func logLine(t *testing.T, out, msg, requestID string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) && strings.Contains(line, `"request_id":"`+requestID+`"`) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one line logging %q for %s, got %d:\n%s", msg, requestID, len(found), out)
	}
	return found[0]
}

// valuesOf returns every value stored under key in one flat JSON object, in
// order. A map keeps only one of two equal keys, and two equal keys is the
// defect the callers are looking for.
func valuesOf(t *testing.T, line, key string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, line)
	}
	var values []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("reading a key: %v\n%s", err, line)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("reading a value: %v\n%s", err, line)
		}
		if name, _ := k.(string); name == key {
			s, _ := v.(string)
			values = append(values, s)
		}
	}
	return values
}

// assertCallLevelSplit checks the layout every wait line keeps: the record's
// severity is the only "level" key, and the call's own level is under
// "call_level". Two "level" keys is the defect that made the rename necessary.
func assertCallLevelSplit(t *testing.T, line, severity, callLevel string) {
	t.Helper()
	if got := valuesOf(t, line, "level"); len(got) != 1 || got[0] != severity {
		t.Errorf("want one \"level\" key, %q, got %q:\n%s", severity, got, line)
	}
	if got := valuesOf(t, line, "call_level"); len(got) != 1 || got[0] != callLevel {
		t.Errorf("want one \"call_level\" key, %q, got %q:\n%s", callLevel, got, line)
	}
}

// TestACancelledWaitIsLoggedWithItsLevelAndPurpose. A cancel ends a wait with
// neither a grant nor a refusal, so the refusal line never runs for it. The
// Debug line before the wait is the only other record, and the default level
// never prints it, so a cancelled wait would be missing from the count.
func TestACancelledWaitIsLoggedWithItsLevelAndPurpose(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: 20 * time.Millisecond,
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 30)
	waiter.Level, waiter.Purpose = "fast", "decide"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	time.Sleep(120 * time.Millisecond) // past the floor, still behind the holder
	m.Cancel(&memqlv1.ModelCallCancel{RequestId: "waiter"})
	if end := rec.wait(t); end.GetErrorCode() != CodeCancelled {
		t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeCancelled)
	}
	assertEndLogged(t, logs.String(), "cancelled")
}

// TestATimedOutWaitIsLogged. The call's own timeout ends a wait through the
// same exit as a cancel, with a different error code.
func TestATimedOutWaitIsLogged(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: 20 * time.Millisecond,
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 30)
	waiter.Limits.TimeoutSeconds = 1
	waiter.Level, waiter.Purpose = "fast", "decide"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	if end := rec.wait(t); end.GetErrorCode() != CodeTimeout {
		t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeTimeout)
	}
	assertEndLogged(t, logs.String(), "timeout")
}

// TestAWaitBelowTheFloorSaysNothing. The floor is what keeps a slot that frees
// within a second from being logged as contention. With an hour's floor, a
// call that waits a hundred milliseconds and is then granted must not log it.
func TestAWaitBelowTheFloorSaysNothing(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, nil)),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: time.Hour,
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	rec := newRecorder()
	m.Start(context.Background(), rec, startWithIdle("waiter", 30))
	time.Sleep(100 * time.Millisecond) // waits below the floor, with the holder still holding
	release()
	awaitRequest(t, arrived)
	if end := rec.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("the waiting call ended %q", end.GetErrorCode())
	}
	if out := logs.String(); strings.Contains(out, `"msg":"model call waited for a concurrency slot"`) {
		t.Errorf("a wait below the floor was logged:\n%s", out)
	}
}

// TestASlotWaitFloorOfZeroOrLessIsOneSecond. Zero is the documented default and
// a negative floor must mean the same. Checking the resolved value pins both
// without sleeping a second to find out what would have been logged.
func TestASlotWaitFloorOfZeroOrLessIsOneSecond(t *testing.T) {
	for _, set := range []time.Duration{0, -time.Millisecond} {
		m := NewManager(Options{SlotWaitLogFloor: set})
		if m.slotWaitFloor != time.Second {
			t.Errorf("SlotWaitLogFloor %v resolved to %v, want 1s", set, m.slotWaitFloor)
		}
	}
}

// TestAWaitedCallsRunningLineCarriesItsCallLevel. A call that waited and then
// ran says its level on two lines, the wait and the running line. The running
// line is Debug, so this logger is at debug to read it.
func TestAWaitedCallsRunningLineCarriesItsCallLevel(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	var logs lockedBuffer
	m := NewManager(Options{
		Inventory:        inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})),
		Logger:           slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Getenv:           func(string) string { return "" },
		SlotWaitLogFloor: 20 * time.Millisecond,
	})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := startWithIdle("waiter", 30)
	waiter.Level, waiter.Purpose = "fast", "decide"
	rec := newRecorder()
	m.Start(context.Background(), rec, waiter)
	time.Sleep(120 * time.Millisecond) // past the floor, with the holder still holding
	release()
	awaitRequest(t, arrived)
	if end := rec.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("the waiting call ended %q", end.GetErrorCode())
	}

	out := logs.String()
	assertCallLevelSplit(t, logLine(t, out, "model call waited for a concurrency slot", "waiter"), "INFO", "fast")
	assertCallLevelSplit(t, logLine(t, out, "model call running", "waiter"), "DEBUG", "fast")
}
