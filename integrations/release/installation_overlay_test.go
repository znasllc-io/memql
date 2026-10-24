package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1" // Git object protocol fixture, not a new signature.
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/installation"
	"gopkg.in/yaml.v3"
)

const overlayBefore = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\nnamespace: memql\nimages:\n  - name: engine\n    newName: registry.example/memql/engine\n    newTag: previous\n  - name: database\n    newName: registry.example/database\n    digest: sha256:unchanged\n"

func overlayPublication(t *testing.T) pipelines.VerifiedPublishedRelease {
	t.Helper()
	digest := "sha256:" + strings.Repeat("b", 64)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	r := pipelines.PublishedRelease{FormatVersion: 1, Publisher: "fixture", CandidateID: digest, ApprovalID: "approval", WorkflowDigest: digest, ProvenanceDigest: digest, Components: []pipelines.PublishedComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: strings.Repeat("d", 40), Artifacts: []pipelines.PublishedArtifact{{Name: "bff", Kind: "oci", Platform: "linux/arm64", Digest: digest, Size: 100, ImageDigest: digest, Locations: []pipelines.PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "memql/engine"}}}}}}}
	envelope, err := pipelines.SignPublishedRelease(r, "key", key)
	require.NoError(t, err)
	verified, err := pipelines.VerifyPublishedRelease(envelope, "fixture", map[string]ed25519.PublicKey{"key": pub})
	require.NoError(t, err)
	return verified
}
func TestInstallationOverlayPinsOnlySignedDeclaredImageSlots(t *testing.T) {
	binding := []overlayImage{{"engine", "engine", "bff", "registry.example/memql/engine"}}
	output, err := pinInstallationImages([]byte(overlayBefore), binding, overlayPublication(t))
	require.NoError(t, err)
	var before, after map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(overlayBefore), &before))
	require.NoError(t, yaml.Unmarshal(output, &after))
	originalImages := before["images"].([]any)
	images := after["images"].([]any)
	require.Equal(t, originalImages[1], images[1])
	require.Equal(t, "sha256:"+strings.Repeat("b", 64), images[0].(map[string]any)["digest"])
	require.NotContains(t, images[0], "newTag")
	delete(before, "images")
	delete(after, "images")
	require.Equal(t, before, after)
	_, err = pinInstallationImages([]byte(strings.Replace(overlayBefore, "newTag: previous", "newTag: 16", 1)), binding, overlayPublication(t))
	require.NoError(t, err, "a literal numeric Kustomize tag remains replaceable")
	for _, tc := range []struct {
		name, input string
		binding     []overlayImage
	}{
		{"duplicate YAML key", overlayBefore + "namespace: other\n", binding},
		{"duplicate slot", overlayBefore + "  - name: engine\n", binding},
		{"multiple documents", overlayBefore + "---\nkind: Kustomization\n", binding},
		{"alias", strings.Replace(overlayBefore, "resources:", "resources: &items", 1), binding},
		{"unpublished destination", overlayBefore, []overlayImage{{"engine", "engine", "bff", "attacker.example/image"}}},
		{"missing slot", overlayBefore, []overlayImage{{"other", "engine", "bff", "registry.example/memql/engine"}}},
		{"unpublished artifact", overlayBefore, []overlayImage{{"engine", "engine", "other", "registry.example/memql/engine"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pinInstallationImages([]byte(tc.input), tc.binding, overlayPublication(t))
			require.Error(t, err)
		})
	}
	_, err = pinInstallationImages([]byte(overlayBefore), binding, pipelines.VerifiedPublishedRelease{})
	require.Error(t, err)
}

type overlayHub struct {
	t                                                             *testing.T
	before, after, base, branch, headRepo, baseRepo, mergeContent string
	proposed, merged, revoked, revokeOnLookup                     bool
	refs, prs, writes                                             int
	message                                                       string
	beforeFiles, afterFiles, mergeOverrides                       map[string]string
}

