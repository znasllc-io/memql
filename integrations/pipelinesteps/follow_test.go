package pipelinesteps

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// follow_test.go -- the follower's one rule (fix round 3): a line the step
// wrote reaches the archive exactly once -- never lost, never twice -- across
// any number of stream cuts, wherever a cut falls (inside a stamp, inside a
// line, inside a long line's pieces, at its last empty piece) and whatever the
// stamp order, as far as stdout and stderr stamp apart (replaySkew).

// nodeLog is a container's log as the kubelet holds it: lines in the order
// they were written, each "<RFC3339Nano stamp> <text>".
type nodeLog []string

// from is what a stream opened at since serves: every line stamped at or
// after since's whole second (Kube.FollowLog truncates it), in the order the
// lines were written; all of them for a zero since.
func (l nodeLog) from(since time.Time) string {
	var b strings.Builder
	for _, line := range l {
		at, _, _ := captureSplitStamp(line)
		if since.IsZero() || !at.Before(since.Truncate(time.Second)) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// testFollower is a follower over a capture of its own, cutting long lines
// into pieces of at most lineMax bytes.
type testFollower struct {
	*follower
	t       *testing.T
	capture *Capture
	path    string
}

func newTestFollower(t *testing.T, lineMax int) *testFollower {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.log")
	c, err := newCapture(CaptureOptions{RunID: "run-1", StepKey: "tests", StoreMaxLines: 1 << 20, ArchiveMax: 256 << 20, ArchivePath: path},
		time.Now, newCaptureBucket(1<<30, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	f := newFollower(&step{capture: c}, "pod")
	f.lineMax = lineMax
	return &testFollower{follower: f, t: t, capture: c, path: path}
}

// stream consumes text as one stream; final says the step had ended.
func (tf *testFollower) stream(text string, final bool) {
	tf.t.Helper()
	if err := tf.consume(strings.NewReader(text), final); err != nil {
		tf.t.Fatal(err)
	}
}

// failing consumes text as a stream that then fails with err.
func (tf *testFollower) failing(text string, err error, final bool) {
	tf.t.Helper()
	if got := tf.consume(io.MultiReader(strings.NewReader(text), errReader{err}), final); got == nil {
		tf.t.Fatal("a stream that failed answered no error")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// archive closes the capture and reads its archive, every piece of a cut line
// joined back: one string the step's lines can be counted in.
func (tf *testFollower) archive() string {
	tf.t.Helper()
	if _, err := tf.capture.Close(); err != nil {
		tf.t.Fatal(err)
	}
	b, err := os.ReadFile(tf.path)
	if err != nil {
		tf.t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\n", "")
}

// wantEachOnce checks every line's text is in the archive exactly once;
// atLeast relaxes that to at least once. A line carrying the generator's
// <i: ... :i> markers has each marker counted too: a duplicated PREFIX of a
// long line followed by a whole copy leaves the whole text once and its
// start marker twice.
func wantEachOnce(t *testing.T, archive string, log nodeLog, atLeast bool) {
	t.Helper()
	for _, line := range log {
		_, text, _ := captureSplitStamp(line)
		marks := []string{text}
		if m := lineMarker.FindStringSubmatch(text); m != nil && strings.HasSuffix(text, ":"+m[1]+">") {
			marks = append(marks, "<"+m[1]+":", ":"+m[1]+">")
		}
		for _, mark := range marks {
			switch n := strings.Count(archive, mark); {
			case n == 0:
				t.Errorf("lost %.60q (of %.60q)", mark, text)
			case n > 1 && !atLeast:
				t.Errorf("%.60q (of %.60q) is in the archive %d times", mark, text, n)
			}
		}
	}
}

// lineMarker finds the generator's start marker, <i:, in a line's text.
var lineMarker = regexp.MustCompile(`<(\d+):`)

// TestFollowerFeedsEveryLineOnceAcrossCuts is the rule, randomized: logs of
// short lines and long ones (some exactly a multiple of the piece size, so
// they end on an empty piece), stamps out of order by up to 900 ms, written
// while the step runs; streams cut at random bytes -- inside stamps, lines,
// pieces -- or failing with an error, any number of times, the last stream
// read to the end once the step has ended. Each line is archived once.
func TestFollowerFeedsEveryLineOnceAcrossCuts(t *testing.T) {
	const lineMax, logs = 64, 400
	rng := rand.New(rand.NewSource(20261004))
	base := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	for n := 0; n < logs; n++ {
		var log nodeLog
		at := base
		for i := 0; i < 2+rng.Intn(14); i++ {
			at = at.Add(time.Duration(rng.Intn(700)) * time.Millisecond)
			stamp := at
			if rng.Intn(3) == 0 {
				stamp = at.Add(-time.Duration(rng.Intn(900)) * time.Millisecond) // stderr, stamped apart
			}
			prefix := stamp.Format(time.RFC3339Nano) + " "
			text := fmt.Sprintf("<%d:", i)
			switch rng.Intn(4) {
			case 0: // ends exactly at a piece boundary
				for k := 1 + rng.Intn(3); len(prefix)+len(text)+len(fmt.Sprintf(":%d>", i)) != k*lineMax; {
					text += "q"
					if len(prefix)+len(text) > k*lineMax {
						k++
					}
				}
			case 1: // long
				text += strings.Repeat("p", rng.Intn(5*lineMax))
			default:
				text += strings.Repeat("s", rng.Intn(20))
			}
			log = append(log, prefix+text+fmt.Sprintf(":%d>", i))
		}

		tf := newTestFollower(t, lineMax)
		written := 0
		for cuts := 0; cuts < 1+rng.Intn(6); cuts++ {
			written = min(len(log), written+rng.Intn(len(log)+1))
			served := log[:written].from(tf.reopenFrom())
			cut := rng.Intn(len(served) + 1)
			if rng.Intn(4) == 0 {
				tf.failing(served[:cut], io.ErrUnexpectedEOF, false)
			} else {
				tf.stream(served[:cut], false)
			}
		}
		tf.stream(log.from(tf.reopenFrom()), true)

		if t.Failed() {
			break
		}
		wantEachOnce(t, tf.archive(), log, false)
		if t.Failed() {
			t.Logf("log %d:\n  %s", n, strings.Join(log, "\n  "))
			break
		}
	}
}

// TestFollowerReplaysWhatACutLeft names each place the rule was broken
// (re-reviews of fix round 2); each fails on the follower before fix round 3.
func TestFollowerReplaysWhatACutLeft(t *testing.T) {
	at := func(ms int) string { return rtAt(ms).Format(time.RFC3339Nano) + " " }

	t.Run("a stream cut inside a line's stamp, the line stamped a second before the last fed", func(t *testing.T) {
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(1100) + "[a]", at(2100) + "[b]", at(1500) + "[c, stamped before b and written after it]"}
		tf.stream(log.from(time.Time{})[:len(log[0])+len(log[1])+2+12], false)
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, false)
	})

	t.Run("a line written after a stream ended, stamped a second before the last fed", func(t *testing.T) {
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(1100) + "[a]", at(2100) + "[b]", at(1500) + "[c, stamped before b and written after the stream ended]"}
		tf.stream(log[:2].from(time.Time{}), false)
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, false)
	})

	t.Run("a held line overwritten by a later cut", func(t *testing.T) {
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(2100) + "[a]", at(2200) + "[b]", at(1500) + "[c, stamped before a and b, written after them]"}
		all := log.from(time.Time{})
		tf.stream(all[:len(all)-6], false) // cut inside c
		again := log.from(tf.reopenFrom())
		tf.stream(again[:strings.Index(again, "[b]\n")], false) // cut inside b, a repeat, before c
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, false)
	})

	t.Run("a held line kept when a later stream is cut, however far back it is stamped", func(t *testing.T) {
		// c is stamped 1.7 s before b, past what replaySkew covers, so only
		// its own cut entry brings the next stream back to it.
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(2100) + "[a]", at(2200) + "[b]", at(500) + "[c, stamped long before a and b, written after them]"}
		all := log.from(time.Time{})
		tf.stream(all[:len(all)-6], false) // cut inside c
		again := log.from(tf.reopenFrom())
		tf.stream(again[:strings.Index(again, "[b]\n")], false) // cut inside b, a repeat, before c
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, false)
	})

	t.Run("a long line cut twice, the second time before what the first fed", func(t *testing.T) {
		tf := newTestFollower(t, 64)
		log := nodeLog{at(1100) + strings.Repeat("p", 3*64)}
		line := log.from(time.Time{})
		tf.stream(line[:2*64+32], false)
		tf.stream(log.from(tf.reopenFrom())[:64+32], false)
		tf.stream(log.from(tf.reopenFrom()), true)
		if n := strings.Count(tf.archive(), "p"); n != 3*64 {
			t.Errorf("the archive holds %d of the line's %d bytes", n, 3*64)
		}
	})

	t.Run("a long line that ends on an empty piece, replayed twice after a cut", func(t *testing.T) {
		tf := newTestFollower(t, 64)
		stamp := at(1100)
		log := nodeLog{stamp + strings.Repeat("q", 2*64-len(stamp)), at(1200) + "[after]"}
		tf.stream(log.from(time.Time{})[:64+32], false)
		tf.stream(log.from(tf.reopenFrom()), false)
		tf.stream(log.from(tf.reopenFrom()), true)
		archive := tf.archive()
		if n := strings.Count(archive, "q"); n != 2*64-len(stamp) {
			t.Errorf("the archive holds %d of the line's %d bytes", n, 2*64-len(stamp))
		}
		wantEachOnce(t, archive, log[1:], false)
	})

	t.Run("a held line that replays as a repeat pins no later stream", func(t *testing.T) {
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(1100) + "[a]", at(2100) + "[b]", at(2200) + "[c]"}
		tf.stream(log.from(time.Time{}), false)
		again := log.from(tf.reopenFrom())
		tf.stream(again[:strings.Index(again, "[b]\n")], false) // cut inside b, already fed
		again = log.from(tf.reopenFrom())
		tf.stream(again[:strings.Index(again, "[b]\n")+4], false) // b served whole, a repeat
		if len(tf.cut) != 0 {
			t.Errorf("b was served whole and is still cut: %v", tf.cut)
		}
		for i := 0; i < 300; i++ {
			log = append(log, at(3000+i*1000)+fmt.Sprintf("line <%d>", i))
		}
		tf.stream(log.from(tf.reopenFrom()), false)
		if from := tf.reopenFrom(); from.Before(rtAt(3000 + 298*1000)) {
			t.Errorf("the next stream opens at %s, pinned by the repeat cut long ago", from.Format(time.RFC3339Nano))
		}
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, false)
	})

	t.Run("a cut line the node no longer serves pins no later stream", func(t *testing.T) {
		// Cut inside c; then the node rotates its log, and serves only what
		// was written after c. c is gone, and must not hold every later
		// stream back at its second.
		tf := newTestFollower(t, followLineMax)
		log := nodeLog{at(2100) + "[a]", at(2200) + "[b]", at(1500) + "[c, stamped before a and b, written after them]"}
		all := log.from(time.Time{})
		tf.stream(all[:len(all)-6], false)
		var rotated nodeLog
		for i := 0; i < 300; i++ {
			rotated = append(rotated, at(3000+i*1000)+fmt.Sprintf("[line %d]", i))
		}
		tf.stream(rotated.from(tf.reopenFrom()), false)
		if from := tf.reopenFrom(); from.Before(rtAt(3000 + 298*1000)) {
			t.Errorf("the next stream opens at %s, pinned by a line the node no longer has", from.Format(time.RFC3339Nano))
		}
		tf.stream(rotated.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), append(log[:2:2], rotated...), false)
	})

	t.Run("the last stream fails inside a long line's continuation", func(t *testing.T) {
		tf := newTestFollower(t, 64)
		log := nodeLog{at(1100) + "<" + strings.Repeat("w", 4*64) + ">", at(1200) + "[after]"}
		all := log.from(time.Time{})
		tf.failing(all[:2*64+16], io.ErrUnexpectedEOF, true)
		tf.stream(log.from(tf.reopenFrom()), true)
		archive := tf.archive()
		if n := strings.Count(archive, "w"); n != 4*64 {
			t.Errorf("the archive holds %d of the line's %d bytes: its rest was judged a repeat", n, 4*64)
		}
		wantEachOnce(t, archive, log[1:], false)
	})

	t.Run("a held line stamped seconds before the last fed: duplicates, never a loss", func(t *testing.T) {
		tf := newTestFollower(t, followLineMax)
		var log nodeLog
		for _, ms := range []int{1100, 2100, 3100, 4100, 5100} {
			log = append(log, at(ms)+fmt.Sprintf("[at %d]", ms))
		}
		log = append(log, at(1500)+"[x, stamped 3.6 s before the last line fed]")
		all := log.from(time.Time{})
		tf.stream(all[:len(all)-4], false)
		tf.stream(log.from(tf.reopenFrom()), true)
		wantEachOnce(t, tf.archive(), log, true)
	})
}
