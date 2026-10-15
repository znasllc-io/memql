package emaildoc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fidelity harness is a developer tool. Given a directory of example
// emails (one subdirectory each, holding email.html and a hand-written
// document.json that describes it) it renders every document, screenshots the
// original and the render in headless Chrome at the email's width, and reports
// the share of pixels that differ. scripts/email/fidelity.sh runs it.
//
// The examples are a client's real emails, so they live outside the
// repository, and nothing here reads one into it: the harness reads the
// directory named by MEMQL_EMAIL_FIDELITY_DIR and writes what it makes (the
// render, both screenshots and the difference) next to each example.

// The environment of the harness. scripts/email/fidelity.sh sets all three.
const (
	fidelityDirVar    = "MEMQL_EMAIL_FIDELITY_DIR"     // the examples; the test skips without it
	fidelityMaxVar    = "MEMQL_EMAIL_FIDELITY_MAX_PCT" // the most percent of an email that may differ
	fidelityChromeVar = "MEMQL_EMAIL_FIDELITY_CHROME"  // the Chrome or Chromium binary
)

const (
	fidelityWidth = 600 // the email's width, and the window's
	// fidelityTolerance is how far apart two pixels may be, in each colour
	// channel (of 255), and still be the same: antialiasing and font rendering
	// move edges by a few levels, and nobody can see them.
	fidelityTolerance = 16
	// fidelityMaxDefault is the percentage of an email's content that may
	// differ when fidelityMaxVar does not say.
	fidelityMaxDefault = 2.0
	// fidelityWindowHeight is taller than any email; an email taller than this
	// is compared as far as the screenshot goes.
	fidelityWindowHeight = 6000
	// fidelityChromeGrace is how long, once Chrome has been stopped, the
	// processes it started may keep its output open before screenshot stops
	// waiting for them.
	fidelityChromeGrace = 2 * time.Second
)

// The files of an example: the first two are the example's own, the rest are
// what the harness writes next to them.
const (
	fidelityOriginalHTML = "email.html"
	fidelityDocumentJSON = "document.json"
	fidelityRenderedHTML = "rendered.html"
	fidelityOriginalPNG  = "original.png"
	fidelityRenderedPNG  = "rendered.png"
	fidelityDiffPNG      = "diff.png"
)

// fidelityChromeTimeout is how long Chrome has to make one screenshot.
var fidelityChromeTimeout = 90 * time.Second

// pixelDifference compares two screenshots of an email. The content is every
// row down to the last one that holds, in either image, a pixel more than
// tolerance from the page colour (background); below it both are page, and
// counting those rows would dilute the result. Past the edge of an image is
// page too, so a shorter screenshot reads as padded with it. Only the first
// fidelityWidth columns are compared. Two pixels differ when any of their red,
// green or blue channels is more than tolerance apart.
//
// percent is the differing pixels over fidelityWidth x rows, as a percentage;
// rows is the height of the content, and zero when neither image has any.
func pixelDifference(a, b image.Image, background color.RGBA, tolerance uint8) (percent float64, rows int) {
	rows = max(contentRows(a, background, tolerance), contentRows(b, background, tolerance))
	if rows == 0 {
		return 0, 0
	}
	differing := 0
	for y := 0; y < rows; y++ {
		for x := 0; x < fidelityWidth; x++ {
			if differ(pixelAt(a, x, y, background), pixelAt(b, x, y, background), tolerance) {
				differing++
			}
		}
	}
	return float64(differing) * 100 / float64(fidelityWidth*rows), rows
}

// contentRows is the height of an image's content: the number of rows down to
// the last one that holds a pixel more than tolerance from background.
func contentRows(img image.Image, background color.RGBA, tolerance uint8) int {
	bounds := img.Bounds()
	for y := bounds.Dy() - 1; y >= 0; y-- {
		for x := 0; x < min(fidelityWidth, bounds.Dx()); x++ {
			if differ(pixelAt(img, x, y, background), background, tolerance) {
				return y + 1
			}
		}
	}
	return 0
}

// pixelAt is the pixel at column x and row y of an image, counted from its
// top left corner; past the image it is background.
func pixelAt(img image.Image, x, y int, background color.RGBA) color.RGBA {
	bounds := img.Bounds()
	if x >= bounds.Dx() || y >= bounds.Dy() {
		return background
	}
	return color.RGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.RGBA)
}

// differ says whether two pixels differ: any of their colour channels is more
// than tolerance apart. Alpha is not compared; a screenshot is opaque.
func differ(p, q color.RGBA, tolerance uint8) bool {
	return channelGap(p.R, q.R) > tolerance || channelGap(p.G, q.G) > tolerance || channelGap(p.B, q.B) > tolerance
}

func channelGap(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

// fidelityMark is the colour diff.png draws a differing pixel in.
var fidelityMark = color.RGBA{R: 0xff, A: 0xff}

// writeDiff writes the picture of a comparison: the first rows rows of the
// email, fidelityWidth pixels wide (a picture is at least one row high), in
// which a pixel that a and b disagree on, as pixelDifference reads it, is red
// and every other pixel is a pale copy of a's, so the picture shows where the
// differences are on the email. a and b are screenshots of one window and the
// same size; a pixel past an image's edge reads as white.
func writeDiff(path string, a, b image.Image, rows int, tolerance uint8) error {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	out := image.NewRGBA(image.Rect(0, 0, fidelityWidth, max(rows, 1)))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: white}, image.Point{}, draw.Src)
	for y := 0; y < rows; y++ {
		for x := 0; x < fidelityWidth; x++ {
			p, q := pixelAt(a, x, y, white), pixelAt(b, x, y, white)
			if differ(p, q, tolerance) {
				out.SetRGBA(x, y, fidelityMark)
				continue
			}
			// A quarter of the original over white.
			out.SetRGBA(x, y, color.RGBA{R: 255 - (255-p.R)/4, G: 255 - (255-p.G)/4, B: 255 - (255-p.B)/4, A: 0xff})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, out); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// fidelityDecode reads a document generically, with its numbers as written,
// so that inlineFileImages can change the pictures and nothing else changes on
// the way to Parse.
func fidelityDecode(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("there is content after the document")
	}
	return doc, nil
}

