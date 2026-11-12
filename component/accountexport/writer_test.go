package accountexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type entry struct {
	name string
	data []byte
}

func readEntries(t *testing.T, b []byte) []entry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out []entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		out = append(out, entry{h.Name, data})
	}
}

func writeEntries(t *testing.T, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// resum rewrites SHA256SUMS so it agrees with every other member: a file
// changed this way passes the checksum check and must be caught by what
// Verify reads inside the members.
func resum(es []entry) []entry {
	var sums strings.Builder
	for _, e := range es {
		if strings.HasSuffix(e.name, "/"+MemberSums) {
			continue
		}
		d := sha256.Sum256(e.data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(d[:]), strings.SplitN(e.name, "/", 2)[1])
	}
	for i := range es {
		if strings.HasSuffix(es[i].name, "/"+MemberSums) {
			es[i].data = []byte(sums.String())
		}
	}
	return es
}

func names(es []entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.name
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	c := defaultFixture()
	file := writeFixture(t, c)

	res, err := Verify(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("Verify of a file the writer wrote: %v", err)
	}
	if res.Manifest.Kind != KindManifest || res.Manifest.FormatVersion != FormatVersion {
		t.Errorf("manifest kind/version not stamped: %+v", res.Manifest)
	}
	if res.Manifest.Account.ID != "acc0001" || !res.Manifest.CreatedAt.Equal(fixtureAt) {
		t.Errorf("manifest did not survive: %+v", res.Manifest)
	}
	if res.Bytes != int64(len(file)) {
		t.Errorf("Bytes = %d, file is %d", res.Bytes, len(file))
	}

	got := names(readEntries(t, file))
	want := []string{
		MemberManifest,
		MemberAccount,
		"rows/library/file/part-000001.jsonl.gz",
		"rows/todos/todo/part-000001.jsonl.gz",
		"schemas/" + fixtureSchema + ".json",
		MemberPeople,
		MemberReferences,
		MemberConnections,
		"bytes/000001",
		MemberBytesIndex,
		MemberReport,
		MemberReadme,
		MemberSums,
	}
	for i := range want {
		want[i] = fixtureStem + "/" + want[i]
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("members out of the layout's order:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res.Members != len(want) {
		t.Errorf("Members = %d, want %d", res.Members, len(want))
	}

	// The rows read back exactly, raw JSON untouched.
	es := readEntries(t, file)
	zr, err := gzip.NewReader(bytes.NewReader(es[2].data))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("file part has %d lines, want 3", len(lines))
	}
	var row Row
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatal(err)
	}
	if row.Kind != KindRow || string(row.Payload) != `{"title":"Café menu","blobUrl":"files/f1"}` {
		t.Errorf("row did not round-trip: %+v", row)
	}

	// bytes.jsonl names the carried object with its checksum and the missing one.
	idx := string(es[9].data)
	sum := sha256.Sum256(c.object)
	if !strings.Contains(idx, `"file":"bytes/000001"`) || !strings.Contains(idx, hex.EncodeToString(sum[:])) {
		t.Errorf("bytes.jsonl lacks the carried object: %s", idx)
	}
	if !strings.Contains(idx, `"key":"files/f2"`) || !strings.Contains(idx, `"missing":true`) {
		t.Errorf("bytes.jsonl lacks the missing object: %s", idx)
	}

	readme := string(es[11].data)
	for _, w := range []string{"Acme", "sha256sum -c SHA256SUMS", "age --decrypt", "v1:library:file", "signin"} {
		if !strings.Contains(readme, w) {
			t.Errorf("README does not say %q:\n%s", w, readme)
		}
	}
}

