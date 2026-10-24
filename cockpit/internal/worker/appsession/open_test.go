package appsession

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeExitFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exit")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// THE EXIT CODE TRAVELS AS AN int32 AND THIS FILE IS AN int, so a value
// the wire cannot represent must be refused HERE rather than truncated
// there.
//
// The failure is the worst shape there is: AppSessionEnd.ExitCode is
// int32, so a file containing 4294967296 becomes 0 on the wire -- and
// the engine reads 0 as a run that SUCCEEDED. A failed session is
// recorded as a good one, in a record people read back later when they
// are deciding whether the thing worked, and nothing downstream can
// detect it.
//
// Refusing is safe because the caller has a better answer: settle()
// falls back to the launcher process's own exit status when this file
// says nothing usable.
func TestReadOpenExitFileRefusesWhatTheWireCannotCarry(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		ok   bool
	}{
		// The ordinary range a process exit status can occupy.
		{"success", "0", 0, true},
		{"failure", "1", 1, true},
		{"a shell's signal code", "137", 137, true},
		{"the top of the range", "255", 255, true},
		{"no exit status", "-1", -1, true},
		{"trailing newline", "2\n", 2, true},
		{"surrounding space", "  3  \n", 3, true},

		// Outside it. Every one of these is a file that is not an exit
		// code, and the launcher this cockpit writes cannot produce one.
		{"above the range", "256", 0, false},
		{"far above the range", "1000", 0, false},
		{"below the range", "-2", 0, false},

		// THE ONE THAT MATTERS: int32 truncation turns a failure into a
		// success. 4294967296 is 2^32, which truncates to exactly 0.
		{"truncates to zero on the wire", "4294967296", 0, false},
		{"truncates to one on the wire", "4294967297", 0, false},
		{"max int64", strconv.FormatInt(math.MaxInt64, 10), 0, false},
		{"min int64", strconv.FormatInt(math.MinInt64, 10), 0, false},

		// Not a number at all.
		{"prose", "the app crashed", 0, false},
		{"empty", "", 0, false},
		{"hex", "0x1", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := readOpenExitFile(writeExitFile(t, tc.body))
			if ok != tc.ok {
				t.Fatalf("readOpenExitFile(%q) ok = %v, want %v (got %d)", tc.body, ok, tc.ok, got)
			}
			if got != tc.want {
				t.Fatalf("readOpenExitFile(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// A missing file is not an error either -- the launcher has not written
// it yet, which is the ordinary state while a session is running.
func TestReadOpenExitFileOnAMissingFile(t *testing.T) {
	if got, ok := readOpenExitFile(filepath.Join(t.TempDir(), "absent")); ok || got != 0 {
		t.Fatalf("got (%d, %v), want (0, false)", got, ok)
	}
}

// EVERY VALUE THIS FUNCTION ACCEPTS SURVIVES THE WIRE UNCHANGED. The
// property, rather than the boundary: whatever comes back must round
// trip through int32, because that is the type it is about to be
// assigned to.
func TestEveryAcceptedExitCodeSurvivesInt32(t *testing.T) {
	for n := -300; n <= 300; n++ {
		got, ok := readOpenExitFile(writeExitFile(t, strconv.Itoa(n)))
		if !ok {
			continue
		}
		if int(int32(got)) != got {
			t.Fatalf("readOpenExitFile accepted %d, which does not survive int32", got)
		}
	}
}
