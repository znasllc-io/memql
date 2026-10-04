package pipelinesteps

import (
	"bufio"
	"context"
	"errors"
	"hash/crc64"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// follow.go -- the step's log, followed into its capture: the follower, the
// line reader and the cursor (split out of runner.go).

// ---------------------------------------------------------------------------
// Following the log
// ---------------------------------------------------------------------------

// follower follows the step container's log into the capture, opening it
// again whenever a stream ends while the step runs.
//
// What it keeps of a stream (review finding 2, ruling R36). stdout and stderr
// are stamped by goroutines of their own, so the log file holds lines out of
// stamp order, and a stream is opened again at a whole second (Kube.FollowLog),
// so it replays that second:
//
//   - Within one stream a line is dropped only as a repeat of one this Run
//     already fed -- judged against where the stream was OPENED from, never
//     against the lines just before it.
//   - A stream opened again drops a replayed line only when its fingerprint
//     (stamp and text) is one this Run fed in that second; any other line of
//     the second is output it never saw.
//   - A line a stream ends inside is held back: the stream opened next
//     replays it whole. Once the step has ended, a last line with no newline
//     is all there is, and is fed.
//   - A Run that ADOPTED the step follows its log from the start. The store
//     has every line up to the adoption cursor, so those reach the archive
//     only, and the store is fed from the cursor on, behind one notice at the
//     seam. The blind spot: a line the previous holder never captured,
//     written after the cursor's line but stamped before it, reaches the
//     archive only.
type follower struct {
	s       *step
	capture *Capture
	pod     string
	cancel  context.CancelFunc
	drainC  chan struct{}
	once    sync.Once
	done    chan struct{}
	// noted are the stream's own sentences already archived.
	noted map[string]bool

	// adopted is the cursor the Run adopted the step at; zero for a step it
	// started, or one whose holder had captured nothing.
	adopted time.Time
	// fed is the latest stamp this Run fed, to the store or the archive: a
	// stream is opened again from its second.
	fed time.Time
	// seen fingerprints the lines fed in seenSecond, fed's second.
	seenSecond time.Time
	seen       map[linePrint]struct{}
}

// follow starts following the pod's step container. draining starts it
// already reading to the end: the step has ended.
func (s *step) follow(pod string, draining bool) *follower {
	// Not s.ctx: a decided step's log is read to its end whatever a late
	// cancel says. The watch stops a follower itself on a cancel.
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	f := &follower{
		s: s, capture: s.capture, pod: pod, cancel: cancel,
		drainC: make(chan struct{}), done: make(chan struct{}), noted: map[string]bool{},
		adopted: s.adoptedAt, seen: map[linePrint]struct{}{},
	}
	if draining {
		f.once.Do(func() { close(f.drainC) })
	}
	go f.run(ctx)
	return f
}

func (f *follower) run(ctx context.Context) {
	defer close(f.done)
	for opened := false; ctx.Err() == nil; opened = true {
		// A stream opened once the step has ended holds its log to the end.
		final := f.draining()
		// A stream that ended while the step ran -- a dropped connection, or
		// this replica cut off from the API server -- is opened again only
		// while this runner still holds the claim. One cut off long enough
		// to lose it would otherwise pour everything the step printed since
		// its own cursor into the store beside the adopter's copy, in the
		// moment before its next poll sees the claim gone (measured on k3s).
		if opened && !final && !f.s.holdsClaim(ctx) {
			if !f.pause(ctx, final) {
				return
			}
			continue
		}
		// From the second of the last line fed; from the start before any --
		// for an adopter too, whose archive is made whole (ruling R36).
		rc, err := f.s.r.kube.FollowLog(ctx, f.pod, ContainerStep, f.fed)
		if err == nil {
			err = f.consume(rc, final)
			_ = rc.Close()
		}
		if ctx.Err() != nil {
			return
		}
		ended := err == nil || errors.Is(err, ErrContainerNotStarted) || deploycontrol.IsNotFound(err)
		if final && ended {
			return
		}
		if !ended {
			f.s.log.Warn("pipelines: reading the step's log", "error", err)
		}
		if !f.pause(ctx, final) {
			return
		}
	}
}

// pause waits before the next stream: a poll interval, or only until the step
// is decided -- unless it already was, when what failed was the final stream
// and the next waits the interval.
func (f *follower) pause(ctx context.Context, final bool) bool {
	select {
	case <-ctx.Done():
		return false
	case <-f.drainC:
		return !final || sleepCtx(ctx, f.s.r.cfg.PollInterval)
	case <-time.After(f.s.r.cfg.PollInterval):
		return true
	}
}

func (f *follower) draining() bool {
	select {
	case <-f.drainC:
		return true
	default:
		return false
	}
}

// drain reads the log to its end, waiting at most timeout, and stops.
func (f *follower) drain(timeout time.Duration) {
	f.once.Do(func() { close(f.drainC) })
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.done:
	case <-t.C:
		f.s.log.Warn("pipelines: the step's log did not end in time; its archive may stop early")
	}
	f.stop()
}

