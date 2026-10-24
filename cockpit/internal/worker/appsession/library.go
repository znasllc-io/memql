package appsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// library.go moves a session's inputs in and its outputs back out
// (memql-cockpit#349), authenticating with the SESSION credential.
//
// That credential is user-scoped: its `sub` is the owning user, so these
// calls see exactly what that user could see in their browser. A refusal
// therefore says something about the USER's access, not about the
// cockpit's configuration, and the error text has to make that
// distinction or an operator spends the afternoon checking a worker token
// that was never the problem.
//
// One wrinkle the issue's wording did not anticipate, recorded here
// because it changes the error text: the engine's download route answers
// **404 on deny**, deliberately, so that a URL cannot be used to probe
// which artifact ids exist. A denied read and a missing artifact are
// therefore indistinguishable on the wire, and this says so rather than
// asserting one of them.

const (
	// libraryTimeout bounds one artifact transfer. Generous, because a
	// Library file can be a site bundle; bounded, because a session that
	// hangs on an input never starts and never fails either.
	libraryTimeout = 10 * time.Minute

	// maxPushFileBytes is the per-file ceiling on what a run's output may
	// push. The engine's own cap is 256 MB; this is deliberately lower.
	// A delegated run produces work product, and a 200 MB artifact from a
	// coding agent is a build directory somebody forgot to exclude.
	maxPushFileBytes int64 = 64 << 20

	// maxPushFiles bounds Library output uploads; larger sets are bundled.
	maxPushFiles = 32
	// Bound archive metadata independently of the number of Library requests.
	maxOutputEntries = 4096
	// Bound aggregate uploads too: 32 individually legal files need not be
	// two GiB of evidence. Omitted candidates remain in the retention manifest.
	maxPushTotalBytes int64 = 128 << 20
)

// pushExcludedDirs never travel back as artifacts. These are caches and
// dependency trees: they are enormous, they are reproducible from the
// files that ARE pushed, and pushing them would bury the actual output.
var pushExcludedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".next": true, ".cache": true,
	".venv": true, "venv": true, "__pycache__": true,
	".terraform": true, ".gradle": true, ".idea": true,
}

// Library is the Library API client for one session.
//
// The credential is held behind a mutex because renewal replaces it
// mid-session, and the transcript push at the end must use whatever the
// current one is rather than the one the session opened with.
type Library struct {
	baseURL string
	client  *http.Client

	mu         sync.RWMutex
	credential string
}

// NewLibrary builds a client against a cluster origin.
func NewLibrary(baseURL, credential string, client *http.Client) *Library {
	if client == nil {
		client = &http.Client{Timeout: libraryTimeout}
	}
	return &Library{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:     client,
		credential: credential,
	}
}

// SetCredential swaps the bearer after a renewal.
func (l *Library) SetCredential(credential string) {
	if l == nil || strings.TrimSpace(credential) == "" {
		return
	}
	l.mu.Lock()
	l.credential = credential
	l.mu.Unlock()
}

func (l *Library) bearer() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.credential
}

// LibraryBaseURL derives the Library origin from the worker's
// cluster_url.
//
// The Library's two byte-bearing routes are served by the same front door
// the worker already dials -- there is no second host to configure, and
// inventing one would be a knob an operator has to keep in sync with
// something they cannot see.
func LibraryBaseURL(clusterURL string) (string, error) {
	raw := strings.TrimSpace(clusterURL)
	if raw == "" {
		return "", errors.New("library: cluster_url is empty")
	}
	if !strings.Contains(raw, "://") {
		// A bare host:port, which is how the worker config may carry a
		// gRPC target. https is the only sane assumption for a route
		// that carries a bearer.
		host := strings.TrimSuffix(raw, ":443")
		return "https://" + host, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("library: parse cluster_url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("library: cluster_url has no host: %s", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "grpc", "http":
		scheme = "http"
	default:
		scheme = "https"
	}
	host := strings.TrimSuffix(u.Host, ":443")
	return scheme + "://" + host, nil
}

// Pull fetches one artifact's bytes into dir and returns the path.
//
// Every input has to land BEFORE the app starts. An agent that begins
// work and finds its inputs half-arrived produces confidently wrong
// output rather than an error, and nothing downstream can tell the
// difference.
func (l *Library) Pull(ctx context.Context, id, dir string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("library: empty artifact id")
	}
	endpoint := l.baseURL + "/artifacts/" + url.PathEscape(id) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.bearer())

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", pullStatusError(id, resp)
	}

	name := downloadFileName(resp, id)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		// BOTH errors, joined. A close on a writable handle is where a
		// short write finally surfaces -- the copy fails with one
		// message and the close fails with ENOSPC -- and reporting only
		// the first sends somebody looking at the network for a full
		// disk. errors.Join with a nil second argument is just the
		// first, so the ordinary case reads exactly as it did.
		closeErr := f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("library: pull %s: %w", id, errors.Join(err, closeErr))
	}
	// The close is checked on the success path too, and it is the ONE
	// that decides whether this file is whole: a buffered write that
	// could not be flushed reports here and nowhere else, so returning
	// the path without checking would hand back a truncated input for
	// an agent to work from.
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	return path, nil
}

