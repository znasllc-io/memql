package pipelinesteps

import (
	"bufio"
	"context"
	"errors"
	"io"
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
// again from the cursor whenever a stream ends while the step runs.
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
		rc, err := f.s.r.kube.FollowLog(ctx, f.pod, ContainerStep, f.s.getCursor())
		if err == nil {
			err = f.consume(rc)
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

// consume feeds one stream to the capture, line by line. A resumed stream
// starts at the cursor's whole second (Kube.FollowLog), so the lines stamped
// at or before the cursor are the store's already and are dropped; every line
// fed moves the cursor.
func (f *follower) consume(r io.Reader) error {
	lines := newLogLines(r, followLineMax)
	cursor := f.s.getCursor()
	var (
		stamp time.Time // the line's timestamp, for its continuation pieces
		skip  bool      // the line is not the step's to capture
	)
	for {
		piece, first, err := lines.next()
		switch {
		case piece == "" && (err != nil || !first):
			// Nothing after the last newline, or after a long line's last cut.
		case first:
			at, _, ok := captureSplitStamp(piece)
			switch {
			case !ok:
				// Followed with timestamps, every line the step writes comes
				// stamped: an unstamped one is the kubelet or the API server
				// speaking in the stream (measured on k3s v1.32, following a
				// pod as it is deleted: "failed to try resolving symlinks in
				// path .../step/0.log ..."). Archived as that, never as the
				// step's own words.
				skip = true
				f.noteStream(piece)
			case !cursor.IsZero() && !at.After(cursor):
				skip = true
			default:
				skip, stamp, cursor = false, at, at
				f.capture.Feed(piece)
				f.s.setCursor(at)
			}
		case !skip:
			// The rest of a long line, stamped like its start.
			f.capture.Feed(stamp.Format(time.RFC3339Nano) + " " + piece)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// logLines reads a stream as lines without holding more than max bytes of any
// one: a longer line comes in pieces of at most max bytes, each cut where a
// rune starts.
type logLines struct {
	br    *bufio.Reader
	max   int
	carry []byte
	mid   bool // the last piece ended inside its line
}

func newLogLines(r io.Reader, max int) *logLines {
	return &logLines{br: bufio.NewReaderSize(r, max), max: max}
}

// next is the next piece of the stream, without its newline. first says it
// begins a line rather than continuing one. A stream that ends inside a line
// answers that line's last piece with the stream's error.
func (l *logLines) next() (piece string, first bool, err error) {
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
		return string(buf), first, nil
	case errors.Is(err, bufio.ErrBufferFull):
		cut := runeCut(buf, l.max)
		l.carry = append([]byte(nil), buf[cut:]...)
		l.mid = true
		return string(buf[:cut]), first, nil
	default:
		l.mid = false
		return string(buf), first, err
	}
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

func (s *step) setCursor(at time.Time) {
	s.mu.Lock()
	s.cursor = at
	s.mu.Unlock()
}

func (s *step) getCursor() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}
