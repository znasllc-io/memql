package appsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// record.go is the session's half of the recording (memql-cockpit#440).
//
// The harness decides WHICH files a call touched, out of the app's own
// protocol; this file decides whether their bytes leave this machine, and
// then sends the call on as an `event` chunk the transcript cap never
// swallows. The split is the point: what a protocol says and what this
// machine allows are different questions, and only the second is the
// machine owner's.

// sessionScaffoldDir is the session's own directory inside the workspace:
// the transcript, and for Codex the per-session CODEX_HOME with the bearer
// in its config.toml and the user's auth.json linked into it.
const sessionScaffoldDir = ".memql-session"

// mcpTempPrefix starts the name of the temporary file writeFileAtomic puts
// the bearer in before renaming it over the configuration.
const mcpTempPrefix = ".memql-mcp-"

// The recording's ceilings.
const (
	// maxInlineFileBytes is the largest file whose bytes travel inside its
	// action. A source file sits far below it; what sits above it -- a
	// lockfile, a bundle, a data file -- travels as its digest, which is
	// what a replay compares.
	maxInlineFileBytes int64 = 256 << 10
	// maxInlineEventBytes bounds what one action's files carry inline
	// together. The stream is shared with every other message this machine
	// sends, and a change touching forty files must not become one message
	// holding up a heartbeat.
	maxInlineEventBytes int64 = 1 << 20
	// maxActionBytes bounds one action on the wire, whatever it carries.
	// The two ceilings above bound only the file bytes the session adds;
	// ARGUMENTS travel whole, and some carry a file of their own -- a
	// Claude Code Write carries what it writes, and a Codex patch that
	// deletes a file carries all of it. The stream's message limit is
	// 32 MiB, shared with everything else in flight, and one action must
	// never be what reaches it (encodeAction).
	maxActionBytes = 8 << 20
	// maxDigestFileBytes is the largest file the recording hashes; past it
	// the size is reported and nothing is read. The same ceiling the
	// produced-file push draws.
	maxDigestFileBytes = maxPushFileBytes
	// maxDigestCacheEntries bounds the digests remembered for files too
	// large to travel (contentPolicy.digests).
	maxDigestCacheEntries = 256
	// digestSettle is how long a file must have gone unmodified before its
	// remembered digest is trusted: a write inside the filesystem's clock
	// tick can leave the size and the mtime exactly as they were, and git
	// guards its own stat cache the same way.
	digestSettle = 2 * time.Second
)

// sessionSink is the harness.Sink every turn of a session writes to.
type sessionSink struct{ s *session }

// Chunk sends narration the way it always went: redacted, numbered, and
// under the transcript cap.
func (k sessionSink) Chunk(stream string, data []byte) {
	_ = k.s.emitChunk(stream, data)
}

// Record sends one action.
//
// UNCAPPED, and that is why the recording has a method of its own.
// limits.max_transcript_bytes bounds the NARRATION the engine keeps on the
// session row; an action is not narration, it is the record of what the
// app did, and dropping the fortieth call because the app was chatty
// about the first thirty-nine would record a session that stopped doing
// things halfway through (emitRecord).
func (k sessionSink) Record(a harness.Action) {
	k.s.recordAction(a)
}

// recordAction reads the files the action names, under this machine's
// policy, and sends it.
func (s *session) recordAction(a harness.Action) {
	s.contentPolicy().fill(a.Contents)
	s.mu.Lock()
	if s.written == nil {
		s.written = map[string]bool{}
	}
	for _, c := range a.Contents {
		if c.Op == harness.ContentWrite {
			path := c.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(s.workspace, path)
			}
			s.written[filepath.Clean(path)] = true
		}
	}
	s.mu.Unlock()
	body, err := encodeAction(a, maxActionBytes)
	if err != nil {
		// Not sent at all. The action seq is dense, so the hole this leaves
		// is how the engine learns a call is missing.
		s.logger.Warn("an app action was too large to record", "id", a.ID, "seq", a.Seq, "error", err)
		return
	}
	// A send that fails for good leaves the same hole.
	if err := s.emitRecord(body); err != nil {
		s.logger.Warn("an app action could not be sent", "id", a.ID, "seq", a.Seq, "error", err)
	}
}

