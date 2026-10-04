package pipelinesteps

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// The capture writes to the log store through LineSink, and the workbench
// hands it logger.CurrentSink(): the two must be assignable both ways.
var (
	_ LineSink    = logger.Sink(nil)
	_ logger.Sink = LineSink(nil)
)

// captureTestSink records every line the store would have received.
type captureTestSink struct {
	mu    sync.Mutex
	lines []logger.Line
}

func (s *captureTestSink) Write(l logger.Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, l)
}

func (s *captureTestSink) all() []logger.Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logger.Line(nil), s.lines...)
}

func (s *captureTestSink) messages() []string {
	var out []string
	for _, l := range s.all() {
		out = append(out, l.Message)
	}
	return out
}

// captureTestClock is the capture's clock in every test: a line with no
// Kubernetes timestamp is stamped with it.
var captureTestClock = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// captureTestMarker is a marker of the shape the Job's MEMQL_ARTIFACT_MARKER
// carries.
const captureTestMarker = "::memql-artifacts::0123456789abcdef"

// captureKubeLine is a line as the log endpoint returns it with
// timestamps=true.
func captureKubeLine(at time.Time, text string) string {
	return at.Format(time.RFC3339Nano) + " " + text
}

// captureFrameLines is what the step wrapper prints for an archive: base64,
// wrapped at 76 columns the way coreutils' and BusyBox's base64 wrap it.
func captureFrameLines(tgz []byte) []string {
	encoded := base64.StdEncoding.EncodeToString(tgz)
	var lines []string
	for len(encoded) > 76 {
		lines = append(lines, encoded[:76])
		encoded = encoded[76:]
	}
	return append(lines, encoded)
}

// newCaptureForTest builds a capture over a PRIVATE store bucket generous
// enough never to refuse a line, and the fixed clock. The node's bucket is
// process-wide, so a test that went through NewCapture would find it drained
// by the test before it.
func newCaptureForTest(t *testing.T, o CaptureOptions) (*Capture, *captureTestSink) {
	t.Helper()
	sink := &captureTestSink{}
	o.Sink = sink
	if o.RunID == "" {
		o.RunID = "r0a1b2c3"
	}
	if o.WorkRunID == "" {
		o.WorkRunID = "w4d5e6f7"
	}
	if o.StepKey == "" {
		o.StepKey = "test/unit"
	}
	if o.StoreMaxLines == 0 {
		o.StoreMaxLines = 1000
	}
	if o.ArchiveMax == 0 {
		o.ArchiveMax = 1 << 20
	}
	if o.ArtifactMax == 0 {
		o.ArtifactMax = 1 << 20
	}
	if o.ArchivePath == "" {
		o.ArchivePath = filepath.Join(t.TempDir(), "step.log")
	}
	c, err := newCapture(o, func() time.Time { return captureTestClock }, newCaptureBucket(1<<20, captureTestClock))
	if err != nil {
		t.Fatalf("newCapture: %v", err)
	}
	return c, sink
}

func closeCaptureForTest(t *testing.T, c *Capture) CaptureResult {
	t.Helper()
	res, err := c.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return res
}

func readArchiveForTest(t *testing.T, res CaptureResult) string {
	t.Helper()
	b, err := os.ReadFile(res.ArchivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	return string(b)
}

func TestCaptureMasksSecretValues(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{
		Secrets: []string{
			"opensesame",
			"pass",          // exactly four bytes: masked
			"pass-and-more", // holds "pass": where both match, the longer one is masked whole
			"abc",           // shorter than four: left alone, or every "abc" in a log would go
			"first-line-of-a-key\nsecond-line-of-a-key\n", // a multi-line value is masked line by line
		},
	})
	c.Feed(captureKubeLine(captureTestClock, "token=opensesame, again opensesame"))
	c.Feed(captureKubeLine(captureTestClock, "pass-and-more then pass"))
	c.Feed(captureKubeLine(captureTestClock, "abc is not a secret"))
	c.Feed(captureKubeLine(captureTestClock, "second-line-of-a-key"))
	// A value straddling the store's 4096-byte split is masked BEFORE the
	// split: 4090 + "opensesame" + 10 is 4110 bytes, which would put "opens"
	// in one store line and "esame" in the next.
	c.Feed(captureKubeLine(captureTestClock, strings.Repeat("x", 4090)+"opensesame"+strings.Repeat("y", 10)))
	res := closeCaptureForTest(t, c)

	straddled := strings.Repeat("x", 4090) + "***" + strings.Repeat("y", 10) // 4103 bytes
	wantStore := []string{
		"token=***, again ***",
		"*** then ***",
		"abc is not a secret",
		"***",
		strings.Repeat("x", 4090) + "***" + "yyy", // 4096
		"yyyyyyy",
	}
	if got := sink.messages(); !reflect.DeepEqual(got, wantStore) {
		t.Errorf("store messages = %q,\nwant %q", got, wantStore)
	}
	wantArchive := "token=***, again ***\n*** then ***\nabc is not a secret\n***\n" + straddled + "\n"
	if got := readArchiveForTest(t, res); got != wantArchive {
		t.Errorf("archive = %q,\nwant %q", got, wantArchive)
	}
	if want := strings.Join(wantStore, "\n"); res.Tail != want {
		t.Errorf("tail = %q,\nwant %q", res.Tail, want)
	}

	everywhere := strings.Join(sink.messages(), "\n") + "\n" + readArchiveForTest(t, res) + "\n" + res.Tail
	for _, secret := range []string{"opensesame", "pass", "first-line-of-a-key", "second-line-of-a-key"} {
		if strings.Contains(everywhere, secret) {
			t.Errorf("%q reached the store, the archive or the tail", secret)
		}
	}
}

