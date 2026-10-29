package main

// Read-only review of candidate bytes through the existing owner Library API.
// Library rows are mutable: only equality to the immutable candidate's digest
// and size makes the resulting local file usable as release-review evidence.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	client "github.com/znasllc-io/memql/sdk/go/client"
)

type reviewAsset struct {
	Name, Kind, Digest string
	Size               int64
	Receipt            struct {
		WorkRunID string `json:"workRunId"`
		StepKey   string `json:"stepKey"`
		Attempt   int    `json:"attempt"`
	}
}
type reviewStep struct {
	RunID           string `json:"runId"`
	Key, Status     string
	Attempt         int
	ArtifactFileIDs []string `json:"artifactFileIds"`
}
type reviewFile struct {
	ID, Name, SHA256, Source string
	Size                     int64
	ProducedByRunID          string `json:"producedByRunId"`
	ProducedByStepKey        string `json:"producedByStepKey"`
}

func readRows(raw any, into any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return err
	}
	return json.Unmarshal(r.Data, into)
}
func cockpitAssetNames(version string) map[string]string {
	m := map[string]string{"headless-checksums": "memql-" + version + "-SHA256SUMS"}
	for _, p := range []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64"} {
		m["headless-"+p] = "memql-" + version + "-" + p + ".tar.gz"
		m["headless-"+p+"-binary"] = "memql-" + p
		m["computeruse-"+p] = "memql-computeruse-" + p
		m["computeruse-"+p+"-checksum"] = "memql-computeruse-" + p + ".sha256"
		for _, f := range []string{"headless", "computeruse"} {
			stem := "memql"
			if f == "computeruse" {
				stem += "-computeruse"
			}
			for _, ext := range []string{"json", "xml"} {
				m[f+"-"+p+"-sbom-"+ext] = stem + "-" + p + ".cdx." + ext
			}
		}
		if strings.HasPrefix(p, "darwin-") {
			for _, kind := range []string{"app", "menubar"} {
				m[kind+"-"+p] = "memql-" + kind + "-" + p + ".tar.gz"
				m[kind+"-"+p+"-checksum"] = "memql-" + kind + "-" + p + ".tar.gz.sha256"
			}
		}
	}
	return m
}

