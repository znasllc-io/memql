package pipelinesteps

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// capture.go -- one step's output on its way to three places (epic
// memql#5478, task #5495; design record D11: "Step output streams into the
// log store tagged with the run as subject ... a size cap applies and the
// full log archives to the Library").
//
//   - The LOG STORE gets one logger.Line per output line, bound to the
//     v1:pipelines:run as its subject, so the run page's log view IS the Logs
//     surface. It is a convenience and is treated as one: capped per step
//     (StoreMaxLines store lines, then exactly one pipeline_log_capped line)
//     and paced per node (StoreRate), so a step printing a gigabyte can
//     neither fill the store nor starve the node's own logging of the
//     store's per-node budget.
//   - The ARCHIVE is a file the capture owns, which the runner stores in the
//     Library. It is the record: every line, whole, up to ArchiveMax bytes,
//     then one line saying how many bytes were dropped. A line the store
//     refused -- capped or paced -- is still archived.
//   - The TAIL is the last 40 store lines within 16 KiB, for a failed step's
//     inline excerpt (pl.StepResult.LogTail). Its byte budget is set here,
//     at the one producer both the cluster and the fleet path share: the
//     runner writes the step's outcome as JSON into a Job annotation, and
//     Kubernetes caps all of an object's annotations at 256 KiB.
//
// Every output line is CLEANED once, before it goes anywhere: NUL bytes are
// dropped (Postgres refuses them in text and in jsonb), invalid UTF-8 becomes
// U+FFFD, and every secret becomes *** exactly as the seam masks text
// (pl.MaskSecrets: each value as stored, without the whitespace around it,
// and line by line, four bytes or more; occurrences that overlap or touch as
// one span) -- Review Focus 1: a step that echoes a resolved secret must put
// it in none of the three, and the seam re-masks the check run but neither
// the store nor the archive, nor can it find again a secret half masked here.
// Masking runs on the whole line BEFORE the store's split, so a value
// straddling a split boundary is masked like any other.
//
// One thing in the stream is not output at all. A step that declares
// artifacts ends with its wrapper printing them as base64 of a tar.gz between
// "<marker> begin" and "<marker> end" (wrapper.go). Those lines are decoded
// into CaptureResult.Artifacts, bounded by ArtifactMax as they arrive, and
// reach neither the store, nor the archive, nor the tail.

// LineSink is the log store: logger.CurrentSink() on the workbench node,
// whose Write never blocks.
type LineSink interface{ Write(logger.Line) }

// CaptureOptions is one step's capture.
type CaptureOptions struct {
	// RunID is the v1:pipelines:run id (bare): every store line's subject.
	// WorkRunID (the v1:work:run) and StepKey ride on each line as
	// attributes, so the run's view can tell its steps apart.
	RunID, WorkRunID, StepKey string
	// Secrets are the resolved values to mask, masked as the seam masks them
	// (pl.MaskSecrets): as stored, without the whitespace around them, and --
	// a value holding newlines -- line by line as well as whole, because the
	// capture sees one line at a time. A form shorter than four bytes is not
	// masked: it would mask ordinary words, and an indent-only line of a
	// key would mask every indentation in the log.
	Secrets []string
	// Marker is the step's artifact marker (MEMQL_ARTIFACT_MARKER on the
	// Job). Set it exactly when the step declares artifacts: it is how the
	// capture knows a frame is due, and an absent frame is then reported
	// as pipeline_artifact_missing. Empty means nothing was declared: no
	// frame is looked for and no note is made.
	Marker string
	// StoreMaxLines is how many store lines the step may write before the
	// one pipeline_log_capped line. Required.
	StoreMaxLines int
	// ArchiveMax is the archive's size in bytes before its truncation line.
	// Required.
	ArchiveMax int64
	// ArtifactMax bounds the decoded artifact archive. Required when Marker
	// is set.
	ArtifactMax int64
	// ArchivePath is a temp file the capture owns: NewCapture creates (or
	// truncates) it, and Close leaves it on disk for the caller to store and
	// remove.
	ArchivePath string
	// Sink is the log store. Nil writes no store copy; the archive and the
	// tail are kept the same.
	Sink LineSink
	// StoreRate is store lines per second, default 200, drawn from ONE
	// bucket per rate on this node: every capture running here shares it.
	// The log store's own sink takes 2000 a second per node by default
	// (component/logstore), so steps get a tenth of it however many run.
	StoreRate int
}