// urlScheme starts a value that is an address, not a file: data:, artifact:,
// https: and the like.
var urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// inlineFileImages makes a document written for reading from files one that
// Parse takes, which takes only embedded pictures: each src and icon, anywhere
// in the document, that names a file is replaced with that file as a data: URI.
// A file is named relative to dir, the example's directory, and must be inside
// it. A value that is empty or has a scheme is not a file and stays for Parse
// to judge. The kind of a picture is read from its content, not its name.
// Errors name the block and the value.
func inlineFileImages(doc map[string]any, dir string) error {
	return inlineFilesIn(doc, dir, "")
}

func inlineFilesIn(node any, dir, block string) error {
	switch node := node.(type) {
	case map[string]any:
		if id, ok := node["id"].(string); ok {
			block = id
		}
		for _, key := range slices.Sorted(maps.Keys(node)) {
			value, isText := node[key].(string)
			if !isText || (key != "src" && key != "icon") {
				if err := inlineFilesIn(node[key], dir, block); err != nil {
					return err
				}
				continue
			}
			inlined, err := inlineFile(value, dir)
			if err != nil {
				return fmt.Errorf("block %q: %s %q: %w", block, key, value, err)
			}
			node[key] = inlined
		}
	case []any:
		for _, item := range node {
			if err := inlineFilesIn(item, dir, block); err != nil {
				return err
			}
		}
	}
	return nil
}