// TestCaptureMasksAsTheSeamMasks (the final review of epic memql#5478, its
// three probes): a line is masked as pl.MaskSecrets masks text -- every form
// of every secret, and occurrences that overlap as one span -- in the store,
// the archive and the tail alike. The seam re-masks the check run, but not
// the store an admin reads nor the owner's archive; and what is half masked
// here is a remnant the seam cannot find again in the public tail.
func TestCaptureMasksAsTheSeamMasks(t *testing.T) {
	for _, c := range []struct {
		name    string
		secrets []string
		printed []string // the step's lines
		want    []string // the same lines as the store, the archive and the tail hold them
		leaks   []string // what must reach none of the three
	}{
		{
			// `echo $S` prints a value stored with whitespace around it
			// without that whitespace.
			name:    "a secret stored padded and printed trimmed",
			secrets: []string{"hunter2-token "},
			printed: []string{"token=hunter2-token", "hunter2-token"},
			want:    []string{"token=***", "***"},
			leaks:   []string{"hunter2-token"},
		},
		{
			// An indent-only line of a multi-line secret is whitespace, not a
			// form: indentation elsewhere in the log stays as printed.
			name:    "a multi-line secret with an indent-only line",
			secrets: []string{"-----BEGIN KEY-----\n    \nMIIBOgIBAAJBAKj34GkxFhD90vcN\n-----END KEY-----\n"},
			printed: []string{"func main() {", "    return nil", "        MIIBOgIBAAJBAKj34GkxFhD90vcN"},
			want:    []string{"func main() {", "    return nil", "        ***"},
			leaks:   []string{"MIIBOgIBAAJBAKj34GkxFhD90vcN"},
		},
		{
			// Two secrets printed overlapping are one span: masking them one
			// after the other leaves "***ijkl".
			name:    "overlapping secrets",
			secrets: []string{"abcdefgh", "efghijkl"},
			printed: []string{"abcdefghijkl", "x efghijklmnop abcdefgh y"},
			want:    []string{"***", "x ***mnop *** y"},
			leaks:   []string{"ijkl", "efgh"},
		},
		{
			// A carriage return breaks a value into parts as a newline does.
			name:    "a secret whose parts a bare carriage return separates, each printed alone",
			secrets: []string{"user-name-abcd\rpass-word-efgh"},
			printed: []string{"pass-word-efgh", "user-name-abcd"},
			want:    []string{"***", "***"},
			leaks:   []string{"pass-word-efgh", "user-name-abcd"},
		},
		{
			name:    "a secret stored with CRLF line ends, its lines printed alone",
			secrets: []string{"line-one-abcd\r\nline-two-efgh\r\n"},
			printed: []string{"x line-two-efgh", "line-one-abcd y"},
			want:    []string{"x ***", "*** y"},
			leaks:   []string{"line-one-abcd", "line-two-efgh"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for i, line := range c.printed {
				if seam := pl.MaskSecrets(line, c.secrets); seam != c.want[i] {
					t.Fatalf("the seam masks %q to %q, and the case wants %q: the capture is held to the seam, so the two must agree", line, seam, c.want[i])
				}
			}
			capture, sink := newCaptureForTest(t, CaptureOptions{Secrets: c.secrets})
			for _, line := range c.printed {
				capture.Feed(captureKubeLine(captureTestClock, line))
			}
			res := closeCaptureForTest(t, capture)

			want := strings.Join(c.want, "\n")
			for _, held := range []struct{ where, text string }{
				{"store", strings.Join(sink.messages(), "\n")},
				{"archive", strings.TrimSuffix(readArchiveForTest(t, res), "\n")},
				{"tail", res.Tail},
			} {
				if held.text != want {
					t.Errorf("the %s holds %q, want %q", held.where, held.text, want)
				}
				for _, leak := range c.leaks {
					if strings.Contains(held.text, leak) {
						t.Errorf("the %s holds %q", held.where, leak)
					}
				}
			}
		})
	}
}

func TestCaptureSplitsLongLinesAndRepairsUTF8(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{})
	long := strings.Repeat("a", 10000)
	c.Feed(captureKubeLine(captureTestClock, long))
	c.Feed(captureKubeLine(captureTestClock, "bad \xff\xfe bytes"))
	// A NUL is valid UTF-8 and still goes: Postgres refuses it in text and
	// in jsonb, where a step result's tail lands.
	c.Feed(captureKubeLine(captureTestClock, "nul\x00byte"))
	// A two-byte rune across the 4096-byte boundary moves to the next store
	// line whole rather than being cut into two invalid halves.
	straddle := strings.Repeat("b", 4095) + "\u00e9" + "tail"
	c.Feed(captureKubeLine(captureTestClock, straddle))
	res := closeCaptureForTest(t, c)

	wantStore := []string{
		strings.Repeat("a", 4096),
		strings.Repeat("a", 4096),
		strings.Repeat("a", 1808),
		"bad \uFFFD bytes",
		"nulbyte",
		strings.Repeat("b", 4095),
		"\u00e9tail",
	}
	got := sink.messages()
	if !reflect.DeepEqual(got, wantStore) {
		t.Fatalf("store lines (lengths %v) are not the split and repaired lines (lengths %v)", captureTestLens(got), captureTestLens(wantStore))
	}
	for i, m := range got {
		if len(m) > 4096 {
			t.Errorf("store line %d is %d bytes", i, len(m))
		}
		if !utf8.ValidString(m) {
			t.Errorf("store line %d is not valid UTF-8", i)
		}
	}

	// The archive keeps each line WHOLE; only the store has a line limit.
	wantArchive := long + "\n" + "bad \uFFFD bytes\n" + "nulbyte\n" + straddle + "\n"
	if archive := readArchiveForTest(t, res); archive != wantArchive {
		t.Errorf("archive line lengths %v, want %v", captureTestLens(strings.Split(archive, "\n")), captureTestLens(strings.Split(wantArchive, "\n")))
	}
	if want := strings.Join(wantStore, "\n"); res.Tail != want {
		t.Errorf("tail lengths %v, want %v", captureTestLens(strings.Split(res.Tail, "\n")), captureTestLens(wantStore))
	}
	if res.Lines != 4 {
		t.Errorf("Lines = %d, want 4: a split line is still one line of output", res.Lines)
	}
}

