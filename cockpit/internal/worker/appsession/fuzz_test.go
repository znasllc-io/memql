package appsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzz targets for the four appsession surfaces that take input from
// somewhere other than this machine.
//
// EACH ONE ASSERTS A SECURITY PROPERTY RATHER THAN A VALUE. A fuzz
// target that only checks "does not panic" finds crashes and nothing
// else, and none of these functions crashes -- their failure mode is
// returning something plausible that is wrong. So each property below
// is the thing that would actually go wrong: a filename that escapes a
// directory, an id that escapes a directory, a bearer that survives
// redaction, a path an app names that the recording reads from outside
// the workspace.

// FuzzSanitizeFileName. The name comes from a SERVER, in the
// Content-Disposition header of an artifact download, and it is joined
// onto a workspace path. The property is that the result is a single
// path segment which cannot leave the directory it is joined to -- for
// any input at all, including ones no reasonable server would send.
func FuzzSanitizeFileName(f *testing.F) {
	for _, seed := range []string{
		"report.pdf", "", "   ", ".", "..", "/", "\\",
		"../../escaped.txt", "..\\..\\escaped.txt",
		"/etc/passwd", "C:\\Windows\\system32",
		"a/b/c.txt", "....//....//x", "\x00null.txt", "\nnewline.txt",
		strings.Repeat("x", 500), "résumé.pdf", "🙂.png",
	} {
		f.Add(seed)
	}

	const dir = "/workspace"
	f.Fuzz(func(t *testing.T, name string) {
		got := sanitizeFileName(name)
		if got == "" {
			// An empty result is a refusal, and the caller falls back.
			return
		}
		if strings.ContainsRune(got, '/') || strings.ContainsRune(got, filepath.Separator) {
			t.Fatalf("sanitizeFileName(%q) = %q, which is not one path segment", name, got)
		}
		if got == "." || got == ".." {
			t.Fatalf("sanitizeFileName(%q) = %q, which names a directory", name, got)
		}
		// THE PROPERTY THAT MATTERS: joined onto a workspace, it stays
		// inside it.
		joined := filepath.Join(dir, got)
		if !strings.HasPrefix(joined, dir+string(filepath.Separator)) {
			t.Fatalf("sanitizeFileName(%q) = %q, which escapes to %q", name, got, joined)
		}
		if filepath.Dir(joined) != dir {
			t.Fatalf("sanitizeFileName(%q) = %q, which lands in %q", name, got, filepath.Dir(joined))
		}
	})
}

// FuzzSanitizeLedgerName. The id is MINTED BY THE SERVER and becomes a
// filename in the MCP-config ledger -- the directory whose entries are
// how a SIGKILLed session's config files are found and deleted. An id
// that escaped it would write outside the state directory, and the
// sweeper would miss the file it was meant to clean up.
func FuzzSanitizeLedgerName(f *testing.F) {
	for _, seed := range []string{
		"sess-123", "", "..", "../../../etc/cron.d/x", "a/b",
		"a\\b", "\x00", strings.Repeat("z", 400), "  ", "…unicode…",
	} {
		f.Add(seed)
	}

	const dir = "/state/ledger"
	f.Fuzz(func(t *testing.T, id string) {
		got := sanitizeLedgerName(id)
		if got == "" {
			t.Fatalf("sanitizeLedgerName(%q) = %q; it must always name something", id, got)
		}
		if len(got) > 128 {
			t.Fatalf("sanitizeLedgerName(%q) is %d bytes, past the cap", id, len(got))
		}
		for _, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Fatalf("sanitizeLedgerName(%q) = %q, which contains %q", id, got, r)
			}
		}
		joined := filepath.Join(dir, got)
		if filepath.Dir(joined) != dir {
			t.Fatalf("sanitizeLedgerName(%q) = %q, which lands in %q", id, got, filepath.Dir(joined))
		}
	})
}