// encodeAction is one action as an event line of at most max bytes.
//
// Nearly every action is small. The ones that are not carry a file in
// their arguments, and they shed what costs least to lose, in order: the
// inline file bytes first (each file keeps its digest), then the
// arguments themselves -- null in their place, their digest in argsDigest
// and argsOmitted saying why, and the command, url and query that were
// read out of them cleared with them. An action still over after both is
// refused, and the caller sends nothing.
func encodeAction(a harness.Action, max int) ([]byte, error) {
	body, err := encodeLine(a)
	if err != nil || len(body) <= max {
		return body, err
	}
	// A copy: the slice is the caller's.
	a.Contents = append([]harness.Content(nil), a.Contents...)
	shed := false
	for i := range a.Contents {
		if c := &a.Contents[i]; c.Data != nil {
			c.Data, c.Encoding, c.Omitted = nil, "", harness.OmittedOverBudget
			shed = true
		}
	}
	if shed {
		if body, err = encodeLine(a); err != nil || len(body) <= max {
			return body, err
		}
	}
	a.ArgsDigest = harness.ArgsDigest(a.Args)
	a.Args, a.ArgsOmitted = json.RawMessage("null"), harness.ArgsTooLarge
	a.Command, a.URL, a.Query = "", "", ""
	if body, err = encodeLine(a); err != nil || len(body) <= max {
		return body, err
	}
	return nil, fmt.Errorf("%d bytes without its arguments or file contents, over the %d an action may be", len(body), max)
}

// encodeLine is v as one line of JSON, with no HTML escaping: "<" is a
// byte the app's text had, and json.Marshal would send six in its place.
func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *session) contentPolicy() *contentPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// contentPolicy decides which of the files an app names this machine
// reads back into the recording, and which of those bytes leave it.
//
// THE APP NAMES THE PATH, AND THE APP IS DRIVEN BY A PROMPT FROM SOMEWHERE
// ELSE. So the path is untrusted input, the stance CheckWorkspace takes
// toward the workspace itself: a file is read only if it resolves --
// symlinks followed -- to somewhere inside this session's workspace, is
// not the session's own scaffolding (the MCP configuration carrying the
// per-run bearer, the transcript, Codex's per-session home with the
// user's auth.json linked into it), and is a regular file, checked on the
// OPEN descriptor so a path swapped for a pipe cannot hang the session.
// Anything else is recorded by its path and the reason, and nothing is
// read.
//
// A READ NEVER SENDS MORE OF A FILE THAN THE APP ALREADY DID. A read's
// bytes travel only when the app's own result carried them whole (read,
// below); a write's travel because they are the session's product, as
// the outputs pushed at its end are. Everything else travels as a digest.
// What this does NOT claim: an app that wants a file off this machine can
// print it into its own tool results, which travel verbatim as they
// always have.
type contentPolicy struct {
	root     string
	excluded []string
	// secrets are the credentials this session was given; bytes holding
	// one never travel.
	secrets *redactor

	mu sync.Mutex
	// digests are remembered for files too large to travel, by resolved
	// path, so a session that reads one large file forty times hashes it
	// once.
	digests map[string]digestEntry
}

// digestEntry is one remembered digest, the file it was taken from, and
// how many credentials the file was scanned for when it was.
type digestEntry struct {
	info    os.FileInfo
	digest  string
	secrets int
}

// newContentPolicy builds the policy for a workspace, never reading the
// paths in excluded or anything beneath them. Every path is compared
// resolved, so the answer is the same on macOS, where /tmp is itself a
// link.
func newContentPolicy(workspace string, secrets *redactor, excluded ...string) *contentPolicy {
	p := &contentPolicy{root: resolvedPath(workspace), secrets: secrets}
	for _, e := range excluded {
		if strings.TrimSpace(e) != "" {
			p.excluded = append(p.excluded, resolvedPath(e))
		}
	}
	return p
}