// CaptureResult is a closed capture.
type CaptureResult struct {
	// Lines counts the step's output lines: a line the store split is one
	// line, and neither a frame line nor a Note is output.
	Lines int
	// StoreCapped says the store copy hit StoreMaxLines and its cap line
	// was written.
	StoreCapped bool
	// ArchivePath is the archive: complete up to ArchiveMax bytes, then a
	// truncation line.
	ArchivePath string
	// ArchiveBytes is the archive's size, its truncation line included.
	ArchiveBytes int64
	// Tail is the last store lines joined by "\n", masked: at most 40 of
	// them and at most 16 KiB, the oldest dropped whole to fit. Lines are as
	// the store has them (at most 4096 bytes), so when the last line of
	// output is longer, the tail holds its end.
	Tail string
	// Artifacts is the decoded tar.gz of the step's frame; nil when no
	// complete frame was decoded within ArtifactMax.
	Artifacts []byte
	// ArtifactNote says why a declared step has no Artifacts:
	// pl.CodeArtifactMissing (no frame, a frame that never ended, an empty
	// or undecodable frame) or pl.CodeArtifactTooLarge. Nil when the step
	// declared none or Artifacts holds them.
	ArtifactNote *pl.Failure
}

const (
	// captureComponent is the component every store line carries.
	captureComponent = "pipelines.step"
	// captureRunConcept is the subject's concept: a store line binds to its
	// v1:pipelines:run by bare id (docs/public/concepts/identifiers.md).
	captureRunConcept = "v1:pipelines:run"
	// captureStoreLineBytes is the longest store line: the store's own
	// message cap (component/logstore.MaxMessageBytes), so the sink never
	// truncates a line of ours. A longer line is split on a rune boundary.
	captureStoreLineBytes = 4096
	// captureTailLines is the tail's length (pl.StepResult.LogTail).
	captureTailLines = 40
	// captureTailBytes is the tail's byte budget. JSON can spend six bytes
	// on one ('<' is \u003c), so 16 KiB of tail marshals to at most about
	// 96 KiB, well inside the 256 KiB every annotation of a Job shares.
	captureTailBytes = 16 << 10
	// captureDefaultStoreRate is StoreRate's default.
	captureDefaultStoreRate = 200
	// captureMask is the seam's mask, which pl.MaskSecrets writes for a span:
	// the follower writes it for a span of a line it cuts, and its tests hold
	// the pieces to what the seam makes of the whole line.
	captureMask = "***"
	// captureNoteQuoteBytes bounds the frame line an undecodable-frame note
	// quotes.
	captureNoteQuoteBytes = 120
)

// Capture is one step's capture. Feed, Note and Close are safe for
// concurrent use; after Close, Feed and Note do nothing.
type Capture struct {
	mu sync.Mutex

	runID, workRunID, stepKey string
	frameBegin, frameEnd      string // "" when no artifacts were declared
	storeMax                  int
	archiveMax                int64
	artifactMax               int64
	sink                      LineSink
	bucket                    *captureBucket
	now                       func() time.Time
	// masked is the forms every line is masked for (captureMaskForms),
	// swapped whole when AddSecrets adds to secrets, so it is read without
	// the lock; nil: nothing to mask.
	masked  atomic.Pointer[[]string]
	secrets []string

	lines       int
	storeLines  int
	storeCapped bool

	archivePath    string
	archiveFile    *os.File
	archive        *bufio.Writer
	archiveBytes   int64
	archiveDropped int64
	archiveErr     error

	tail     [captureTailLines]string
	tailNext int
	tailLen  int

	frame captureFrame

	closed   bool
	result   CaptureResult
	closeErr error
}

// captureFrame is the artifact frame being decoded. The LAST frame wins: a
// step can print the marker itself (it is in its environment), but the
// wrapper's frame is printed after the command has exited, so it is last.
type captureFrame struct {
	seen     bool   // a begin line was seen
	open     bool   // between a begin and its end
	decoded  []byte // the frame's archive so far
	carry    []byte // base64 characters short of a whole quantum
	tooLarge bool
	bad      string // why the frame cannot be decoded, as the note's sentence
}