func (h *overlayHub) files() map[string]string {
	if h.beforeFiles != nil {
		return h.beforeFiles
	}
	return map[string]string{"deploy/kustomization.yaml": h.before}
}
func overlayBlobID(content string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	return fmt.Sprintf("%x", sum[:])
}
func (h *overlayHub) RoundTrip(r *http.Request) (*http.Response, error) {
	h.t.Helper()
	code := 200
	var value any
	endpoint := strings.TrimPrefix(r.URL.Path, "/repos/acme/install")
	baseTree, newTree, commit := strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	if r.Header.Get("Authorization") == "" {
		h.t.Fatal("missing app token")
	}
	switch {
	case endpoint == "/branches/main":
		value = map[string]any{"commit": map[string]any{"sha": h.base}}
	case strings.HasPrefix(endpoint, "/branches/memql/update-"):
		if h.branch == "" {
			code = 404
			value = map[string]any{}
		} else {
			value = map[string]any{"commit": map[string]any{"sha": h.branch}}
		}
	case strings.HasPrefix(endpoint, "/contents/"):
		file := strings.TrimPrefix(endpoint, "/contents/")
		content, exists := h.files()[file]
		if r.URL.Query().Get("ref") == strings.Repeat("9", 40) {
			content, exists = h.afterFiles[file]
			if altered, ok := h.mergeOverrides[file]; ok {
				content = altered
			}
			if h.mergeContent != "" {
				content = h.mergeContent
			}
		}
		if !exists {
			code = 404
			value = map[string]any{}
		} else {
			value = map[string]any{"type": "file", "content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"}
		}

	case endpoint == "/git/commits/"+h.base:
		value = map[string]any{"tree": map[string]any{"sha": baseTree}}
	case endpoint == "/git/trees/"+baseTree:
		entries := []map[string]string{}
		dirs := map[string]bool{}
		for file, content := range h.files() {
			for dir := path.Dir(file); dir != "."; dir = path.Dir(dir) {
				if !dirs[dir] {
					entries = append(entries, map[string]string{"path": dir, "mode": "040000", "type": "tree", "sha": strings.Repeat("1", 40)})
					dirs[dir] = true
				}
			}
			entries = append(entries, map[string]string{"path": file, "mode": "100644", "type": "blob", "sha": overlayBlobID(content)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i]["path"] < entries[j]["path"] })
		value = map[string]any{"truncated": false, "tree": entries}
	case strings.HasPrefix(endpoint, "/git/blobs/"):
		for _, content := range h.files() {
			if strings.TrimPrefix(endpoint, "/git/blobs/") == overlayBlobID(content) {
				value = map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(content)), "size": len(content), "encoding": "base64"}
			}
		}
		require.NotNil(h.t, value)

	case endpoint == "/git/trees" && r.Method == "POST":
		var body struct {
			Tree []struct{ Path, Content string }
		}
		require.NoError(h.t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(h.t, body.Tree, len(h.files()))
		h.afterFiles = map[string]string{}
		for _, file := range body.Tree {
			_, ok := h.files()[file.Path]
			require.True(h.t, ok)
			h.afterFiles[file.Path] = file.Content
		}
		h.after = h.afterFiles["deploy/kustomization.yaml"]
		value = map[string]string{"sha": newTree}
	case endpoint == "/git/commits" && r.Method == "POST":
		var body struct{ Message string }
		require.NoError(h.t, json.NewDecoder(r.Body).Decode(&body))
		h.message = body.Message
		value = map[string]string{"sha": commit}
	case endpoint == "/git/commits/"+commit:
		value = map[string]any{"tree": map[string]string{"sha": newTree}, "parents": []any{map[string]string{"sha": h.base}}, "message": h.message}
	case endpoint == "/git/refs" && r.Method == "POST":
		h.refs++
		h.branch = commit
		code = 502
		value = map[string]any{} // uncertain response: native must read back
	case endpoint == "/pulls":
		if r.Method == "GET" && h.revokeOnLookup {
			h.revoked = true
		}
		if r.Method == "POST" {
			h.prs++
			h.proposed = true
			code = 502
			value = map[string]any{}
		} else if h.proposed {
			value = []any{map[string]string{"state": "open", "html_url": "https://github.com/acme/install/pull/7"}}
		} else {
			value = []any{}
		}
	case endpoint == "/pulls/7":
		state := "open"
		if h.merged {
			state = "closed"
		}
		value = map[string]any{"number": 7, "state": state, "merged": h.merged, "merge_commit_sha": strings.Repeat("9", 40), "html_url": "https://github.com/acme/install/pull/7", "head": map[string]any{"sha": h.branch, "ref": h.headRef(), "repo": map[string]string{"full_name": h.headRepo}}, "base": map[string]any{"sha": h.base, "ref": "main", "repo": map[string]string{"full_name": h.baseRepo}}}
	default:
		h.t.Fatalf("unexpected GitHub operation %s %s", r.Method, r.URL)
	}
	body, err := json.Marshal(value)
	require.NoError(h.t, err)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}
