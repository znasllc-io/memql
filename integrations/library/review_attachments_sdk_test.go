package library

import (
	"bytes"
	"context"
	"encoding/base64"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"image"
	"image/gif"
	"image/jpeg"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The SDK's hand-off (sdk/ts/src/client/materialize.ts) does not trust the
// Library to take whatever a person lists as a reference. It applies the
// Library's review-attachment rules itself, from a file's name and size, so a
// PDF or an over-sized image is left off the comment and reported instead of
// getting the whole comment refused. And for what only the bytes can show (a
// ".md" file that is not UTF-8), it sends the comment once more without
// attachments when the Library's refusal is about attachments, which it
// recognises by the word "attach".
//
// Both are copies of this package's behaviour in another language, and a copy
// that drifts fails quietly: a limit the Library raised leaves references off
// that it would have taken; one it lowered gets the whole comment refused; a
// refusal reworded without "attach" turns the retry off and a person's
// description is lost to a stray attachment. These tests hold the copies to the
// source.
//
// There is a third copy, and the server's decision rests on it. Which described
// Markdown requests take the Library's revision workflow is decided on this same
// per-file rule (describedMarkdown in integrations/compose), and compose may not
// import this package, so it declares the limits and the picture formats again in
// described.go. TestComposeMirrorsTheReviewAttachmentRules holds those to the
// constants here, and a limit that drifts is a request sent to the revision
// workflow with a file it refuses, or sent to the composer with one it would have
// taken.
//
// The person's client makes that same decision from the same rows. The SDK's
// isDescribedMarkdown applies the rule through attachmentKind, the function the
// limits above feed, rather than a list of its own, so the SDK holds one copy of
// the rule and not two; TestSDKDecidesTheDescribedPathWithTheHandOffsRule holds
// it to that. A client that decided otherwise would hand on a description the
// server had composed (a second draft over the first), or leave a file filed
// empty with its description unsent.

const sdkMaterializeSource = "../../sdk/ts/src/client/materialize.ts"

// The server's copy of the rule.
const composeDescribedSource = "../compose/described.go"

// The rules as the SDK states them.
type sdkMirror struct {
	numbers    map[string]int // MAX_* constants, shifts evaluated
	images     []string       // IMAGE_EXTENSIONS
	refusal    *regexp.Regexp // ATTACHMENT_REFUSAL
	handOffID  *regexp.Regexp // HAND_OFF_ID: the request ids a described request may use
	handOffMin int            // its shortest and longest length, read from its {min,max}
	handOffMax int
	suffixes   []string // whatever the hand-off appends to a request id, as written: "-comment", "-revision"
}

var (
	sdkNumberConst = regexp.MustCompile(`(?m)^const (MAX_[A-Z_]+) = (\d+)(?: << (\d+))?;`)
	sdkImagesConst = regexp.MustCompile(`(?m)^const IMAGE_EXTENSIONS = new Set\(\[([^\]]*)\]\);`)
	sdkRefusal     = regexp.MustCompile(`(?m)^const ATTACHMENT_REFUSAL = /([^/\n]+)/([a-z]*);`)
	sdkQuoted      = regexp.MustCompile(`"([^"]*)"`)
	sdkHandOffID   = regexp.MustCompile(`(?m)^const HAND_OFF_ID = /([^/\n]+)/;`)
	sdkBounds      = regexp.MustCompile(`\{(\d+),(\d+)\}\$$`)
	sdkSuffix      = regexp.MustCompile("\\$\\{requestId\\}([^`$]*)`")
)

// readSDKMirror reads the SDK's copy of the rules. Go's test cache does not notice
// the file changing, so a test that calls this passes from the cache after the SDK
// is edited: run these tests with -count=1.
func readSDKMirror(t *testing.T) sdkMirror {
	t.Helper()
	raw, err := os.ReadFile(sdkMaterializeSource)
	if err != nil {
		t.Fatalf("the SDK hand-off is not at %s (%v): if it moved, update sdkMaterializeSource -- this test is what keeps its copy of the Library's attachment rules honest", sdkMaterializeSource, err)
	}
	src := string(raw)
	mirror := sdkMirror{numbers: map[string]int{}}
	for _, m := range sdkNumberConst.FindAllStringSubmatch(src, -1) {
		value, _ := strconv.Atoi(m[2])
		if m[3] != "" {
			shift, _ := strconv.Atoi(m[3])
			value <<= shift
		}
		mirror.numbers[m[1]] = value
	}
	images := sdkImagesConst.FindStringSubmatch(src)
	if images == nil {
		t.Fatal("IMAGE_EXTENSIONS is gone from the SDK hand-off, or no longer written as `new Set([...])` on one line; update this test with it")
	}
	for _, q := range sdkQuoted.FindAllStringSubmatch(images[1], -1) {
		mirror.images = append(mirror.images, q[1])
	}
	refusal := sdkRefusal.FindStringSubmatch(src)
	if refusal == nil {
		t.Fatal("ATTACHMENT_REFUSAL is gone from the SDK hand-off, or no longer a one-line regular expression; update this test with it")
	}
	if strings.Trim(refusal[2], "i") != "" {
		t.Fatalf("ATTACHMENT_REFUSAL uses flags %q; this test understands only i", refusal[2])
	}
	pattern := refusal[1]
	if strings.Contains(refusal[2], "i") {
		pattern = "(?i)" + pattern
	}
	mirror.refusal = regexp.MustCompile(pattern)
	handOff := sdkHandOffID.FindStringSubmatch(src)
	if handOff == nil {
		t.Fatal("HAND_OFF_ID is gone from the SDK hand-off, or no longer a one-line regular expression; update this test with it")
	}
	bounds := sdkBounds.FindStringSubmatch(handOff[1])
	if bounds == nil {
		t.Fatalf("HAND_OFF_ID %q no longer ends in a {min,max} length; update this test with it", handOff[1])
	}
	mirror.handOffMin, _ = strconv.Atoi(bounds[1])
	mirror.handOffMax, _ = strconv.Atoi(bounds[2])
	mirror.handOffID = regexp.MustCompile(handOff[1])
	for _, m := range sdkSuffix.FindAllStringSubmatch(src, -1) {
		if !slices.Contains(mirror.suffixes, m[1]) {
			mirror.suffixes = append(mirror.suffixes, m[1])
		}
	}
	return mirror
}

// It reads the SDK's source, which Go's test cache does not notice changing: run it with -count=1 after editing that file.
func TestSDKMirrorsTheReviewAttachmentRules(t *testing.T) {
	mirror := readSDKMirror(t)

	want := map[string]int{
		"MAX_ATTACHMENTS":          reviewAttachmentCount,
		"MAX_ATTACHMENT_BYTES":     reviewAttachmentBytes,
		"MAX_MARKDOWN_BYTES":       reviewMarkdownBytes,
		"MAX_MARKDOWN_TOTAL_BYTES": reviewMarkdownTotalBytes,
		"MAX_IMAGE_BYTES":          reviewImageBytes,
	}
	for name, limit := range want {
		got, ok := mirror.numbers[name]
		if !ok {
			t.Errorf("%s is gone from the SDK hand-off: it mirrors a limit the Library enforces (%d)", name, limit)
			continue
		}
		if got != limit {
			t.Errorf("the SDK hand-off has %s = %d and the Library enforces %d: they must be the same, or the hand-off leaves off references the Library would take, or sends ones it refuses", name, got, limit)
		}
	}
	for name := range mirror.numbers {
		if _, ok := want[name]; !ok {
			t.Errorf("the SDK hand-off has a limit %s that this test does not hold to the Library: add the Go constant it mirrors", name)
		}
	}

	wantImages := []string{}
	for _, format := range reviewImageFormats {
		wantImages = append(wantImages, "."+format)
	}
	gotImages := append([]string{}, mirror.images...)
	sort.Strings(wantImages)
	sort.Strings(gotImages)
	if !slices.Equal(gotImages, wantImages) {
		t.Errorf("the SDK hand-off takes images with the extensions %v and the Library takes %v (reviewImageFormats, with .jpg read as .jpeg)", gotImages, wantImages)
	}
}

// compose's copy of the per-file rule (integrations/compose/described.go) states the two
// byte limits and the picture formats as top-level declarations, so this test can read them
// off the source without importing a package that may not be imported from there. The limits
// are constant expressions and are evaluated, so `32 << 10` and `32768` are the same limit; the
// formats are a list of string literals, compared as a set.
//
// It reads compose's source, which Go's test cache does not notice changing: run it with -count=1 after editing that file.
func TestComposeMirrorsTheReviewAttachmentRules(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, composeDescribedSource, nil, 0)
	if err != nil {
		t.Fatalf("compose's copy of the Library's attachment rules is not at %s (%v): if it moved, update composeDescribedSource -- this test is what keeps that copy honest", composeDescribedSource, err)
	}
	declared := func(name string) ast.Expr {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && len(value.Values) == 1 && len(value.Names) == 1 && value.Names[0].Name == name {
					return value.Values[0]
				}
			}
		}
		return nil
	}

	for name, limit := range map[string]int{
		"libraryAttachMarkdownBytes": reviewMarkdownBytes,
		"libraryAttachImageBytes":    reviewImageBytes,
	} {
		expr := declared(name)
		if expr == nil {
			t.Errorf("%s is gone from %s, or is no longer a top-level declaration with one value: it is compose's copy of a limit the Library enforces (%d), which decides whether a described request is sent to the revision workflow. Update this test with it", name, composeDescribedSource, limit)
			continue
		}
		text := types.ExprString(expr)
		evaluated, err := types.Eval(token.NewFileSet(), nil, token.NoPos, text)
		if err != nil || evaluated.Value == nil {
			t.Errorf("%s = %s in %s, which this test cannot evaluate as a constant (%v): state it as a constant expression, such as 32 << 10", name, text, composeDescribedSource, err)
			continue
		}
		got, exact := constant.Int64Val(constant.ToInt(evaluated.Value))
		if !exact {
			t.Errorf("%s = %s in %s is not a whole number of bytes", name, text, composeDescribedSource)
			continue
		}
		if got != int64(limit) {
			t.Errorf("compose has %s = %s = %d and the Library enforces %d: they must be the same, or a described request is sent to the revision workflow with a file it refuses, or kept from it with a file it would take", name, text, got, limit)
		}
	}

	formats := declared("libraryAttachImageFormats")
	list, ok := formats.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("libraryAttachImageFormats is gone from %s, or is no longer a list of string literals: it is compose's copy of reviewImageFormats. Update this test with it", composeDescribedSource)
	}
	got := []string{}
	for _, element := range list.Elts {
		literal, ok := element.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Fatalf("libraryAttachImageFormats in %s holds something other than a string literal; this test reads only literals", composeDescribedSource)
		}
		format, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, format)
	}
	want := slices.Clone(reviewImageFormats)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("compose takes the picture formats %v and the Library takes %v (reviewImageFormats; \".jpg\" is read as \".jpeg\" by both): they must be the same", got, want)
	}
}