// NewCapture opens a step's capture.
func NewCapture(o CaptureOptions) (*Capture, error) {
	rate := o.StoreRate
	if rate <= 0 {
		rate = captureDefaultStoreRate
	}
	return newCapture(o, time.Now, captureNodeBucket(rate))
}

// newCapture is NewCapture with the clock and the store bucket handed in.
func newCapture(o CaptureOptions, now func() time.Time, bucket *captureBucket) (*Capture, error) {
	switch {
	case strings.TrimSpace(o.RunID) == "":
		return nil, errors.New("pipelinesteps: a capture needs the pipelines run id its store lines bind to")
	case strings.TrimSpace(o.StepKey) == "":
		return nil, errors.New("pipelinesteps: a capture needs the step key")
	case o.ArchivePath == "":
		return nil, errors.New("pipelinesteps: a capture needs an archive path")
	case o.StoreMaxLines <= 0:
		return nil, fmt.Errorf("pipelinesteps: StoreMaxLines must be positive, not %d", o.StoreMaxLines)
	case o.ArchiveMax <= 0:
		return nil, fmt.Errorf("pipelinesteps: ArchiveMax must be positive, not %d", o.ArchiveMax)
	case o.Marker != "" && o.ArtifactMax <= 0:
		return nil, fmt.Errorf("pipelinesteps: a step with artifacts needs a positive ArtifactMax, not %d", o.ArtifactMax)
	}
	f, err := os.OpenFile(o.ArchivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pipelinesteps: opening the log archive: %w", err)
	}
	c := &Capture{
		runID:       o.RunID,
		workRunID:   o.WorkRunID,
		stepKey:     o.StepKey,
		storeMax:    o.StoreMaxLines,
		archiveMax:  o.ArchiveMax,
		artifactMax: o.ArtifactMax,
		sink:        o.Sink,
		bucket:      bucket,
		now:         now,
		secrets:     append([]string(nil), o.Secrets...),
		archivePath: o.ArchivePath,
		archiveFile: f,
		archive:     bufio.NewWriterSize(f, 64<<10),
	}
	c.masked.Store(captureMasked(c.secrets))
	if o.Marker != "" {
		c.frameBegin = o.Marker + " begin"
		c.frameEnd = o.Marker + " end"
	}
	return c, nil
}

// Feed consumes one raw log line as Kubernetes returns it with
// timestamps=true ("<RFC3339Nano> <text>"); it returns the line's timestamp
// for the log cursor. A line with no parseable timestamp keeps its whole text
// and takes the capture's clock. One trailing newline is ignored, and a raw
// string holding several lines (a fleet chunk) is taken line by line, every
// line at the one timestamp.
func (c *Capture) Feed(raw string) time.Time {
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	at, text, ok := captureSplitStamp(raw)
	if !ok {
		at, text = c.now(), raw
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return at
	}
	if !strings.Contains(text, "\n") {
		c.feedLine(at, text, true)
		return at
	}
	for _, line := range strings.Split(text, "\n") {
		c.feedLine(at, strings.TrimSuffix(line, "\r"), true)
	}
	return at
}

// FeedArchived consumes one raw log line, as Feed does, that the log store
// already has: a line a replica that held the step before this one captured,
// replayed so the archive is whole (ruling R36). It is the step's output --
// cleaned and masked, archived, in the tail, counted in Lines, read for the
// artifact frame -- and never reaches the store, which has it.
func (c *Capture) FeedArchived(raw string) time.Time {
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	at, text, ok := captureSplitStamp(raw)
	if !ok {
		at, text = c.now(), raw
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return at
	}
	for _, line := range strings.Split(text, "\n") {
		c.feedLine(at, strings.TrimSuffix(line, "\r"), false)
	}
	return at
}

