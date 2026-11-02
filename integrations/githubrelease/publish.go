package githubrelease

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Target is trusted operator configuration, never artifact-supplied authority.
// The caller binds all non-secret values (including trust and credential
// references) into its reviewed destination. ReleaseID selects an existing
// draft; tag creation, draft creation and promotion are separate effects.
type Target struct {
	APIOrigin, UploadOrigin string
	DownloadOrigins         []string
	Repository, Tag         string
	SourceCommit, AssetName string
	ReleaseID               int64
	Token                   string
	RootCAs                 *x509.CertPool
	AllowLoopbackHTTP       bool
}

type Publisher struct {
	target    Target
	api       string
	upload    string
	downloads map[string]bool
}

// Receipt proves the exact asset observed at publication time, not continuing
// custody after an administrator edits a release or deletes its assets.
// ReplacedAssetID names the incomplete asset this publication removed first, so
// the caller can leave a trace of a remote delete; it is zero when there was none.
// It is the only field set when Publish returns an error, because a removal is
// worth reporting even when the upload after it failed.
type Receipt struct {
	Repository, Tag, SourceCommit, AssetName, SHA256 string
	ReleaseID, AssetID, Size                         int64
	ReplacedAssetID                                  int64
}

// ErrUncertain means an upload might have happened without verified readback.
// Preserve the caller's intent and artifact pins and reconcile the SAME target.
// Never create another release, overwrite an asset or report success on it.
var ErrUncertain = errors.New("GitHub release asset publication outcome is uncertain")

// ErrConflict means a complete asset's metadata or bytes differ from this
// receipt, or its state is one this adapter cannot read. It is never removed or
// overwritten: only the owner can say which copy is wrong. So it must mean a read
// that finished and differed: a read that failed proves nothing about the bytes
// and is an ordinary error. An asset GitHub still reports as incomplete is not a
// conflict either (asset.incomplete). After an attempted upload ErrUncertain wins.
var ErrConflict = errors.New("GitHub release asset conflicts with immutable receipt")

// ConflictError is ErrConflict with the asset it concerns, because a release can
// hold many and the owner has to find the one that is wrong. Its name comes from
// the approved target, never from a remote response.
type ConflictError struct{ AssetName string }

func (e *ConflictError) Error() string { return ErrConflict.Error() + ": " + e.AssetName }
func (e *ConflictError) Unwrap() error { return ErrConflict }