// resolve returns the file to open for path, or the reason not to.
func (p *contentPolicy) resolve(path string) (string, string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.root, path)
	}
	real := resolvedPath(path)
	if !within(p.root, real) {
		return "", harness.OmittedOutsideWorkspace
	}
	for _, e := range p.excluded {
		if within(e, real) {
			return "", harness.OmittedScaffolding
		}
	}
	// Compared lowercased: on a Mac the filesystem ignores case, and
	// `.MEMQL-MCP-1` is the same file as `.memql-mcp-1`.
	base := strings.ToLower(filepath.Base(real))
	if strings.HasSuffix(base, backupSuffix) {
		// A configuration the session moved aside is the person's own,
		// held out of the way until the session restores it -- not part of
		// the run.
		return "", harness.OmittedScaffolding
	}
	if strings.HasPrefix(base, mcpTempPrefix) {
		// The bearer on its way into the configuration.
		return "", harness.OmittedScaffolding
	}
	return real, ""
}

// fill reads the files an action names, in place, under one inline
// budget for the whole action.
func (p *contentPolicy) fill(contents []harness.Content) {
	if p == nil {
		return
	}
	budget := maxInlineEventBytes
	for i := range contents {
		p.read(&contents[i], &budget)
	}
}

// read fills in one file: what is at its path now, and whether its bytes
// travel.
//
// A READ'S BYTES TRAVEL ONLY WHEN THE APP ITSELF REPORTED THEM WHOLE --
// when the file still holds exactly what the app's own result carried
// (Content.Seen). Those bytes left the machine already, in that result,
// so carrying them again widens nothing. A read that looked at one line
// of a file of secrets (`head -1 .env`) reported one line, and the
// recording must not be what sends the rest; the digest stands in.
//
// A WRITE'S BYTES ARE THE APP'S OWN OUTPUT and travel -- except inside a
// dependency or version-control directory. Nothing under node_modules is
// the session's work, and `.git/config` holds remote URLs and sometimes
// the tokens in them.
//
// And whatever else holds, bytes carrying a credential this session was
// given never travel, and neither does their digest.
func (p *contentPolicy) read(c *harness.Content, budget *int64) {
	seen := c.Seen
	c.Seen = nil
	if c.Op == harness.ContentDelete {
		// Nothing is left at the path to read, by definition.
		return
	}
	real, why := p.resolve(c.Path)
	if why != "" {
		c.Omitted = why
		return
	}
	got := p.readFile(real)
	c.Bytes, c.Digest, c.Omitted = got.size, got.digest, got.omitted
	switch {
	case got.omitted != "":
	case got.credential:
		c.Digest, c.Omitted = "", harness.OmittedCredential
	case got.data == nil:
		c.Omitted = harness.OmittedOverCeiling
	case !p.mayCarry(c.Op, got.where, got.data, seen):
		c.Omitted = harness.OmittedDigestOnly
	case int64(len(got.data)) > *budget:
		c.Omitted = harness.OmittedOverBudget
	default:
		*budget -= int64(len(got.data))
		c.Encoding, c.Data = encodeInline(got.data)
	}
}

// mayCarry is the rule read describes: whether a file's bytes may travel,
// once everything else allows it.
func (p *contentPolicy) mayCarry(op, real string, data, seen []byte) bool {
	if op == harness.ContentRead {
		return seen != nil && bytes.Equal(seen, data)
	}
	rel, err := filepath.Rel(p.root, real)
	if err != nil {
		return false
	}
	for _, dir := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		if pushExcludedDirs[strings.ToLower(dir)] {
			return false
		}
	}
	// Selection is not the only upload path: recheck every ancestor on the
	// resolved path before carrying a write's bytes or retaining an output.
	for dir := filepath.Dir(real); ; dir = filepath.Dir(dir) {
		if isBrowserRuntimeDir(dir) {
			return false
		}
		if dir == p.root || filepath.Dir(dir) == dir {
			break
		}
	}
	return true
}

