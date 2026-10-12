package compose

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	pure "github.com/znasllc-io/memql/component/compose"
)

// The Materializer app in the MemQL OS (clients/os/src/apps/materializer/make.ts) tells a person what
// a file will be before it is made: which files it can be made from, whether it can start blank, and
// the name it will be filed under. Each of those is a rule the engine holds, and the app holds a COPY
// of it, because the person has to be told before the engine is asked.
//
// A copy that drifts says something false to a person. It did: the app said an email template is filed
// as <stem>.json, and the engine files it as <stem>.email.json. These tests hold every copy the app
// makes to the engine, in the way each can be held:
//
//   - a CONSTANT (a list of extensions, of formats, a sentence) is read off the app's source and
//     compared with what the engine does with each value, in both directions: a value the app lists and
//     the engine refuses, and a value the engine takes and the app does not list;
//   - a BEHAVIOUR (the filed name) is a table of names and what the engine files them as, which is
//     computed here through the engine's own parse and filing, and by the app's own tests through its
//     own function (clients/os/test/materializer/make.test.ts reads the same file).
//
// They read the app's TypeScript, which Go's test cache does not notice changing: run them with
// -count=1 after editing it. A declaration that is missing or reshaped fails with a sentence naming the
// file, so that the guard is moved with the code it guards and never left quietly checking nothing.

const (
	osMakeSource    = "../../clients/os/src/apps/materializer/make.ts"
	osSetupSource   = "../../clients/os/src/items/materialize.ts"
	filedNamesTable = "testdata/filed_names.json"
)

// A `//` comment to the end of its line, or a `/* */` block. A string literal that holds a `//` is cut
// short too, which can only make a guard that reads code fail, never pass.
var osComment = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)

var osQuoted = regexp.MustCompile(`"([^"]*)"`)

// osSource is an app source file with its comments removed: a comment that names a value is not a
// declaration of it.
func osSource(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the Materializer app's source is not at %s (%v): if it moved, update this test -- it is what keeps the app's copies of the engine's rules honest", file, err)
	}
	return osComment.ReplaceAllString(string(raw), "")
}

// osList reads the string literals of a top-level `const NAME ... = [ ... ]` declaration, or one wrapped
// in `new Set([ ... ])` or followed by `as const`.
func osList(t *testing.T, file, name string) []string {
	t.Helper()
	declaration := regexp.MustCompile(`(?s)\bconst ` + name + `\b[^=]*=\s*(?:new Set\()?\[(.*?)\]\)?(?:\s+as const)?\s*;`).FindStringSubmatch(osSource(t, file))
	if declaration == nil {
		t.Fatalf("%s is gone from %s, or is no longer a top-level `const %s = [...]` (or `new Set([...])`) of string literals. It is the app's copy of a rule the engine holds; update this test with it", name, file, name)
	}
	var values []string
	for _, quoted := range osQuoted.FindAllStringSubmatch(declaration[1], -1) {
		values = append(values, quoted[1])
	}
	if len(values) == 0 {
		t.Fatalf("%s in %s lists no string literal; this test reads only literals", name, file)
	}
	return values
}

// osString reads a top-level `const NAME = "...";` declaration.
func osString(t *testing.T, file, name string) string {
	t.Helper()
	declaration := regexp.MustCompile(`\bconst ` + name + `\b[^=]*=\s*"([^"]*)"\s*;`).FindStringSubmatch(osSource(t, file))
	if declaration == nil {
		t.Fatalf("%s is gone from %s, or is no longer a top-level `const %s = \"...\";`. It is the app's copy of a sentence the engine words; update this test with it", name, file, name)
	}
	return declaration[1]
}

// ---- which files the composer reads ----

// readableSamples are bytes the composer reads as each kind of file it takes. A name is read when ANY of
// them is accepted under it, so a name that fails only because the bytes were wrong for it (a PNG's
// bytes under .txt) is not mistaken for one the composer refuses.
func readableSamples(t *testing.T) (members [][]byte, archive []byte) {
	t.Helper()
	pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var pngBytes, jpegBytes, gifBytes bytes.Buffer
	if err := png.Encode(&pngBytes, pixel); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegBytes, pixel, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBytes, pixel, nil); err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	writer := zip.NewWriter(&zipped)
	member, err := writer.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return [][]byte{[]byte("hello\n"), pngBytes.Bytes(), jpegBytes.Bytes(), gifBytes.Bytes()}, zipped.Bytes()
}