func captureTestLens(lines []string) []int {
	out := make([]int, 0, len(lines))
	for _, l := range lines {
		out = append(out, len(l))
	}
	return out
}

func TestCaptureCapsTheStoreCopy(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{StoreMaxLines: 5, RunID: "r9", StepKey: "test/unit"})
	var fed []string
	for i := 0; i < 12; i++ {
		line := fmt.Sprintf("output line %02d", i)
		fed = append(fed, line)
		c.Feed(captureKubeLine(captureTestClock.Add(time.Duration(i)*time.Second), line))
	}
	res := closeCaptureForTest(t, c)

	got := sink.all()
	if len(got) != 6 {
		t.Fatalf("store got %d lines (%q), want the first 5 and one cap line", len(got), sink.messages())
	}
	for i := 0; i < 5; i++ {
		if got[i].Message != fed[i] {
			t.Errorf("store line %d = %q, want %q", i, got[i].Message, fed[i])
		}
	}
	capLine := got[5]
	if !strings.Contains(capLine.Message, pl.CodeLogCapped) || !strings.Contains(capLine.Message, "Library") {
		t.Errorf("cap line = %q, want it to name %s and the Library", capLine.Message, pl.CodeLogCapped)
	}
	if capLine.Subject != "r9" || capLine.SubjectConcept != "v1:pipelines:run" {
		t.Errorf("cap line subject = %q/%q, want the run's", capLine.SubjectConcept, capLine.Subject)
	}
	if !res.StoreCapped {
		t.Error("StoreCapped = false, want true")
	}
	// The archive is the record: every line, capped store or not.
	if want := strings.Join(fed, "\n") + "\n"; readArchiveForTest(t, res) != want {
		t.Errorf("archive = %q, want every fed line", readArchiveForTest(t, res))
	}
	if res.Lines != 12 {
		t.Errorf("Lines = %d, want 12", res.Lines)
	}
	if want := strings.Join(fed, "\n"); res.Tail != want {
		t.Errorf("tail = %q, want every fed line (fewer than 40)", res.Tail)
	}

	t.Run("exactly at the cap nothing is withheld, so there is no cap line", func(t *testing.T) {
		c, sink := newCaptureForTest(t, CaptureOptions{StoreMaxLines: 3})
		for i := 0; i < 3; i++ {
			c.Feed(captureKubeLine(captureTestClock, fmt.Sprintf("line %d", i)))
		}
		res := closeCaptureForTest(t, c)
		if got := sink.messages(); !reflect.DeepEqual(got, []string{"line 0", "line 1", "line 2"}) {
			t.Errorf("store = %q", got)
		}
		if res.StoreCapped {
			t.Error("StoreCapped = true with nothing withheld")
		}
	})
}

func TestCaptureArchiveTruncatesWithAMarker(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{ArchiveMax: 100})
	line := strings.Repeat("z", 30) // 31 archive bytes with its newline
	for i := 0; i < 10; i++ {
		c.Feed(captureKubeLine(captureTestClock, line))
	}
	// Two bytes would still fit under the cap by size; the archive stays a
	// PREFIX of the log rather than a sample of it, so it is dropped too.
	c.Feed(captureKubeLine(captureTestClock, "!"))
	res := closeCaptureForTest(t, c)

	archive := readArchiveForTest(t, res)
	if !strings.HasSuffix(archive, "\n") {
		t.Fatalf("archive does not end with a newline: %q", archive)
	}
	lines := strings.Split(strings.TrimSuffix(archive, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("archive has %d lines (%q), want 3 that fit under 100 bytes and one truncation line", len(lines), lines)
	}
	for i := 0; i < 3; i++ {
		if lines[i] != line {
			t.Errorf("archive line %d = %q, want %q", i, lines[i], line)
		}
	}
	// 7 dropped lines of 31 bytes, and the 2 of "!\n".
	if !strings.Contains(lines[3], "219 bytes") {
		t.Errorf("truncation line = %q, want it to say 219 bytes were dropped", lines[3])
	}
	if res.ArchiveBytes != int64(len(archive)) {
		t.Errorf("ArchiveBytes = %d, want the file's %d", res.ArchiveBytes, len(archive))
	}
	// The archive's cap is the archive's: the store, the count and the tail
	// still see every line.
	if got := len(sink.messages()); got != 11 {
		t.Errorf("store got %d lines, want 11", got)
	}
	if res.Lines != 11 {
		t.Errorf("Lines = %d, want 11", res.Lines)
	}
	if !strings.HasSuffix(res.Tail, "\n!") {
		t.Errorf("tail = %q, want it to end with the last line", res.Tail)
	}
}