// FeedLine consumes output that carries no timestamp -- a fleet machine's
// stream, whose lines are the step's own text from the first byte (epic
// memql#5478, #5494). Feed would read a line that BEGINS with an RFC 3339
// token as the kubelet's stamp: it would eat the token and let the step
// choose the line's time. Here the whole line is output, stamped with the
// capture's clock, which it returns. One trailing newline is ignored, and
// text holding several lines is taken line by line.
func (c *Capture) FeedLine(text string) time.Time {
	text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
	at := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return at
	}
	for _, line := range strings.Split(text, "\n") {
		c.feedLine(at, strings.TrimSuffix(line, "\r"), true)
	}
	return at
}

// Note appends a runner-authored line -- a clone section, an adoption
// notice, a wait notice -- to the ARCHIVE only, cleaned and masked like
// output and bound by the same cap. It never reaches the store, whose live
// view is the step's own output, nor the tail, which is the step's last words
// rather than the runner's; and it is not counted in Lines. Text holding
// newlines is archived as that many lines.
func (c *Capture) Note(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.archiveLine(c.clean(strings.TrimRight(text, "\r\n")))
}

// Notice appends a runner line a person watching the step live must see --
// the wait for a free slot under the ceiling, a re-attach after a replica
// was lost -- to the store as well as the archive. Like Note it is cleaned
// and masked, stays out of the tail and is not counted in Lines. Unlike
// output it is neither paced by the node's bucket nor counted against
// StoreMaxLines: the runner writes one or two per step, and each must land.
// Once the store copy is capped nothing more reaches the store, a notice
// included: the cap line says the live log stops there.
func (c *Capture) Notice(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	line := c.clean(strings.TrimRight(text, "\r\n"))
	c.archiveLine(line)
	if c.sink == nil || c.storeCapped {
		return
	}
	at := c.now()
	for _, l := range strings.Split(line, "\n") {
		for _, piece := range captureSplit(l, captureStoreLineBytes) {
			c.sink.Write(c.storeLine(at, slog.LevelInfo, piece, ""))
		}
	}
}

// Mask is s with the repair and the masking every captured line gets: NUL
// bytes dropped, invalid UTF-8 replaced, every secret value masked. It is for
// text the step controls that reaches its result outside the log -- the name
// of a tar entry it refused, quoted in a note -- and works before and after
// Close.
func (c *Capture) Mask(s string) string {
	return c.clean(s)
}

// AddSecrets masks values too, in every line from now on and in Mask: a
// secret the step's runner learned after the capture opened -- a clone token
// minted again while the step waited under the ceiling, or read from the
// Secret of a step the runner adopted.
func (c *Capture) AddSecrets(values ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secrets = append(c.secrets, values...)
	c.masked.Store(captureMasked(c.secrets))
}

// Close finishes the capture: the archive's truncation line when it was
// capped, the file flushed and closed, the artifact frame judged. The error
// is the archive's (a write that failed); the result is filled either way. A
// second Close returns the first one's answer.
func (c *Capture) Close() (CaptureResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.result, c.closeErr
	}
	c.closed = true

	if c.archiveDropped > 0 && c.archiveErr == nil {
		line := fmt.Sprintf("memql: the log archive stops here, at its %d-byte cap; %d bytes of output after this point were dropped",
			c.archiveMax, c.archiveDropped)
		c.archiveWrite(line)
	}
	var errs []error
	if c.archiveErr != nil {
		errs = append(errs, c.archiveErr)
	} else if err := c.archive.Flush(); err != nil {
		errs = append(errs, err)
	}
	if err := c.archiveFile.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		c.closeErr = fmt.Errorf("pipelinesteps: writing the log archive %s: %w", c.archivePath, errors.Join(errs...))
	}

	artifacts, note := c.artifacts()
	c.result = CaptureResult{
		Lines:        c.lines,
		StoreCapped:  c.storeCapped,
		ArchivePath:  c.archivePath,
		ArchiveBytes: c.archiveBytes,
		Tail:         c.tailString(),
		Artifacts:    artifacts,
		ArtifactNote: note,
	}
	return c.result, c.closeErr
}

// feedLine routes one line: frame syntax, frame content, or output, which
// reaches the store when stored says so. Caller holds c.mu.
func (c *Capture) feedLine(at time.Time, text string, stored bool) {
	if c.frameBegin != "" {
		switch strings.TrimSpace(text) {
		case c.frameBegin:
			c.frame = captureFrame{seen: true, open: true}
			return
		case c.frameEnd:
			// An end with no begin is frame syntax too, and not output.
			if c.frame.open {
				c.frameFinish()
			}
			return
		}
		if c.frame.open {
			c.frameAdd(text)
			return
		}
	}
	c.output(at, c.clean(text), stored)
}