// composerReads asks the composer's own readers whether a file of this name can be read: a top-level
// archive is opened and its members read (readReference), and anything else must be a kind of file
// readReferenceMember takes.
func composerReads(t *testing.T, name string) bool {
	t.Helper()
	members, archive := readableSamples(t)
	if strings.EqualFold(path.Ext(name), ".zip") {
		_, err := readReference(name, "", archive, &referenceBudget{})
		return err == nil
	}
	for _, data := range members {
		if _, err := readReferenceMember(name, data, &referenceBudget{}); err == nil {
			return true
		}
	}
	return false
}

// The tray offers a Library file only when the composer can read it (readableReference), because the
// composer refuses the WHOLE request over a reference it cannot read.
//
// The composer is asked about a universe of extensions: the ones the app lists, the ones the composer is
// known to read, and the ones a Library holds that it does not (".webp" is the one that matters, since the
// Library takes it and the composer does not). What the composer answers is the truth, and the app's list
// is held to it for every one, so an extension the app drops, or one the app lists that the composer
// refuses, or one the composer starts to read, each fails. A new kind of file the composer reads that is
// not in the universe is the one case this cannot see; add it to the universe when the composer learns it.
func TestTheAppOffersExactlyTheFilesTheComposerReads(t *testing.T) {
	listed := osList(t, osMakeSource, "READABLE_EXTENSIONS")
	universe := slices.Clone(listed)
	for _, extension := range []string{
		// read
		".md", ".markdown", ".txt", ".html", ".htm", ".css", ".json", ".csv", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".zip",
		// refused
		".pdf", ".docx", ".doc", ".xlsx", ".pptx", ".webp", ".avif", ".heic", ".bmp", ".tif", ".tiff", ".ico", ".xml", ".yaml", ".yml",
		".toml", ".js", ".ts", ".tsx", ".mdx", ".rst", ".log", ".svgz", ".htmlx", ".markdownx", ".gz", ".tar", ".7z", ".mp3", ".mp4", ".bak", ".",
		"",
	} {
		if !slices.Contains(universe, extension) {
			universe = append(universe, extension)
		}
	}

	for _, extension := range universe {
		reads, offers := composerReads(t, "file"+extension), slices.Contains(listed, extension)
		switch {
		case offers && !reads:
			t.Errorf("%s lists %q as a file the composer reads (READABLE_EXTENSIONS), and the composer refuses \"file%s\" (source_content.go): the tray would offer a file whose whole request is then refused", osMakeSource, extension, extension)
		case reads && !offers:
			t.Errorf("the composer reads \"file%s\" (source_content.go) and %s does not list %q (READABLE_EXTENSIONS): the tray would hide a file the composer takes", extension, osMakeSource, extension)
		}
		// A name is read in whatever case its extension is written.
		if offers && !composerReads(t, "file"+strings.ToUpper(extension)) {
			t.Errorf("%s lists %q, and the composer refuses it in upper case (\"file%s\"): the tray reads extensions without regard to case", osMakeSource, extension, strings.ToUpper(extension))
		}
	}
}

// ---- which kinds can start blank ----

func TestTheAppStartsBlankExactlyWhereTheEngineDoes(t *testing.T) {
	listed := osList(t, osMakeSource, "BLANK_FORMATS")
	for _, format := range pure.Formats() {
		_, err := pure.RenderBlank(format, "Notes")
		if blank := err == nil; blank != slices.Contains(listed, string(format)) {
			t.Errorf("RenderBlank(%s) takes it = %v and %s lists it in BLANK_FORMATS = %v: they must agree, or the app offers a blank file the engine refuses, or hides one it would write", format, blank, osMakeSource, !blank)
		}
	}
	for _, name := range listed {
		if _, err := pure.ParseFormat(name); err != nil {
			t.Errorf("%s lists %q in BLANK_FORMATS, which is not a format the engine writes", osMakeSource, name)
		}
	}
}

// The sentence under "Describe it or pick what to make it from" is the engine's own refusal of a blank
// file that has no empty document.
func TestTheAppSaysWhatTheEngineSaysOfABlankFile(t *testing.T) {
	if got := osString(t, osMakeSource, "BLANK_DETAIL"); got != pure.ErrBlankFormat.Error() {
		t.Errorf("%s says %q (BLANK_DETAIL) and the engine refuses a blank file with %q (ErrBlankFormat): the app's floor and the engine's refusal must read the same", osMakeSource, got, pure.ErrBlankFormat.Error())
	}
}

// ---- the kinds of file ----