func TestCaptureStoreLinesCarryTheRunSubject(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 30, 15, 123456789, time.UTC)
	c, sink := newCaptureForTest(t, CaptureOptions{RunID: "r7f3a", WorkRunID: "w91c2", StepKey: "test/unit#2"})
	if got := c.Feed(captureKubeLine(at, "hello from the step")); !got.Equal(at) {
		t.Errorf("Feed returned %v, want the line's own timestamp %v for the log cursor", got, at)
	}
	// A line with no Kubernetes timestamp (a fleet chunk) keeps its whole
	// text and takes the capture's clock.
	if got := c.Feed("no timestamp on this line"); !got.Equal(captureTestClock) {
		t.Errorf("Feed returned %v, want the clock's %v", got, captureTestClock)
	}
	// A chunk of several lines is that many lines, at the one timestamp.
	c.Feed("chunk one\nchunk two\n")
	closeCaptureForTest(t, c)

	lines := sink.all()
	if len(lines) != 4 {
		t.Fatalf("store got %d lines (%q), want 4", len(lines), sink.messages())
	}
	l := lines[0]
	if l.Subject != "r7f3a" {
		t.Errorf("Subject = %q, want the run id", l.Subject)
	}
	if l.SubjectConcept != "v1:pipelines:run" {
		t.Errorf("SubjectConcept = %q", l.SubjectConcept)
	}
	if l.Component != "pipelines.step" {
		t.Errorf("Component = %q", l.Component)
	}
	if want := map[string]any{"stepKey": "test/unit#2", "workRunId": "w91c2"}; !reflect.DeepEqual(l.Attributes, want) {
		t.Errorf("Attributes = %v, want %v", l.Attributes, want)
	}
	if !l.At.Equal(at) {
		t.Errorf("At = %v, want the Kubernetes timestamp %v", l.At, at)
	}
	if l.Message != "hello from the step" {
		t.Errorf("Message = %q, want the text without its timestamp", l.Message)
	}
	if l.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want info", l.Level)
	}
	if lines[1].Message != "no timestamp on this line" || !lines[1].At.Equal(captureTestClock) {
		t.Errorf("unstamped line = %q at %v, want its whole text at the clock", lines[1].Message, lines[1].At)
	}
	for i, want := range []string{"chunk one", "chunk two"} {
		if l := lines[2+i]; l.Message != want || !l.At.Equal(captureTestClock) || l.Subject != "r7f3a" {
			t.Errorf("chunk line %d = %q at %v under %q, want %q at the clock under the run", i, l.Message, l.At, l.Subject, want)
		}
	}
}

func TestArtifactFrameIsNotLogged(t *testing.T) {
	tgz := extractTestTgz(t, []extractTestEntry{
		{name: "dist/report.xml", body: strings.Repeat("<testcase name=\"ok\"/>\n", 40)},
	})
	frame := captureFrameLines(tgz)
	if len(frame) < 2 {
		t.Fatalf("fixture frame is %d line(s); the point is a frame of several", len(frame))
	}

	c, sink := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
	c.Feed(captureKubeLine(captureTestClock, "building"))
	c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
	for _, l := range frame {
		c.Feed(captureKubeLine(captureTestClock, l))
	}
	c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
	c.Feed(captureKubeLine(captureTestClock, "done"))
	res := closeCaptureForTest(t, c)

	if got, want := sink.messages(), []string{"building", "done"}; !reflect.DeepEqual(got, want) {
		t.Errorf("store = %q, want %q", got, want)
	}
	if got, want := readArchiveForTest(t, res), "building\ndone\n"; got != want {
		t.Errorf("archive = %q, want %q", got, want)
	}
	if res.Tail != "building\ndone" {
		t.Errorf("tail = %q", res.Tail)
	}
	if res.Lines != 2 {
		t.Errorf("Lines = %d, want 2: frame lines are not output", res.Lines)
	}
	if !bytes.Equal(res.Artifacts, tgz) {
		t.Errorf("Artifacts = %d bytes, want the framed archive's %d", len(res.Artifacts), len(tgz))
	}
	if res.ArtifactNote != nil {
		t.Errorf("ArtifactNote = %+v, want none", res.ArtifactNote)
	}

	t.Run("the last frame wins", func(t *testing.T) {
		// A step can print the marker itself (it is in its environment);
		// the wrapper's frame comes after the command, so it is the last.
		planted := extractTestTgz(t, []extractTestEntry{{name: "dist/planted", body: "planted"}})
		c, _ := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		for _, archive := range [][]byte{planted, tgz} {
			c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
			for _, l := range captureFrameLines(archive) {
				c.Feed(captureKubeLine(captureTestClock, l))
			}
			c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
		}
		if res := closeCaptureForTest(t, c); !bytes.Equal(res.Artifacts, tgz) {
			t.Errorf("Artifacts are not the last frame's archive")
		}
	})
}