// fileRead is what the recording learned about one file.
type fileRead struct {
	size   *int64
	digest string
	// data is the file's bytes when there were at most keepMax of them.
	data    []byte
	omitted string
	// credential says the file holds a credential this session was given,
	// found while it was hashed, at any size.
	credential bool
	// where is the file that opened, as confirm placed it.
	where string
}

// readFile hashes one file the policy resolved, keeping its bytes when
// they could travel and scanning them for the session's credentials in
// the same pass.
//
// Everything after the open is decided on the file that OPENED (where):
// the write rule and the remembered digests too, not only confirm.
func (p *contentPolicy) readFile(real string) fileRead {
	f, info, got := openRegular(real)
	if f == nil {
		return got
	}
	defer f.Close()
	where, why := p.confirm(f, info, real)
	if why != "" {
		return fileRead{omitted: why}
	}
	secrets := p.secrets.count()
	if info.Size() > maxInlineFileBytes && info.Size() <= maxDigestFileBytes {
		if digest, ok := p.remembered(where, info, secrets); ok {
			size := info.Size()
			return fileRead{size: &size, digest: digest, where: where}
		}
	}
	scan := p.secrets.scanner()
	got = hashOpen(f, info, maxInlineFileBytes, scan)
	got.where = where
	if scan != nil && scan.found {
		got.credential = true
		got.data = nil
		return got
	}
	if got.digest != "" && got.data == nil && got.size != nil && *got.size == info.Size() {
		p.remember(where, info, got.digest, secrets)
	}
	return got
}

// confirm checks the file that OPENED, not the path that was asked for.
//
// resolve judged a path, and the open came after it. In between, a
// directory on the way could have been swapped for a link out of the
// workspace -- O_NOFOLLOW guards only the last component. So the open
// descriptor is asked where it is (/proc on Linux); where nothing can
// say, the path is resolved again and must name the very file already
// open. Then the file, and every directory between it and the workspace,
// is compared BY IDENTITY with the session's own files, because a name
// compare misses a second name for the same file: a hard link to the
// bearer's configuration, or `.MCP.json` on a filesystem that ignores
// case -- the default on a Mac.
//
// It returns where the open file is, or the reason not to read it.
func (p *contentPolicy) confirm(f *os.File, info os.FileInfo, real string) (string, string) {
	where, ok := openedPath(f)
	if !ok {
		where = resolvedPath(real)
		now, err := os.Stat(where)
		if err != nil || !os.SameFile(now, info) {
			// The path names another file than the one open: it changed
			// under the read, and what is open cannot be placed.
			return "", harness.OmittedUnreadable
		}
	}
	if !within(p.root, where) {
		return "", harness.OmittedOutsideWorkspace
	}
	for _, e := range p.excluded {
		if within(e, where) {
			return "", harness.OmittedScaffolding
		}
	}
	if p.isScaffolding(where, info) {
		return "", harness.OmittedScaffolding
	}
	return where, ""
}

// isScaffolding reports whether the open file, or a directory between it
// and the workspace, IS one of the excluded paths.
func (p *contentPolicy) isScaffolding(where string, info os.FileInfo) bool {
	var marks []os.FileInfo
	for _, e := range p.excluded {
		if m, err := os.Stat(e); err == nil {
			marks = append(marks, m)
		}
	}
	for _, m := range marks {
		if os.SameFile(m, info) {
			return true
		}
	}
	for dir := filepath.Dir(where); within(p.root, dir); dir = filepath.Dir(dir) {
		if d, err := os.Stat(dir); err == nil {
			for _, m := range marks {
				if m.IsDir() && os.SameFile(m, d) {
					return true
				}
			}
		}
		if dir == p.root || dir == filepath.Dir(dir) {
			break
		}
	}
	return false
}