func TestAccountWithNoRows(t *testing.T) {
	c := defaultFixture()
	c.kinds = map[string][]Row{}
	c.people = nil
	c.object = nil
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range c.accountRows {
		if err := w.AccountRow(row); err != nil {
			t.Fatal(err)
		}
	}
	r := c.report()
	r.Members = nil
	r.Kinds = r.Kinds[:1]
	r.Kinds = append(r.Kinds, KindCount{Concept: "v1:todos:todo"})
	if err := w.Finish(r); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	res, err := Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Report.Kinds) != 2 {
		t.Errorf("report lost its kinds: %+v", res.Report.Kinds)
	}
	got := names(readEntries(t, buf.Bytes()))
	for _, want := range []string{MemberAccount, MemberPeople, MemberReferences, MemberConnections, MemberBytesIndex} {
		found := false
		for _, g := range got {
			found = found || g == fixtureStem+"/"+want
		}
		if !found {
			t.Errorf("an account with no rows still has %s; members: %v", want, got)
		}
	}
}

func withBounds(t *testing.T, maxBytes, maxLines int) {
	t.Helper()
	withLimits(t, func(l *limits) { l.partBytes, l.partLines = maxBytes, maxLines })
}

func partsOf(t *testing.T, file []byte, dir string) [][]string {
	t.Helper()
	var out [][]string
	for _, e := range readEntries(t, file) {
		if !strings.HasPrefix(e.name, fixtureStem+"/"+dir+"/part-") {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(e.data))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Split(strings.TrimSuffix(string(body), "\n"), "\n"))
	}
	return out
}