func TestArtifactFrameMissingIsANoteNotAFailure(t *testing.T) {
	assertMissing := func(t *testing.T, c *Capture) CaptureResult {
		t.Helper()
		res, err := c.Close()
		if err != nil {
			t.Fatalf("Close: %v -- a missing artifact is a note, never a capture failure", err)
		}
		if res.ArtifactNote == nil || res.ArtifactNote.Code != pl.CodeArtifactMissing {
			t.Fatalf("ArtifactNote = %+v, want %s", res.ArtifactNote, pl.CodeArtifactMissing)
		}
		if class, ok := pl.ClassOf(res.ArtifactNote.Code); !ok || class != pl.ClassNote {
			t.Errorf("%s is class %q, want %q", res.ArtifactNote.Code, class, pl.ClassNote)
		}
		if res.ArtifactNote.Message == "" {
			t.Error("ArtifactNote has no sentence for a person")
		}
		if res.Artifacts != nil {
			t.Errorf("Artifacts = %d bytes, want nil", len(res.Artifacts))
		}
		return res
	}

	t.Run("artifacts declared and no frame", func(t *testing.T) {
		c, _ := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		c.Feed(captureKubeLine(captureTestClock, "the command ran and exited 0"))
		res := assertMissing(t, c)
		if res.Lines != 1 {
			t.Errorf("Lines = %d, want 1", res.Lines)
		}
	})

	t.Run("an image without base64 frames an error, not an archive", func(t *testing.T) {
		c, sink := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
		c.Feed(captureKubeLine(captureTestClock, "sh: 1: base64: not found"))
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
		res := assertMissing(t, c)
		// The shell's complaint is the reason, so the note carries it.
		if !strings.Contains(res.ArtifactNote.Message, "base64: not found") {
			t.Errorf("note = %q, want it to quote what the frame held", res.ArtifactNote.Message)
		}
		if len(sink.messages()) != 0 {
			t.Errorf("store = %q: frame lines are never logged", sink.messages())
		}
	})

	t.Run("an image without tar frames nothing", func(t *testing.T) {
		c, _ := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
		assertMissing(t, c)
	})

	t.Run("a frame that never ends", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{{name: "dist/a", body: "a"}})
		c, _ := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
		c.Feed(captureKubeLine(captureTestClock, captureFrameLines(tgz)[0]))
		assertMissing(t, c)
	})

	t.Run("a frame cut short of a whole quantum", func(t *testing.T) {
		tgz := extractTestTgz(t, []extractTestEntry{{name: "dist/a", body: "a"}})
		frame := captureFrameLines(tgz)
		last := frame[len(frame)-1]
		frame[len(frame)-1] = last[:len(last)-1]
		c, _ := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker})
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
		for _, l := range frame {
			c.Feed(captureKubeLine(captureTestClock, l))
		}
		c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
		assertMissing(t, c)
	})

	t.Run("no marker: nothing was declared, so nothing is missing", func(t *testing.T) {
		c, _ := newCaptureForTest(t, CaptureOptions{})
		c.Feed(captureKubeLine(captureTestClock, "output"))
		res := closeCaptureForTest(t, c)
		if res.ArtifactNote != nil || res.Artifacts != nil {
			t.Errorf("ArtifactNote = %+v, Artifacts = %d bytes, want neither", res.ArtifactNote, len(res.Artifacts))
		}
	})
}

func TestArtifactFrameOverTheCapIsDropped(t *testing.T) {
	// 4 KiB of xorshift noise does not compress: the archive is well over
	// the 1 KiB cap.
	noise := make([]byte, 4096)
	x := uint32(5495)
	for i := range noise {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		noise[i] = byte(x)
	}
	tgz := extractTestTgz(t, []extractTestEntry{{name: "dist/noise.bin", body: string(noise)}})
	if len(tgz) <= 1024 {
		t.Fatalf("fixture archive is %d bytes; it must exceed the 1024-byte cap", len(tgz))
	}

	c, sink := newCaptureForTest(t, CaptureOptions{Marker: captureTestMarker, ArtifactMax: 1024})
	c.Feed(captureKubeLine(captureTestClock, "before the frame"))
	c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
	for _, l := range captureFrameLines(tgz) {
		c.Feed(captureKubeLine(captureTestClock, l))
	}
	c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
	c.Feed(captureKubeLine(captureTestClock, "after the frame"))
	res, err := c.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if res.ArtifactNote == nil || res.ArtifactNote.Code != pl.CodeArtifactTooLarge {
		t.Fatalf("ArtifactNote = %+v, want %s", res.ArtifactNote, pl.CodeArtifactTooLarge)
	}
	if res.Artifacts != nil {
		t.Errorf("Artifacts = %d bytes, want nil", len(res.Artifacts))
	}
	// The capture continues past the dropped frame.
	want := []string{"before the frame", "after the frame"}
	if got := sink.messages(); !reflect.DeepEqual(got, want) {
		t.Errorf("store = %q, want %q", got, want)
	}
	if got := readArchiveForTest(t, res); got != "before the frame\nafter the frame\n" {
		t.Errorf("archive = %q", got)
	}
	if res.Tail != "before the frame\nafter the frame" {
		t.Errorf("tail = %q", res.Tail)
	}
}

