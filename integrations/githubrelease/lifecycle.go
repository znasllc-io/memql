package githubrelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Lifecycle supplies bounded protocol operations, not a release workflow. Its
// caller authorizes and journals each intent before mutation. In particular,
// GitHub offers no idempotency key for draft creation: after an uncertain POST,
// the durable caller must use FindDraft and MUST NOT call CreateDraft again.
type Lifecycle struct{ p *Publisher }

type Metadata struct {
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
	Latest     bool   `json:"latest"`
}

type AssetExpectation struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type ReleaseState struct {
	ReleaseID      int64           `json:"releaseId"`
	Draft          bool            `json:"draft"`
	Assets         []AssetIdentity `json:"assets"`
	LatestVerified bool            `json:"latestVerified"`
}

type AssetIdentity struct {
	AssetExpectation
	ID int64 `json:"id"`
}

var markerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// NewLifecycle permits ReleaseID=0 for a not-yet-created draft. It otherwise
// uses exactly the publisher's explicit origins, credentials and trust policy.
func NewLifecycle(t Target) (*Lifecycle, error) {
	p, err := newPublisher(t, false)
	if err != nil {
		return nil, err
	}
	return &Lifecycle{p: p}, nil
}

func (m Metadata) Validate() error {
	if m.Prerelease && m.Latest {
		return errors.New("a prerelease cannot be selected as the latest release")
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 256 || len(m.Body) > 64<<10 ||
		strings.ContainsAny(m.Name, "\r\n\x00") || strings.ContainsRune(m.Body, 0) || strings.Contains(m.Body, "<!-- memql-release-intent:") {
		return errors.New("release metadata requires a bounded name and exact reviewed body")
	}
	return nil
}

func releaseBody(m Metadata, marker string) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	if marker == "" {
		return m.Body, nil
	}
	if !markerPattern.MatchString(marker) {
		return "", errors.New("invalid durable draft intent identity")
	}
	return m.Body + "\n\n<!-- memql-release-intent:" + marker + " -->", nil
}

func (l *Lifecycle) session(releaseID int64) (*publication, func(), error) {
	if l == nil || l.p == nil || l.p.target.Token == "" || releaseID < 0 {
		return nil, nil, errors.New("release lifecycle requires explicit credentials and a valid identity")
	}
	p := *l.p
	p.target.ReleaseID = releaseID
	s, close := p.session()
	// Two full inventories of at most 64 assets, each allowing three download
	// redirects, plus bounded tag/release reads. No unbounded polling/retries.
	s.maxRequests = 640
	return s, close, nil
}

// EnsureTag creates only a missing lightweight tag. Git's ref identity makes
// duplicate creates conflict; a retry first resolves the existing immutable
// ref. An existing annotated tag is accepted only at the exact source commit.
func (l *Lifecycle) EnsureTag(ctx context.Context) error {
	s, close, err := l.session(0)
	if err != nil {
		return err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var ref json.RawMessage
	status, err := s.lifecycleJSON(ctx, http.MethodGet, "/repos/"+s.p.target.Repository+"/git/ref/tags/"+url.PathEscape(s.p.target.Tag), nil, &ref)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return s.checkTag(ctx)
	}
	if status != http.StatusNotFound {
		return errors.New("release tag observation was refused")
	}
	_, _ = s.lifecycleJSON(ctx, http.MethodPost, "/repos/"+s.p.target.Repository+"/git/refs", map[string]any{"ref": "refs/tags/" + s.p.target.Tag, "sha": s.p.target.SourceCommit}, nil)
	if err := s.checkTag(ctx); err != nil {
		return ErrUncertain
	}
	return nil
}