// output sends one cleaned line of the step's output to the archive whole,
// and to the tail -- and, when stored, the store -- as store lines. Caller
// holds c.mu.
func (c *Capture) output(at time.Time, line string, stored bool) {
	c.lines++
	c.archiveLine(line)
	for _, piece := range captureSplit(line, captureStoreLineBytes) {
		c.tail[c.tailNext] = piece
		c.tailNext = (c.tailNext + 1) % captureTailLines
		if c.tailLen < captureTailLines {
			c.tailLen++
		}
		if stored {
			c.store(at, piece)
		}
	}
}

// store writes one store line, unless the step's store copy is capped or the
// node's bucket is empty. A paced line is simply not in the store: the
// archive and the tail have it. Caller holds c.mu.
func (c *Capture) store(at time.Time, piece string) {
	if c.sink == nil || c.storeCapped {
		return
	}
	if c.storeLines >= c.storeMax {
		// The one line after the cap, outside the bucket: it must land.
		c.storeCapped = true
		msg := fmt.Sprintf("%s: the live log of this step stops here, at %d lines; the complete log is archived to the Library",
			pl.CodeLogCapped, c.storeMax)
		c.sink.Write(c.storeLine(at, slog.LevelWarn, msg, pl.CodeLogCapped))
		return
	}
	if !c.bucket.take(c.now()) {
		return
	}
	c.sink.Write(c.storeLine(at, slog.LevelInfo, piece, ""))
	c.storeLines++
}

// storeLine is a line bound to the run, with the step on its attributes.
func (c *Capture) storeLine(at time.Time, level slog.Level, msg, code string) logger.Line {
	attrs := map[string]any{"stepKey": c.stepKey, "workRunId": c.workRunID}
	if code != "" {
		attrs["code"] = code
	}
	return logger.Line{
		At:             at,
		Level:          level,
		Component:      captureComponent,
		Message:        msg,
		Subject:        c.runID,
		SubjectConcept: captureRunConcept,
		Attributes:     attrs,
	}
}

// archiveLine appends one line to the archive while it fits under the cap.
// The first line that does not fit ends the archive: every byte from there on
// is counted as dropped, so the file stays a PREFIX of the log rather than a
// sample of it. Caller holds c.mu.
func (c *Capture) archiveLine(line string) {
	if c.archiveErr != nil {
		return
	}
	n := int64(len(line)) + 1
	if c.archiveDropped > 0 || c.archiveBytes+n > c.archiveMax {
		c.archiveDropped += n
		return
	}
	c.archiveWrite(line)
}

// archiveWrite writes one line and its newline past any cap. Caller holds
// c.mu.
func (c *Capture) archiveWrite(line string) {
	if _, err := c.archive.WriteString(line); err != nil {
		c.archiveErr = err
		return
	}
	if err := c.archive.WriteByte('\n'); err != nil {
		c.archiveErr = err
		return
	}
	c.archiveBytes += int64(len(line)) + 1
}

// tailString joins the ring oldest first. Caller holds c.mu.
func (c *Capture) tailString() string {
	if c.tailLen == 0 {
		return ""
	}
	start := (c.tailNext - c.tailLen + captureTailLines) % captureTailLines
	parts := make([]string, 0, c.tailLen)
	for i := 0; i < c.tailLen; i++ {
		parts = append(parts, c.tail[(start+i)%captureTailLines])
	}
	return captureTail(parts, captureTailBytes)
}

// captureTail joins store lines, oldest first, within budget bytes. The
// oldest lines are dropped whole until the rest fits; a single line still
// over the budget keeps its END -- a failed step's last words are at the end
// -- cut forward to where a rune starts.
func captureTail(pieces []string, budget int) string {
	if budget <= 0 || len(pieces) == 0 {
		return ""
	}
	size := len(pieces) - 1 // the newlines between them
	for _, p := range pieces {
		size += len(p)
	}
	for len(pieces) > 1 && size > budget {
		size -= len(pieces[0]) + 1
		pieces = pieces[1:]
	}
	if last := pieces[0]; len(pieces) == 1 && len(last) > budget {
		cut := len(last) - budget
		for cut < len(last) && !utf8.RuneStart(last[cut]) {
			cut++
		}
		return last[cut:]
	}
	return strings.Join(pieces, "\n")
}