func TestOversizedLineIsAPartOfItsOwn(t *testing.T) {
	withBounds(t, 4096, 50000)
	c := defaultFixture()
	// Incompressible, so the line is larger than the bound even gzipped.
	rnd := make([]byte, 6000)
	for i := range rnd {
		rnd[i] = byte('a' + (i*7919+i*i*31)%26)
	}
	big := `{"title":"` + string(rnd) + strings.Repeat("x", 4000) + `"}`
	c.kinds["v1:todos:todo"] = []Row{
		fixtureRow("v1:todos:todo", "t1", 6, `{"title":"small"}`),
		fixtureRow("v1:todos:todo", "t2", 7, big),
		fixtureRow("v1:todos:todo", "t3", 8, `{"title":"after"}`),
	}
	file := writeFixture(t, c)
	if _, err := Verify(bytes.NewReader(file)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	parts := partsOf(t, file, "rows/todos/todo")
	if len(parts) != 3 || len(parts[1]) != 1 || !strings.Contains(parts[1][0], `"id":"v1:todos:todo:t2"`) {
		t.Fatalf("the oversized line is not a part of its own: %d parts", len(parts))
	}
}

func TestPartIsCutAtTheLineBound(t *testing.T) {
	withBounds(t, 8<<20, 2)
	c := defaultFixture()
	file := writeFixture(t, c)
	if _, err := Verify(bytes.NewReader(file)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	parts := partsOf(t, file, "rows/library/file")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 1 {
		t.Fatalf("three lines under a bound of two: got %d parts", len(parts))
	}
}

func TestSameInputGivesTheSameBytes(t *testing.T) {
	a := writeFixture(t, defaultFixture())
	b := writeFixture(t, defaultFixture())
	if !bytes.Equal(a, b) {
		t.Fatal("the same input written twice differs")
	}
	// The gzip header carries no time and no name.
	for _, e := range readEntries(t, a) {
		if !strings.HasSuffix(e.name, ".jsonl.gz") {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(e.data))
		if err != nil {
			t.Fatal(err)
		}
		if !zr.ModTime.IsZero() || zr.Name != "" || zr.Comment != "" {
			t.Errorf("%s: gzip header carries time %v name %q", e.name, zr.ModTime, zr.Name)
		}
	}
}

func TestCutBeforeSumsIsRefused(t *testing.T) {
	es := readEntries(t, writeFixture(t, defaultFixture()))
	file := writeEntries(t, es[:len(es)-1])
	_, err := Verify(bytes.NewReader(file))
	mustContain(t, err, "cut short")

	// A stream that stops in the middle of a member is cut short too.
	whole := writeFixture(t, defaultFixture())
	_, err = Verify(bytes.NewReader(whole[:len(whole)/2]))
	if err == nil {
		t.Fatal("half a file verified")
	}
}

func TestChangedByteIsRefusedNamingTheMember(t *testing.T) {
	orig := readEntries(t, writeFixture(t, defaultFixture()))
	for i := range orig {
		// An empty member has no byte to change; its sum is still checked.
		if strings.HasSuffix(orig[i].name, "/"+MemberSums) || len(orig[i].data) == 0 {
			continue
		}
		t.Run(orig[i].name, func(t *testing.T) {
			es := readEntries(t, writeFixture(t, defaultFixture()))
			d := es[i].data
			// The last byte of a member: for JSON a newline or brace, for a
			// gzip part its length trailer. Changed to a space where it is a
			// newline so the JSON stays parseable and only the sum can tell.
			if d[len(d)-1] == '\n' {
				d[len(d)-1] = ' '
			} else {
				d[len(d)-1] ^= 0x01
			}
			_, err := Verify(bytes.NewReader(writeEntries(t, es)))
			mustContain(t, err, strings.SplitN(es[i].name, "/", 2)[1])
		})
	}
}

func TestWrittenCountDifferingFromTheLinesIsRefused(t *testing.T) {
	es := readEntries(t, writeFixture(t, defaultFixture()))
	for i := range es {
		if !strings.HasSuffix(es[i].name, "/"+MemberReport) {
			continue
		}
		var r Report
		if err := json.Unmarshal(es[i].data, &r); err != nil {
			t.Fatal(err)
		}
		r.Kinds[1].WrittenVersions++
		es[i].data, _ = json.Marshal(r)
	}
	_, err := Verify(bytes.NewReader(writeEntries(t, resum(es))))
	mustContain(t, err, "v1:library:file", "written")
}

func TestFinishRefusesAReportThatDisagreesWithTheWriter(t *testing.T) {
	c := defaultFixture()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range c.accountRows {
		if err := w.AccountRow(row); err != nil {
			t.Fatal(err)
		}
	}
	r := c.report()
	r.Bytes = BytesReport{}
	r.Members = nil
	r.Kinds = r.Kinds[:1]
	r.Kinds[0].WrittenVersions = 7
	mustContain(t, w.Finish(r), "v1:accounts:account")
}

func TestNewerFormatIsRefusedBeforeAnythingElse(t *testing.T) {
	m := `{"kind":"memql.account.export","formatVersion":2,"somethingNew":{"x":1}}`
	file := writeEntries(t, []entry{
		{fixtureStem + "/" + MemberManifest, []byte(m)},
		{fixtureStem + "/garbage", []byte("not a member of format 1")},
	})
	_, err := Verify(bytes.NewReader(file))
	var tooNew *ErrFormatTooNew
	if !errors.As(err, &tooNew) || tooNew.Found != 2 || tooNew.Supported != FormatVersion {
		t.Fatalf("want ErrFormatTooNew{2,%d}, got %v", FormatVersion, err)
	}

	// The manifest must be first.
	file = writeEntries(t, []entry{
		{fixtureStem + "/" + MemberAccount, []byte("")},
		{fixtureStem + "/" + MemberManifest, []byte(m)},
	})
	_, err = Verify(bytes.NewReader(file))
	mustContain(t, err, MemberManifest)
}

func TestBytesFailsWhenTheReaderIsShortOrLong(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		size int64
	}{
		{"short", "abc", 5},
		{"long", "abcdefg", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
			if err != nil {
				t.Fatal(err)
			}
			err = w.Bytes(BytesMeta{Key: "k"}, tc.size, strings.NewReader(tc.data))
			mustContain(t, err, tc.name)
			// The stream is broken from there on; every later call says so.
			if err := w.Finish(Report{}); err == nil {
				t.Fatal("Finish after a failed Bytes succeeded")
			}
		})
	}
}

func TestWriterRefusesMembersOutOfOrder(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Line(MemberPeople, map[string]any{"id": "u"}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, w.AccountRow(fixtureRow("v1:accounts:account", "a", 1, `{}`)), "order")
}

func TestPartWriterRefusesAForeignRow(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	p := w.Parted(RowsDir("v1:library:file"))
	mustContain(t, p.Write(fixtureRow("v1:todos:todo", "t", 1, `{}`)), "v1:todos:todo")
}