// The SDK decides whether a request is a described one with the hand-off's own rule: the body
// of isDescribedMarkdown calls attachmentKind (the function MAX_MARKDOWN_BYTES, MAX_IMAGE_BYTES
// and IMAGE_EXTENSIONS feed, which TestSDKMirrorsTheReviewAttachmentRules holds to the
// Library's), and readDescribedMarkdown is exported for a client that must read the rows first.
// A second list of types and sizes in the predicate would be a copy nothing holds.
//
// It reads the SDK's source, which Go's test cache does not notice changing: run it with -count=1 after editing that file.
func TestSDKDecidesTheDescribedPathWithTheHandOffsRule(t *testing.T) {
	raw, err := os.ReadFile(sdkMaterializeSource)
	if err != nil {
		t.Fatalf("the SDK hand-off is not at %s (%v): if it moved, update sdkMaterializeSource -- this test is what keeps its decision of the described path honest", sdkMaterializeSource, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "export function isDescribedMarkdown(") })
	if start < 0 {
		t.Fatal("isDescribedMarkdown is gone from the SDK hand-off, or no longer begins a line with `export function isDescribedMarkdown(`; update this test with it")
	}
	end := slices.Index(lines[start:], "}")
	if end < 0 {
		t.Fatal("isDescribedMarkdown has no closing brace on a line of its own in the SDK hand-off; update this test with it")
	}
	body := strings.Join(lines[start:start+end+1], "\n")
	if !strings.Contains(body, "attachmentKind(") {
		t.Errorf("isDescribedMarkdown no longer calls attachmentKind in its own body. The SDK must decide the described path with the same rule the hand-off applies to a reference (the function the limits mirrored above feed), or its answer can differ from the server's and from what the hand-off will attach:\n%s", body)
	}
	if !regexp.MustCompile(`(?m)^export (async )?function readDescribedMarkdown\(`).Match(raw) {
		t.Error("readDescribedMarkdown is not exported from the SDK hand-off: a client that does not know the picked files' rows has no way to decide the path before it starts")
	}
}

