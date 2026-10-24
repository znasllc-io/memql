//go:build linux || darwin

package appsession

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// record_test.go holds the content policy to what it promises: a file the
// app names is read only when it is inside the workspace, outside the
// session's scaffolding, and a regular file -- and the bytes that travel
// are the file's own, bounded, and only ever bytes the app itself already
// reported or wrote.

func writeFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func policyFor(ws string) *contentPolicy {
	return newContentPolicy(ws, newRedactor(testBearer),
		filepath.Join(ws, sessionScaffoldDir),
		filepath.Join(ws, ".mcp.json"),
		filepath.Join(ws, ".mcp.json"+backupSuffix))
}

func readOne(p *contentPolicy, op, path string) harness.Content {
	c := []harness.Content{{Op: op, Path: path}}
	p.fill(c)
	return c[0]
}

// readSeen is a read whose app reported seen as the file's bytes.
func readSeen(p *contentPolicy, path, seen string) harness.Content {
	c := []harness.Content{{Op: harness.ContentRead, Path: path, Seen: []byte(seen)}}
	p.fill(c)
	return c[0]
}

func TestContentPolicyReadsOnlyInsideTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	notes := writeFile(t, ws, "notes.txt", "alpha\nbeta\n")
	secret := writeFile(t, outside, "secret.txt", "not the session's")
	writeFile(t, ws, ".mcp.json", `{"mcpServers":{"memql":{"headers":{"Authorization":"Bearer `+testBearer+`"}}}}`)
	writeFile(t, ws, sessionScaffoldDir+"/codex/config.toml", "bearer = \""+testBearer+"\"\n")
	writeFile(t, ws, ".mcp.json"+backupSuffix, "the person's own config")
	if err := os.Symlink(secret, filepath.Join(ws, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(notes, filepath.Join(ws, "link-in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := policyFor(ws)

	cases := []struct {
		name, path, omitted, data string
	}{
		{"a file in the workspace", notes, "", "alpha\nbeta\n"},
		{"a relative path, under the workspace", "notes.txt", "", "alpha\nbeta\n"},
		{"a link that stays inside", filepath.Join(ws, "link-in"), "", "alpha\nbeta\n"},
		{"a file elsewhere", secret, harness.OmittedOutsideWorkspace, ""},
		{"a link out of the workspace", filepath.Join(ws, "link-out"), harness.OmittedOutsideWorkspace, ""},
		{"a climb out with ..", filepath.Join(ws, "..", filepath.Base(outside), "secret.txt"), harness.OmittedOutsideWorkspace, ""},
		{"the MCP configuration with the bearer", filepath.Join(ws, ".mcp.json"), harness.OmittedScaffolding, ""},
		{"the session directory", filepath.Join(ws, sessionScaffoldDir, "codex", "config.toml"), harness.OmittedScaffolding, ""},
		{"a configuration moved aside", filepath.Join(ws, ".mcp.json"+backupSuffix), harness.OmittedScaffolding, ""},
		{"nothing there", filepath.Join(ws, "missing.txt"), harness.OmittedNotFound, ""},
		{"a directory", filepath.Join(ws, "dir"), harness.OmittedNotRegular, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each read reports the bytes it expects, so the only thing
			// deciding whether they travel is where the file is.
			got := readSeen(p, tc.path, tc.data)
			if got.Omitted != tc.omitted {
				t.Fatalf("omitted = %q, want %q (%+v)", got.Omitted, tc.omitted, got)
			}
			if tc.omitted != "" {
				if got.Data != nil || got.Encoding != "" {
					t.Errorf("an omitted file still carried bytes: %+v", got)
				}
				if tc.omitted != harness.OmittedNotRegular && got.Digest != "" {
					t.Errorf("a file that was not read has a digest: %+v", got)
				}
				return
			}
			if got.Data == nil || *got.Data != tc.data || got.Encoding != harness.EncodingUTF8 {
				t.Errorf("data = %+v, want %q", got, tc.data)
			}
			if got.Digest != harness.Digest([]byte(tc.data)) || got.Bytes == nil || *got.Bytes != int64(len(tc.data)) {
				t.Errorf("digest/bytes = %s %v", got.Digest, got.Bytes)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want the path the app named", got.Path)
			}
		})
	}
}

// TestContentPolicyNeverWaitsOnAPipe: a named pipe opened for reading
// blocks until a writer appears, which here would be never. The open is
// non-blocking and the pipe is refused on its descriptor.
func TestContentPolicyNeverWaitsOnAPipe(t *testing.T) {
	ws := t.TempDir()
	fifo := filepath.Join(ws, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan harness.Content, 1)
	go func() { done <- readOne(policyFor(ws), harness.ContentRead, fifo) }()
	select {
	case got := <-done:
		if got.Omitted != harness.OmittedNotRegular {
			t.Errorf("a pipe = %+v, want not_regular", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a named pipe hung the recording")
	}
}

func TestContentPolicyBoundsWhatTravels(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)

	big := writeFile(t, ws, "big.bin", strings.Repeat("x", int(maxInlineFileBytes)+1))
	got := readOne(p, harness.ContentWrite, big)
	if got.Omitted != harness.OmittedOverCeiling || got.Data != nil {
		t.Errorf("a file over the inline ceiling = %+v", got)
	}
	if got.Digest != harness.Digest([]byte(strings.Repeat("x", int(maxInlineFileBytes)+1))) {
		t.Error("a file over the inline ceiling lost its digest; it can still be compared")
	}

	// Five files of the per-file ceiling in one action: four fit the event
	// budget, and the fifth is digested but not carried.
	var batch []harness.Content
	for i := 0; i < 5; i++ {
		path := writeFile(t, ws, "part"+string(rune('a'+i)), strings.Repeat("y", int(maxInlineFileBytes)))
		batch = append(batch, harness.Content{Op: harness.ContentWrite, Path: path})
	}
	p.fill(batch)
	for i, c := range batch[:4] {
		if c.Data == nil || c.Omitted != "" {
			t.Errorf("file %d = %+v, want it inline", i, c)
		}
	}
	if batch[4].Omitted != harness.OmittedOverBudget || batch[4].Data != nil || batch[4].Digest == "" {
		t.Errorf("the fifth file = %+v, want over_budget with its digest", batch[4])
	}

	// Past the digest ceiling nothing is read at all; the size still says
	// how big it was. A sparse file makes this cheap to build.
	huge := filepath.Join(ws, "huge.bin")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxDigestFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got = readOne(p, harness.ContentRead, huge)
	if got.Omitted != harness.OmittedTooLarge || got.Digest != "" || got.Bytes == nil || *got.Bytes != maxDigestFileBytes+1 {
		t.Errorf("a file past the digest ceiling = %+v", got)
	}
}

func TestContentPolicyCarriesBinaryAsBase64AndEmptyAsEmpty(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	raw := []byte{0xff, 0xfe, 0x00, 0x41}
	path := filepath.Join(ws, "blob.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSeen(p, path, string(raw))
	if got.Encoding != harness.EncodingBase64 || got.Data == nil || *got.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("binary = %+v", got)
	}
	if got.Digest != harness.Digest(raw) {
		t.Error("the digest is over the bytes, not over their encoding")
	}

	empty := writeFile(t, ws, "empty.txt", "")
	got = readOne(p, harness.ContentWrite, empty)
	if got.Data == nil || *got.Data != "" || got.Bytes == nil || *got.Bytes != 0 || got.Omitted != "" {
		t.Errorf("an empty file = %+v, want present, empty and inline", got)
	}
}

// TestContentPolicyCarriesAReadOnlyAsTheAppReportedIt: a read's bytes
// travel only when the app's own result carried them whole. `head -1
// .env` reported one line of a file of secrets; the recording must not be
// what sends the other lines.
func TestContentPolicyCarriesAReadOnlyAsTheAppReportedIt(t *testing.T) {
	ws := t.TempDir()
	env := writeFile(t, ws, ".env", "USER=me\nTOKEN=hunter2\n")
	p := policyFor(ws)
	for _, tc := range []struct {
		name    string
		seen    *string
		omitted string
	}{
		{"the app reported nothing of it", nil, harness.OmittedDigestOnly},
		{"the app reported one line", ptr("USER=me\n"), harness.OmittedDigestOnly},
		{"the file changed after the app read it", ptr("USER=you\nTOKEN=hunter2\n"), harness.OmittedDigestOnly},
		{"the app reported it whole", ptr("USER=me\nTOKEN=hunter2\n"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := []harness.Content{{Op: harness.ContentRead, Path: env}}
			if tc.seen != nil {
				c[0].Seen = []byte(*tc.seen)
			}
			p.fill(c)
			got := c[0]
			if got.Omitted != tc.omitted {
				t.Fatalf("omitted = %q, want %q (%+v)", got.Omitted, tc.omitted, got)
			}
			if got.Digest != harness.Digest([]byte("USER=me\nTOKEN=hunter2\n")) {
				t.Errorf("digest = %q, want the whole file's in every case", got.Digest)
			}
			if (tc.omitted == "") != (got.Data != nil) {
				t.Errorf("data = %v with omitted %q", got.Data, got.Omitted)
			}
			if got.Seen != nil {
				t.Error("what the app reported is still on the content after the policy used it")
			}
		})
	}
}

func ptr(s string) *string { return &s }

// TestContentPolicyKeepsWritesUnderDependencyDirsToADigest: a write the
// app made travels, except under a directory nobody's work lives in --
// and `.git/config` is where remote URLs keep their tokens.
func TestContentPolicyKeepsWritesUnderDependencyDirsToADigest(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	for rel, carried := range map[string]bool{
		"src/main.go":                  true,
		".gitignore":                   true,
		".git/config":                  false,
		"node_modules/left-pad/pad.js": false,
		"web/NODE_MODULES/x.js":        false,
		"pkg/vendor/lib.go":            false,
	} {
		path := writeFile(t, ws, rel, "content of "+rel)
		got := readOne(p, harness.ContentWrite, path)
		if carried && (got.Data == nil || got.Omitted != "") {
			t.Errorf("%s = %+v, want its bytes", rel, got)
		}
		if !carried && (got.Data != nil || got.Omitted != harness.OmittedDigestOnly || got.Digest == "") {
			t.Errorf("%s = %+v, want digest_only with the digest", rel, got)
		}
	}
}

// TestContentPolicyNeverCarriesTheCredential: bytes holding the session's
// own bearer travel neither as text, nor as base64, nor as a digest --
// wherever in the workspace they were written, and whatever the app
// itself printed.
func TestContentPolicyNeverCarriesTheCredential(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	text := writeFile(t, ws, "notes/copied.txt", "the header was Bearer "+testBearer+"\n")
	blob := filepath.Join(ws, "copied.bin")
	if err := os.WriteFile(blob, append([]byte{0xff, 0xfe, 0x00}, testBearer...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []harness.Content{
		readOne(p, harness.ContentWrite, text),
		readSeen(p, text, "the header was Bearer "+testBearer+"\n"),
		readOne(p, harness.ContentWrite, blob),
	} {
		if c.Omitted != harness.OmittedCredential || c.Data != nil || c.Digest != "" {
			t.Errorf("%s = %+v, want contains_credential with nothing derived from it", c.Path, c)
		}
	}
	// At any size: a file too large to travel inline would otherwise carry
	// its digest, and the bearer can sit across the boundary between two
	// of the reads that hash it.
	big := writeFile(t, ws, "big.log", strings.Repeat("z", 32<<10-10)+testBearer+strings.Repeat("z", int(maxInlineFileBytes)))
	if got := readOne(p, harness.ContentWrite, big); got.Omitted != harness.OmittedCredential || got.Digest != "" || got.Bytes == nil {
		t.Errorf("a large file holding the bearer = %+v, want contains_credential with its size and no digest", got)
	}

	// A credential renewed mid-session is refused as well -- including in
	// a large file whose digest was remembered before the renewal.
	renewed := "renewed-" + strings.Repeat("r", 40)
	after := writeFile(t, ws, "after.txt", renewed)
	settled := writeFile(t, ws, "settled.dat", renewed+strings.Repeat("s", int(maxInlineFileBytes)))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(settled, old, old); err != nil {
		t.Fatal(err)
	}
	if got := readOne(p, harness.ContentWrite, settled); got.Digest == "" {
		t.Fatalf("before the renewal = %+v, want a digest", got)
	}
	p.secrets.add(renewed)
	if got := readOne(p, harness.ContentWrite, after); got.Omitted != harness.OmittedCredential {
		t.Errorf("the renewed credential = %+v", got)
	}
	if got := readOne(p, harness.ContentWrite, settled); got.Omitted != harness.OmittedCredential || got.Digest != "" {
		t.Errorf("a remembered file holding the renewed credential = %+v, want it scanned again and refused", got)
	}
}

// TestContentPolicyKnowsTheScaffoldingByIdentity: a second name for the
// bearer's file is the bearer's file. On a Mac a name that differs only in
// case is one; here a hard link is.
func TestContentPolicyKnowsTheScaffoldingByIdentity(t *testing.T) {
	ws := t.TempDir()
	cfg := writeFile(t, ws, ".mcp.json", `{"note":"no bearer in this copy"}`)
	alias := filepath.Join(ws, "innocent.json")
	if err := os.Link(cfg, alias); err != nil {
		t.Skipf("hard links: %v", err)
	}
	if got := readOne(policyFor(ws), harness.ContentWrite, alias); got.Omitted != harness.OmittedScaffolding || got.Digest != "" {
		t.Errorf("a hard link to the configuration = %+v, want session_scaffolding and nothing read", got)
	}
	// And the temporary file the bearer is written through, whatever its
	// case.
	tmp := writeFile(t, ws, ".MEMQL-MCP-123456", "Bearer x")
	if got := readOne(policyFor(ws), harness.ContentWrite, tmp); got.Omitted != harness.OmittedScaffolding {
		t.Errorf("the configuration's temp file = %+v", got)
	}

	// A directory's second name: an excluded path kept UNRESOLVED -- a link
	// to the scaffolding -- misses every name compare, and the walk over
	// the file's directories still finds it.
	scaffold := filepath.Join(ws, sessionScaffoldDir)
	inside := writeFile(t, ws, sessionScaffoldDir+"/codex/config.toml", "x")
	link := filepath.Join(ws, "scaffold-link")
	if err := os.Symlink(scaffold, link); err != nil {
		t.Fatal(err)
	}
	p := &contentPolicy{root: resolvedPath(ws), excluded: []string{link}}
	f, info, got := openRegular(resolvedPath(inside))
	if f == nil {
		t.Fatalf("open: %+v", got)
	}
	defer f.Close()
	if _, why := p.confirm(f, info, resolvedPath(inside)); why != harness.OmittedScaffolding {
		t.Errorf("confirm = %q, want session_scaffolding through the directory's identity", why)
	}
}

// TestContentPolicyChecksTheFileThatOpened: the policy judges a path and
// then opens it, and a directory on the way can be swapped for a link out
// of the workspace in between. What decides is the file that opened.
func TestContentPolicyChecksTheFileThatOpened(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	secret := writeFile(t, outside, "secret.txt", "not the session's")
	claimed := writeFile(t, ws, "notes.txt", "alpha")
	p := policyFor(ws)

	// The swap already happened: what opened is outside, whatever the
	// path the policy approved says.
	f, info, got := openRegular(secret)
	if f == nil {
		t.Fatalf("open: %+v", got)
	}
	defer f.Close()
	where, why := p.confirm(f, info, resolvedPath(claimed))
	// /proc names the file outright; elsewhere the approved path names a
	// different file than the one open, which is refused as unreadable.
	if why != harness.OmittedOutsideWorkspace && why != harness.OmittedUnreadable || where != "" {
		t.Errorf("confirm = %q %q, want the opened file refused", where, why)
	}
	g, ginfo, _ := openRegular(claimed)
	if g == nil {
		t.Fatal("open the real one")
	}
	defer g.Close()
	if where, why := p.confirm(g, ginfo, resolvedPath(claimed)); why != "" || where != resolvedPath(claimed) {
		t.Errorf("the file the path names = %q %q, want it allowed where it is", where, why)
	}
}

// TestContentPolicyRemembersTheDigestOfALargeSettledFile: a large file
// read again unchanged is not hashed again -- the remembered digest is
// taken on the file's identity, size and mtime, and only once the mtime is
// old enough that a write inside the same clock tick cannot hide.
func TestContentPolicyRemembersTheDigestOfALargeSettledFile(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	size := int(maxInlineFileBytes) + 10
	path := writeFile(t, ws, "big.dat", strings.Repeat("a", size))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	first := readOne(p, harness.ContentWrite, path)
	if first.Digest != harness.Digest([]byte(strings.Repeat("a", size))) {
		t.Fatalf("first read = %+v", first)
	}

	// Same bytes count, same mtime, same file: the stamp says unchanged,
	// and the remembered digest is what comes back -- which is how this
	// test can see that nothing was hashed.
	if err := os.WriteFile(path, []byte(strings.Repeat("b", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := readOne(p, harness.ContentWrite, path); got.Digest != first.Digest {
		t.Errorf("an unchanged stamp re-hashed the file: %s", got.Digest)
	}

	// A fresh mtime is never trusted: a write inside the same clock tick
	// can leave the stamp exactly as it was. Read at a fresh mtime, rewrite
	// at the same size, put the same mtime back, and the second read must
	// still hash.
	fresh := time.Now()
	if err := os.Chtimes(path, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if got := readOne(p, harness.ContentWrite, path); got.Digest != harness.Digest([]byte(strings.Repeat("b", size))) {
		t.Fatalf("a file written just now was served from memory: %s", got.Digest)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("d", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if got := readOne(p, harness.ContentWrite, path); got.Digest != harness.Digest([]byte(strings.Repeat("d", size))) {
		t.Errorf("a rewrite inside the settle window was served the digest of the bytes before it: %s", got.Digest)
	}
	replacement := writeFile(t, ws, "big.new", strings.Repeat("c", size))
	if err := os.Chtimes(replacement, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	_ = readOne(p, harness.ContentWrite, path)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := readOne(p, harness.ContentWrite, path); got.Digest != harness.Digest([]byte(strings.Repeat("c", size))) {
		t.Errorf("a file renamed over the old one, stamp and all, was served the old digest: %s", got.Digest)
	}
}

// TestEncodeActionShedsWhatCostsLeastFirst: an action too large for the
// stream sheds its inline file bytes, then its arguments, and is refused
// only when neither is enough.
func TestEncodeActionShedsWhatCostsLeastFirst(t *testing.T) {
	data := strings.Repeat("d", 600)
	size := int64(len(data))
	args := json.RawMessage(`{"file_path":"/w/a.txt","content":"` + strings.Repeat("e", 600) + `"}`)
	base := harness.Action{Type: harness.ActionEventType, V: harness.RecordVersion, Seq: 3, Turn: 1,
		ID: "toolu_w", Tool: harness.ActionExec, AppTool: "Bash", Args: args, Cwd: "/w",
		Command: strings.Repeat("c", 100),
		Contents: []harness.Content{{Op: harness.ContentWrite, Path: "/w/a.txt", Digest: harness.Digest([]byte(data)),
			Bytes: &size, Encoding: harness.EncodingUTF8, Data: &data}}}

	decode := func(t *testing.T, body []byte) harness.Action {
		t.Helper()
		var a harness.Action
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return a
	}

	body, err := encodeAction(base, 1<<20)
	if err != nil || decode(t, body).Contents[0].Data == nil || string(decode(t, body).Args) != string(args) {
		t.Fatalf("an action that fits = %s, %v", body, err)
	}

	body, err = encodeAction(base, 1100)
	if err != nil {
		t.Fatalf("shedding the file: %v", err)
	}
	got := decode(t, body)
	if got.Contents[0].Data != nil || got.Contents[0].Omitted != harness.OmittedOverBudget ||
		got.Contents[0].Digest == "" || string(got.Args) != string(args) || got.ArgsOmitted != "" {
		t.Errorf("= %+v, want the file's bytes shed and its digest and the arguments kept", got)
	}
	if base.Contents[0].Data == nil {
		t.Error("encodeAction changed the caller's contents")
	}

	body, err = encodeAction(base, 600)
	if err != nil {
		t.Fatalf("shedding the arguments: %v", err)
	}
	got = decode(t, body)
	if string(got.Args) != "null" || got.ArgsOmitted != harness.ArgsTooLarge ||
		got.ArgsDigest != harness.ArgsDigest(args) || got.Command != "" {
		t.Errorf("= %+v, want null arguments with their digest and the command cleared", got)
	}

	if _, err := encodeAction(base, 100); err == nil {
		t.Error("an action over the limit with nothing left to shed was encoded")
	}

	// No HTML escaping: the bytes the app had are the bytes that travel.
	body, _ = encodeAction(harness.Action{ID: "x", Command: "a < b && c > d", Args: json.RawMessage(`{"q":"<&>"}`)}, 1<<20)
	if !strings.Contains(string(body), `a < b && c > d`) || !strings.Contains(string(body), `"<&>"`) {
		t.Errorf("escaped: %s", body)
	}
	if body[len(body)-1] != '\n' {
		t.Error("an event line ends in a newline")
	}
}

// TestSession_RecordActionSendsWhatEncodeActionAllows: the session's own
// send path is the one that sheds -- an action whose arguments carry more
// than the stream may take leaves without them, and text is not escaped.
func TestSession_RecordActionSendsWhatEncodeActionAllows(t *testing.T) {
	f := newFakeSender()
	s := &session{id: "s-big", sender: f, logger: slog.Default()}
	huge := json.RawMessage(`{"patch":"` + strings.Repeat("x", maxActionBytes) + `"}`)
	s.recordAction(harness.Action{Type: harness.ActionEventType, V: harness.RecordVersion, Seq: 1, Turn: 1,
		ID: "big", Tool: harness.ActionFSWrite, AppTool: "fileChange", Args: huge, Cwd: "/w"})
	s.recordAction(harness.Action{Type: harness.ActionEventType, V: harness.RecordVersion, Seq: 2, Turn: 1,
		ID: "small", Tool: harness.ActionExec, AppTool: "commandExecution", Args: json.RawMessage(`{}`),
		Cwd: "/w", Command: "a < b && c > d"})
	got := f.recorded()
	if len(got) != 2 {
		t.Fatalf("sent %d chunks, want 2", len(got))
	}
	if len(got[0].data) > maxActionBytes {
		t.Errorf("the large action went out at %d bytes, over %d", len(got[0].data), maxActionBytes)
	}
	var big harness.Action
	if err := json.Unmarshal([]byte(got[0].data), &big); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(big.Args) != "null" || big.ArgsOmitted != harness.ArgsTooLarge || big.ArgsDigest != harness.ArgsDigest(huge) {
		t.Errorf("the large action = %+v, want its arguments shed with their digest", big)
	}
	if !strings.Contains(got[1].data, "a < b && c > d") {
		t.Errorf("the command was escaped on the way out: %s", got[1].data)
	}
}

func TestContentPolicyLeavesADeleteAlone(t *testing.T) {
	ws := t.TempDir()
	path := writeFile(t, ws, "still-here.txt", "x")
	got := readOne(policyFor(ws), harness.ContentDelete, path)
	if got.Digest != "" || got.Data != nil || got.Omitted != "" || got.Bytes != nil {
		t.Errorf("a delete read the file it deleted: %+v", got)
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w", "/w/..foo", true},
		{"/w", "/wx", false},
		{"/w", "/", false},
		{"/w", "/other/w", false},
	} {
		if got := within(tc.root, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}
