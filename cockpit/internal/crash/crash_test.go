package crash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCatch_NoPanicReturnsNil pins the happy path: when fn returns
// normally, Catch returns nil. This is the contract every caller
// depends on -- a non-nil report means "panic happened, render the
// placeholder", a nil report means "carry on".
func TestCatch_NoPanicReturnsNil(t *testing.T) {
	report := Catch("test:happy-path", func() {
		// Touch some state so the compiler can't trivially eliminate
		// the closure. (Without this `_ = something` line a future
		// inliner could erase the call entirely.)
		_ = 1 + 1
	})
	if report != nil {
		t.Fatalf("Catch on a non-panicking fn returned %+v; want nil", report)
	}
}

// TestCatch_StringPanic covers the most common panic shape -- a bare
// string literal. The Report's Value must contain the panic text.
func TestCatch_StringPanic(t *testing.T) {
	withTempLogDir(t, func() {
		report := Catch("test:string-panic", func() {
			panic("oh no")
		})
		if report == nil {
			t.Fatal("Catch on a panicking fn returned nil; want non-nil report")
		}
		if !strings.Contains(report.Value, "oh no") {
			t.Errorf("Report.Value = %q; want it to contain panic message", report.Value)
		}
		if report.Label != "test:string-panic" {
			t.Errorf("Report.Label = %q; want %q", report.Label, "test:string-panic")
		}
		assertValidCode(t, report.Code)
		assertRecentTime(t, report.At)
	})
}

// TestCatch_ErrorPanic exercises panic(error). %v formatting of an
// error returns its message, so Value should contain the error text.
func TestCatch_ErrorPanic(t *testing.T) {
	withTempLogDir(t, func() {
		report := Catch("test:error-panic", func() {
			panic(errors.New("wrapped error case"))
		})
		if report == nil {
			t.Fatal("Catch on a panicking fn returned nil; want non-nil report")
		}
		if !strings.Contains(report.Value, "wrapped error case") {
			t.Errorf("Report.Value = %q; want it to contain the error message", report.Value)
		}
	})
}

// TestCatch_RuntimeErrorPanic covers runtime panics (nil deref,
// index out of range, etc.). The Value should still be a useful
// string -- runtime.Error implements fmt.Stringer.
func TestCatch_RuntimeErrorPanic(t *testing.T) {
	withTempLogDir(t, func() {
		report := Catch("test:runtime-panic", func() {
			// Trigger the actual user-reported crash class:
			// index out of range on a length-0 slice.
			var empty []int
			_ = empty[42]
		})
		if report == nil {
			t.Fatal("Catch on a runtime panic returned nil; want non-nil report")
		}
		if !strings.Contains(report.Value, "index out of range") {
			t.Errorf("Report.Value = %q; want it to mention index out of range", report.Value)
		}
		// The stack must include the panicking frame. Defending against a
		// future refactor that changes WHEN debug.Stack is captured.
		if !strings.Contains(report.Stack, "TestCatch_RuntimeErrorPanic") {
			t.Errorf("Report.Stack missing the test frame; got:\n%s", report.Stack)
		}
	})
}

// TestCatch_NilPanic guards against the weird-but-legal `panic(nil)`.
// Go 1.21+ converts a literal nil panic into a *runtime.PanicNilError
// so recover()!=nil; we still want a Report with a sensible Value.
func TestCatch_NilPanic(t *testing.T) {
	withTempLogDir(t, func() {
		report := Catch("test:nil-panic", func() {
			panic(nil)
		})
		if report == nil {
			t.Fatal("Catch on panic(nil) returned nil; want non-nil report")
		}
		if report.Value == "" {
			t.Error("Report.Value is empty; want a stringified panic value (panic(nil) -> runtime.PanicNilError)")
		}
	})
}

// TestCatch_ComplexPanicValue makes sure %v stringification doesn't
// itself panic when the panic value is a struct (a long-standing
// foot-gun in recovery handlers that try to type-assert the value).
func TestCatch_ComplexPanicValue(t *testing.T) {
	withTempLogDir(t, func() {
		type weirdStruct struct {
			Field1 string
			Field2 int
		}
		report := Catch("test:complex-panic", func() {
			panic(weirdStruct{Field1: "x", Field2: 42})
		})
		if report == nil {
			t.Fatal("Catch on struct-valued panic returned nil; want non-nil report")
		}
		if !strings.Contains(report.Value, "x") || !strings.Contains(report.Value, "42") {
			t.Errorf("Report.Value = %q; want it to %%v-format the struct fields", report.Value)
		}
	})
}

// TestCatch_DoesNotRethrow is the critical guarantee: a panic in fn
// must NOT propagate out of Catch. Otherwise the cockpit's outer
// loop crashes anyway and our recovery layers are useless.
func TestCatch_DoesNotRethrow(t *testing.T) {
	withTempLogDir(t, func() {
		// If Catch rethrows, this test goroutine panics and the
		// test process exits non-zero. We assert by reaching the
		// final line.
		_ = Catch("test:no-rethrow", func() {
			panic("must not escape")
		})
		// If we get here, the panic was caught.
	})
}