// remembered is the digest taken earlier of this very file, unchanged
// since, settled, and scanned for every credential the session holds now.
// Only a file found to hold none is ever remembered.
func (p *contentPolicy) remembered(real string, info os.FileInfo, secrets int) (string, bool) {
	if time.Since(info.ModTime()) < digestSettle {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.digests[real]
	if !ok || e.secrets != secrets || !os.SameFile(e.info, info) || e.info.Size() != info.Size() ||
		!e.info.ModTime().Equal(info.ModTime()) {
		return "", false
	}
	return e.digest, true
}

// remember keeps a digest for remembered, dropping an arbitrary entry
// when the cache is full.
func (p *contentPolicy) remember(real string, info os.FileInfo, digest string, secrets int) {
	if time.Since(info.ModTime()) < digestSettle {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.digests == nil {
		p.digests = map[string]digestEntry{}
	}
	if _, held := p.digests[real]; !held && len(p.digests) >= maxDigestCacheEntries {
		for k := range p.digests {
			delete(p.digests, k)
			break
		}
	}
	p.digests[real] = digestEntry{info: info, digest: digest, secrets: secrets}
}

// readForRecord hashes a file the session itself put somewhere -- a
// Library input it pulled -- keeping its bytes when there are at most
// keepMax of them. No policy: the path is the session's own.
func readForRecord(path string, keepMax int64) fileRead {
	f, info, got := openRegular(path)
	if f == nil {
		return got
	}
	defer f.Close()
	return hashOpen(f, info, keepMax, nil)
}

// openRegular opens a file for the recording and refuses anything but a
// regular file, on the descriptor. A nil file comes with the reason.
func openRegular(path string) (*os.File, os.FileInfo, fileRead) {
	f, err := openForRecord(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fileRead{omitted: harness.OmittedNotFound}
		}
		return nil, nil, fileRead{omitted: harness.OmittedUnreadable}
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fileRead{omitted: harness.OmittedUnreadable}
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fileRead{omitted: harness.OmittedNotRegular}
	}
	return f, info, fileRead{}
}

// hashOpen hashes an open regular file and keeps its bytes when there
// are at most keepMax of them; scan, when there is one, sees every byte
// the hash does.
func hashOpen(f *os.File, info os.FileInfo, keepMax int64, scan *secretScan) fileRead {
	size := info.Size()
	if size > maxDigestFileBytes {
		return fileRead{size: &size, omitted: harness.OmittedTooLarge}
	}
	keep := &capBuffer{max: keepMax}
	if size > keepMax {
		keep.max = 0
	}
	hash := sha256.New()
	sinks := []io.Writer{hash, keep}
	if scan != nil {
		sinks = append(sinks, scan)
	}
	n, err := io.Copy(io.MultiWriter(sinks...), io.LimitReader(f, maxDigestFileBytes+1))
	if err != nil {
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	if n > maxDigestFileBytes {
		// It grew while it was being read.
		return fileRead{size: &n, omitted: harness.OmittedTooLarge}
	}
	out := fileRead{size: &n, digest: harness.DigestPrefix + hex.EncodeToString(hash.Sum(nil))}
	if n <= keepMax && int64(len(keep.buf)) == n {
		out.data = keep.buf
		if out.data == nil {
			out.data = []byte{}
		}
	}
	return out
}

// encodeInline carries bytes as the text they are, when they are text,
// and as base64 otherwise.
func encodeInline(data []byte) (string, *string) {
	if utf8.Valid(data) {
		s := string(data)
		return harness.EncodingUTF8, &s
	}
	s := base64.StdEncoding.EncodeToString(data)
	return harness.EncodingBase64, &s
}

// capBuffer keeps at most max bytes and reports every write whole, so the
// hash beside it in a MultiWriter still sees the entire file.
type capBuffer struct {
	buf []byte
	max int64
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - int64(len(b.buf)); room > 0 {
		if int64(len(p)) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

// resolvedPath resolves symlinks as far as the path exists and keeps the
// rest as written, so a file that does not exist yet still compares
// against the directory it would be in.
func resolvedPath(path string) string {
	clean := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean
	}
	return filepath.Join(resolvedPath(parent), filepath.Base(clean))
}

// within reports whether path is root or lies beneath it. Both must be
// resolved: the test is lexical, and only sound on paths with no links
// left in them.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
