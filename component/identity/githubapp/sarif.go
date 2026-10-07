package githubapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// SARIF uploads are asynchronous. An accepted upload is not evidence of
// successful ingestion; the caller journals its id and reads SARIFUploadStatus.
// Publication policy, artifact/source verification and recovery belong to the
// caller, not this bounded protocol adapter. No method retries a POST or follows
// a response-provided URL. The bearer needs code-scanning alerts permission.
const (
	MaxSARIFBytes = 32 << 20   // local bound on uncompressed analysis evidence
	maxSARIFGzip  = 10_000_000 // GitHub's documented compressed upload limit
)

var ErrSARIFUploadUncertain = errors.New("GitHub SARIF upload outcome is uncertain; reconcile before retrying")

var sarifCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var sarifPullRef = regexp.MustCompile(`^refs/pull/[1-9][0-9]*/(head|merge)$`)
var sarifUploadID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type SARIFAnalysis struct {
	CommitSHA   string
	Ref         string
	CheckoutURI string
	ToolName    string
	StartedAt   time.Time
	SARIF       []byte
}

type SARIFUpload struct {
	ID          string
	SARIFSHA256 string // identifies the exact uncompressed bytes submitted
}

type SARIFProcessing struct {
	Status string   `json:"processing_status"` // pending, complete or failed
	Errors []string `json:"errors,omitempty"`
}

func (c *Client) UploadSARIF(ctx context.Context, bearer, owner, repo string, analysis SARIFAnalysis) (SARIFUpload, error) {
	if c == nil {
		return SARIFUpload{}, ErrNotConfigured
	}
	if err := sarifRepository(owner, repo); err != nil {
		return SARIFUpload{}, err
	}
	if !sarifCommit.MatchString(analysis.CommitSHA) || !validSARIFRef(analysis.Ref) {
		return SARIFUpload{}, errors.New("SARIF needs an exact commit SHA and a full branch, tag or pull request ref")
	}
	if len(analysis.CheckoutURI) > 4096 || len(analysis.ToolName) > 128 || strings.ContainsAny(analysis.ToolName, "\r\n\x00") {
		return SARIFUpload{}, errors.New("invalid SARIF analysis metadata")
	}
	if analysis.CheckoutURI != "" {
		u, err := url.Parse(analysis.CheckoutURI)
		if err != nil || u.Scheme != "file" || u.Host != "" || !strings.HasPrefix(u.Path, "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return SARIFUpload{}, errors.New("SARIF checkout URI must name an absolute local file directory")
		}
	}
	if len(analysis.SARIF) == 0 || len(analysis.SARIF) > MaxSARIFBytes {
		return SARIFUpload{}, errors.New("SARIF analysis is empty or exceeds the uncompressed size limit")
	}
	// One snapshot is validated, hashed and compressed; the receipt cannot
	// accidentally identify different bytes from those carried on the wire.
	snapshot := bytes.Clone(analysis.SARIF)
	var report struct {
		Version string            `json:"version"`
		Runs    []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(snapshot, &report); err != nil || report.Version != "2.1.0" || len(report.Runs) == 0 || len(report.Runs) > 20 {
		return SARIFUpload{}, errors.New("SARIF must be a 2.1.0 report with one through twenty runs")
	}
	// This is framing validation only. GitHub validates its supported SARIF
	// schema; callers separately verify successful scanner execution.
	for _, run := range report.Runs {
		if len(run) == 0 || run[0] != '{' {
			return SARIFUpload{}, errors.New("every SARIF run must be an object")
		}
	}
	digest := sha256.Sum256(snapshot)
	upload := SARIFUpload{SARIFSHA256: hex.EncodeToString(digest[:])}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(snapshot); err != nil {
		return SARIFUpload{}, err
	}
	if err := w.Close(); err != nil {
		return SARIFUpload{}, err
	}
	if compressed.Len() > maxSARIFGzip {
		return SARIFUpload{}, errors.New("SARIF exceeds GitHub's compressed size limit")
	}
	body := struct {
		CommitSHA   string `json:"commit_sha"`
		Ref         string `json:"ref"`
		SARIF       string `json:"sarif"`
		CheckoutURI string `json:"checkout_uri,omitempty"`
		ToolName    string `json:"tool_name,omitempty"`
		StartedAt   string `json:"started_at,omitempty"`
		Validate    bool   `json:"validate"`
	}{analysis.CommitSHA, analysis.Ref, base64.StdEncoding.EncodeToString(compressed.Bytes()), analysis.CheckoutURI, analysis.ToolName, isoTime(analysis.StartedAt), true}
	var answer struct {
		ID string `json:"id"`
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return SARIFUpload{}, err
	}
	response, err := c.sarifRequest(ctx, http.MethodPost, repoEndpoint(owner, repo)+"/code-scanning/sarifs", bearer, encoded, &answer)
	if err != nil {
		// An explicit 4xx other than timeout means rejection. A transport,
		// server or malformed accepted reply may follow an accepted upload.
		if status := StatusOf(err); status >= 400 && status < 500 && status != http.StatusRequestTimeout {
			return upload, err
		}
		return upload, fmt.Errorf("%w: %w", ErrSARIFUploadUncertain, err)
	}
	if response.StatusCode != http.StatusAccepted || !sarifUploadID.MatchString(answer.ID) {
		return upload, ErrSARIFUploadUncertain
	}
	upload.ID = answer.ID
	return upload, nil
}

func (c *Client) SARIFUploadStatus(ctx context.Context, bearer, owner, repo, id string) (SARIFProcessing, error) {
	if c == nil {
		return SARIFProcessing{}, ErrNotConfigured
	}
	if err := sarifRepository(owner, repo); err != nil {
		return SARIFProcessing{}, err
	}
	if !sarifUploadID.MatchString(id) {
		return SARIFProcessing{}, errors.New("invalid SARIF upload id")
	}
	var state SARIFProcessing
	response, err := c.sarifRequest(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/code-scanning/sarifs/"+id, bearer, nil, &state)
	if err != nil {
		return SARIFProcessing{}, err
	}
	if response.StatusCode != http.StatusOK {
		return SARIFProcessing{}, errors.New("unexpected SARIF processing response")
	}
	switch state.Status {
	case "pending", "failed":
		return state, nil
	case "complete":
		if len(state.Errors) == 0 {
			return state, nil
		}
	}
	return SARIFProcessing{}, errors.New("GitHub returned an unknown or contradictory SARIF processing state")
}

func (c *Client) sarifRequest(ctx context.Context, method, endpoint, bearer string, body []byte, out any) (*http.Response, error) {
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c.doWithHTTP(ctx, &client, method, endpoint, bearer, body, out, maxBody)
}

func sarifRepository(owner, repo string) error {
	for _, part := range []string{owner, repo} {
		if part == "" || len(part) > 100 || part == "." || part == ".." || strings.ContainsFunc(part, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
		}) {
			return errors.New("SARIF needs a repository owner and name")
		}
	}
	return nil
}

func validSARIFRef(ref string) bool {
	if len(ref) > 1024 || strings.ContainsFunc(ref, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) || strings.ContainsAny(ref, "~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	if sarifPullRef.MatchString(ref) {
		return true
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return false
		}
	}
	return true
}