// The hand-off sends a described file's description under two ids derived from the
// request id (`<id>-comment`, `<id>-revision`), and the Library accepts only
// reviewRequestID in either. The SDK refuses a described request whose id would
// make one of them too long, before the file is made (HAND_OFF_ID), and that is a
// copy of this package's rule: the same characters, and a longest id that is the
// Library's longest less the longest suffix. A copy that drifts is a described
// request that makes its file and then cannot send its description, or an id
// refused that the Library would have taken.
//
// It reads the SDK's source, which Go's test cache does not notice changing: run it with -count=1 after editing that file.
func TestSDKHandOffIdsAreIdsTheLibraryAccepts(t *testing.T) {
	mirror := readSDKMirror(t)
	bounds := sdkBounds.FindStringSubmatch(reviewRequestID.String())
	if bounds == nil {
		t.Fatalf("reviewRequestID %q no longer ends in a {min,max} length; update this test with it", reviewRequestID)
	}
	libMin, _ := strconv.Atoi(bounds[1])
	libMax, _ := strconv.Atoi(bounds[2])

	if len(mirror.suffixes) == 0 {
		t.Fatal("found no `${requestId}-suffix` in the SDK hand-off, so the ids it derives are not held to anything; update this test with it")
	}
	longest, shortest := 0, 1<<30
	for _, suffix := range mirror.suffixes {
		longest, shortest = max(longest, len(suffix)), min(shortest, len(suffix))
	}

	// The same characters. Every printable ASCII character, and a few that are not.
	chars := []rune{0, '\t', '\n', 0xe9, 0xa0, 0x2013, 0x1f600}
	for c := rune(0x20); c <= 0x7e; c++ {
		chars = append(chars, c)
	}
	for _, c := range chars {
		sdk := mirror.handOffID.MatchString("a" + string(c))
		library := reviewRequestID.MatchString(strings.Repeat("a", libMin-1) + string(c))
		if sdk != library {
			t.Errorf("%q: the SDK accepts it in a request id = %v, the Library accepts it in one = %v", c, sdk, library)
		}
	}

	// The longest and the shortest.
	if mirror.handOffMax+longest != libMax {
		t.Errorf("the SDK allows request ids of up to %d, and the longest id it derives adds %d (%v): that is %d, and the Library's longest is %d. HAND_OFF_ID's maximum is the Library's less the longest suffix", mirror.handOffMax, longest, mirror.suffixes, mirror.handOffMax+longest, libMax)
	}
	if mirror.handOffMin+shortest < libMin {
		t.Errorf("the SDK allows request ids as short as %d, and the shortest id it derives adds %d (%v): that is below the Library's shortest, %d", mirror.handOffMin, shortest, mirror.suffixes, libMin)
	}

	// And the real expressions agree at the edge: the longest id the SDK allows
	// derives ids the Library takes, and one more does not pass either.
	edge := strings.Repeat("a", mirror.handOffMax)
	if !mirror.handOffID.MatchString(edge) || mirror.handOffID.MatchString(edge+"a") {
		t.Errorf("HAND_OFF_ID does not stop at %d characters", mirror.handOffMax)
	}
	for _, suffix := range mirror.suffixes {
		if !reviewRequestID.MatchString(edge + suffix) {
			t.Errorf("the Library refuses %d characters plus %q, which the SDK would send", mirror.handOffMax, suffix)
		}
		if suffix == "-revision" && reviewRequestID.MatchString(edge+"a"+suffix) {
			t.Errorf("the Library takes one character more than the SDK allows: its maximum is wider than HAND_OFF_ID, so the SDK refuses ids that would have worked")
		}
		if !reviewRequestID.MatchString(strings.Repeat("a", mirror.handOffMin) + suffix) {
			t.Errorf("the Library refuses the shortest id the SDK allows with %q", suffix)
		}
	}
}