// settleDelays bounds how long a read after a write waits for GitHub to show that
// write. On 2026-10-30 a draft POST reported uncertain although GitHub held the
// draft two seconds earlier, and a retry reconciled it; the assumed cause, list
// lag, is not proven. A stopped call is expensive, since a many-file publication
// loses the rest of its run, and a re-read costs one GET. So each read gets a few
// more chances over fifteen seconds. It stays read-only: the write is never
// repeated, and a write still unseen afterward stays uncertain.
var settleDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// settled repeats a read-only observation until it reports done or the delays run
// out. It returns the last read's error; done false with no error is an answer
// the caller judges, since absence is an observation and never proof.
func settled(ctx context.Context, read func() (bool, error)) error {
	for attempt := 0; ; attempt++ {
		done, err := read()
		if done || attempt == len(settleDelays) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleDelays[attempt]):
		}
	}
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	assetNamePattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,253}[A-Za-z0-9_-])?$`)
)

func origin(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || strings.ContainsAny(u.Hostname(), "*%\\ \t\r\n") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("release endpoint must be an explicit origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !allowHTTP || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return "", errors.New("release endpoints require HTTPS")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

func validTag(tag string) bool {
	if len(tag) == 0 || len(tag) > 255 || strings.ContainsAny(tag, " ~^:?*[\\\x7f") || strings.Contains(tag, "..") || strings.Contains(tag, "@{") {
		return false
	}
	for _, c := range tag {
		if c < 33 || c > 126 {
			return false
		}
	}
	for _, part := range strings.Split(tag, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// NewPublisher validates without network activity. An empty Token permits
// configuration validation, but Publish requires an explicit credential.
// No ambient proxy, netrc, Git credential helper or host discovery is used.
func NewPublisher(t Target) (*Publisher, error) {
	return newPublisher(t, true)
}

func newPublisher(t Target, requireID bool) (*Publisher, error) {
	api, err := origin(t.APIOrigin, t.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	upload, err := origin(t.UploadOrigin, t.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	if !repositoryPattern.MatchString(t.Repository) || !validTag(t.Tag) || !commitPattern.MatchString(t.SourceCommit) || !assetNamePattern.MatchString(t.AssetName) || t.ReleaseID < 0 || (requireID && t.ReleaseID == 0) || len(t.Token) > 16<<10 {
		return nil, errors.New("release target requires exact repository, draft ID, tag, commit and stable asset name")
	}
	for _, c := range t.Token {
		if c < 33 || c > 126 {
			return nil, errors.New("release credential is invalid")
		}
	}
	if len(t.DownloadOrigins) > 16 {
		return nil, errors.New("too many release download origins")
	}
	downloads := map[string]bool{}
	for _, raw := range t.DownloadOrigins {
		d, err := origin(raw, false)
		if err != nil || d == api || d == upload || downloads[d] {
			return nil, errors.New("release downloads require distinct exact HTTPS origins")
		}
		downloads[d] = true
	}
	t.DownloadOrigins = append([]string(nil), t.DownloadOrigins...)
	if t.RootCAs != nil {
		t.RootCAs = t.RootCAs.Clone()
	}
	return &Publisher{target: t, api: api, upload: upload, downloads: downloads}, nil
}

// Publish performs one bounded protocol effect. The caller must already own an
// approved destination, durable intent and retained immutable source, and must
// be the release's only writer: removing an incomplete asset is safe only while
// no upload of ours is still sending it. A release is checked by ID and its tag
// resolved to the expected commit before and after upload/readback. GitHub has
// no conditional upload that atomically locks these facts; detected concurrent
// drift refuses or returns ErrUncertain.
func (p *Publisher) Publish(ctx context.Context, v *VerifiedFile) (Receipt, error) {
	if p == nil || v == nil || p.target.Token == "" {
		return Receipt{}, errors.New("publisher, explicit credential and verified file are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := v.acquire(ctx); err != nil {
		return Receipt{}, err
	}
	defer func() { <-v.gate }()
	if v.dir == "" {
		return Receipt{}, errors.New("verified release file is closed or uninitialized")
	}
	s, close := p.session()
	defer close()
	assetID, err := s.publish(ctx, v)
	if err != nil {
		partial := Receipt{ReplacedAssetID: s.replaced}
		if s.attempted {
			return partial, ErrUncertain
		}
		if ctx.Err() != nil {
			return partial, ctx.Err()
		}
		return partial, err
	}
	t := p.target
	return Receipt{Repository: t.Repository, Tag: t.Tag, SourceCommit: t.SourceCommit, ReleaseID: t.ReleaseID,
		AssetID: assetID, AssetName: t.AssetName, SHA256: v.expected.SHA256, Size: v.expected.Size, ReplacedAssetID: s.replaced}, nil
}

func (p *Publisher) session() (*publication, func()) {
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.target.RootCAs},
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10, MaxIdleConns: 4, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second, DisableCompression: true,
	}
	s := &publication{p: p, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return s, transport.CloseIdleConnections
}

type publication struct {
	p           *Publisher
	client      *http.Client
	requests    int
	maxRequests int
	attempted   bool
	replaced    int64
}

func (s *publication) releasePath() string {
	return "/repos/" + s.p.target.Repository + "/releases/" + strconv.FormatInt(s.p.target.ReleaseID, 10)
}

func (s *publication) publish(ctx context.Context, v *VerifiedFile) (int64, error) {
	if err := s.checkRelease(ctx); err != nil {
		return 0, err
	}
	a, count, err := s.findAsset(ctx)
	if err != nil {
		return 0, err
	}
	if a != nil && a.incomplete() {
		if a, count, err = s.removeIncomplete(ctx, a); err != nil {
			return 0, err
		}
	}
	if a == nil {
		if count >= 1000 {
			return 0, errors.New("release has reached its asset limit")
		}
		// Recheck mutable release facts immediately before the one POST. There
		// is deliberately no automatic POST retry, even on a lost response.
		if err := s.checkRelease(ctx); err != nil {
			return 0, err
		}
		f, err := os.Open(filepath.Join(v.dir, "asset"))
		if err != nil {
			return 0, errors.New("verified asset snapshot is unavailable")
		}
		u := s.p.upload + s.releasePath() + "/assets?name=" + url.QueryEscape(s.p.target.AssetName)
		s.attempted = true
		response, _ := s.request(ctx, http.MethodPost, u, f, v.expected.Size, "application/vnd.github+json", true)
		if response != nil {
			// Remote text and upload metadata are not proof. Reconcile by a
			// fresh listing and complete binary read regardless of POST status.
			_ = response.Body.Close()
		}
		_ = f.Close()
		// Its own upload may still be finishing, so one call never removes what it
		// just sent. One still incomplete after the wait falls to matches below and
		// leaves the outcome uncertain; the next call recovers it.
		err = settled(ctx, func() (bool, error) {
			var err error
			a, _, err = s.findAsset(ctx)
			return a != nil && !a.incomplete(), err
		})
		if err != nil {
			return 0, err
		}
		if a == nil {
			return 0, errors.New("uploaded release asset is absent")
		}
	}
	if err := a.matches(v.expected, s.p.target.AssetName); err != nil {
		return 0, err
	}
	if err := s.verifyDownload(ctx, a.ID, s.p.target.AssetName, v.expected); err != nil {
		return 0, err
	}
	if err := s.checkRelease(ctx); err != nil {
		return 0, err
	}
	// Detect replacement/deletion between listing and byte verification. An
	// asset ID is immutable; metadata alone never establishes the checksum.
	after, _, err := s.findAsset(ctx)
	if err != nil {
		return 0, err
	}
	if after == nil || after.ID != a.ID {
		return 0, errors.New("release asset identity changed during verification")
	}
	if err := after.matches(v.expected, s.p.target.AssetName); err != nil {
		return 0, err
	}
	return a.ID, nil
}

func (s *publication) request(ctx context.Context, method, endpoint string, body io.Reader, size int64, accept string, credential bool) (*http.Response, error) {
	s.requests++
	limit := s.maxRequests
	if limit == 0 {
		limit = 96
	}
	if s.requests > limit {
		return nil, errors.New("release protocol request limit exceeded")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("invalid release protocol request")
	}
	req.Header.Set("Accept", accept)
	if credential {
		req.Header.Set("Authorization", "Bearer "+s.p.target.Token)
		req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	response, err := s.client.Do(req)
	if err != nil {
		// Do not leak signed URLs, credentials, or remote response bodies.
		return nil, errors.New("release protocol request failed")
	}
	return response, nil
}

func (s *publication) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := s.request(ctx, http.MethodGet, s.p.api+path, nil, 0, "application/vnd.github+json", true)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return errors.New("release API read was refused")
	}
	const maxJSON = 2 << 20
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON || json.Unmarshal(data, out) != nil {
		return errors.New("release API returned invalid or excessive metadata")
	}
	return nil
}

type gitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

func (s *publication) checkRelease(ctx context.Context) error {
	var release struct {
		ID        int64  `json:"id"`
		Draft     bool   `json:"draft"`
		Immutable bool   `json:"immutable"`
		Tag       string `json:"tag_name"`
		UploadURL string `json:"upload_url"`
	}
	if err := s.getJSON(ctx, s.releasePath(), &release); err != nil {
		return err
	}
	t := s.p.target
	if release.ID != t.ReleaseID || !release.Draft || release.Immutable || release.Tag != t.Tag || release.UploadURL != s.p.upload+s.releasePath()+"/assets{?name,label}" {
		return errors.New("release is not the configured draft or upload authority changed")
	}
	return s.checkTag(ctx)
}

func (s *publication) checkTag(ctx context.Context) error {
	t := s.p.target
	var ref struct {
		Ref    string    `json:"ref"`
		Object gitObject `json:"object"`
	}
	prefix := "/repos/" + t.Repository + "/git/"
	if err := s.getJSON(ctx, prefix+"ref/tags/"+url.PathEscape(t.Tag), &ref); err != nil {
		return err
	}
	if ref.Ref != "refs/tags/"+t.Tag {
		return errors.New("release tag reference differs")
	}
	object, seen := ref.Object, map[string]bool{}
	for depth := 0; depth <= 8; depth++ {
		if !commitPattern.MatchString(object.SHA) || seen[object.SHA] {
			return errors.New("release tag object is invalid or cyclic")
		}
		seen[object.SHA] = true
		if object.Type == "commit" {
			if object.SHA != t.SourceCommit {
				return errors.New("release tag no longer names the approved source commit")
			}
			return nil
		}
		if object.Type != "tag" || depth == 8 {
			return errors.New("unsupported or excessive annotated tag chain")
		}
		var tag struct {
			SHA    string    `json:"sha"`
			Object gitObject `json:"object"`
		}
		if err := s.getJSON(ctx, prefix+"tags/"+object.SHA, &tag); err != nil {
			return err
		}
		if tag.SHA != object.SHA {
			return errors.New("annotated tag identity differs")
		}
		object = tag.Object
	}
	return errors.New("release tag did not resolve")
}

type asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Size   *int64 `json:"size"`
	Digest string `json:"digest"`
}

// incomplete reports an asset that is not finished by GitHub's own account. GitHub
// documents "starter" for an upload that failed upstream, and that such an asset
// "can be safely deleted". Its schema's only other value besides "uploaded" is
// "open", which it does not define; such an asset is treated the same way because
// this adapter accepts only "uploaded", so it could never complete the release as
// it stands, and on a draft nothing public can be lost. Any other value, including
// none, is not proof of anything and stays a conflict.
func (a *asset) incomplete() bool { return a.State == "starter" || a.State == "open" }

// removeIncomplete is the recovery for an upload cut off part way. GitHub keeps
// the asset it opened and refuses every later upload under that name, so without
// this one interrupted upload stops the release for good. The approved upload owns
// this name on this draft, so clearing its own leftover is part of that approved
// effect and needs no separate approval.
//
// It re-checks the draft and tag, then lists the asset once more as the last read
// before the DELETE, and returns whatever it finds if that changed: an asset that
// finished meanwhile goes on to the ordinary byte comparison. GitHub has no
// conditional delete, so one finishing in the instant after that read would still
// be removed. Only a release left open to another writer could do that, and
// readback proves whatever replaces it. attempted is set before the DELETE, because
// any failure after it leaves the remote state unknown. The returned count is the
// inventory size, for the caller's asset limit.
func (s *publication) removeIncomplete(ctx context.Context, a *asset) (*asset, int, error) {
	if err := s.checkRelease(ctx); err != nil {
		return nil, 0, err
	}
	current, count, err := s.findAsset(ctx)
	if err != nil {
		return nil, count, err
	}
	if current == nil || current.ID != a.ID || !current.incomplete() {
		return current, count, nil
	}
	s.attempted, s.replaced = true, a.ID
	deleting, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := s.request(deleting, http.MethodDelete, s.p.api+"/repos/"+s.p.target.Repository+"/releases/assets/"+strconv.FormatInt(a.ID, 10), nil, 0, "application/vnd.github+json", true)
	if err != nil {
		return nil, count, err
	}
	_ = r.Body.Close()
	// 404 means an earlier removal landed and its answer was lost.
	if r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusNotFound {
		return nil, count, errors.New("incomplete release asset could not be removed")
	}
	var present *asset
	err = settled(ctx, func() (bool, error) {
		var err error
		present, count, err = s.findAsset(ctx)
		return err == nil && present == nil, err
	})
	if err != nil {
		return nil, count, err
	}
	if present != nil {
		return nil, count, errors.New("incomplete release asset is still listed after its removal")
	}
	return nil, count, nil
}

func (a *asset) matches(want Expected, name string) error {
	if a.ID <= 0 || a.Name != name || a.State != "uploaded" || a.Size == nil || *a.Size != want.Size || (a.Digest != "" && a.Digest != want.SHA256) {
		return &ConflictError{name}
	}
	return nil
}

func (s *publication) findAsset(ctx context.Context) (*asset, int, error) {
	var found *asset
	seen, total := map[int64]bool{}, 0
	for page := 1; page <= 11; page++ {
		var entries []asset
		if err := s.getJSON(ctx, s.releasePath()+"/assets?per_page=100&page="+strconv.Itoa(page), &entries); err != nil {
			return nil, total, err
		}
		total += len(entries)
		if entries == nil || len(entries) > 100 || total > 1000 {
			return nil, total, errors.New("release asset inventory is invalid or exceeds its bound")
		}
		for _, a := range entries {
			if a.ID <= 0 || seen[a.ID] || a.Name == "" || len(a.Name) > 1024 {
				return nil, total, errors.New("release asset inventory is inconsistent")
			}
			seen[a.ID] = true
			if a.Name == s.p.target.AssetName {
				if found != nil {
					return nil, total, &ConflictError{s.p.target.AssetName}
				}
				copy := a
				found = &copy
			}
		}
		if len(entries) < 100 {
			return found, total, nil
		}
	}
	return nil, total, errors.New("release asset inventory did not terminate")
}

func (s *publication) verifyDownload(ctx context.Context, assetID int64, name string, want Expected) error {
	endpoint := s.p.api + "/repos/" + s.p.target.Repository + "/releases/assets/" + strconv.FormatInt(assetID, 10)
	credential := true
	for redirects := 0; redirects <= 3; redirects++ {
		r, err := s.request(ctx, http.MethodGet, endpoint, nil, 0, "application/octet-stream", credential)
		if err != nil {
			return err
		}
		if r.StatusCode == http.StatusOK {
			digest, size, err := hashCopy(ctx, io.Discard, r.Body, want.Size)
			closeErr := r.Body.Close()
			if size > want.Size {
				return &ConflictError{name}
			}
			// A read that did not finish proves nothing about the bytes.
			if err != nil || closeErr != nil {
				return errors.New("release asset download was interrupted")
			}
			if size != want.Size || digest != want.SHA256 {
				return &ConflictError{name}
			}
			return nil
		}
		location := r.Header.Get("Location")
		_ = r.Body.Close()
		if redirects == 3 || (r.StatusCode != 301 && r.StatusCode != 302 && r.StatusCode != 303 && r.StatusCode != 307 && r.StatusCode != 308) {
			return errors.New("release asset download was refused")
		}
		u, err := url.Parse(location)
		if err != nil || len(location) > 16384 || u.User != nil || u.Fragment != "" || u.Opaque != "" || !s.p.downloads[u.Scheme+"://"+u.Host] {
			return errors.New("release asset download left configured origins")
		}
		endpoint, credential = u.String(), false
	}
	return errors.New("release asset download exceeded redirect limit")
}