// TestCatch_UniqueCodesPerPanic asserts each panic instance gets a
// fresh error code, even when the panic value is identical. Support
// relies on the code-to-log mapping being one-to-one.
func TestCatch_UniqueCodesPerPanic(t *testing.T) {
	withTempLogDir(t, func() {
		const N = 50
		seen := make(map[string]struct{}, N)
		for i := 0; i < N; i++ {
			report := Catch("test:unique-codes", func() {
				panic("identical panic")
			})
			if report == nil {
				t.Fatalf("iteration %d: Catch returned nil", i)
			}
			if _, dup := seen[report.Code]; dup {
				t.Fatalf("iteration %d: duplicate error code %q", i, report.Code)
			}
			seen[report.Code] = struct{}{}
		}
	})
}

// TestCatch_ConcurrentSafe runs Catch from many goroutines at once.
// The package keeps no shared mutable state, so this should Just
// Work; the test pins that.
func TestCatch_ConcurrentSafe(t *testing.T) {
	withTempLogDir(t, func() {
		const G = 32
		var wg sync.WaitGroup
		reports := make([]*Report, G)
		for i := 0; i < G; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				reports[idx] = Catch("test:concurrent", func() {
					panic(fmt.Sprintf("g%d", idx))
				})
			}(i)
		}
		wg.Wait()
		seen := make(map[string]struct{}, G)
		for i, r := range reports {
			if r == nil {
				t.Errorf("goroutine %d: nil report", i)
				continue
			}
			if _, dup := seen[r.Code]; dup {
				t.Errorf("goroutine %d: duplicate code %q", i, r.Code)
			}
			seen[r.Code] = struct{}{}
		}
	})
}

// TestCatch_NestedRecovers verifies that Catch inside Catch works --
// the inner one catches the panic, the outer one returns nil. The
// post-crash-draw code path in app.go depends on this.
func TestCatch_NestedRecovers(t *testing.T) {
	withTempLogDir(t, func() {
		outer := Catch("test:outer", func() {
			inner := Catch("test:inner", func() {
				panic("inner blast")
			})
			if inner == nil {
				t.Error("inner Catch returned nil; want non-nil")
			}
			// outer should NOT see this panic since inner caught it.
		})
		if outer != nil {
			t.Errorf("outer Catch returned a report; want nil (inner caught it). got %+v", outer)
		}
	})
}

// TestCatch_NestedPanicAfterRecover covers the scary case: code that
// panics AGAIN after the inner Catch returned (the inner panic was
// caught + a new one fired). The outer Catch must catch it.
func TestCatch_NestedPanicAfterRecover(t *testing.T) {
	withTempLogDir(t, func() {
		outer := Catch("test:outer-after-inner", func() {
			_ = Catch("test:inner", func() {
				panic("first")
			})
			panic("second")
		})
		if outer == nil {
			t.Fatal("outer Catch returned nil; want non-nil (second panic should escape inner)")
		}
		if !strings.Contains(outer.Value, "second") {
			t.Errorf("outer.Value = %q; want it to capture the SECOND panic", outer.Value)
		}
	})
}

// TestWriteLog_WritesReadableFile checks that the file actually
// lands on disk + is parseable. Support's first move with a crash
// code is to `cat` the matching log; the file has to be useful.
func TestWriteLog_WritesReadableFile(t *testing.T) {
	dir := withTempLogDir(t, nil)
	r := &Report{
		Code:  "deadbeef",
		At:    time.Date(2026, 5, 17, 18, 30, 0, 0, time.UTC),
		Label: "test:writelog",
		Value: "oh dear",
		Stack: "goroutine 1 [running]:\nmain.main()\n",
	}
	if err := WriteLog(r); err != nil {
		t.Fatalf("WriteLog returned error: %v", err)
	}
	if r.LogPath == "" {
		t.Fatal("WriteLog left LogPath empty on success")
	}
	if !strings.HasPrefix(r.LogPath, dir) {
		t.Errorf("LogPath %q not under temp dir %q", r.LogPath, dir)
	}
	data, err := os.ReadFile(r.LogPath)
	if err != nil {
		t.Fatalf("could not read back log file: %v", err)
	}
	body := string(data)
	for _, want := range []string{"deadbeef", "test:writelog", "oh dear", "goroutine 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("log body missing %q; full body:\n%s", want, body)
		}
	}
	// File permissions must be 0600 -- crash logs may contain sensitive
	// stack contents (in-flight tokens, partial DSL bodies, etc.).
	info, err := os.Stat(r.LogPath)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("log file mode = %o; want 0600", mode)
	}
}