func (h *overlayHub) headRef() string {
	parts := strings.Split(h.message, "MemQL-Operation: ")
	if len(parts) != 2 {
		return "missing"
	}
	return "memql/update-" + parts[1][:32]
}

type overlayTestAccess struct {
	hub    *overlayHub
	client *githubapp.Client
}

func (a overlayTestAccess) GitHub() *githubapp.Client { return a.client }
func (a overlayTestAccess) InstallationToken(context.Context, string, string, string, string) (string, int64, error) {
	if a.hub.revoked {
		return "", 0, errors.New("revoked")
	}
	return "read-token", 1, nil
}
func (a overlayTestAccess) RepositoryWriterToken(context.Context, string, string, string, string) (string, int64, error) {
	a.hub.writes++
	if a.hub.revoked {
		return "", 0, errors.New("revoked")
	}
	return "write-token", 1, nil
}
func overlayFixture(t *testing.T) (*overlayScope, *overlayHub) {
	t.Helper()
	owner, err := overlayOwner(ownerCtx())
	require.NoError(t, err)
	hub := &overlayHub{t: t, before: overlayBefore, base: strings.Repeat("a", 40), headRepo: "acme/install", baseRepo: "acme/install"}
	cfg := overlayConfiguration{FormatVersion: 1, InstallationID: "installation", Repository: "acme/install", OwnerUserID: owner, CredentialID: "credential", BaseBranch: "main", Path: "deploy/kustomization.yaml", ProposalWorkflow: "installationOverlayProposalWorkflow", ObservationWorkflow: "installationOverlayObservationWorkflow", Images: []overlayImage{{"engine", "engine", "bff", "registry.example/memql/engine"}}}
	raw, _ := json.Marshal(cfg)
	i := &Integration{resolver: resolver{systemVariable: func(context.Context, string) (string, error) { return string(raw), nil }}}
	i.installation.Store(&installation.Preparer{})
	require.NoError(t, i.ConfigureInstallationOverlay(overlayTestAccess{hub, githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: hub}))}))
	db := candidateTestDB(t)
	i.candidateDB = func() *sql.DB { return db }
	source := installation.PublicationSource{InstallationID: cfg.InstallationID, RepositoryURL: "https://github.com/acme/install", Path: "deploy", Publication: overlayPublication(t)}
	r := overlayRequest{cfg.InstallationID, id.NewShortId(), "sha256:" + strings.Repeat("b", 64), source.Publication.Digest(), hub.base}
	s := &overlayScope{integration: i, db: db, configuration: cfg, request: r, owner: owner, source: &source}
	s.id = overlayHash([]string{owner, r.InstallationID, r.RequestID})
	s.snapshot, err = workflowhost.CaptureEntries(cfg.ProposalWorkflow, overlayContract, []string{cfg.ObservationWorkflow}, nil, s.operations())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, e := db.Exec(`DELETE FROM release_installation_proposals WHERE proposal_id=$1`, s.id)
		require.NoError(t, e)
	})
	return s, hub
}
func TestInstallationOverlayRecoveryAndVerifiedMergeAcrossReplicas(t *testing.T) {
	s, h := overlayFixture(t)
	ctx := ownerCtx()
	_, err := s.build(ctx, nil)
	require.NoError(t, err)
	_, err = s.propose(ctx, nil)
	require.ErrorContains(t, err, "durable")
	require.Zero(t, h.refs)
	_, err = s.reserve(ctx, nil)
	require.NoError(t, err)
	_, err = s.propose(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	saved, err := s.load(ctx, s.id)
	require.NoError(t, err)
	require.Equal(t, "proposed", saved.summary().State)
	// A different process recovers only the journal and frozen sources. Replaying
	// immutable Git objects does not repeat a branch or pull request write.
	other := &overlayScope{integration: s.integration, db: s.db, configuration: s.configuration, request: s.request, owner: s.owner, id: s.id, snapshot: saved.Record.Snapshot, record: &saved.Record, source: s.source}
	_, err = other.build(ctx, nil)
	require.NoError(t, err)
	_, err = other.reserve(ctx, nil)
	require.NoError(t, err)
	_, err = other.propose(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	h.merged = true
	for _, corruption := range []string{"head repository", "base repository", "merged bytes"} {
		t.Run(corruption, func(t *testing.T) {
			switch corruption {
			case "head repository":
				h.headRepo = "attacker/install"
			case "base repository":
				h.baseRepo = "attacker/install"
			case "merged bytes":
				h.mergeContent = "unreviewed"
			}
			_, err := other.observe(ctx, nil)
			require.Error(t, err)
			h.headRepo, h.baseRepo, h.mergeContent = "acme/install", "acme/install", ""
			stored, err := other.load(ctx, s.id)
			require.NoError(t, err)
			require.Empty(t, stored.MergedCommit)
		})
	}
	ops := map[string]workflowhost.Operation{"releaseOverlayObserve": other.observe}
	_, err = workflowhost.RunSnapshotEntry(ctx, other.snapshot, overlayContract, s.configuration.ObservationWorkflow, nil, workflowhost.Options{Operations: ops})
	require.NoError(t, err)
	stored, err := other.load(ctx, s.id)
	require.NoError(t, err)
	require.Equal(t, "merged", stored.summary().State)
	require.Equal(t, strings.Repeat("9", 40), stored.MergedCommit)
	_, err = other.integration.handleInstallationPropose(ctx, map[string]any{"installationId": s.request.InstallationID, "requestId": s.request.RequestID, "candidateId": s.request.CandidateID, "catalogDigest": s.request.CatalogDigest, "expectedBaseRevision": s.request.ExpectedBaseRevision}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	_, err = workflowhost.RunSnapshot(ctx, other.snapshot, overlayContract, nil, workflowhost.Options{Operations: ops})
	require.Error(t, err, "read-only observation cannot call proposal effects")
	h.revoked = true
	_, err = other.propose(ctx, nil)
	require.Error(t, err)
	require.Equal(t, 1, h.refs)
	// Journal conflicts cannot turn a different merge into a successful receipt.
	err = other.recordEffect(ctx, `UPDATE release_installation_proposals SET merged_commit=$2 WHERE proposal_id=$1 AND (merged_commit IS NULL OR merged_commit=$2) RETURNING proposal_id`, s.id, strings.Repeat("8", 40))
	require.Error(t, err)
}
func TestInstallationOverlayRefusesMovedBaseAndChangedAuthority(t *testing.T) {
	s, h := overlayFixture(t)
	h.base = strings.Repeat("8", 40)
	_, err := s.build(ownerCtx(), nil)
	require.ErrorContains(t, err, "moved")
	require.Zero(t, h.refs)
	h.base = s.request.ExpectedBaseRevision
	_, err = s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	s.configuration.Images[0].Repository = "attacker.example/image"
	_, err = s.propose(ownerCtx(), nil)
	require.ErrorContains(t, err, "authority changed")
	require.Zero(t, h.refs)
	_, err = s.integration.handleInstallationPropose(context.Background(), nil, 0)
	require.Error(t, err)
}

func TestInstallationOverlayRechecksAuthorityAfterPullRequestLookup(t *testing.T) {
	s, h := overlayFixture(t)
	_, err := s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	h.revokeOnLookup = true
	_, err = s.propose(ownerCtx(), nil)
	require.Error(t, err)
	require.Equal(t, 1, h.refs, "branch was authorized before revocation")
	require.Zero(t, h.prs, "revoked grant must not create a PR after lookup")
	saved, err := s.load(ownerCtx(), s.id)
	require.NoError(t, err)
	require.Zero(t, saved.PullRequest)
	h.revokeOnLookup = false
	h.revoked = false
	_, err = s.propose(ownerCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
}