// pullStatusError turns a refusal into text that names the right party.
func pullStatusError(id string, resp *http.Response) error {
	return libraryStatusError("input", id, resp)
}

// pushStatusError is the same for the upload direction. A push has no 404
// -- there is no id to miss yet -- so the shared function's 404 branch is
// simply never reached from here.
func pushStatusError(name string, resp *http.Response) error {
	return libraryStatusError("push", name, resp)
}

// libraryStatusError turns a refusal into text that names the right party.
//
// The credential acts as the OWNING USER, so most refusals are statements
// about that user's access rather than about this machine. Getting this
// wrong sends an operator to check a worker token that is working fine.
//
// THE 401 BRANCH USED TO SAY THE WRONG THING, and the reason is worth
// keeping because the wrong reading is the one a search engine will still
// hand you. It said "the bearer is expired or malformed". Before
// memql#4863 that was false in the ordinary case: the bearer was fine and
// the Library's byte routes simply did not admit its class, because an
// app session's back-channel was minted as class="service_account" and
// those routes require the actor to resolve to a USER. Since memql#4863
// the class is `app_session` and its `sub` IS the owning user's id, so
// that reading is now DEAD for an app session -- and stating it would
// send the next person to re-read an auth path that is working.
//
// What is left is genuinely two things the wire does not distinguish (the
// route answers a bare "unauthorized" either way), so both are named,
// most likely first:
//
//   - EXPIRY, which is the expected one. The mint caps an app-session
//     credential's lifetime whatever the engine asked for, precisely
//     because the bearer lands in a file on somebody's laptop. A session
//     that outlives its credential is therefore normal rather than
//     exceptional, and the fix is a renewal in place --
//     AppSessionControl{action: "renew_credential"} -- not a restart.
//   - A credential of some OTHER class reaching this code at all, which
//     would be a cockpit bug rather than a user's access problem.
//
// Row authorization is NOT in here on purpose: it is 403 or 404 below,
// and folding it into the 401 would report "you may not read this" as
// "your login failed".
func libraryStatusError(what, subject string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("library: %s %s: the session credential was rejected (401) -- "+
			"most likely it expired, which is ordinary for a long session because the mint caps "+
			"its lifetime; the engine renews it in place with "+
			"AppSessionControl{action: \"renew_credential\"}. If it has not expired, this bearer is "+
			"not the app-session credential (class \"app_session\") the Library admits, which is a "+
			"cockpit bug rather than an access problem for the owning user", what, subject)
	case http.StatusForbidden:
		return fmt.Errorf("library: %s %s: the owning user cannot reach this artifact (403) -- "+
			"this is an access question for that user, not a cockpit misconfiguration", what, subject)
	case http.StatusNotFound:
		// The route answers 404 on deny by design, so a URL cannot probe
		// which ids exist. Both readings are stated because the wire
		// genuinely does not distinguish them.
		return fmt.Errorf("library: %s %s: not found (404) -- either the artifact does not exist, "+
			"or the owning user cannot reach it; the Library answers 404 for both so a link cannot "+
			"be used to probe which ids exist", what, subject)
	default:
		return fmt.Errorf("library: %s %s: %s", what, subject, strings.TrimSpace(resp.Status))
	}
}

// downloadFileName picks the on-disk name for a pulled artifact, from the
// server's Content-Disposition when it offered one.
func downloadFileName(resp *http.Response, id string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := sanitizeFileName(params["filename"]); name != "" {
				return name
			}
		}
	}
	if name := sanitizeFileName(id); name != "" {
		return name
	}
	return "artifact"
}

// PushResult is one uploaded artifact.
type PushResult struct {
	ArtifactId string `json:"artifactId"`
	FileId     string `json:"fileId"`
}

// Push uploads one file and returns its artifact id.
func (l *Library) Push(ctx context.Context, path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	return l.pushReader(ctx, f, name)
}

// PushBytes uploads an in-memory body -- the transcript path, which never
// needs to touch disk.
func (l *Library) PushBytes(ctx context.Context, body []byte, name string) (string, error) {
	return l.pushReader(ctx, bytes.NewReader(body), name)
}

func (l *Library) pushReader(ctx context.Context, r io.Reader, name string) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": libraryFormFileKey, "filename": name}))
	header.Set("Content-Type", producedContentType(name, data))
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if err := writer.WriteField(libraryFormNameKey, name); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/artifacts", &buf)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.bearer())
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// Through the same reader as the pull direction, so a 401 on the
		// way out says what a 401 on the way in says. It did not before,
		// and a run that pulled its inputs fine and then failed to push
		// its output reported the two halves of one expiry as two
		// unrelated problems.
		return "", pushStatusError(name, resp)
	}
	var out PushResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("library: push %s: decode response: %w", name, err)
	}
	if strings.TrimSpace(out.ArtifactId) == "" {
		return "", fmt.Errorf("library: push %s: server returned no artifact id", name)
	}
	return out.ArtifactId, nil
}