// TestWriteLog_FilenameContainsCode pins the filename convention so
// support's "find the log for code X" doesn't break on a future
// rename.
func TestWriteLog_FilenameContainsCode(t *testing.T) {
	withTempLogDir(t, func() {
		report := Catch("test:filename", func() { panic("nope") })
		if report == nil || report.LogPath == "" {
			t.Fatal("expected a written report with a log path")
		}
		base := filepath.Base(report.LogPath)
		if !strings.Contains(base, report.Code) {
			t.Errorf("filename %q does not contain code %q", base, report.Code)
		}
		if !strings.HasSuffix(base, ".log") {
			t.Errorf("filename %q does not end in .log", base)
		}
	})
}

// TestUserMessage_ContainsKeyInfo asserts the user-facing message
// includes the code + log path + the "contact support" phrasing.
func TestUserMessage_ContainsKeyInfo(t *testing.T) {
	r := &Report{
		Code:    "abc12345",
		LogPath: "/home/op/.memql/cockpit-crashes/whatever.log",
	}
	msg := UserMessage(r)
	for _, want := range []string{"abc12345", r.LogPath, "contact support"} {
		if !strings.Contains(msg, want) {
			t.Errorf("UserMessage missing %q; got:\n%s", want, msg)
		}
	}
}

// TestUserMessage_NilReportDoesNotCrash guards the path where we
// somehow end up calling UserMessage with a nil report. Better to
// print a generic message than to panic in the panic handler.
func TestUserMessage_NilReportDoesNotCrash(t *testing.T) {
	msg := UserMessage(nil)
	if msg == "" {
		t.Error("UserMessage(nil) returned empty string; want a generic message")
	}
	if !strings.Contains(strings.ToLower(msg), "support") {
		t.Errorf("UserMessage(nil) missing 'support' hint; got %q", msg)
	}
}

// TestUserMessage_MissingLogPath covers the case where WriteLog
// failed (e.g. unwritable home dir). The user message should fall
// back to a clear hint rather than printing an empty path.
func TestUserMessage_MissingLogPath(t *testing.T) {
	r := &Report{Code: "xyz98765"} // no LogPath
	msg := UserMessage(r)
	if !strings.Contains(msg, "xyz98765") {
		t.Errorf("missing code in message: %q", msg)
	}
	if strings.Contains(msg, "Crash log: \n") || strings.Contains(msg, "Crash log:  \n") {
		t.Errorf("message includes empty 'Crash log:' line: %q", msg)
	}
}

// TestShortCode_FormatIsEightHexChars pins the wire format of the
// error code so a future "make codes longer" refactor doesn't
// silently break the user-facing message + log filename pattern.
func TestShortCode_FormatIsEightHexChars(t *testing.T) {
	pat := regexp.MustCompile(`^[0-9a-f]{8}$`)
	for i := 0; i < 100; i++ {
		c := shortCode()
		if !pat.MatchString(c) {
			t.Fatalf("shortCode() returned %q; want 8 hex chars", c)
		}
	}
}

// TestLogDir_FallsBackOnHomeUnreadable covers the "no $HOME" path.
// The LogDir helper must still return SOMETHING usable so crash
// logs land on disk even when the user's home dir resolution fails.
func TestLogDir_FallsBackOnHomeUnreadable(t *testing.T) {
	saved := os.Getenv("HOME")
	defer os.Setenv("HOME", saved)
	// Empty HOME causes user.Current() to fail on most platforms.
	_ = os.Unsetenv("HOME")
	dir := LogDir()
	if dir == "" {
		t.Fatal("LogDir() returned empty path when HOME is unset")
	}
}

// --- helpers ---------------------------------------------------------

// withTempLogDir redirects LogDir to a t.TempDir for the duration of
// fn (or returns the dir path if fn is nil so the test can build a
// path under it). Implementation hops over LogDir() entirely by
// pointing HOME at the temp dir -- LogDir derives from $HOME, so
// the redirect is transparent to the production code path.
func withTempLogDir(t *testing.T, fn func()) string {
	t.Helper()
	dir := t.TempDir()
	saved, hadHome := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", saved)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	expectedDir := filepath.Join(dir, ".memql", "cockpit-crashes")
	if fn != nil {
		fn()
	}
	return expectedDir
}

func assertValidCode(t *testing.T, code string) {
	t.Helper()
	if matched, _ := regexp.MatchString(`^[0-9a-f]{8}$`, code); !matched {
		t.Errorf("error code %q is not 8 lowercase hex chars", code)
	}
}

func assertRecentTime(t *testing.T, at time.Time) {
	t.Helper()
	if at.IsZero() {
		t.Error("Report.At is zero")
		return
	}
	if delta := time.Since(at); delta < 0 || delta > 5*time.Second {
		t.Errorf("Report.At = %v; not within last 5s (delta=%v)", at, delta)
	}
}