func TestCapturePacedLinesAreStillArchived(t *testing.T) {
	sink := &captureTestSink{}
	// Two lines a second, and a clock that never moves: the bucket's burst
	// is all the store gets.
	c, err := newCapture(CaptureOptions{
		RunID: "r1", WorkRunID: "w1", StepKey: "build/compile",
		StoreMaxLines: 100, ArchiveMax: 1 << 20, ArtifactMax: 1 << 20,
		ArchivePath: filepath.Join(t.TempDir(), "step.log"),
		Sink:        sink,
	}, func() time.Time { return captureTestClock }, newCaptureBucket(2, captureTestClock))
	if err != nil {
		t.Fatal(err)
	}
	fed := []string{"one", "two", "three", "four", "five"}
	for _, l := range fed {
		c.Feed(captureKubeLine(captureTestClock, l))
	}
	res := closeCaptureForTest(t, c)

	if got, want := sink.messages(), []string{"one", "two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("store = %q, want the two lines the bucket allowed", got)
	}
	if got := readArchiveForTest(t, res); got != "one\ntwo\nthree\nfour\nfive\n" {
		t.Errorf("archive = %q, want every line: the archive is the record", got)
	}
	if res.Tail != "one\ntwo\nthree\nfour\nfive" {
		t.Errorf("tail = %q, want every line", res.Tail)
	}
	if res.StoreCapped {
		t.Error("StoreCapped = true: pacing is not the cap")
	}

	t.Run("the bucket refills with time", func(t *testing.T) {
		b := newCaptureBucket(2, captureTestClock)
		for i := 1; i <= 2; i++ {
			if !b.take(captureTestClock) {
				t.Fatalf("a fresh bucket holds its burst of 2; token %d was refused", i)
			}
		}
		if b.take(captureTestClock) {
			t.Fatal("an empty bucket gave a token")
		}
		if !b.take(captureTestClock.Add(500 * time.Millisecond)) {
			t.Fatal("half a second at two a second is one token")
		}
		if b.take(captureTestClock.Add(500 * time.Millisecond)) {
			t.Fatal("and only one")
		}
	})

	t.Run("captures on one node share one bucket", func(t *testing.T) {
		open := func(rate int) *Capture {
			c, err := NewCapture(CaptureOptions{
				RunID: "r1", StepKey: "s", StoreMaxLines: 1, ArchiveMax: 1, ArtifactMax: 1,
				ArchivePath: filepath.Join(t.TempDir(), "step.log"), StoreRate: rate,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = c.Close() })
			return c
		}
		a, b := open(0), open(0)
		if a.bucket != b.bucket {
			t.Error("two captures on one node got two buckets: N concurrent steps would get N times the rate")
		}
		if open(captureDefaultStoreRate).bucket != a.bucket {
			t.Error("an unset StoreRate is not the default rate's bucket")
		}
	})
}

func TestCaptureNotesGoToTheArchiveOnly(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{Secrets: []string{"opensesame"}})
	c.Note("waiting for a free slot under the pipelines ceiling")
	c.Feed(captureKubeLine(captureTestClock, "step output"))
	c.Note("--- clone ---\nfetched with opensesame")
	res := closeCaptureForTest(t, c)

	if got, want := sink.messages(), []string{"step output"}; !reflect.DeepEqual(got, want) {
		t.Errorf("store = %q, want only the step's output", got)
	}
	want := "waiting for a free slot under the pipelines ceiling\nstep output\n--- clone ---\nfetched with ***\n"
	if got := readArchiveForTest(t, res); got != want {
		t.Errorf("archive = %q,\nwant %q", got, want)
	}
	if res.Tail != "step output" {
		t.Errorf("tail = %q, want the step's own last words", res.Tail)
	}
	if res.Lines != 1 {
		t.Errorf("Lines = %d, want 1: a note is not output", res.Lines)
	}
}

// TestCaptureNoticesReachTheStoreAndTheArchive: a runner notice -- the wait
// for a free slot under the ceiling, a re-attach -- is for a person watching
// the step live, so unlike a Note it reaches the store as well as the archive.
// It is cleaned and masked like output, but it is not output: it stays out of
// the tail and the line count, and neither the node's pacing nor the step's
// store cap holds it back -- except after the cap line, which says the live
// log stops there.
func TestCaptureNoticesReachTheStoreAndTheArchive(t *testing.T) {
	spent := newCaptureBucket(1, captureTestClock)
	spent.take(captureTestClock) // the node's budget is gone: output is paced out
	sink := &captureTestSink{}
	c, err := newCapture(CaptureOptions{
		RunID: "r0a1b2c3", WorkRunID: "w4d5e6f7", StepKey: "test/unit", Secrets: []string{"opensesame"},
		StoreMaxLines: 1, ArchiveMax: 1 << 20, ArchivePath: filepath.Join(t.TempDir(), "step.log"), Sink: sink,
	}, func() time.Time { return captureTestClock }, spent)
	if err != nil {
		t.Fatalf("newCapture: %v", err)
	}
	c.Notice("memql: waiting for a free slot (opensesame)\x00")
	c.Feed(captureKubeLine(captureTestClock, "step output"))
	res := closeCaptureForTest(t, c)

	lines := sink.all()
	if len(lines) != 1 {
		t.Fatalf("store = %q, want the notice alone: the output was paced out", sink.messages())
	}
	want := logger.Line{
		At: captureTestClock, Level: slog.LevelInfo, Component: "pipelines.step",
		Message: "memql: waiting for a free slot (***)", Subject: "r0a1b2c3", SubjectConcept: "v1:pipelines:run",
		Attributes: map[string]any{"stepKey": "test/unit", "workRunId": "w4d5e6f7"},
	}
	if !reflect.DeepEqual(lines[0], want) {
		t.Errorf("notice store line:\n  got  %+v\n  want %+v", lines[0], want)
	}
	if archive := readArchiveForTest(t, res); archive != "memql: waiting for a free slot (***)\nstep output\n" {
		t.Errorf("archive = %q, want the notice, masked, then the output", archive)
	}
	if res.Tail != "step output" || res.Lines != 1 {
		t.Errorf("tail %q, lines %d; want the step's own output only", res.Tail, res.Lines)
	}

	t.Run("after the cap line the live log has stopped, notices included", func(t *testing.T) {
		c, sink := newCaptureForTest(t, CaptureOptions{StoreMaxLines: 1})
		c.Feed(captureKubeLine(captureTestClock, "one"))
		c.Feed(captureKubeLine(captureTestClock, "two")) // the cap line
		c.Notice("memql: re-attached on workbench-b")
		res := closeCaptureForTest(t, c)
		msgs := sink.messages()
		if len(msgs) != 2 || strings.Contains(msgs[1], "re-attached") {
			t.Errorf("store = %q, want one line and the cap line, nothing after it", msgs)
		}
		if !strings.Contains(readArchiveForTest(t, res), "memql: re-attached on workbench-b\n") {
			t.Error("the notice is not in the archive")
		}
	})
}