func TestProtectRoundTrip(t *testing.T) {
	file := writeFixture(t, defaultFixture())
	var sealed bytes.Buffer
	pw, err := Protect(&sealed, "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Bytes(), []byte("Acme")) {
		t.Fatal("the protected file holds plain text")
	}

	r, err := Unprotect(bytes.NewReader(sealed.Bytes()), "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(r); err != nil {
		t.Fatalf("Verify through Unprotect: %v", err)
	}

	if _, err := Unprotect(bytes.NewReader(sealed.Bytes()), "ABCDE-FGHJK-LMNPQ-RSTUV-WXYZ3"); err == nil {
		t.Fatal("a wrong passphrase opened the file")
	}
}

// A protected file opens with the stock age tool and its passphrase prompt.
// Skipped where age or script (which gives age the terminal it prompts on)
// is absent.
func TestProtectedFileOpensWithTheStockTool(t *testing.T) {
	agePath, err := exec.LookPath("age")
	if err != nil {
		t.Skip("age is not on PATH")
	}
	scriptPath, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not on PATH")
	}
	file := writeFixture(t, defaultFixture())
	pass, err := GeneratePassphrase()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "x.tar.age")
	out := filepath.Join(dir, "x.tar")
	f, err := os.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := Protect(f, pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// script runs age on a pseudo-terminal and forwards stdin to it, which
	// is where age reads the passphrase. The BSD and util-linux spellings
	// differ.
	var cmd *exec.Cmd
	if v, _ := exec.Command(scriptPath, "--version").CombinedOutput(); strings.Contains(string(v), "util-linux") {
		cmd = exec.Command(scriptPath, "-q", "-e", "-c", agePath+" --decrypt -o "+out+" "+in, "/dev/null")
	} else {
		cmd = exec.Command(scriptPath, "-q", "/dev/null", agePath, "--decrypt", "-o", out, in)
	}
	cmd.Stdin = strings.NewReader(pass + "\n")
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("age --decrypt: %v\n%s", err, o)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, file) {
		t.Fatal("age --decrypt gave different bytes")
	}
}

// `cd <stem> && sha256sum -c SHA256SUMS` checks every member with the stock
// tool. Skipped where tar or a sha256 tool is absent.
func TestSumsCheckWithTheStockTool(t *testing.T) {
	tarPath, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("tar is not on PATH")
	}
	check := []string{"sha256sum", "-c", MemberSums}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("neither sha256sum nor shasum is on PATH")
		}
		check = []string{"shasum", "-a", "256", "-c", MemberSums}
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.tar")
	if err := os.WriteFile(archive, writeFixture(t, defaultFixture()), 0o600); err != nil {
		t.Fatal(err)
	}
	x := exec.Command(tarPath, "-xf", archive)
	x.Dir = dir
	if o, err := x.CombinedOutput(); err != nil {
		t.Fatalf("tar -xf: %v\n%s", err, o)
	}
	c := exec.Command(check[0], check[1:]...)
	c.Dir = filepath.Join(dir, fixtureStem)
	o, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(check, " "), err, o)
	}
	lines := strings.Split(strings.TrimSpace(string(o)), "\n")
	if len(lines) != 12 {
		t.Errorf("the stock tool checked %d members, want 12:\n%s", len(lines), o)
	}
}

var passphraseShape = regexp.MustCompile(`^[A-HJ-NP-Z2-9]{5}(-[A-HJ-NP-Z2-9]{5}){4}$`)

func TestGeneratePassphrase(t *testing.T) {
	a, err := GeneratePassphrase()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePassphrase()
	if err != nil {
		t.Fatal(err)
	}
	if !passphraseShape.MatchString(a) || !passphraseShape.MatchString(b) {
		t.Fatalf("passphrase shape: %q %q", a, b)
	}
	if a == b {
		t.Fatal("two passphrases are the same")
	}
}

func TestRowsDir(t *testing.T) {
	for in, want := range map[string]string{
		"v1:library:file":     "rows/library/file",
		"v1:accounts:account": "rows/accounts/account",
		"library:file":        "",
		"v1:../x:file":        "",
		"v1:a:b:c":            "",
	} {
		if got := RowsDir(in); got != want {
			t.Errorf("RowsDir(%q) = %q, want %q", in, got, want)
		}
	}
}
