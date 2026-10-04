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
// THE RULE (fix round 3): a line the step wrote reaches the archive exactly
// once -- never lost, never twice -- across any number of stream cuts,
// wherever a cut falls (inside a stamp, inside a line, inside a long line's
// pieces, at its last empty piece) and whatever the stamp order, as far apart
// as stdout and stderr stamp (replaySkew). The kubelet serves a stream in the
// order lines were written, from the second it is opened at; stdout and
// stderr are stamped by goroutines of their own, so the order of the stamps
// is not the order of the lines.
//
// One ledger keeps it, simple enough to see that it does:
//
//   - lines: every line fed any of, by fingerprint, with how many of its
//     bytes were fed and whether that was all. A line a stream serves is fed
//     from where the ledger says it stopped -- not at all when whole. A line
//     is whole at its newline, or, once the step has ended, at the clean end
//     of its log; any other end of a stream is a cut.
//   - cut: the stamp of each line a stream ended inside before it was fed
//     whole, with the latest stamp fed whole when it was cut. A line stays
//     cut until it is fed whole (or served as a repeat) -- or until a line
//     stamped later than that is, which the kubelet serves only after the
//     cut line: one still cut then is gone from the node (its log rotated).
//   - A stream opens again at the earliest cut line, or replaySkew before the
//     second of the latest line fed whole -- a line written after a stream
//     ended, or cut inside its stamp, may be stamped that much before it --
//     whichever is earlier. The ledger keeps every line from that second on,
//     so all a stream replays is known. A line stamped further back than
//     replaySkew is fed again rather than lost.
//
// A Run that ADOPTED the step follows its log from the start. The store has
// every line up to the adoption cursor, so those reach the archive only, and
// the store is fed from the cursor on, behind one notice at the seam. The
// blind spot: a line the previous holder never captured, written after the
// cursor's line but stamped before it, reaches the archive only.
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
	// lineMax is the longest piece of a line it holds: followLineMax.
	lineMax int

	// adopted is the cursor the Run adopted the step at; zero for a step it
	// started, or one whose holder had captured nothing.
	adopted time.Time
	// The ledger (THE RULE above): lines fed, by fingerprint, from floor's
	// second on; fed, the latest stamp of a line fed whole; cut, each line a
	// stream ended inside, by stamp, with fed as it stood then.
	lines map[linePrint]fedLine
	fed   time.Time
	cut   map[int64]time.Time
	floor time.Time
	// trouble is what the follower last logged of the errors its streams
	// ended on.
	trouble apiTrouble
}

// replaySkew is how far before the latest line fed whole a line written after
// it can be stamped and still be fed exactly once: a stream opens again that
// far back, and the ledger keeps that far.
const replaySkew = time.Second

// fedLine is what the ledger knows of one line: how many of its bytes, as the
// reader gives them out, were fed, and whether that was all of it.
type fedLine struct {
	n     int
	whole bool
}

// follow starts following the pod's step container. draining starts it
// already reading to the end: the step has ended.
func (s *step) follow(pod string, draining bool) *follower {
	// Not s.ctx: a decided step's log is read to its end whatever a late
	// cancel says. The watch stops a follower itself on a cancel.
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	f := newFollower(s, pod)
	f.cancel = cancel
	if draining {
		f.once.Do(func() { close(f.drainC) })
	}
	go f.run(ctx)
	return f
}