func inlineFile(value, dir string) (string, error) {
	if value == "" || urlScheme.MatchString(value) {
		return value, nil
	}
	if !filepath.IsLocal(value) {
		return "", errors.New("is not a file inside the example's directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(value)))
	if err != nil {
		return "", err
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		return "", errors.New("is not a PNG, JPEG or GIF picture")
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// pageColour is the colour of the page behind the email: what a screenshot is
// where there is no email.
func pageColour(theme Theme) (color.RGBA, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(theme.Page, "#"), 16, 24)
	if err != nil || len(theme.Page) != 7 {
		return color.RGBA{}, fmt.Errorf("the theme's page colour %q is not #rrggbb", theme.Page)
	}
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}, nil
}

func readPNG(path string) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// chromeCandidates are tried, in order, when fidelityChromeVar is not set.
var chromeCandidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"google-chrome",
	"chromium",
}

// findChrome returns the Chrome or Chromium binary to photograph with:
// fidelityChromeVar when it is set, and then nothing else, so that a mistake in
// it is reported and not hidden by another browser; otherwise the first of
// chromeCandidates there is.
func findChrome() (string, error) {
	if named := os.Getenv(fidelityChromeVar); named != "" {
		path, err := exec.LookPath(named)
		if err != nil {
			return "", fmt.Errorf("%s=%q is not a binary that can run: %w", fidelityChromeVar, named, err)
		}
		return path, nil
	}
	for _, name := range chromeCandidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no Chrome or Chromium found (tried %s); set %s to the binary", strings.Join(chromeCandidates, ", "), fidelityChromeVar)
}

// screenshot has Chrome make a picture of an HTML file, as wide as an email
// and taller than any, in pngPath. Chrome logs a good deal to stderr and its
// exit status says little, so the verdict is the picture: a screenshot exists
// afterwards or it does not. A leftover from an earlier run is removed first so
// that it cannot be mistaken for this one's. Chrome gets a new profile
// directory, removed afterwards, and fidelityChromeTimeout.
func screenshot(ctx context.Context, chrome, htmlPath, pngPath string) error {
	page, err := filepath.Abs(htmlPath)
	if err != nil {
		return err
	}
	shot, err := filepath.Abs(pngPath)
	if err != nil {
		return err
	}
	if err := os.Remove(shot); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	profile, err := os.MkdirTemp("", "emaildoc-fidelity-chrome-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)

	ctx, cancel := context.WithTimeout(ctx, fidelityChromeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
		fmt.Sprintf("--window-size=%d,%d", fidelityWidth, fidelityWindowHeight),
		"--user-data-dir="+profile, "--screenshot="+shot,
		(&url.URL{Scheme: "file", Path: filepath.ToSlash(page)}).String())
	var said bytes.Buffer
	cmd.Stdout, cmd.Stderr = &said, &said
	cmd.WaitDelay = fidelityChromeGrace // Chrome's helpers can outlive it and hold its output open
	runErr := cmd.Run()
	if info, err := os.Stat(shot); err == nil && info.Size() > 0 {
		return nil
	}
	words := strings.TrimSpace(said.String())
	if len(words) > 600 {
		words = "..." + strings.ToValidUTF8(words[len(words)-600:], "")
	}
	return fmt.Errorf("chrome made no screenshot of %s (%v): %s", filepath.Base(page), runErr, words)
}

// fidelityExamples lists the examples in root, by name: the subdirectories
// that hold both email.html and document.json. incomplete lists those that
// hold one of them. Names that begin with a dot are not looked at.
func fidelityExamples(root string) (examples, incomplete []string, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	has := func(dir, name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular()
	}
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if info, err := os.Stat(dir); strings.HasPrefix(entry.Name(), ".") || err != nil || !info.IsDir() {
			continue
		}
		switch hasEmail, hasDocument := has(dir, fidelityOriginalHTML), has(dir, fidelityDocumentJSON); {
		case hasEmail && hasDocument:
			examples = append(examples, entry.Name())
		case hasEmail || hasDocument:
			incomplete = append(incomplete, entry.Name())
		}
	}
	return examples, incomplete, nil
}

// renderExample renders an example's document.json as it would be sent: its
// pictures read from files, no sample values, no resolver for Library
// pictures. It also returns the document's page colour. The document must be a
// layout; a custom HTML email has no theme to say which colour is the page.
func renderExample(ctx context.Context, dir string) (Result, color.RGBA, error) {
	fail := func(err error) (Result, color.RGBA, error) {
		return Result{}, color.RGBA{}, fmt.Errorf("%s: %w", fidelityDocumentJSON, err)
	}
	source, err := os.ReadFile(filepath.Join(dir, fidelityDocumentJSON))
	if err != nil {
		return Result{}, color.RGBA{}, err
	}
	doc, err := fidelityDecode(source)
	if err != nil {
		return fail(err)
	}
	if err := inlineFileImages(doc, dir); err != nil {
		return fail(err)
	}
	inlined, err := json.Marshal(doc)
	if err != nil {
		return fail(err)
	}
	parsed, err := Parse(inlined)
	if err != nil {
		return fail(err)
	}
	if parsed.Mode() != ModeLayout {
		return fail(errors.New("is a custom HTML email; the harness compares layouts, whose theme says which colour is the page"))
	}
	page, err := pageColour(parsed.Theme)
	if err != nil {
		return fail(err)
	}
	result, err := Render(ctx, parsed, nil, RenderOptions{})
	if err != nil {
		return fail(err)
	}
	return result, page, nil
}

// fidelityResult is the comparison of one example.
type fidelityResult struct {
	percent float64 // of the content that differs
	rows    int     // the height of the content
	issues  []Issue // of the render
}

// compareExample renders an example, photographs the original and the render,
// compares the photographs and writes all of it next to the example:
// rendered.html, original.png, rendered.png and diff.png. What an earlier run
// left there is removed first, so that a failure never leaves an old result
// looking like a new one.
func compareExample(ctx context.Context, chrome, dir string) (fidelityResult, error) {
	in := func(name string) string { return filepath.Join(dir, name) }
	for _, name := range []string{fidelityRenderedHTML, fidelityOriginalPNG, fidelityRenderedPNG, fidelityDiffPNG} {
		if err := os.Remove(in(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fidelityResult{}, err
		}
	}
	rendered, page, err := renderExample(ctx, dir)
	if err != nil {
		return fidelityResult{}, err
	}
	if err := os.WriteFile(in(fidelityRenderedHTML), []byte(rendered.HTML), 0o644); err != nil {
		return fidelityResult{}, err
	}
	for _, pair := range [][2]string{{fidelityOriginalHTML, fidelityOriginalPNG}, {fidelityRenderedHTML, fidelityRenderedPNG}} {
		if err := screenshot(ctx, chrome, in(pair[0]), in(pair[1])); err != nil {
			return fidelityResult{}, err
		}
	}
	original, err := readPNG(in(fidelityOriginalPNG))
	if err != nil {
		return fidelityResult{}, err
	}
	rendering, err := readPNG(in(fidelityRenderedPNG))
	if err != nil {
		return fidelityResult{}, err
	}
	percent, rows := pixelDifference(original, rendering, page, fidelityTolerance)
	if rows == 0 {
		return fidelityResult{}, fmt.Errorf("the screenshots have no content to compare: every pixel is near the page colour #%02x%02x%02x", page.R, page.G, page.B)
	}
	if err := writeDiff(in(fidelityDiffPNG), original, rendering, rows, fidelityTolerance); err != nil {
		return fidelityResult{}, err
	}
	return fidelityResult{percent: percent, rows: rows, issues: rendered.Issues}, nil
}

// fidelityMaxPercent reads the most percent of an email that may differ: the
// default when text is empty, otherwise a percentage from 0 up.
func fidelityMaxPercent(text string) (float64, error) {
	if text == "" {
		return fidelityMaxDefault, nil
	}
	limit, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 {
		return 0, fmt.Errorf("%s=%q is not a percentage", fidelityMaxVar, text)
	}
	return limit, nil
}

// TestFidelityAgainstExamples is the harness. It compares a render of each
// example with the email it describes, and fails for an example of which more
// than MEMQL_EMAIL_FIDELITY_MAX_PCT percent of the content (default 2) differs.
// It skips without MEMQL_EMAIL_FIDELITY_DIR, the directory of examples (a
// relative path is taken from this package's directory); scripts/email/
// fidelity.sh sets that and the rest. Each example is a subdirectory with
// email.html, the original email, and document.json, an email document
// written by hand to reproduce it; a picture in the document may be a file
// named relative to the example. The harness writes rendered.html, original.png,
// rendered.png and diff.png into the example's directory and logs, for each
// example, the share that differs and the size of the content it is of.
func TestFidelityAgainstExamples(t *testing.T) {
	dir := os.Getenv(fidelityDirVar)
	if dir == "" {
		t.Skipf("%s is not set; scripts/email/fidelity.sh runs the harness on a directory of example emails", fidelityDirVar)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	limit, err := fidelityMaxPercent(os.Getenv(fidelityMaxVar))
	if err != nil {
		t.Fatal(err)
	}
	chrome, err := findChrome()
	if err != nil {
		t.Fatal(err)
	}
	examples, incomplete, err := fidelityExamples(root)
	if err != nil {
		t.Fatalf("%s: %v", fidelityDirVar, err)
	}
	for _, name := range incomplete {
		t.Logf("%s: skipped; an example needs both %s and %s", name, fidelityOriginalHTML, fidelityDocumentJSON)
	}
	if len(examples) == 0 {
		t.Fatalf("no example in %s; an example is a subdirectory with %s and %s", root, fidelityOriginalHTML, fidelityDocumentJSON)
	}
	t.Logf("chrome %s; at most %g%% may differ", chrome, limit)

	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			got, err := compareExample(t.Context(), chrome, filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, issue := range got.issues {
				t.Logf("%s: %s: %s", name, issue.Code, issue.Message)
			}
			t.Logf("%s: %.2f%% of %dx%d", name, got.percent, fidelityWidth, got.rows)
			if got.percent > limit {
				t.Errorf("%s: %.2f%% differs, more than the %g%% allowed; %s shows where", name, got.percent, limit, filepath.Join(root, name, fidelityDiffPNG))
			}
		})
	}
}

// What follows tests the harness itself. None of it needs Chrome or the
// examples: a stand-in script plays Chrome where Chrome's part is needed.

// The synthetic screenshots are drawn in the neutral theme's page colour and
// its primary colour.
var (
	fidelityPage = color.RGBA{R: 0xee, G: 0xf0, B: 0xf3, A: 0xff}
	fidelityInk  = color.RGBA{R: 0x1f, G: 0x5f, B: 0x8b, A: 0xff}
)

// fidelityFill paints a rectangle of an image in one colour.
func fidelityFill(img draw.Image, r image.Rectangle, c color.RGBA) {
	draw.Draw(img, r, &image.Uniform{C: c}, image.Point{}, draw.Src)
}

// fidelityShot is a synthetic screenshot of a short email: 600 x 120 pixels of
// page colour, with ink over rows 0..99.
func fidelityShot() *image.RGBA { return fidelityShotOn(fidelityPage) }

// fidelityShotOn is fidelityShot on a page of another colour.
func fidelityShotOn(page color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, fidelityWidth, 120))
	fidelityFill(img, img.Bounds(), page)
	fidelityFill(img, image.Rect(0, 0, fidelityWidth, 100), fidelityInk)
	return img
}