// clean is the one repair every line gets: NUL dropped, invalid UTF-8
// replaced, secrets masked as the seam masks them.
func (c *Capture) clean(s string) string {
	s = captureRepair(s)
	if forms := c.masked.Load(); forms != nil {
		s = pl.MaskSecrets(s, *forms)
	}
	return s
}

// frameAdd decodes one frame line onto the frame's archive as it arrives, so
// the memory a frame holds is its decoded size, and a frame past ArtifactMax
// is dropped the moment it passes it rather than at its end. Caller holds
// c.mu.
func (c *Capture) frameAdd(text string) {
	f := &c.frame
	if f.tooLarge || f.bad != "" {
		return
	}
	chunk := strings.TrimSpace(text)
	if chunk == "" {
		return
	}
	// Complete a quantum carried from the line before. The wrapper's base64
	// wraps at 76 columns, a multiple of four, so this is rare.
	for len(f.carry) > 0 && len(f.carry) < 4 && chunk != "" {
		f.carry = append(f.carry, chunk[0])
		chunk = chunk[1:]
	}
	if len(f.carry) == 4 {
		if !c.frameDecode(f.carry, text) {
			return
		}
		f.carry = f.carry[:0]
	}
	whole := len(chunk) / 4 * 4
	// Refuse by size BEFORE decoding: a quantum of four decodes to at least
	// one byte and at most three, and only the last may hold padding.
	if int64(len(f.decoded))+int64(whole/4*3)-2 > c.artifactMax {
		c.frameTooLarge()
		return
	}
	if whole > 0 && !c.frameDecode([]byte(chunk[:whole]), text) {
		return
	}
	f.carry = append(f.carry, chunk[whole:]...)
}

// frameDecode appends the decoding of whole base64 quanta to the frame, and
// reports whether the frame is still good. Caller holds c.mu.
func (c *Capture) frameDecode(quanta []byte, line string) bool {
	f := &c.frame
	start := len(f.decoded)
	need := base64.StdEncoding.DecodedLen(len(quanta))
	if cap(f.decoded)-start < need {
		// Double, but never past the cap: the frame is refused there anyway,
		// so the buffer a step can make the workbench hold is ArtifactMax.
		size := 2*cap(f.decoded) + need
		if limit := int(c.artifactMax) + need; size > limit {
			size = limit
		}
		grown := make([]byte, start, size)
		copy(grown, f.decoded)
		f.decoded = grown
	}
	n, err := base64.StdEncoding.Decode(f.decoded[start:start+need], quanta)
	if err != nil {
		// What the frame holds instead is the reason -- "sh: 1: base64: not
		// found" -- so the note quotes it, cleaned and masked like output.
		f.bad = fmt.Sprintf("the artifact frame is not base64 (it holds %s): the image may lack tar or base64",
			captureQuote(c.clean(line)))
		f.decoded, f.carry = nil, nil
		return false
	}
	f.decoded = f.decoded[:start+n]
	if int64(len(f.decoded)) > c.artifactMax {
		c.frameTooLarge()
		return false
	}
	return true
}

// frameTooLarge drops the frame's bytes and remembers why. Caller holds c.mu.
func (c *Capture) frameTooLarge() {
	c.frame.tooLarge = true
	c.frame.decoded, c.frame.carry = nil, nil
}

// frameFinish closes the open frame at its end line. Base64 that stops short
// of a whole quantum is a frame cut short, and its bytes are not the archive.
// Caller holds c.mu.
func (c *Capture) frameFinish() {
	f := &c.frame
	f.open = false
	if f.tooLarge || f.bad != "" || len(f.carry) == 0 {
		return
	}
	f.bad = "the artifact frame's base64 stops partway through a quantum: the frame was cut short"
	f.decoded, f.carry = nil, nil
}