// The constants are only the Library's limits if the code enforces them: the
// edge of each is where a copy would be wrong by one.
func TestTheReviewAttachmentLimitsAreTheOnesEnforced(t *testing.T) {
	markdown := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }
	if _, err := reviewAttachmentMIME("notes.md", markdown(reviewMarkdownBytes)); err != nil {
		t.Errorf("Markdown of exactly reviewMarkdownBytes was refused: %v", err)
	}
	if _, err := reviewAttachmentMIME("notes.md", markdown(reviewMarkdownBytes+1)); err == nil {
		t.Error("Markdown one byte over reviewMarkdownBytes was admitted")
	}
	// Only the header of an image is decoded, so padding after a real PNG reaches the size limit.
	png := attachmentPNG()
	pad := func(n int) []byte { return append(append([]byte{}, png...), make([]byte, n-len(png))...) }
	if _, err := reviewAttachmentMIME("pic.png", pad(reviewImageBytes)); err != nil {
		t.Errorf("an image of exactly reviewImageBytes was refused: %v", err)
	}
	if _, err := reviewAttachmentMIME("pic.png", pad(reviewImageBytes+1)); err == nil {
		t.Error("an image one byte over reviewImageBytes was admitted")
	}
	// Each format the Library lists is admitted under its own extension, and ".jpg" is read as
	// ".jpeg": the SDK's extension set rests on these.
	rgba := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var jpegBytes, gifBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, rgba, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBytes, rgba, nil); err != nil {
		t.Fatal(err)
	}
	webpBytes, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	samples := map[string][]byte{"png": png, "jpeg": jpegBytes.Bytes(), "gif": gifBytes.Bytes(), "webp": webpBytes}
	for _, format := range reviewImageFormats {
		content, ok := samples[format]
		if !ok {
			t.Errorf("reviewImageFormats lists %q, and this test has no sample image of it: add one", format)
			continue
		}
		names := []string{"pic." + format}
		if format == "jpeg" {
			names = append(names, "pic.jpg")
		}
		for _, name := range names {
			if mime, err := reviewAttachmentMIME(name, content); err != nil || mime != "image/"+format {
				t.Errorf("%s was not admitted as an image/%s: %q, %v", name, format, mime, err)
			}
		}
	}
	for format := range samples {
		if !slices.Contains(reviewImageFormats, format) {
			t.Errorf("%s is admitted by the sample table here and is not in reviewImageFormats", format)
		}
	}
}