// newFollower is a follower of the pod's step container into the step's
// capture, not started.
func newFollower(s *step, pod string) *follower {
	return &follower{
		s: s, capture: s.capture, pod: pod,
		drainC: make(chan struct{}), done: make(chan struct{}), noted: map[string]bool{}, lineMax: followLineMax,
		adopted: s.adoptedAt, lines: map[linePrint]fedLine{}, cut: map[int64]time.Time{}, trouble: apiTrouble{},
	}
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
		// From the second of reopenFrom; from the start before any line was
		// fed whole -- for an adopter too, whose archive is made whole
		// (ruling R36).
		rc, err := f.s.r.kube.FollowLog(ctx, f.pod, ContainerStep, f.reopenFrom())
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
			f.trouble.warn(f.s, "reading the step's log", err)
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

// consume feeds one stream to the capture, line by line, keeping the ledger
// (THE RULE on follower). final says the step had ended when the stream was
// opened, so its clean end is the log's: a last line with no newline is all
// there is of it.
func (f *follower) consume(r io.Reader, final bool) error {
	lines := newLogLines(r, f.lineMax, captureMaskForms(f.s.maskSecrets()))
	var (
		in    bool // inside a line whose first piece was read whole
		fate  lineFate
		stamp time.Time // the line's timestamp, for its continuation pieces
		print linePrint
		given int // bytes of the line given out by this stream so far
		done  int // bytes of it the ledger says were fed before
	)
	for {
		piece, first, end, err := lines.next()
		whole := end || (final && errors.Is(err, io.EOF))
		cut := err != nil && !whole
		if first {
			in = false
			switch {
			case cut:
				f.cutBefore(piece)
			case piece != "" || end:
				fate, stamp, print = f.judge(piece)
				in, given, done = true, 0, f.lines[print].n
			}
		}
		if in {
			if cut {
				f.cutInside(fate, stamp, print, given)
			} else {
				f.feedPart(fate, stamp, piece, first, given, done)
				given += len(piece)
				if whole {
					f.ended(fate, stamp, print)
					in = false
				}
			}
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

// judge decides what becomes of a line from its first piece: a repeat when the
// ledger has all of it.
func (f *follower) judge(piece string) (lineFate, time.Time, linePrint) {
	at, _, ok := captureSplitStamp(piece)
	if !ok {
		// Followed with timestamps, every line the step writes comes stamped:
		// an unstamped one is the kubelet or the API server speaking in the
		// stream (measured on k3s v1.32, following a pod as it is deleted:
		// "failed to try resolving symlinks in path .../step/0.log ...").
		return fateStream, time.Time{}, linePrint{}
	}
	print := linePrint{at: at.UnixNano(), n: len(piece), sum: crc64.Checksum([]byte(piece), linePrintTable)}
	switch {
	case f.lines[print].whole:
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
		if !f.s.replayedHead {
			f.s.replayedHead, f.s.firstReplayed = true, stamp
		}
		f.capture.FeedArchived(raw)
	case fateStore:
		if first && !f.adopted.IsZero() {
			// The seam: the first line after the cursor.
			f.s.noteReattach()
		}
		f.capture.Feed(raw)
	}
}

// feedPart feeds the bytes of a line a piece holds -- given out of the
// line before it, done fed by an earlier stream -- that were not fed yet.
func (f *follower) feedPart(fate lineFate, stamp time.Time, piece string, first bool, given, done int) {
	switch {
	case piece == "" || given+len(piece) <= done:
	case given < done:
		// A piece cut where the earlier stream did not cut it (the reader
		// cuts the same bytes the same way, so this is a guard): the part
		// not fed yet goes as the line's continuation.
		f.feed(fate, stamp, piece[done-given:], false)
	default:
		f.feed(fate, stamp, piece, first)
	}
}

// ended records a line fed whole -- or served whole as a repeat: no longer
// cut; the latest stamp fed whole, and any cut line it shows gone; and, for a
// stored line, the cursor the heartbeat publishes, which only moves forward.
func (f *follower) ended(fate lineFate, at time.Time, print linePrint) {
	if fate == fateStream {
		return
	}
	delete(f.cut, at.UnixNano())
	f.lines[print] = fedLine{whole: true}
	if at.After(f.fed) {
		f.fed = at
		for c, fedThen := range f.cut {
			if at.After(fedThen) {
				delete(f.cut, c) // served before this line, and not served: gone
			}
		}
	}
	f.prune()
	if fate == fateStore {
		f.s.publishCursor(at)
		f.s.noteFirstStored(at)
	}
}

// cutBefore records a line a stream ended inside its first piece: by its
// stamp, when the part read holds the stamp whole. Cut inside its stamp, it
// is replayed all the same: a stream opens replaySkew before the latest line
// fed whole, and a line written after it is stamped no further back.
func (f *follower) cutBefore(part string) {
	stamp, _, found := strings.Cut(part, " ")
	if !found {
		return
	}
	if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		f.cut[at.UnixNano()] = f.fed
	}
}

// cutInside records a line a stream ended inside after giving out given of
// its bytes: the ledger keeps the most of it any stream fed.
func (f *follower) cutInside(fate lineFate, at time.Time, print linePrint, given int) {
	if fate != fateStore && fate != fateArchive {
		return // a repeat is in the ledger whole; the stream's words are no line
	}
	if l := f.lines[print]; !l.whole && given > l.n {
		f.lines[print] = fedLine{n: given}
	}
	f.cut[at.UnixNano()] = f.fed
}

// reopenFrom is where the next stream opens: the log's start before any line
// was fed whole; else the earliest cut line, or replaySkew before the second
// of the latest line fed whole, whichever is earlier (Kube.FollowLog opens at
// its whole second).
func (f *follower) reopenFrom() time.Time {
	if f.fed.IsZero() {
		return time.Time{}
	}
	from := f.fed.Truncate(time.Second).Add(-replaySkew)
	for at := range f.cut {
		if c := time.Unix(0, at).UTC(); c.Before(from) {
			from = c
		}
	}
	return from
}

// prune forgets the lines of seconds no stream opened from now on can replay.
func (f *follower) prune() {
	from := f.reopenFrom()
	if from.IsZero() {
		return
	}
	floor := from.Truncate(time.Second)
	if !floor.After(f.floor) {
		return
	}
	f.floor = floor
	for p := range f.lines {
		if p.at < floor.UnixNano() {
			delete(f.lines, p)
		}
	}
}

// logLines reads a stream as lines without holding more than about max bytes
// of any one: a longer line comes in pieces of at most max bytes (Review
// Focus 5), and no piece ends inside a secret.
//
// MASK FIRST, THEN CUT (fix round 2, Important A). The capture masks each
// piece on its own, so a secret a cut fell inside would be masked in neither
// piece. A piece of a cut line is therefore repaired and masked HERE, as the
// capture repairs and masks a whole line -- every whole secret replaced --
// and it ends before the first position whose masking is not decided yet: a
// secret that may continue into bytes not read yet, or a rune cut short.
// Those bytes go back to be read with the next chunk. With every whole secret
// masked before a cut is chosen, and every undecided one held back, a cut can
// split no secret, and the pieces mask to exactly what the whole line masks
// to. A line that fits in one read is left whole, to the capture.
type logLines struct {
	br     *bufio.Reader
	max    int
	masker *lineMasker
	// carry is what was read of the line and not yet given out: repaired
	// text, then the raw bytes of a rune cut short.
	carry []byte
	ended bool // carry is the rest of a line whose newline was read
	mid   bool // the last piece ended inside its line
}

func newLogLines(r io.Reader, max int, forms []string) *logLines {
	return &logLines{br: bufio.NewReaderSize(r, max), max: max, masker: newLineMasker(forms)}
}

// next is the next piece of the stream, without its newline. first says it
// begins a line rather than continuing one, end that it ends it: a newline
// followed. A stream that ends inside a line answers what it had of the line
// with end false and the stream's error.
func (l *logLines) next() (piece string, first, end bool, err error) {
	first = !l.mid
	for {
		if l.ended {
			if len(l.carry) <= l.max {
				piece = string(l.carry)
				l.carry, l.ended, l.mid = nil, false, false
				return piece, first, true, nil
			}
			piece, l.carry = l.cut(l.carry, true)
			l.mid = true
			return piece, first, false, nil
		}
		chunk, rerr := l.br.ReadSlice('\n')
		buf := append(l.carry, chunk...)
		l.carry = nil
		switch {
		case rerr == nil:
			buf = buf[:len(buf)-1]
			if n := len(buf); n > 0 && buf[n-1] == '\r' {
				buf = buf[:n-1]
			}
			if !l.mid && len(buf) <= l.max {
				// The whole line in one read: the capture masks it.
				return string(buf), first, true, nil
			}
			l.carry, l.ended = buf, true
		case errors.Is(rerr, bufio.ErrBufferFull):
			piece, l.carry = l.cut(buf, false)
			if piece == "" {
				continue // nothing of it is decided yet: read on
			}
			l.mid = true
			return piece, first, false, nil
		default:
			l.mid = false
			return string(buf), first, false, rerr
		}
	}
}

// cut is the next piece of b, its line's bytes from where the last piece
// ended, and what is left of b for the next: at most max bytes, repaired and
// masked, ending before the first undecided position. complete says nothing
// follows b in its line. The line's timestamp, at the start of its first
// piece, is never masked: the capture splits it off before it masks.
func (l *logLines) cut(b []byte, complete bool) (piece string, rest []byte) {
	prefix := 0
	if !l.mid {
		prefix = stampPrefix(b)
	}
	body, short := b[prefix:], []byte(nil)
	if !complete {
		n := wholeRunes(body)
		body, short = body[:n], body[n:]
	}
	// Repaired as the capture repairs a whole line. Each piece is repaired
	// apart, so a cut inside a run of two or more invalid bytes repairs to
	// two U+FFFD where the whole line has one -- and a secret holding such a
	// run would mask differently. No secret can: they cross to the runner as
	// JSON strings, which are valid UTF-8 (accepted in fix round 3).
	text := captureRepair(string(body))
	out, used := l.masker.scan(text, l.max-prefix, complete)
	if used == 0 {
		return "", append(append(append([]byte(nil), b[:prefix]...), text...), short...)
	}
	return string(b[:prefix]) + out, append([]byte(text[used:]), short...)
}

// stampPrefix is how many bytes at the start of b are a line's timestamp and
// the space after it (timestamps=true; captureSplitStamp's rule), 0 when b
// starts with none.
func stampPrefix(b []byte) int {
	head := b[:min(len(b), 64)]
	for i, c := range head {
		if c == ' ' {
			if _, err := time.Parse(time.RFC3339Nano, string(head[:i])); err == nil {
				return i + 1
			}
			return 0
		}
	}
	return 0
}

// wholeRunes is how many bytes of b come before a last rune that is cut
// short; all of b when it ends on a whole rune (or on bytes no rune could
// complete).
func wholeRunes(b []byte) int {
	for c := len(b) - 1; c >= 0 && c > len(b)-utf8.UTFMax; c-- {
		if utf8.RuneStart(b[c]) {
			if utf8.FullRune(b[c:]) {
				return len(b)
			}
			return c
		}
	}
	return len(b)
}

// lineMasker masks a line a piece at a time exactly as strings.Replacer over
// the same forms, given longest first, masks it whole: at each position the
// first form in that order that matches there is replaced, and the scan moves
// past it (strings.Replacer's own rule for a position several forms match).
type lineMasker struct {
	starts  [256]bool         // the bytes a form starts with
	byFirst map[byte][]string // the forms by their first byte, in order
}

func newLineMasker(forms []string) *lineMasker {
	m := &lineMasker{byFirst: map[byte][]string{}}
	for _, f := range forms {
		m.starts[f[0]] = true
		m.byFirst[f[0]] = append(m.byFirst[f[0]], f)
	}
	return m
}

// scan masks s, repaired text, from its start, into at most room bytes of
// output, and answers that and how much of s it covers. It stops before the
// first position it cannot decide yet: one where a form earlier in the order
// than any form matching whole there could still match, s ending before the
// form does -- unless complete says s ends where its line does. A first token
// longer than room is given whole: a piece holds something.
func (m *lineMasker) scan(s string, room int, complete bool) (string, int) {
	var out strings.Builder
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && !m.starts[s[j]] {
			j++
		}
		if j > i {
			// A run no form starts in, as it is.
			n := j - i
			if free := room - out.Len(); n > free {
				n = runeFloor(s[i:j], free)
				if n == 0 && out.Len() == 0 {
					_, n = utf8.DecodeRuneInString(s[i:])
				}
				out.WriteString(s[i : i+n])
				return out.String(), i + n
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		tok, n, decided := m.at(s, i, complete)
		if !decided || (out.Len()+len(tok) > room && out.Len() > 0) {
			break
		}
		out.WriteString(tok)
		i += n
	}
	return out.String(), i
}

// at is what the masker makes of s at i, where some form starts with s[i]:
// the mask over the first form in order that matches whole there, or the one
// rune of s there. decided is false when a form earlier in the order than any
// whole match may still match with more of the line.
func (m *lineMasker) at(s string, i int, complete bool) (tok string, n int, decided bool) {
	rest := s[i:]
	for _, f := range m.byFirst[s[i]] {
		switch {
		case strings.HasPrefix(rest, f):
			return captureMask, len(f), true
		case !complete && len(rest) < len(f) && strings.HasPrefix(f, rest):
			return "", 0, false
		}
	}
	_, n = utf8.DecodeRuneInString(rest)
	return rest[:n], n, true
}

// runeFloor is the longest prefix of s, valid UTF-8, of whole runes and at
// most max bytes.
func runeFloor(s string, max int) int {
	if max >= len(s) {
		return len(s)
	}
	for n := max; n > 0; n-- {
		if utf8.RuneStart(s[n]) {
			return n
		}
	}
	return 0
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