// artifacts judges the frame at Close. Caller holds c.mu.
func (c *Capture) artifacts() ([]byte, *pl.Failure) {
	if c.frameBegin == "" {
		return nil, nil
	}
	f := &c.frame
	missing := func(format string, args ...any) *pl.Failure {
		return &pl.Failure{Code: pl.CodeArtifactMissing, Message: fmt.Sprintf(format, args...)}
	}
	switch {
	case f.tooLarge:
		return nil, &pl.Failure{Code: pl.CodeArtifactTooLarge,
			Message: fmt.Sprintf("the step's artifacts are larger than %d bytes and were dropped", c.artifactMax)}
	case !f.seen:
		// The wrapper echoes both markers even in an image without tar or
		// base64, so no frame at all means the output ended before the
		// wrapper reached it.
		return nil, missing("the step declares artifacts and its output holds no artifact frame: it ended before the wrapper printed one")
	case f.open:
		return nil, missing("the artifact frame never ended: the step's output stopped inside it")
	case f.bad != "":
		return nil, missing("%s", f.bad)
	case len(f.decoded) == 0:
		return nil, missing("the artifact frame is empty: the image may lack tar or base64")
	}
	return f.decoded, nil
}

// captureSplitStamp splits a Kubernetes log line into its timestamp and its
// text. ok is false when the line does not begin with an RFC 3339 timestamp.
func captureSplitStamp(raw string) (time.Time, string, bool) {
	stamp, text, found := strings.Cut(raw, " ")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", false
	}
	if !found {
		text = ""
	}
	return at, text, true
}

// captureRepair drops NUL bytes and replaces invalid UTF-8 with U+FFFD.
func captureRepair(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// captureSplit cuts a line into store lines of at most limit bytes, each cut
// on a rune boundary. Pieces of a longer line are copies, so a 40-line tail
// of pieces never pins a long line's whole backing array.
func captureSplit(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	pieces := make([]string, 0, len(s)/limit+1)
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = limit
		}
		pieces = append(pieces, strings.Clone(s[:cut]))
		s = s[cut:]
	}
	return append(pieces, strings.Clone(s))
}

// captureMaskForms is every form of the secrets a line is masked for: the
// seam's (pl.MaskForms), of each value repaired the way lines are, so the two
// can meet. A line is masked with pl.MaskSecrets over them, which masks over
// the forms exactly what it masks over the values -- the seam's masking, in
// one implementation. The follower keeps its cuts out of the same forms, so
// no piece it feeds ends inside one.
func captureMaskForms(secrets []string) []string {
	repaired := make([]string, 0, len(secrets))
	for _, s := range secrets {
		repaired = append(repaired, captureRepair(s))
	}
	return pl.MaskForms(repaired)
}

// captureMasked is what the capture holds to mask with: the forms, or nil
// when no value is long enough to mask.
func captureMasked(secrets []string) *[]string {
	forms := captureMaskForms(secrets)
	if len(forms) == 0 {
		return nil
	}
	return &forms
}

// captureQuote is a frame line for a note: quoted, and cut on a rune
// boundary when long.
func captureQuote(s string) string {
	if len(s) > captureNoteQuoteBytes {
		s = captureSplit(s, captureNoteQuoteBytes)[0] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// captureBucket paces the store copy: a token bucket of rate lines a second,
// with a burst of one second's worth.
type captureBucket struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newCaptureBucket(rate int, now time.Time) *captureBucket {
	return &captureBucket{rate: float64(rate), tokens: float64(rate), last: now}
}

// take spends one token when there is one.
func (b *captureBucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.rate {
			b.tokens = b.rate
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var (
	captureBucketsMu sync.Mutex
	captureBuckets   = map[int]*captureBucket{}
)

// captureNodeBucket is this node's bucket for a rate. Every capture on the
// node asking for the same rate draws from it, so N concurrent steps share
// StoreRate lines a second between them rather than getting N times it.
func captureNodeBucket(rate int) *captureBucket {
	captureBucketsMu.Lock()
	defer captureBucketsMu.Unlock()
	b, ok := captureBuckets[rate]
	if !ok {
		b = newCaptureBucket(rate, time.Now())
		captureBuckets[rate] = b
	}
	return b
}