// Every sentence the Library refuses an attachment with must match what the SDK
// listens for. Part one reads the code, so a refusal added on a path no test
// reaches (a blob store that is down, a document that changed) is still held;
// part two runs the refusals that need no engine, so a sentence built at run
// time is held too.
//
// It reads the SDK's source, which Go's test cache does not notice changing: run it with -count=1 after editing that file.
func TestEveryReviewAttachmentRefusalSaysAttach(t *testing.T) {
	mirror := readSDKMirror(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "review_attachments.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reviewAttachments", "reviewAttachmentMIME"} {
		var decl *ast.FuncDecl
		for _, d := range file.Decls {
			if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == name {
				decl = f
			}
		}
		if decl == nil {
			t.Fatalf("%s is not in review_attachments.go: update this test with the function that refuses attachments", name)
		}
		sentences := 0
		// The call that last set `err`: a bare `return ..., err` is a sentence the Library did not write here.
		source := ""
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "err" {
						source = calleeName(n.Rhs)
					}
				}
			case *ast.ReturnStmt:
				if len(n.Results) == 0 {
					return true
				}
				last := n.Results[len(n.Results)-1]
				where := fset.Position(last.Pos())
				switch e := last.(type) {
				case *ast.Ident:
					switch {
					case e.Name == "nil":
					case e.Name == "err" && source == "reviewAttachmentMIME":
						// Passed on as it is: its sentences are the ones checked in reviewAttachmentMIME.
					default:
						t.Errorf("%s: %s returns %q as it is. The SDK retries a refused comment without its attachments only when the refusal says \"attach\": wrap it, as in fmt.Errorf(\"an attachment could not be read: %%w\", err)", where, name, e.Name)
					}
				case *ast.CallExpr:
					sentence, ok := errorSentence(e)
					if !ok {
						t.Errorf("%s: %s returns an error this test cannot read the sentence of; build it with fmt.Errorf or errors.New and a literal", where, name)
						break
					}
					sentences++
					if !mirror.refusal.MatchString(sentence) {
						t.Errorf("%s: %q does not say \"attach\" (the SDK matches %s). The SDK would not retry the comment without its attachments, and the person's description would be lost to one bad reference", where, sentence, mirror.refusal)
					}
				default:
					t.Errorf("%s: %s returns something this test cannot read", where, name)
				}
			}
			return true
		})
		if sentences == 0 {
			t.Errorf("no refusal sentences found in %s: this test is not looking at anything", name)
		}
	}

	// The refusals that need no document, blob store or engine.
	var noEngine Integration
	tooMany := make([]any, reviewAttachmentCount+1)
	for n := range tooMany {
		tooMany[n] = map[string]any{"artifactId": "a" + strconv.Itoa(n), "version": 1, "revision": "file:1"}
	}
	for name, input := range map[string]any{
		"references too large to read": []any{map[string]any{"artifactId": strings.Repeat("a", 16001)}},
		"more than the count allowed":  tooMany,
		"a reference naming nothing":   []any{map[string]any{"version": 1}},
	} {
		_, _, err := noEngine.reviewAttachments(context.Background(), input)
		if err == nil {
			t.Errorf("%s was admitted", name)
		} else if !mirror.refusal.MatchString(err.Error()) {
			t.Errorf("%s is refused with %q, which does not say \"attach\" (the SDK matches %s)", name, err, mirror.refusal)
		}
	}
	png := attachmentPNG()
	for name, c := range map[string]struct {
		file    string
		content []byte
	}{
		"empty Markdown":            {"notes.md", nil},
		"Markdown that is not text": {"notes.md", []byte{0xff}},
		"Markdown over the limit":   {"notes.md", bytes.Repeat([]byte{'x'}, reviewMarkdownBytes+1)},
		"an empty image":            {"pic.png", nil},
		"an image over the limit":   {"pic.png", append(append([]byte{}, png...), make([]byte, reviewImageBytes)...)},
		"an image that is not one":  {"pic.png", []byte("incomplete")},
		"an unsupported type":       {"notes.pdf", []byte("%PDF")},
		"a name that lies":          {"pic.gif", png},
	} {
		_, err := reviewAttachmentMIME(c.file, c.content)
		if err == nil {
			t.Errorf("%s was admitted", name)
		} else if !mirror.refusal.MatchString(err.Error()) {
			t.Errorf("%s is refused with %q, which does not say \"attach\" (the SDK matches %s)", name, err, mirror.refusal)
		}
	}
}

// The name of the function a call expression calls, or "".
func calleeName(rhs []ast.Expr) string {
	if len(rhs) == 0 {
		return ""
	}
	call, ok := rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// The literal sentence of fmt.Errorf("...") or errors.New("..."), if that is what the call is.
func errorSentence(call *ast.CallExpr) (string, bool) {
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	pkg, ok := fn.X.(*ast.Ident)
	if !ok || !((pkg.Name == "fmt" && fn.Sel.Name == "Errorf") || (pkg.Name == "errors" && fn.Sel.Name == "New")) {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(lit.Value)
	return text, err == nil
}