func TestTheAppOffersExactlyTheFormatsAndPackagesTheEngineWrites(t *testing.T) {
	var formats []string
	for _, format := range pure.Formats() {
		formats = append(formats, string(format))
	}
	slices.Sort(formats)
	listed := osList(t, osSetupSource, "COMPOSE_FORMATS")
	slices.Sort(listed)
	if !slices.Equal(listed, formats) {
		t.Errorf("%s lists the formats %v (COMPOSE_FORMATS) and the engine writes %v (Formats): the kinds of file the app offers must be the ones the engine can make", osSetupSource, listed, formats)
	}

	var packages []string
	for _, kind := range pure.DeployableKinds() {
		packages = append(packages, string(kind))
	}
	slices.Sort(packages)
	offered := osList(t, osMakeSource, "PACKAGE_KINDS")
	slices.Sort(offered)
	if !slices.Equal(offered, packages) {
		t.Errorf("%s lists the package kinds %v (PACKAGE_KINDS) and the engine writes %v (DeployableKinds): the app offers a package kind the engine refuses, or hides one it makes", osMakeSource, offered, packages)
	}
}

// ---- the name a file is filed under ----

type filedNameRow struct {
	Name           string `json:"name"`
	Format         string `json:"format"`
	DeployableKind string `json:"deployableKind"`
	OutputKind     string `json:"outputKind"`
	// Filed is the engine's name for the file, or "" where the engine refuses the name.
	Filed string `json:"filed"`
}

type filedNames struct {
	About string         `json:"about"`
	Rows  []filedNameRow `json:"rows"`
}

func readFiledNames(t *testing.T) filedNames {
	t.Helper()
	raw, err := os.ReadFile(filedNamesTable)
	if err != nil {
		t.Fatalf("the table of filed names is not at %s (%v): clients/os/test/materializer/make.test.ts reads the same file, and this test is what holds it to the engine", filedNamesTable, err)
	}
	var table filedNames
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("%s is not the table this test reads (%v): an object with \"about\" and \"rows\"", filedNamesTable, err)
	}
	return table
}

// kindOf names a row's kind of file, as the app's choice does not: the engine knows it by three fields.
func kindOf(row filedNameRow) string {
	switch {
	case row.OutputKind == "email_template":
		return "email template"
	case row.DeployableKind != "":
		return row.DeployableKind + " package"
	}
	return row.Format
}

// Every row is the engine's own answer: the name is parsed as a request's is (parseMaterializeArgs cleans
// it), and filed by the one function the filing site calls (filedFileName). The app's filedName is held to
// the same rows by clients/os/test/materializer/make.test.ts.
func TestTheEngineFilesTheNamesInTheTableAsTheTableSays(t *testing.T) {
	table := readFiledNames(t)
	kinds := map[string]int{}
	refused, cut, email := 0, 0, 0
	for _, row := range table.Rows {
		kinds[kindOf(row)]++
		args := map[string]any{"name": row.Name, "format": row.Format}
		if row.DeployableKind != "" {
			args["deployableKind"] = row.DeployableKind
		}
		if row.OutputKind != "" {
			args["outputKind"] = row.OutputKind
		}
		parsed, err := parseMaterializeArgs(args)
		if row.Filed == "" {
			refused++
			if err == nil {
				t.Errorf("%s %q: the table says the engine refuses this name, and it files it as %q", kindOf(row), row.Name, filedFileName(parsed.Name, parsed.Format, parsed.DeployableKind, parsed.OutputKind))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %q: the table says the engine files this as %q, and it refuses the name: %v", kindOf(row), row.Name, row.Filed, err)
			continue
		}
		got := filedFileName(parsed.Name, parsed.Format, parsed.DeployableKind, parsed.OutputKind)
		// A stem the engine cuts at 120 bytes can be cut inside a character, which leaves the bytes of
		// its start. The table writes that as ONE replacement character, as the app shows it.
		if !utf8.ValidString(got) {
			cut++
			got = strings.ToValidUTF8(got, "�")
		}
		if row.OutputKind == "email_template" {
			email++
		}
		if got != row.Filed {
			t.Errorf("%s %q: the engine files this as %q and the table says %q", kindOf(row), row.Name, got, row.Filed)
		}
	}

	// A table that lost a kind, its refused names or its cuts would guard less than it says.
	if len(kinds) != 11 {
		t.Errorf("%s holds rows for %d kinds of file (%v); it must hold all eleven the app offers", filedNamesTable, len(kinds), kinds)
	}
	for kind, rows := range kinds {
		if rows < 10 {
			t.Errorf("%s holds only %d rows for %s", filedNamesTable, rows, kind)
		}
	}
	if len(table.Rows) < 300 || refused < 20 || cut < 4 || email < 20 {
		t.Errorf("%s holds %d rows, of which %d refused, %d cut inside a character and %d for an email template: it is meant to hold a few hundred, with each of those well represented", filedNamesTable, len(table.Rows), refused, cut, email)
	}
}