func downloadCandidateAssets(ctx context.Context, qc *client.QueryClient, token string, o options) ([]map[string]any, error) {
	// Reuse the exact profile/source/destination review, allowing ready state.
	check := o
	check.op = "approve"
	if err := preflightCandidate(ctx, qc, check); err != nil {
		return nil, err
	}
	r, err := qc.ReleaseGetCandidate(ctx, client.ReleaseGetCandidateArgs{CandidateId: o.candidate})
	if err != nil {
		return nil, err
	}
	var c struct {
		CandidateID string `json:"candidateId"`
		State       string
		Manifest    struct {
			Components []struct {
				Name, Version, Commit, Repository string
				Artifacts                         []reviewAsset
			}
		}
	}
	if err = decodePayload(r.Raw(), &c); err != nil {
		return nil, err
	}
	if c.CandidateID != o.candidate || (c.State != "ready" && c.State != "approved") || len(c.Manifest.Components) != 1 {
		return nil, errors.New("candidate changed or is not reviewable")
	}
	comp := c.Manifest.Components[0]
	if comp.Name != "cockpit" || comp.Commit != o.commit || comp.Repository != "znasllc-io/memql" || "cockpit/v"+comp.Version != o.tag || len(comp.Artifacts) != 41 {
		return nil, errors.New("candidate source or asset count changed")
	}
	names, seen := cockpitAssetNames(comp.Version), map[string]bool{}
	var total int64
	for _, a := range comp.Artifacts {
		if names[a.Name] == "" || seen[a.Name] || a.Kind != "file" || a.Size <= 0 || a.Size > 512<<20 || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(a.Digest) || a.Receipt.Attempt < 1 || a.Receipt.WorkRunID == "" || a.Receipt.StepKey == "" {
			return nil, errors.New("invalid or oversized candidate file")
		}
		seen[a.Name] = true
		total += a.Size
	}
	if total > 4<<30 {
		return nil, errors.New("candidate downloads exceed 4 GiB aggregate bound")
	}
	root, err := filepath.Abs(o.output)
	if err != nil {
		return nil, err
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return nil, errors.New("output directory must be new with an existing parent")
	}
	if err = os.Mkdir(filepath.Join(root, "assets"), 0700); err != nil {
		return nil, err
	}
	meta, err := json.MarshalIndent(sanitize(r.Raw(), token), "", "  ")
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(root, "candidate.json"), meta, 0600); err != nil {
		return nil, err
	}
	steps := map[string]reviewStep{}
	files := map[string]reviewFile{}
	hc := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Library redirects are refused") }}
	var out []map[string]any
	for _, a := range comp.Artifacts {
		key := a.Receipt.WorkRunID + ":" + a.Receipt.StepKey
		s, ok := steps[key]
		if !ok {
			res, e := qc.WorkStepForOwnerRun(ctx, client.WorkStepForOwnerRunArgs{RunId: a.Receipt.WorkRunID, StepKey: a.Receipt.StepKey})
			if e != nil {
				return nil, e
			}
			var rows []reviewStep
			if e = readRows(res.Raw(), &rows); e != nil {
				return nil, e
			}
			if len(rows) != 1 {
				return nil, errors.New("producer step missing or ambiguous")
			}
			s = rows[0]
			steps[key] = s
		}
		if strings.TrimPrefix(s.RunID, "v1:work:run:") != a.Receipt.WorkRunID || s.Key != a.Receipt.StepKey || s.Attempt != a.Receipt.Attempt || s.Status != "done" || len(s.ArtifactFileIDs) > 100 {
			return nil, errors.New("producer receipt scope changed")
		}
		var match reviewFile
		count := 0
		for _, id := range s.ArtifactFileIDs {
			f, ok := files[id]
			if !ok {
				res, e := qc.LibraryFileById(ctx, client.LibraryFileByIdArgs{FileId: id})
				if e != nil {
					return nil, e
				}
				var rows []reviewFile
				if e = readRows(res.Raw(), &rows); e != nil {
					return nil, e
				}
				if len(rows) != 1 {
					return nil, errors.New("producer Library file unavailable")
				}
				f = rows[0]
				files[id] = f
			}
			if strings.TrimPrefix(f.ProducedByRunID, "v1:work:run:") == a.Receipt.WorkRunID && f.ProducedByStepKey == a.Receipt.StepKey && f.Source == "pipeline" && f.Size == a.Size && "sha256:"+f.SHA256 == a.Digest {
				match = f
				count++
			}
		}
		if count != 1 {
			return nil, fmt.Errorf("candidate asset %s has no unique matching Library head", a.Name)
		}
		res, e := qc.LibraryArtifactForFile(ctx, client.LibraryArtifactForFileArgs{FileId: match.ID})
		if e != nil {
			return nil, e
		}
		var artifacts []struct{ ID, Kind, SourceConceptRef string }
		if e = readRows(res.Raw(), &artifacts); e != nil {
			return nil, e
		}
		if len(artifacts) != 1 || artifacts[0].ID == "" || artifacts[0].Kind != "file" {
			return nil, errors.New("Library artifact index missing or ambiguous")
		}
		ref := strings.TrimPrefix(artifacts[0].SourceConceptRef, "v1:library:file:")
		fileID := strings.TrimPrefix(match.ID, "v1:library:file:")
		if ref != fileID {
			return nil, errors.New("Library artifact refers to a different file")
		}
		endpoint := "https://api.memql.znas.io/artifacts/" + url.PathEscape(artifacts[0].ID) + "/content"
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, e := hc.Do(req)
		if e != nil {
			return nil, errors.New("authenticated Library transfer failed")
		}
		path := filepath.Join(root, "assets", names[a.Name])
		e = saveVerifiedAsset(resp, path, a.Size, a.Digest)
		if e != nil {
			return nil, fmt.Errorf("%s: %w", a.Name, e)
		}
		out = append(out, map[string]any{"artifact": a.Name, "name": names[a.Name], "fileId": match.ID, "libraryArtifactId": artifacts[0].ID, "size": a.Size, "digest": a.Digest, "path": path})
	}
	manifest, err := json.MarshalIndent(map[string]any{"candidateId": o.candidate, "tag": o.tag, "sourceCommit": o.commit, "remoteWrites": false, "bytesVerified": true, "files": out}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(root, "download-evidence.json"), manifest, 0600); err != nil {
		return nil, err
	}
	return out, nil
}

func saveVerifiedAsset(resp *http.Response, path string, size int64, digest string) (err error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Library returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return errors.New("Library content length differs from candidate")
	}
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path + ".partial")
		}
	}()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("downloaded bytes differ from immutable candidate")
	}
	return os.Rename(path+".partial", path)
}