// fidelityShifted is fidelityShot with the first 60 pixels of row 10 a delta
// of red away from the ink.
func fidelityShifted(delta uint8) *image.RGBA {
	img := fidelityShot()
	for x := 0; x < 60; x++ {
		c := fidelityInk
		c.R += delta
		img.SetRGBA(x, 10, c)
	}
	return img
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func savePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestFidelityComparator pins the comparison rules of the harness without
// Chrome: the content is every row down to the last that is not the page
// colour in either image, a pixel differs when a channel is more than the
// tolerance away, and the result is the share of the content that differs.
func TestFidelityComparator(t *testing.T) {
	t.Run("sixty differing pixels over a hundred rows are a tenth of a percent", func(t *testing.T) {
		percent, rows := pixelDifference(fidelityShot(), fidelityShifted(0x80), fidelityPage, fidelityTolerance)
		if rows != 100 || !closeTo(percent, 0.1) {
			t.Fatalf("want 0.1%% over 100 rows, got %v%% over %d rows", percent, rows)
		}
	})

	t.Run("a channel difference of ten counts as equal", func(t *testing.T) {
		percent, rows := pixelDifference(fidelityShot(), fidelityShifted(10), fidelityPage, fidelityTolerance)
		if rows != 100 || percent != 0 {
			t.Fatalf("want 0%% over 100 rows, got %v%% over %d rows", percent, rows)
		}
	})
}

// TestFidelityComparatorEdges pins the rules the first test does not reach.
func TestFidelityComparatorEdges(t *testing.T) {
	t.Run("the tolerance itself is equal and one more is not", func(t *testing.T) {
		if percent, _ := pixelDifference(fidelityShot(), fidelityShifted(fidelityTolerance), fidelityPage, fidelityTolerance); percent != 0 {
			t.Errorf("a gap of %d must count as equal, got %v%%", fidelityTolerance, percent)
		}
		if percent, _ := pixelDifference(fidelityShot(), fidelityShifted(fidelityTolerance+1), fidelityPage, fidelityTolerance); !closeTo(percent, 0.1) {
			t.Errorf("a gap of %d must count as different, got %v%%", fidelityTolerance+1, percent)
		}
	})

	t.Run("any one channel is enough", func(t *testing.T) {
		b := fidelityShot()
		b.SetRGBA(0, 0, color.RGBA{R: fidelityInk.R, G: fidelityInk.G, B: fidelityInk.B + 17, A: 0xff})
		b.SetRGBA(1, 0, color.RGBA{R: fidelityInk.R, G: fidelityInk.G + 17, B: fidelityInk.B, A: 0xff})
		if percent, _ := pixelDifference(fidelityShot(), b, fidelityPage, fidelityTolerance); !closeTo(percent, 2*100.0/(600*100)) {
			t.Errorf("a green and a blue gap must each count, got %v%%", percent)
		}
	})

	t.Run("the content is as tall as the taller of the two", func(t *testing.T) {
		tall := fidelityShot()
		fidelityFill(tall, image.Rect(0, 100, 600, 111), fidelityInk)
		for _, order := range [][2]*image.RGBA{{fidelityShot(), tall}, {tall, fidelityShot()}} {
			percent, rows := pixelDifference(order[0], order[1], fidelityPage, fidelityTolerance)
			if rows != 111 || !closeTo(percent, 600*11*100.0/(600*111)) {
				t.Errorf("want 11 differing rows of 111, got %v%% over %d rows", percent, rows)
			}
		}
	})

	t.Run("a shorter screenshot reads as padded with the page", func(t *testing.T) {
		// short is 50 rows of ink. tall has the same 50, then page, then ten
		// more rows of ink, then page again and one ink pixel on row 110. Below
		// its edge short is page, so it differs from tall only where tall has
		// ink: those ten rows, and the pixel.
		short := image.NewRGBA(image.Rect(0, 0, 600, 50))
		fidelityFill(short, short.Bounds(), fidelityInk)
		tall := image.NewRGBA(image.Rect(0, 0, 600, 120))
		fidelityFill(tall, tall.Bounds(), fidelityPage)
		fidelityFill(tall, image.Rect(0, 0, 600, 50), fidelityInk)
		fidelityFill(tall, image.Rect(0, 60, 600, 70), fidelityInk)
		tall.SetRGBA(7, 110, fidelityInk)
		want := (600*10 + 1) * 100.0 / (600 * 111)
		for _, order := range [][2]image.Image{{short, tall}, {tall, short}} {
			percent, rows := pixelDifference(order[0], order[1], fidelityPage, fidelityTolerance)
			if rows != 111 || !closeTo(percent, want) {
				t.Errorf("want %v%% over 111 rows, got %v%% over %d rows", want, percent, rows)
			}
		}
	})

	t.Run("only the width of the email is compared", func(t *testing.T) {
		wide := func() *image.RGBA {
			img := image.NewRGBA(image.Rect(0, 0, 700, 120))
			fidelityFill(img, img.Bounds(), fidelityPage)
			fidelityFill(img, image.Rect(0, 0, 600, 100), fidelityInk)
			return img
		}
		b := wide()
		b.SetRGBA(650, 10, fidelityMark)
		if percent, rows := pixelDifference(wide(), b, fidelityPage, fidelityTolerance); percent != 0 || rows != 100 {
			t.Errorf("want 0%% over 100 rows, got %v%% over %d rows", percent, rows)
		}
	})

	t.Run("no content is nothing to compare", func(t *testing.T) {
		blank := image.NewRGBA(image.Rect(0, 0, 600, 120))
		fidelityFill(blank, blank.Bounds(), fidelityPage)
		if percent, rows := pixelDifference(blank, blank, fidelityPage, fidelityTolerance); percent != 0 || rows != 0 {
			t.Errorf("want 0%% over 0 rows, got %v%% over %d rows", percent, rows)
		}
	})
}

func TestFidelityWriteDiff(t *testing.T) {
	a, b := fidelityShot(), fidelityShot()
	b.SetRGBA(3, 7, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
	b.SetRGBA(5, 7, color.RGBA{R: fidelityInk.R + fidelityTolerance, G: fidelityInk.G, B: fidelityInk.B, A: 0xff}) // within the tolerance

	path := filepath.Join(t.TempDir(), "diff.png")
	if err := writeDiff(path, a, b, 100, fidelityTolerance); err != nil {
		t.Fatal(err)
	}
	got, err := readPNG(path)
	if err != nil {
		t.Fatal(err)
	}
	if size := got.Bounds().Size(); size != (image.Point{X: 600, Y: 100}) {
		t.Fatalf("want a 600x100 picture of the content, got %v", size)
	}
	red := color.RGBA{R: 0xff, A: 0xff}
	if at := pixelAt(got, 3, 7, fidelityPage); at != red {
		t.Errorf("a differing pixel must be red, got %v", at)
	}
	same := pixelAt(got, 4, 7, fidelityPage)
	if same == red || same == fidelityInk || same.R < fidelityInk.R || same.G < fidelityInk.G || same.B < fidelityInk.B {
		t.Errorf("a pixel that agrees must be a paler copy of the original (%v), got %v", fidelityInk, same)
	}
	if at := pixelAt(got, 5, 7, fidelityPage); at == red {
		t.Errorf("a pixel within the tolerance must not be red")
	}

	// Rows past the screenshots and no rows at all still make a picture.
	for _, rows := range []int{130, 0} {
		path := filepath.Join(t.TempDir(), "diff.png")
		if err := writeDiff(path, a, b, rows, fidelityTolerance); err != nil {
			t.Fatalf("%d rows: %v", rows, err)
		}
		img, err := readPNG(path)
		if err != nil {
			t.Fatal(err)
		}
		if height := img.Bounds().Dy(); height != max(rows, 1) {
			t.Errorf("%d rows: want a picture %d high, got %d", rows, max(rows, 1), height)
		}
	}

	if err := writeDiff(filepath.Join(t.TempDir(), "no", "such", "dir.png"), a, b, 100, fidelityTolerance); err == nil {
		t.Error("an unwritable path must be an error")
	}
}

func TestFidelityInlineFileImages(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "example")
	small := image.NewRGBA(image.Rect(0, 0, 4, 2))
	fidelityFill(small, small.Bounds(), fidelityInk)
	var asJPEG, asGIF bytes.Buffer
	if err := jpeg.Encode(&asJPEG, small, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&asGIF, small, nil); err != nil {
		t.Fatal(err)
	}
	asPNG := tinyPNG(t, 4, 2)
	for name, data := range map[string][]byte{
		filepath.Join(dir, "images", "banner.png"):      asPNG,
		filepath.Join(dir, "images", "photo.jpg"):       asJPEG.Bytes(),
		filepath.Join(dir, "images", "mislabelled.gif"): asPNG, // a PNG with the name of a GIF: the content decides
		filepath.Join(dir, "icons", "x.gif"):            asGIF.Bytes(),
		filepath.Join(dir, "notes.txt"):                 []byte("not a picture"),
		filepath.Join(root, "outside.png"):              asPNG,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	uri := func(mime string, data []byte) string {
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	}

	source := `{"subject": "s", "blocks": [
		{"id": "banner", "type": "image", "src": "images/banner.png", "width": 600, "height": 12.50},
		{"id": "photo", "type": "image", "src": "images/photo.jpg"},
		{"id": "mislabelled", "type": "image", "src": "images/mislabelled.gif"},
		{"id": "cols", "type": "columns", "columns": [
			{"width": 50, "blocks": [{"id": "library", "type": "image", "src": "artifact:abc"}]},
			{"width": 50, "blocks": [{"id": "social", "type": "social", "links": [
				{"network": "x", "href": "https://x.com/a", "icon": "icons/x.gif", "width": 32}]}]}]},
		{"id": "empty", "type": "image", "src": ""},
		{"id": "embedded", "type": "image", "src": "data:image/png;base64,AAAA"},
		{"id": "web", "type": "image", "src": "https://example.com/images/banner.png"},
		{"id": "words", "type": "text", "text": "images/banner.png"}]}`
	doc, err := fidelityDecode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := inlineFileImages(doc, dir); err != nil {
		t.Fatal(err)
	}
	// The files named by a src or an icon are now pictures of the kind they
	// are, and nothing else has changed.
	want, err := fidelityDecode([]byte(strings.NewReplacer(
		`"images/banner.png", "width"`, `"`+uri("image/png", asPNG)+`", "width"`,
		`"images/photo.jpg"`, `"`+uri("image/jpeg", asJPEG.Bytes())+`"`,
		`"images/mislabelled.gif"`, `"`+uri("image/png", asPNG)+`"`,
		`"icons/x.gif"`, `"`+uri("image/gif", asGIF.Bytes())+`"`,
	).Replace(source)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, want) {
		got, _ := json.Marshal(doc)
		t.Errorf("unexpected document:\n%s", got)
	}
	// Numbers reach Parse as they were written: 12.50 is still 12.50.
	if written, err := json.Marshal(doc); err != nil || !strings.Contains(string(written), `"height":12.50`) {
		t.Errorf("a number must be written as it was read (%v): %s", err, written)
	}

	for _, c := range []struct{ name, src, want string }{
		{"a file that is not there", "images/missing.png", "no such file"},
		{"a file that is not a picture", "notes.txt", "not a PNG, JPEG or GIF"},
		{"a file outside the example", "../outside.png", "inside the example's directory"},
		{"an absolute path", filepath.Join(root, "outside.png"), "inside the example's directory"},
	} {
		doc, err := fidelityDecode([]byte(`{"blocks": [{"id": "hero", "type": "image", "src": ` + jsonString(c.src) + `}]}`))
		if err != nil {
			t.Fatal(err)
		}
		err = inlineFileImages(doc, dir)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), `block "hero"`) {
			t.Errorf("%s: want an error naming the block and saying %q, got %v", c.name, c.want, err)
		}
	}
}

func TestFidelityDecodeIsStrict(t *testing.T) {
	for name, source := range map[string]string{
		"not json":         `{"subject": `,
		"not an object":    `["subject"]`,
		"trailing content": `{"subject": "s"} {"subject": "t"}`,
	} {
		if _, err := fidelityDecode([]byte(source)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFidelityPageColour(t *testing.T) {
	for page, want := range map[string]color.RGBA{
		"#eef0f3": {R: 0xee, G: 0xf0, B: 0xf3, A: 0xff},
		"#EEF0F3": {R: 0xee, G: 0xf0, B: 0xf3, A: 0xff},
		"#000000": {A: 0xff},
		"#0a1b2c": {R: 0x0a, G: 0x1b, B: 0x2c, A: 0xff},
	} {
		if got, err := pageColour(Theme{Page: page}); err != nil || got != want {
			t.Errorf("%s: want %v, got %v and %v", page, want, got, err)
		}
	}
	for _, page := range []string{"", "eef0f3", "#eef0f", "#eef0f3a", "#gggggg", "#-eef0f"} {
		if got, err := pageColour(Theme{Page: page}); err == nil {
			t.Errorf("%q: want an error, got %v", page, got)
		}
	}
}

func TestFidelityMaxPercent(t *testing.T) {
	for text, want := range map[string]float64{"": 2, "0": 0, "0.5": 0.5, "2": 2, "12.25": 12.25, "100": 100} {
		if got, err := fidelityMaxPercent(text); err != nil || got != want {
			t.Errorf("%q: want %v, got %v and %v", text, want, got, err)
		}
	}
	for _, text := range []string{"abc", "-1", "-0.5", "NaN", "Inf", "+Inf", "1e999", "5%", " 2"} {
		if got, err := fidelityMaxPercent(text); err == nil || !strings.Contains(err.Error(), fidelityMaxVar) {
			t.Errorf("%q: want an error naming %s, got %v and %v", text, fidelityMaxVar, got, err)
		}
	}
}

// fakeChrome writes a stand-in for Chrome and returns its path. The script has
// read the --screenshot= argument into $shot and the page's address, without
// its scheme, into $page when body runs.
func fakeChrome(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for Chrome is a shell script")
	}
	script := `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	--screenshot=*) shot="${arg#--screenshot=}" ;;
	file://*) page="${arg#file://}" ;;
	esac
done
` + body
	path := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFidelityFindChrome(t *testing.T) {
	stand := fakeChrome(t, "")
	candidates := func(t *testing.T, names ...string) {
		t.Helper()
		before := chromeCandidates
		t.Cleanup(func() { chromeCandidates = before })
		chromeCandidates = names
	}

	t.Run("the variable names the binary", func(t *testing.T) {
		t.Setenv(fidelityChromeVar, stand)
		candidates(t, "/no/such/chrome")
		if got, err := findChrome(); err != nil || got != stand {
			t.Errorf("want %s, got %q and %v", stand, got, err)
		}
	})
	t.Run("a variable that names nothing is an error, not a reason to look elsewhere", func(t *testing.T) {
		t.Setenv(fidelityChromeVar, filepath.Join(t.TempDir(), "no-chrome"))
		candidates(t, stand)
		if got, err := findChrome(); err == nil || !strings.Contains(err.Error(), fidelityChromeVar) {
			t.Errorf("want an error naming %s, got %q and %v", fidelityChromeVar, got, err)
		}
	})
	t.Run("without the variable the first candidate that exists wins", func(t *testing.T) {
		t.Setenv(fidelityChromeVar, "")
		candidates(t, "/no/such/chrome", stand, "/also/missing")
		if got, err := findChrome(); err != nil || got != stand {
			t.Errorf("want %s, got %q and %v", stand, got, err)
		}
	})
	t.Run("none is an error that names the variable", func(t *testing.T) {
		t.Setenv(fidelityChromeVar, "")
		candidates(t, "/no/such/chrome", "no-such-chromium-anywhere")
		if got, err := findChrome(); err == nil || !strings.Contains(err.Error(), fidelityChromeVar) {
			t.Errorf("want an error naming %s, got %q and %v", fidelityChromeVar, got, err)
		}
	})
}

func TestFidelityScreenshot(t *testing.T) {
	// The files the subtests share live in this test's directory, whose name
	// holds nothing a file: address would write differently.
	base := t.TempDir()
	quote := func(path string) string { return "'" + path + "'" }
	fixture := filepath.Join(base, "fixture.png")
	savePNG(t, fixture, image.NewRGBA(image.Rect(0, 0, 2, 2)))

	t.Run("chrome is given the window, a profile of its own and the page", func(t *testing.T) {
		args := filepath.Join(base, "args")
		chrome := fakeChrome(t, `printf '%s\n' "$@" > `+quote(args)+`
cp `+quote(fixture)+` "$shot"
echo '[0101/000000.000000:ERROR:gpu_process_host.cc(1)] noise on stderr' >&2
exit 1`) // Chrome's exit status is not the verdict; the picture is
		dir := filepath.Join(base, "with space")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		html, shot := filepath.Join(dir, "email.html"), filepath.Join(dir, "email.png")
		if err := os.WriteFile(html, []byte("<p>hi</p>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := screenshot(t.Context(), chrome, html, shot); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(shot); err != nil {
			t.Fatal(err)
		}
		recorded, err := os.ReadFile(args)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
		wantStatic := []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1", "--window-size=600,6000"}
		if len(got) != len(wantStatic)+3 || !reflect.DeepEqual(got[:len(wantStatic)], wantStatic) {
			t.Fatalf("unexpected arguments: %q", got)
		}
		profile, ok := strings.CutPrefix(got[5], "--user-data-dir=")
		if !ok || profile == "" || strings.HasPrefix(profile, dir) {
			t.Errorf("want a profile directory of its own, got %q", got[5])
		}
		if _, err := os.Stat(profile); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the profile directory must be removed afterwards (%v)", err)
		}
		if got[6] != "--screenshot="+shot {
			t.Errorf("want --screenshot=%s, got %s", shot, got[6])
		}
		if want := "file://" + strings.ReplaceAll(html, " ", "%20"); got[7] != want {
			t.Errorf("want the page %s, got %s", want, got[7])
		}
	})

	t.Run("no picture is an error that carries what chrome said", func(t *testing.T) {
		chrome := fakeChrome(t, "echo 'boom: could not start' >&2\nexit 3")
		err := screenshot(t.Context(), chrome, "email.html", filepath.Join(t.TempDir(), "email.png"))
		if err == nil || !strings.Contains(err.Error(), "boom: could not start") || !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("want chrome's words and status in the error, got %v", err)
		}
	})

	t.Run("an empty file is not a picture", func(t *testing.T) {
		chrome := fakeChrome(t, `: > "$shot"`)
		if err := screenshot(t.Context(), chrome, "email.html", filepath.Join(t.TempDir(), "email.png")); err == nil {
			t.Error("a file with nothing in it must not count as a screenshot")
		}
	})

	t.Run("a picture left by an earlier run is not this run's", func(t *testing.T) {
		shot := filepath.Join(t.TempDir(), "email.png")
		if err := os.WriteFile(shot, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		chrome := fakeChrome(t, "exit 0") // makes no picture
		if err := screenshot(t.Context(), chrome, "email.html", shot); err == nil {
			t.Error("the leftover must not be taken for a screenshot")
		}
		if _, err := os.Stat(shot); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the leftover must be removed (%v)", err)
		}
	})

	t.Run("chrome that outlives its deadline is stopped", func(t *testing.T) {
		before := fidelityChromeTimeout
		t.Cleanup(func() { fidelityChromeTimeout = before })
		fidelityChromeTimeout = 200 * time.Millisecond
		chrome := fakeChrome(t, "exec sleep 30")
		start := time.Now()
		err := screenshot(t.Context(), chrome, "email.html", filepath.Join(t.TempDir(), "email.png"))
		if err == nil || time.Since(start) > 10*time.Second {
			t.Errorf("want an error well before chrome is done, got %v after %v", err, time.Since(start))
		}
	})
}

func TestFidelityExamplesAreTheDirectoriesWithBothFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b/email.html", "a/email.html", "a/document.json", "c/email.html", "c/document.json", ".hidden/email.html", ".hidden/document.json", "only-document/document.json", "notes.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	examples, incomplete, err := fidelityExamples(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(examples, want) {
		t.Errorf("want examples %v, got %v", want, examples)
	}
	if want := []string{"b", "only-document"}; !reflect.DeepEqual(incomplete, want) {
		t.Errorf("want incomplete %v, got %v", want, incomplete)
	}
	if _, _, err := fidelityExamples(filepath.Join(root, "missing")); err == nil {
		t.Error("a directory that is not there must be an error")
	}
}

func TestFidelityExample(t *testing.T) {
	article, ok := Layout("builtin:article")
	if !ok {
		t.Fatal("no article layout")
	}
	documentJSON, err := article.JSON()
	if err != nil {
		t.Fatal(err)
	}
	page, err := pageColour(article.Theme)
	if err != nil {
		t.Fatal(err)
	}

	// The stand-in for Chrome "takes" the same two screenshots every time: the
	// original's and the render's, which differ in 60 pixels over 100 rows.
	// takes says which of the files in fixtures it takes for which page.
	fixtures := t.TempDir()
	original, rendered, blank := fidelityShotOn(page), fidelityShotOn(page), image.NewRGBA(image.Rect(0, 0, 600, 120))
	for x := 0; x < 60; x++ {
		rendered.SetRGBA(x, 10, fidelityMark)
	}
	fidelityFill(blank, blank.Bounds(), page)
	savePNG(t, filepath.Join(fixtures, "original.png"), original)
	savePNG(t, filepath.Join(fixtures, "rendered.png"), rendered)
	savePNG(t, filepath.Join(fixtures, "blank.png"), blank)
	takes := func(forOriginal, forRendered string) string {
		return `case "$page" in
*/rendered.html) cp '` + filepath.Join(fixtures, forRendered) + `' "$shot" ;;
*) cp '` + filepath.Join(fixtures, forOriginal) + `' "$shot" ;;
esac`
	}
	newExample := func(t *testing.T, document []byte) string {
		t.Helper()
		dir := t.TempDir()
		for name, data := range map[string][]byte{"email.html": []byte("<p>the original</p>"), "document.json": document} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	t.Run("an example is rendered, photographed and compared", func(t *testing.T) {
		dir := newExample(t, documentJSON)
		got, err := compareExample(t.Context(), fakeChrome(t, takes("original.png", "rendered.png")), dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.rows != 100 || !closeTo(got.percent, 0.1) {
			t.Errorf("want 0.1%% over 100 rows, got %v%% over %d rows", got.percent, got.rows)
		}
		// The render is the one a direct Render of the same layout makes.
		want, err := Render(t.Context(), article, nil, RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(filepath.Join(dir, "rendered.html"))
		if err != nil || string(written) != want.HTML {
			t.Errorf("rendered.html must hold the send-mode render (read: %v)", err)
		}
		if !reflect.DeepEqual(got.issues, want.Issues) {
			t.Errorf("want the render's issues %v, got %v", want.Issues, got.issues)
		}
		for _, name := range []string{"original.png", "rendered.png", "diff.png"} {
			if _, err := readPNG(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if diff, err := readPNG(filepath.Join(dir, "diff.png")); err != nil || diff.Bounds().Dy() != 100 {
			t.Errorf("want a diff of the 100 rows of content (%v)", err)
		}
	})

	t.Run("a document that is not a layout is refused and leaves no stale result", func(t *testing.T) {
		dir := newExample(t, []byte(`{"subject": "s", "textBody": "hi", "htmlBody": "<p>hi</p>"}`))
		stale := filepath.Join(dir, "diff.png")
		if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := compareExample(t.Context(), fakeChrome(t, takes("original.png", "rendered.png")), dir)
		if err == nil || !strings.Contains(err.Error(), "custom") {
			t.Errorf("want an error saying the document is custom HTML, got %v", err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an earlier run's diff must not outlive this run (%v)", err)
		}
	})

	t.Run("a picture that is not there names the block", func(t *testing.T) {
		document := strings.Replace(string(documentJSON), `"id": "banner",`, `"id": "banner", "src": "images/missing.png",`, 1)
		if document == string(documentJSON) {
			t.Fatal("the article has no banner block to give a picture")
		}
		dir := newExample(t, []byte(document))
		_, err := compareExample(t.Context(), fakeChrome(t, takes("original.png", "rendered.png")), dir)
		if err == nil || !strings.Contains(err.Error(), "document.json") || !strings.Contains(err.Error(), `block "banner"`) {
			t.Errorf("want an error naming the file and the block, got %v", err)
		}
	})

	t.Run("two blank screenshots compare nothing and say so", func(t *testing.T) {
		dir := newExample(t, documentJSON)
		_, err := compareExample(t.Context(), fakeChrome(t, takes("blank.png", "blank.png")), dir)
		if err == nil || !strings.Contains(err.Error(), "no content") {
			t.Errorf("want an error saying there is no content to compare, got %v", err)
		}
	})
}