// FuzzRedactor. THE PROPERTY IS THAT THE SECRET NEVER SURVIVES.
//
// Everything an app writes to stdout or stderr passes through here on
// its way to the cluster, and what it is protecting is the per-run
// bearer: an app that echoes its own MCP configuration, or curl printing
// a request it made, puts the credential in the stream. A single escape
// publishes it into a transcript that is stored and read later.
//
// The fuzzer supplies both the secret and the surrounding text, so it
// gets to try the overlapping and repeated arrangements a hand-written
// case would not think of ("aaa" inside "aaaa", a secret that is a
// prefix of itself, a secret spanning a rune boundary).
func FuzzRedactor(f *testing.F) {
	f.Add("0000000[", "00000000000000[")
	f.Add("session credential", "a session credential here")
	for _, seed := range []struct{ secret, body string }{
		{"mql_wkr_abc123", "Authorization: Bearer mql_wkr_abc123"},
		{"aa", "aaaa"},
		{"a", "aaaaaaaa"},
		{"secret", "no secret here, well actually secret"},
		{"x", ""},
		{"", "nothing to redact"},
		{"🙂", "prefix 🙂 suffix"},
		{"line\nbreak", "a line\nbreak inside"},
	} {
		f.Add(seed.secret, seed.body)
	}

	f.Fuzz(func(t *testing.T, secret, body string) {
		r := newRedactor(secret)

		// SHORT STRINGS ARE DELIBERATELY NOT REDACTED, and asserting
		// that is half the point of this target. A redactor that
		// matched a two-character string would scribble over ordinary
		// output until a transcript was unreadable; no credential this
		// cockpit handles is that short. The threshold is read from the
		// production constant rather than copied, so the two cannot
		// drift.
		if len(secret) < minRedactableSecret {
			if got := r.apply2(body); got != body {
				t.Fatalf("a %d-byte string changed the output: apply2(%q) = %q",
					len(secret), body, got)
			}
			if hb := r.holdBack(); hb != 0 {
				t.Fatalf("a %d-byte string set holdBack to %d", len(secret), hb)
			}
			return
		}

		if got := r.apply2(body); strings.Contains(got, secret) {
			t.Fatalf("the secret survived redaction: apply2(%q) = %q", body, got)
		}
		if got := r.apply([]byte(body)); strings.Contains(string(got), secret) {
			t.Fatalf("the secret survived redaction: apply(%q) = %q", body, got)
		}

		// And the hold-back is large enough that a secret cannot be
		// split across two flushes and survive in halves. It is the
		// invariant the chunker depends on, and it is stated in bytes
		// because that is what the chunker holds.
		if hb := r.holdBack(); hb < len(secret)-1 {
			t.Fatalf("holdBack() = %d for a %d-byte secret; a split secret could survive", hb, len(secret))
		}
	})
}

// FuzzContentPolicyStaysInTheWorkspace. The path comes from the APP, and
// a prompt from somewhere else drives the app; the policy decides whether
// this machine reads the file back into the recording. The property is
// the one that would actually go wrong: whatever the path -- climbs,
// links, the scaffolding by another spelling -- the policy either refuses
// it or names a file inside the workspace, outside the scaffolding, with
// no link left in the path to follow.
func FuzzContentPolicyStaysInTheWorkspace(f *testing.F) {
	for _, seed := range []string{
		"notes.txt", "/etc/passwd", "../../etc/passwd", "..", ".", "", "/",
		".mcp.json", ".memql-session/codex/auth.json", "./.memql-session/../.mcp.json",
		"sub/../../x", "a/./b", "link-out", "link-out/deeper", "link-in/../notes.txt",
		".mcp.json.memql-session-backup", "\x00", strings.Repeat("../", 40) + "etc",
		".memql-mcp-123", "sub/.MEMQL-MCP-x", "A.MCP.JSON.MEMQL-SESSION-BACKUP",
	} {
		f.Add(seed)
	}
	ws, outside := f.TempDir(), f.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("x"), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link-out")); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink(ws, filepath.Join(ws, "link-in")); err != nil {
		f.Fatal(err)
	}
	p := newContentPolicy(ws, nil, filepath.Join(ws, sessionScaffoldDir), filepath.Join(ws, ".mcp.json"))
	root := resolvedPath(ws)

	f.Fuzz(func(t *testing.T, path string) {
		real, why := p.resolve(path)
		if why != "" {
			if real != "" {
				t.Fatalf("resolve(%q) refused (%s) and still named %q", path, why, real)
			}
			return
		}
		sep := string(filepath.Separator)
		if real != root && !strings.HasPrefix(real, root+sep) {
			t.Fatalf("resolve(%q) = %q, outside the workspace %q", path, real, root)
		}
		for _, name := range []string{sessionScaffoldDir, ".mcp.json"} {
			scaffold := filepath.Join(root, name)
			if real == scaffold || strings.HasPrefix(real, scaffold+sep) {
				t.Fatalf("resolve(%q) = %q, the session's scaffolding", path, real)
			}
		}
		// The bearer's temporary file and a configuration moved aside, in
		// any case a filesystem might ignore.
		if base := strings.ToLower(filepath.Base(real)); strings.HasPrefix(base, ".memql-mcp-") ||
			strings.HasSuffix(base, ".memql-session-backup") {
			t.Fatalf("resolve(%q) = %q, the session's scaffolding by another spelling", path, real)
		}
		if again, err := filepath.EvalSymlinks(real); err == nil && again != real {
			t.Fatalf("resolve(%q) = %q, which still has a link in it (-> %q)", path, real, again)
		}
	})
}