// The Library honors an explicit part type. CreateFormFile's octet-stream
// default therefore stranded JSON manifests and logs behind the binary-reader
// gate. Classify the existing bytes here; never rewrite them or relax that gate.
// A filename alone cannot turn binary data into text, and a 512-byte sniff can
// miss a binary suffix, so text declarations require the whole body to be UTF-8
// without binary control bytes. Recognized binary signatures keep their type.
// This is a format hint, not trust, schema validation or an indexing guarantee.
func producedContentType(name string, data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}
	sniffed := http.DetectContentType(data)
	if !strings.HasPrefix(sniffed, "text/") {
		return sniffed
	}
	if !utf8.Valid(data) {
		return "application/octet-stream"
	}
	for _, b := range data {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' || b == 0x7f {
			return "application/octet-stream"
		}
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		if json.Valid(data) {
			return "application/json"
		}
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	}
	return sniffed
}

// The multipart field names the Library's upload route reads.
const (
	libraryFormFileKey = "file"
	libraryFormNameKey = "name"
)

// producedFile is one candidate for the push at session end.
type producedFile struct {
	path string
	rel  string
	size int64
	info os.FileInfo
}

// collectProduced walks the workspace for files the run created or
// changed, measured against a snapshot taken before it started.
//
// Bounded on purpose, and the caller LOGS what was dropped. A silent
// truncation reads downstream as "this is everything the run produced",
// which is the one thing a record must never be wrong about.
func collectProduced(workspace string, before map[string]fileStamp, writes ...map[string]bool) ([]producedFile, []string) {
	var out []producedFile
	var skipped []string
	inputs := map[string]bool{}
	explicit := map[string]bool{}
	for _, paths := range writes {
		for path := range paths {
			explicit[path] = true
		}
	}

	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (cannot inspect output: %v)", path, err))
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if isBrowserRuntimeDir(path) {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				files, err := repositoryInputs(path)
				if err != nil {
					skipped = append(skipped, fmt.Sprintf("%s (cannot distinguish checkout inputs from outputs: %v)", path, err))
					return filepath.SkipDir
				}
				for file := range files {
					inputs[file] = true
				}
			}
			if path == workspace {
				return nil
			}
			if pushExcludedDirs[name] {
				return filepath.SkipDir
			}
			// The session's own scaffolding is not output.
			if name == ".memql-session" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".git" || name == ".mcp.json" || strings.HasSuffix(name, backupSuffix) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		if prior, existed := before[rel]; existed && prior.matches(info) {
			return nil
		}
		// A checkout or branch switch changes timestamps without authoring
		// evidence. Only dirty files or recorded writes override repository input.
		if inputs[path] && !explicit[path] {
			return nil
		}
		if info.Size() > maxPushFileBytes {
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte per-file cap)",
				rel, info.Size(), maxPushFileBytes))
			return nil
		}
		out = append(out, producedFile{path: path, rel: rel, size: info.Size(), info: info})
		return nil
	})

	// Deterministic order, so two runs that produced the same files
	// report their ids in the same order.
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	var selected []producedFile
	var bytes int64
	for _, f := range out {
		if len(selected) >= maxOutputEntries {
			skipped = append(skipped, fmt.Sprintf("%s (over the %d-file cap)", f.rel, maxOutputEntries))
		} else if f.size > maxPushTotalBytes-bytes {
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte total cap)", f.rel, f.size, maxPushTotalBytes))
		} else {
			selected = append(selected, f)
			bytes += f.size
		}
	}
	return selected, skipped
}

// artifactName flattens a workspace-relative path into one path segment.
//
// The Library stores an artifact under the LAST segment of the name it is
// given -- multipart's own reader applies filepath.Base to a part
// filename, and the storage key is a single segment either way. So a
// nested path sent verbatim arrives as its basename, and a run that wrote
// both `api/schema.json` and `web/schema.json` would push two artifacts
// indistinguishably named `schema.json`.
//
// Flattening keeps the location in the name instead of throwing it away.
// The separator is doubled so it cannot be confused with an underscore
// the file itself had.
func artifactName(rel string) string {
	flat := strings.ReplaceAll(filepath.ToSlash(rel), "/", "__")
	if name := sanitizeFileName(flat); name != "" {
		return name
	}
	return "artifact"
}

// fileStamp identifies a file well enough to notice a rewrite.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func (s fileStamp) matches(info os.FileInfo) bool {
	return s.size == info.Size() && s.modTime.Equal(info.ModTime())
}

// snapshotWorkspace records the workspace as it stood before the run, so
// the push at the end carries what the run PRODUCED rather than
// everything that happens to be in the directory.
func snapshotWorkspace(workspace string) map[string]fileStamp {
	out := make(map[string]fileStamp)
	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isBrowserRuntimeDir(path) || (path != workspace && (pushExcludedDirs[d.Name()] || d.Name() == ".memql-session")) {
				return filepath.SkipDir
			}
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		out[rel] = fileStamp{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	return out
}

// sanitizeFileName reduces a server-supplied name to a single safe path
// segment. A Content-Disposition filename is attacker-influenced input in
// the general case, and this one lands inside somebody's workspace.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = filepath.Base(filepath.FromSlash(name))
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