// TestCaptureMaskCleansLikeOutput: text the step controls reaches a step's
// result outside the log too -- the name of a tar entry it refused, in a
// note -- and gets the same repair and masking a line does, after the
// capture is closed as well as before.
func TestCaptureMaskCleansLikeOutput(t *testing.T) {
	c, _ := newCaptureForTest(t, CaptureOptions{Secrets: []string{"opensesame", "abc"}})
	closeCaptureForTest(t, c)
	got := c.Mask("entry dist/open\x00sesame.txt holds \xff and abc")
	if want := "entry dist/***.txt holds � and abc"; got != want {
		t.Errorf("Mask = %q, want %q", got, want)
	}
}

// TestCaptureAddSecretsMasksFromThenOn (fix round 1, minors 7 and 8): a secret
// the runner learns after the capture opened -- a clone token minted again, or
// read from an adopted step's Secret -- is masked in every line after it and
// in Mask, beside the ones the capture opened with.
func TestCaptureAddSecretsMasksFromThenOn(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{Secrets: []string{"opensesame"}})
	c.Feed(captureKubeLine(captureTestClock, "before: ghs_learned-later opensesame"))
	c.AddSecrets("ghs_learned-later")
	c.Feed(captureKubeLine(captureTestClock, "after: ghs_learned-later opensesame"))
	res := closeCaptureForTest(t, c)

	want := []string{"before: ghs_learned-later ***", "after: *** ***"}
	if got := sink.messages(); !reflect.DeepEqual(got, want) {
		t.Errorf("store = %q, want %q", got, want)
	}
	if got := c.Mask("quoted ghs_learned-later"); got != "quoted ***" {
		t.Errorf("Mask = %q, want the secret added masked", got)
	}
	if archive := readArchiveForTest(t, res); archive != strings.Join(want, "\n")+"\n" {
		t.Errorf("archive = %q, want %q", archive, want)
	}
}

// TestCaptureFeedArchivedIsOutputTheStoreHasAlready (ruling R36): an adopter
// replays the lines its step printed before the adoption cursor so the
// archive is whole. They are the step's output -- masked, archived, in the
// tail, counted -- and the store, which has them, never sees them again. A
// frame begun before the cursor is read across the seam like any frame.
func TestCaptureFeedArchivedIsOutputTheStoreHasAlready(t *testing.T) {
	c, sink := newCaptureForTest(t, CaptureOptions{Secrets: []string{"opensesame"}, Marker: captureTestMarker})
	tgz := extractTestTgz(t, []extractTestEntry{{name: "dist/a.txt", body: "artifact"}})
	frame := captureFrameLines(tgz)

	c.FeedArchived(captureKubeLine(captureTestClock, "before the cursor: opensesame"))
	c.FeedArchived(captureKubeLine(captureTestClock, captureTestMarker+" begin"))
	c.FeedArchived(captureKubeLine(captureTestClock, frame[0]))
	for _, l := range frame[1:] {
		c.Feed(captureKubeLine(captureTestClock, l))
	}
	c.Feed(captureKubeLine(captureTestClock, captureTestMarker+" end"))
	c.Feed(captureKubeLine(captureTestClock, "after the cursor"))
	res := closeCaptureForTest(t, c)

	if got := sink.messages(); !reflect.DeepEqual(got, []string{"after the cursor"}) {
		t.Errorf("store = %q, want only the line after the cursor", got)
	}
	if archive := readArchiveForTest(t, res); archive != "before the cursor: ***\nafter the cursor\n" {
		t.Errorf("archive = %q, want both lines, masked, and none of the frame", archive)
	}
	if res.Tail != "before the cursor: ***\nafter the cursor" || res.Lines != 2 {
		t.Errorf("tail %q, lines %d; want both lines of output", res.Tail, res.Lines)
	}
	if res.ArtifactNote != nil || len(res.Artifacts) == 0 {
		t.Errorf("artifacts %d bytes, note %+v; want the frame read across the seam", len(res.Artifacts), res.ArtifactNote)
	}
}