func (f *follower) stop() {
	f.cancel()
	<-f.done
}

// noteStream archives, once, a sentence the kubelet or the API server wrote
// into the stream: a stream opened again repeats it, having no timestamp to
// be dropped by.
func (f *follower) noteStream(text string) {
	if f.noted[text] || len(f.noted) >= maxStreamNotes {
		return
	}
	f.noted[text] = true
	f.capture.Note("memql: the log stream reported: " + text)
}

// lineFate is what becomes of one line of a stream.
type lineFate int

const (
	fateStore   lineFate = iota // output: the store, the archive, the tail
	fateArchive                 // output the store has already: the archive and the tail
	fateRepeat                  // fed already by this Run: dropped
	fateStream                  // the kubelet's words, not the step's: one note
)

// consume feeds one stream to the capture, line by line (the rules on
// follower). final says the step had ended when the stream was opened, so a
// last line with no newline is all there is.
func (f *follower) consume(r io.Reader, final bool) error {
	lines := newLogLines(r, followLineMax, captureMaskForms(f.s.secrets))
	opened := f.fed
	var (
		fate  lineFate
		stamp time.Time // the line's timestamp, for its continuation pieces
		print linePrint
	)
	for {
		piece, first, end, err := lines.next()
		if err != nil && !end && !final {
			// The stream ended inside a line: the next one replays it whole.
			piece = ""
		}
		if first && (piece != "" || end) {
			fate, stamp, print = f.judge(piece, opened)
		}
		if piece != "" {
			f.feed(fate, stamp, piece, first)
		}
		if piece != "" && (end || err != nil) {
			f.fedLine(fate, stamp, print)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// linePrint tells the lines of one second of a step's log apart: a line's
// stamp to the nanosecond, its length and a checksum of it -- the first piece
// of a long line. It lives in memory for one second of output and never
// becomes an id or leaves the process, so it is a fast checksum rather than a
// core/id content address, which costs a SHA-256 per byte (1 ms a 4 KiB line,
// measured) on a path every line takes.
type linePrint struct {
	at  int64
	n   int
	sum uint64
}

var linePrintTable = crc64.MakeTable(crc64.ISO)

// judge decides what becomes of a line from its first piece. opened is where
// the stream was opened from: only a line at or before it can be a repeat.
func (f *follower) judge(piece string, opened time.Time) (lineFate, time.Time, linePrint) {
	at, _, ok := captureSplitStamp(piece)
	if !ok {
		// Followed with timestamps, every line the step writes comes stamped:
		// an unstamped one is the kubelet or the API server speaking in the
		// stream (measured on k3s v1.32, following a pod as it is deleted:
		// "failed to try resolving symlinks in path .../step/0.log ...").
		return fateStream, time.Time{}, linePrint{}
	}
	print := linePrint{at: at.UnixNano(), n: len(piece), sum: crc64.Checksum([]byte(piece), linePrintTable)}
	switch _, seen := f.seen[print]; {
	case !opened.IsZero() && !at.After(opened) && seen:
		return fateRepeat, at, print
	case !f.adopted.IsZero() && !at.After(f.adopted):
		return fateArchive, at, print
	}
	return fateStore, at, print
}

// feed hands one piece of a line to the capture as its fate says. A
// continuation piece of a long line is stamped like the line's start.
func (f *follower) feed(fate lineFate, stamp time.Time, piece string, first bool) {
	raw := piece
	if !first {
		raw = stamp.Format(time.RFC3339Nano) + " " + piece
	}
	switch fate {
	case fateStream:
		if first {
			f.noteStream(piece)
		}
	case fateArchive:
		f.s.replayedHead = true
		f.capture.FeedArchived(raw)
	case fateStore:
		if first && !f.adopted.IsZero() {
			// The seam: the first line after the cursor.
			f.s.noteReattach(!f.s.replayedHead)
		}
		f.capture.Feed(raw)
	}
}

// fedLine records a line fed whole: its fingerprint, for the second it is in,
// and how far the Run has fed -- and, for a stored line, the cursor the
// heartbeat publishes, which only ever moves forward.
func (f *follower) fedLine(fate lineFate, at time.Time, print linePrint) {
	if fate == fateRepeat || fate == fateStream {
		return
	}
	switch second := at.Truncate(time.Second); {
	case second.After(f.seenSecond):
		f.seenSecond = second
		f.seen = map[linePrint]struct{}{print: {}}
	case second.Equal(f.seenSecond):
		f.seen[print] = struct{}{}
	}
	if at.After(f.fed) {
		f.fed = at
	}
	if fate == fateStore {
		f.s.publishCursor(at)
	}
}

// logLines reads a stream as lines without holding more than max bytes of any
// one: a longer line comes in pieces of at most max bytes, each cut where a
// rune starts -- and never inside a secret (review finding 4). Each piece is
// masked on its own, so a secret a cut fell inside would be masked in neither:
// a piece that ends with the beginning of one of secrets is cut before it,
// and the secret goes whole into the next piece (the cockpit's chunker holds
// back the same way).
type logLines struct {
	br      *bufio.Reader
	max     int
	secrets []string // the capture's mask forms
	carry   []byte
	mid     bool // the last piece ended inside its line
}

func newLogLines(r io.Reader, max int, secrets []string) *logLines {
	return &logLines{br: bufio.NewReaderSize(r, max), max: max, secrets: secrets}
}

// next is the next piece of the stream, without its newline. first says it
// begins a line rather than continuing one, end that it ends it: a newline
// followed. A stream that ends inside a line answers what it had of the line
// with end false and the stream's error.
func (l *logLines) next() (piece string, first, end bool, err error) {
	first = !l.mid
	chunk, err := l.br.ReadSlice('\n')
	buf := append(l.carry, chunk...)
	l.carry = nil
	switch {
	case err == nil:
		l.mid = false
		buf = buf[:len(buf)-1]
		if n := len(buf); n > 0 && buf[n-1] == '\r' {
			buf = buf[:n-1]
		}
		return string(buf), first, true, nil
	case errors.Is(err, bufio.ErrBufferFull):
		cut := holdBack(buf, runeCut(buf, l.max), l.secrets)
		l.carry = append([]byte(nil), buf[cut:]...)
		l.mid = true
		return string(buf[:cut]), first, false, nil
	default:
		l.mid = false
		return string(buf), first, false, err
	}
}

// holdBack moves a cut in b back to before the earliest place where what
// precedes the cut is the beginning -- a proper prefix -- of a secret, so no
// secret straddles it. A secret starts where a rune does, so the cut stays on
// a rune boundary. It never moves the cut to the start: a piece holds
// something.
func holdBack(b []byte, cut int, secrets []string) int {
	longest := 0
	for _, s := range secrets {
		longest = max(longest, len(s))
	}
	for k := max(1, cut-longest+1); k < cut; k++ {
		tail := b[k:cut]
		for _, s := range secrets {
			if len(tail) < len(s) && strings.HasPrefix(s, string(tail)) {
				return k
			}
		}
	}
	return cut
}

// runeCut is where a piece of b of at most max bytes ends on a rune boundary:
// at max, moved back to where a rune starts when it falls inside one -- or,
// when b is no longer than max, at its end, less an incomplete last rune.
func runeCut(b []byte, max int) int {
	if len(b) > max {
		for c := max; c > 0 && c > max-utf8.UTFMax; c-- {
			if utf8.RuneStart(b[c]) {
				return c
			}
		}
		return max
	}
	for c := len(b) - 1; c >= 0 && c > len(b)-1-utf8.UTFMax; c-- {
		if utf8.RuneStart(b[c]) {
			if utf8.FullRune(b[c:]) {
				return len(b)
			}
			return c
		}
	}
	return len(b)
}

// publishCursor moves the cursor the heartbeat publishes to at, never back.
func (s *step) publishCursor(at time.Time) {
	s.mu.Lock()
	if at.After(s.cursor) {
		s.cursor = at
	}
	s.mu.Unlock()
}

func (s *step) getCursor() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}