// FindDraft reconciles a creation intent through the authenticated release
// inventory (which includes drafts). Absence is an observation, never proof
// that a previous POST cannot still complete. A conflicting tag refuses.
func (l *Lifecycle) FindDraft(ctx context.Context, m Metadata, marker string) (ReleaseState, bool, error) {
	if marker == "" {
		return ReleaseState{}, false, errors.New("draft lookup requires its durable intent marker")
	}
	body, err := releaseBody(m, marker)
	if err != nil {
		return ReleaseState{}, false, err
	}
	s, close, err := l.session(0)
	if err != nil {
		return ReleaseState{}, false, err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.findDraft(ctx, m, body)
}

type releaseMetadata struct {
	ID         int64  `json:"id"`
	Draft      *bool  `json:"draft"`
	Immutable  bool   `json:"immutable"`
	Tag        string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease *bool  `json:"prerelease"`
	UploadURL  string `json:"upload_url"`
}

func (r releaseMetadata) check(p *Publisher, m Metadata, body string) error {
	if r.ID <= 0 || r.Draft == nil || r.Prerelease == nil || r.Tag != p.target.Tag || r.Name != m.Name || r.Body != body || *r.Prerelease != m.Prerelease ||
		(*r.Draft && r.Immutable) || r.UploadURL != p.upload+"/repos/"+p.target.Repository+"/releases/"+strconv.FormatInt(r.ID, 10)+"/assets{?name,label}" {
		return errors.New("release identity or reviewed metadata changed")
	}
	return nil
}

func (s *publication) findDraft(ctx context.Context, m Metadata, body string) (ReleaseState, bool, error) {
	var found *releaseMetadata
	seen, total := map[int64]bool{}, 0
	for page := 1; page <= 11; page++ {
		var entries []releaseMetadata
		if err := s.getJSON(ctx, "/repos/"+s.p.target.Repository+"/releases?per_page=100&page="+strconv.Itoa(page), &entries); err != nil {
			return ReleaseState{}, false, err
		}
		total += len(entries)
		if entries == nil || len(entries) > 100 || total > 1000 {
			return ReleaseState{}, false, errors.New("release inventory exceeds its bound or is invalid")
		}
		for _, entry := range entries {
			if entry.ID <= 0 || seen[entry.ID] {
				return ReleaseState{}, false, errors.New("release inventory repeats an identity")
			}
			seen[entry.ID] = true
			if entry.Tag != s.p.target.Tag {
				continue
			}
			if found != nil {
				return ReleaseState{}, false, errors.New("release tag has more than one release")
			}
			if err := entry.check(s.p, m, body); err != nil {
				return ReleaseState{}, false, err
			}
			copy := entry
			found = &copy
		}
		if len(entries) < 100 {
			if found == nil {
				return ReleaseState{}, false, nil
			}
			if err := s.checkTag(ctx); err != nil {
				return ReleaseState{}, false, err
			}
			return ReleaseState{ReleaseID: found.ID, Draft: *found.Draft, Assets: []AssetIdentity{}}, true, nil
		}
	}
	return ReleaseState{}, false, errors.New("release inventory did not terminate")
}

// CreateDraft attempts at most one POST. The caller must durably fence that
// attempt BEFORE entering; never retry it based on a missing release alone.
func (l *Lifecycle) CreateDraft(ctx context.Context, m Metadata, marker string) (ReleaseState, error) {
	if marker == "" {
		return ReleaseState{}, errors.New("draft creation requires its durable intent marker")
	}
	body, err := releaseBody(m, marker)
	if err != nil {
		return ReleaseState{}, err
	}
	s, close, err := l.session(0)
	if err != nil {
		return ReleaseState{}, err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := s.checkTag(ctx); err != nil {
		return ReleaseState{}, err
	}
	state, found, err := s.findDraft(ctx, m, body)
	if err != nil {
		return ReleaseState{}, err
	}
	if found {
		return state, nil
	}
	_, _ = s.lifecycleJSON(ctx, http.MethodPost, "/repos/"+s.p.target.Repository+"/releases", map[string]any{
		"tag_name": s.p.target.Tag, "target_commitish": s.p.target.SourceCommit, "name": m.Name, "body": body,
		"draft": true, "prerelease": m.Prerelease, "generate_release_notes": false, "make_latest": "false",
	}, nil)
	state, found, err = s.findDraft(ctx, m, body)
	if err != nil || !found || !state.Draft {
		return ReleaseState{}, ErrUncertain
	}
	return state, nil
}

func expectedAssets(expected []AssetExpectation) (map[string]AssetExpectation, error) {
	if len(expected) == 0 || len(expected) > 64 {
		return nil, errors.New("release requires 1 to 64 approved assets")
	}
	out, total := map[string]AssetExpectation{}, int64(0)
	for _, a := range expected {
		if !assetNamePattern.MatchString(a.Name) || out[a.Name].Name != "" || a.Size < 0 || a.Size >= 2<<30 || !digestPattern.MatchString(a.SHA256) {
			return nil, errors.New("release inventory requires unique names, sizes and SHA256 values")
		}
		total += a.Size
		out[a.Name] = a
	}
	if total > 8<<30 {
		return nil, errors.New("release inventory exceeds eight GiB")
	}
	return out, nil
}

func (s *publication) readRelease(ctx context.Context, m Metadata, body string) (releaseMetadata, error) {
	var r releaseMetadata
	if err := s.getJSON(ctx, s.releasePath(), &r); err != nil {
		return r, err
	}
	if r.ID != s.p.target.ReleaseID {
		return r, errors.New("release ID changed")
	}
	return r, r.check(s.p, m, body)
}

func (s *publication) inventory(ctx context.Context, want map[string]AssetExpectation) ([]AssetIdentity, error) {
	var entries []asset
	// The approved inventory has at most 64 entries, so a single 100-entry
	// page suffices to prove exact cardinality without trusting a total header.
	if err := s.getJSON(ctx, s.releasePath()+"/assets?per_page=100&page=1", &entries); err != nil {
		return nil, err
	}
	if entries == nil || len(entries) != len(want) {
		return nil, errors.New("release asset inventory differs from approval")
	}
	out, seen, ids := make([]AssetIdentity, 0, len(entries)), map[string]bool{}, map[int64]bool{}
	for _, a := range entries {
		expected, ok := want[a.Name]
		if !ok || seen[a.Name] || ids[a.ID] {
			return nil, errors.New("release has unexpected or duplicate assets")
		}
		if err := a.matches(Expected{SHA256: expected.SHA256, Size: expected.Size}, expected.Name); err != nil {
			return nil, err
		}
		seen[a.Name], ids[a.ID] = true, true
		out = append(out, AssetIdentity{AssetExpectation: expected, ID: a.ID})
	}
	slices.SortFunc(out, func(a, b AssetIdentity) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *publication) observe(ctx context.Context, m Metadata, body string, want map[string]AssetExpectation) (ReleaseState, error) {
	r, err := s.readRelease(ctx, m, body)
	if err != nil {
		return ReleaseState{}, err
	}
	if err := s.checkTag(ctx); err != nil {
		return ReleaseState{}, err
	}
	assets, err := s.inventory(ctx, want)
	if err != nil {
		return ReleaseState{}, err
	}
	for _, a := range assets {
		if err := s.verifyDownload(ctx, a.ID, Expected{SHA256: a.SHA256, Size: a.Size}); err != nil {
			return ReleaseState{}, err
		}
	}
	after, err := s.inventory(ctx, want)
	if err != nil {
		return ReleaseState{}, err
	}
	if !slices.Equal(assets, after) {
		return ReleaseState{}, errors.New("release assets changed during byte verification")
	}
	end, err := s.readRelease(ctx, m, body)
	if err != nil {
		return ReleaseState{}, err
	}
	if *r.Draft != *end.Draft {
		return ReleaseState{}, errors.New("release state changed during verification")
	}
	if err := s.checkTag(ctx); err != nil {
		return ReleaseState{}, err
	}
	state := ReleaseState{ReleaseID: r.ID, Draft: *r.Draft, Assets: assets}
	if !state.Draft && m.Latest {
		var latest struct {
			ID int64 `json:"id"`
		}
		if err := s.getJSON(ctx, "/repos/"+s.p.target.Repository+"/releases/latest", &latest); err != nil {
			return ReleaseState{}, err
		}
		if latest.ID != state.ReleaseID {
			return ReleaseState{}, errors.New("published release is not the approved latest selection")
		}
		state.LatestVerified = true
	}
	return state, nil
}

// Observe verifies the complete approved asset set, every byte, metadata and
// immutable source. It performs no writes and works after controller replacement.
func (l *Lifecycle) Observe(ctx context.Context, releaseID int64, m Metadata, marker string, assets []AssetExpectation) (ReleaseState, error) {
	return l.verify(ctx, releaseID, m, marker, assets, false)
}

// CheckDraft protects metadata around a separately authorized asset upload.
// It is a bounded observation and supplies no asset-completion receipt.
func (l *Lifecycle) CheckDraft(ctx context.Context, releaseID int64, m Metadata, marker string) error {
	if releaseID <= 0 {
		return errors.New("draft observation requires its exact ID")
	}
	body, err := releaseBody(m, marker)
	if err != nil {
		return err
	}
	s, close, err := l.session(releaseID)
	if err != nil {
		return err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := s.readRelease(ctx, m, body)
	if err != nil {
		return err
	}
	if !*r.Draft {
		return errors.New("release is no longer a draft")
	}
	return s.checkTag(ctx)
}

// Promote verifies before and after one PATCH. GitHub provides no atomic CAS
// across a release, its tag and assets. The native caller must fence its own
// uploads and promotion; an external writer can still race the remote effect.
// After uncertainty use Observe, never automatically issue a second PATCH.
func (l *Lifecycle) Promote(ctx context.Context, releaseID int64, m Metadata, marker string, assets []AssetExpectation) (ReleaseState, error) {
	return l.verify(ctx, releaseID, m, marker, assets, true)
}

func (l *Lifecycle) verify(ctx context.Context, releaseID int64, m Metadata, marker string, assets []AssetExpectation, promote bool) (ReleaseState, error) {
	if releaseID <= 0 {
		return ReleaseState{}, errors.New("release verification requires an exact ID")
	}
	body, err := releaseBody(m, marker)
	if err != nil {
		return ReleaseState{}, err
	}
	want, err := expectedAssets(assets)
	if err != nil {
		return ReleaseState{}, err
	}
	s, close, err := l.session(releaseID)
	if err != nil {
		return ReleaseState{}, err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	state, err := s.observe(ctx, m, body, want)
	if err != nil || !promote || !state.Draft {
		return state, err
	}
	_, _ = s.lifecycleJSON(ctx, http.MethodPatch, s.releasePath(), map[string]any{"draft": false, "make_latest": strconv.FormatBool(m.Latest)}, nil)
	after, err := s.observe(ctx, m, body, want)
	if err != nil || after.Draft || !slices.Equal(state.Assets, after.Assets) {
		return ReleaseState{}, ErrUncertain
	}
	return after, nil
}

// Mutation responses are not success evidence. All operations reconcile using
// independent GETs, including after HTTP errors or connection loss.
func (s *publication) lifecycleJSON(ctx context.Context, method, path string, value, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data []byte
	if value != nil {
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			return 0, errors.New("invalid release request")
		}
	}
	s.requests++
	if s.requests > s.maxRequests {
		return 0, errors.New("release protocol request limit exceeded")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.p.api+path, bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("invalid release request")
	}
	req.Header.Set("Authorization", "Bearer "+s.p.target.Token)
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("Accept", "application/vnd.github+json")
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := s.client.Do(req)
	if err != nil {
		return 0, errors.New("release request failed")
	}
	defer r.Body.Close()
	if out != nil && r.StatusCode >= 200 && r.StatusCode < 300 {
		body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		if err != nil || len(body) > 2<<20 || json.Unmarshal(body, out) != nil {
			return r.StatusCode, errors.New("invalid or excessive release metadata")
		}
	}
	return r.StatusCode, nil
}