func TestNewCaptureRefusesAnIncompleteContract(t *testing.T) {
	valid := func(t *testing.T) CaptureOptions {
		return CaptureOptions{
			RunID: "r1", StepKey: "test/unit", StoreMaxLines: 10, ArchiveMax: 100, ArtifactMax: 100,
			ArchivePath: filepath.Join(t.TempDir(), "step.log"), Marker: captureTestMarker,
		}
	}
	cases := []struct {
		name   string
		mutate func(*CaptureOptions)
	}{
		{"no run id: store lines would bind to nothing", func(o *CaptureOptions) { o.RunID = " " }},
		{"no step key", func(o *CaptureOptions) { o.StepKey = "" }},
		{"no archive path", func(o *CaptureOptions) { o.ArchivePath = "" }},
		{"no store cap", func(o *CaptureOptions) { o.StoreMaxLines = 0 }},
		{"no archive cap", func(o *CaptureOptions) { o.ArchiveMax = 0 }},
		{"a marker with no artifact cap", func(o *CaptureOptions) { o.ArtifactMax = 0 }},
		{"an archive path that cannot be created", func(o *CaptureOptions) {
			o.ArchivePath = filepath.Join(o.ArchivePath, "not-a-dir", "step.log")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := valid(t)
			tc.mutate(&o)
			if c, err := NewCapture(o); err == nil {
				_, _ = c.Close()
				t.Fatal("NewCapture accepted it")
			}
		})
	}

	t.Run("a valid contract creates the archive", func(t *testing.T) {
		o := valid(t)
		c, err := NewCapture(o)
		if err != nil {
			t.Fatal(err)
		}
		res := closeCaptureForTest(t, c)
		if res.ArchivePath != o.ArchivePath {
			t.Errorf("ArchivePath = %q, want %q", res.ArchivePath, o.ArchivePath)
		}
		if _, err := os.Stat(o.ArchivePath); err != nil {
			t.Errorf("archive: %v", err)
		}
	})
}

func TestCaptureTailIsTheLastFortyLines(t *testing.T) {
	c, _ := newCaptureForTest(t, CaptureOptions{})
	for i := 0; i < 45; i++ {
		c.Feed(captureKubeLine(captureTestClock, fmt.Sprintf("line %02d", i)))
	}
	res := closeCaptureForTest(t, c)
	lines := strings.Split(res.Tail, "\n")
	if len(lines) != 40 || lines[0] != "line 05" || lines[39] != "line 44" {
		t.Fatalf("tail is %d lines from %q to %q, want the 40 from \"line 05\" to \"line 44\"", len(lines), lines[0], lines[len(lines)-1])
	}
}

func TestCaptureReportsAnArchiveThatCannotBeWritten(t *testing.T) {
	c, _ := newCaptureForTest(t, CaptureOptions{})
	c.Feed(captureKubeLine(captureTestClock, "written to a buffer the file will refuse"))
	// The file goes away under the capture -- a full disk answers the same
	// way at the flush.
	if err := c.archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := c.Close()
	if err == nil {
		t.Fatal("Close = nil error for an archive that was never written")
	}
	if res.Lines != 1 || res.Tail != "written to a buffer the file will refuse" {
		t.Errorf("result = %+v, want it filled beside the error", res)
	}
	if again, err2 := c.Close(); err2 == nil || again.Lines != 1 {
		t.Errorf("second Close = (%+v, %v), want the first answer again", again, err2)
	}
}

func TestCaptureTailFitsTheAnnotationBudget(t *testing.T) {
	// The tail rides in the step's outcome, which the runner writes as JSON
	// into a Job annotation -- and Kubernetes caps ALL of an object's
	// annotations at 256 KiB. JSON escapes '<' in six bytes, so forty
	// 4096-byte lines of them would marshal to almost a megabyte.
	c, _ := newCaptureForTest(t, CaptureOptions{})
	line := strings.Repeat("<", 4096)
	for i := 0; i < 40; i++ {
		c.Feed(captureKubeLine(captureTestClock, line))
	}
	c.Feed(captureKubeLine(captureTestClock, "the last words"))
	res := closeCaptureForTest(t, c)

	if len(res.Tail) > 16<<10 {
		t.Errorf("tail is %d bytes, over its 16 KiB budget", len(res.Tail))
	}
	marshaled, err := json.Marshal(res.Tail)
	if err != nil {
		t.Fatal(err)
	}
	if len(marshaled) > 128<<10 {
		t.Errorf("the tail marshals to %d bytes: not well under the 256 KiB annotation cap", len(marshaled))
	}
	// The newest lines are the ones kept, and kept whole.
	if !strings.HasSuffix(res.Tail, "\nthe last words") {
		t.Errorf("tail ends %q, want the last line", res.Tail[max(0, len(res.Tail)-40):])
	}
	for i, l := range strings.Split(res.Tail, "\n") {
		if l != line && l != "the last words" {
			t.Errorf("tail line %d is a %d-byte fragment, want whole store lines", i, len(l))
		}
	}
}

func TestCaptureTailCutLandsOnARuneBoundary(t *testing.T) {
	// Whole store lines are dropped oldest first until the tail fits; one
	// line still over the budget keeps its END, cut where a rune starts.
	cases := []struct {
		name   string
		pieces []string
		budget int
		want   string
	}{
		{"the oldest lines go first", []string{"aaaa", "bbbb", "cc\u00e9\u00e9"}, 7, "cc\u00e9\u00e9"},
		{"a tail that fits is whole", []string{"a", "b"}, 3, "a\nb"},
		{"one line over the budget keeps its end", []string{"aaaa", "bbbbbbbbbb"}, 5, "bbbbb"},
		{"the cut moves forward to a rune start", []string{"xx\u00e9\u00e9"}, 3, "\u00e9"},
		{"no lines", nil, 10, ""},
		{"no budget", []string{"abc"}, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureTail(tc.pieces, tc.budget)
			if got != tc.want {
				t.Errorf("captureTail = %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) || len(got) > tc.budget {
				t.Errorf("captureTail = %q: %d bytes, valid UTF-8 %v, budget %d", got, len(got), utf8.ValidString(got), tc.budget)
			}
		})
	}
}
