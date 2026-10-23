package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/repositoryauthority"
)

// This transport exercises the production GitHub client and app adapter. It
// implements the provider protocol, never a successful proposal port or grant.
type authorityProtocol struct {
	t                                     *testing.T
	client                                *githubapp.Client
	grant                                 repositoryauthority.Authorization
	repositoryID                          int64
	before, mode                          string
	base, baseTree, newTree, commit, blob string
	branch, branchName, message           string
	commitBodies                          []string
	treeFiles                             []map[string]string
	reads                                 []string
	posts                                 []string
	refWrites, prWrites, authorizations   int
	prExists, prClosed, revoked           bool
	revokeAt                              string
	extraFiles                            int
	reportedSize                          int
}

func newAuthorityProtocol(t *testing.T) *authorityProtocol {
	t.Helper()
	p := &authorityProtocol{t: t, repositoryID: 42, before: "package example\n", mode: "100644", base: strings.Repeat("a", 40), baseTree: strings.Repeat("b", 40), newTree: strings.Repeat("c", 40), commit: strings.Repeat("d", 40), blob: strings.Repeat("e", 40), reportedSize: -1}
	p.grant = repositoryauthority.Authorization{EffectID: strings.Repeat("f", 64), EffectCreatedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), RequestID: "request", RepositoryID: 42, Source: repositoryauthority.Source{ID: "source", OwnerUserID: "owner", CredentialID: "credential", Repository: "acme/repo"}}
	p.client = githubapp.New(githubapp.Config{}, githubapp.WithHTTPClient(&http.Client{Transport: p}))
	return p
}
func (p *authorityProtocol) GitHub() *githubapp.Client { return p.client }
func (p *authorityProtocol) InstallationToken(_ context.Context, credential, owner, org, repo string) (string, int64, error) {
	require.Equal(p.t, []string{"credential", "owner", "acme", "repo"}, []string{credential, owner, org, repo})
	if p.revoked {
		return "", 0, errors.New("provider grant revoked")
	}
	return "read-token", 7, nil
}
func (p *authorityProtocol) RepositoryWriterToken(c context.Context, credential, owner, org, repo string) (string, int64, error) {
	token, id, err := p.InstallationToken(c, credential, owner, org, repo)
	if token != "" {
		token = "write-token"
	}
	return token, id, err
}
func (p *authorityProtocol) RepositoryAccess(context.Context, string, string, string, string) (packages.RepositoryAuthority, error) {
	return packages.RepositoryAuthority{RepositoryID: p.repositoryID, Repository: "acme/repo", InstallationID: 7, CanWrite: !p.revoked}, nil
}
func (p *authorityProtocol) AuthorizeEffect(_ context.Context, request, operation string, paths []string) (repositoryauthority.Authorization, error) {
	p.authorizations++
	require.Equal(p.t, "request", request)
	require.Equal(p.t, repositoryauthority.OperationPropose, operation)
	if p.revoked {
		return repositoryauthority.Authorization{}, errors.New("original request revoked")
	}
	return p.grant, nil
}
func (p *authorityProtocol) proposal() repositoryauthority.Proposal {
	digest := sha256.Sum256([]byte(p.before))
	return repositoryauthority.Proposal{RequestID: "request", ExpectedHead: p.base, BaseBranch: "main", Title: "Fix approved behavior", Body: "Exact reviewed edit; CI still required.", Files: []repositoryauthority.Replacement{{Path: "src/example.go", BeforeSHA256: hex.EncodeToString(digest[:]), Content: "package example\n// Reviewed change.\n"}}}
}
func (p *authorityProtocol) RoundTrip(r *http.Request) (*http.Response, error) {
	p.t.Helper()
	require.NotEmpty(p.t, r.Header.Get("Authorization"))
	endpoint := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo")
	code := 200
	var out any
	if r.Method == http.MethodGet {
		p.reads = append(p.reads, endpoint)
	} else {
		p.posts = append(p.posts, r.Method+" "+endpoint)
	}
	switch {
	case endpoint == "":
		out = map[string]any{"id": p.repositoryID, "full_name": "acme/repo", "default_branch": "main", "private": true}
	case endpoint == "/branches/main":
		out = map[string]any{"commit": map[string]string{"sha": p.base}}
	case strings.HasPrefix(endpoint, "/branches/memql/change-"):
		if p.revokeAt == "branch lookup" {
			p.revoked = true
		}
		if p.branch == "" {
			code = 404
			out = map[string]any{}
		} else {
			out = map[string]any{"commit": map[string]string{"sha": p.branch}}
		}
	case endpoint == "/git/trees/"+p.base || endpoint == "/git/trees/"+p.baseTree:
		entries := []any{map[string]string{"path": "src", "mode": "040000", "type": "tree", "sha": p.baseTree}, map[string]string{"path": "src/example.go", "mode": p.mode, "type": "blob", "sha": p.blob}, map[string]string{"path": "unapproved.txt", "mode": "100644", "type": "blob", "sha": strings.Repeat("1", 40)}}
		for n := 0; n < p.extraFiles; n++ {
			entries = append(entries, map[string]string{"path": fmt.Sprintf("src/file%d.go", n), "mode": "100644", "type": "blob", "sha": p.blob})
		}
		out = map[string]any{"tree": entries}
	case endpoint == "/git/blobs/"+p.blob:
		size := len(p.before)
		if p.reportedSize >= 0 {
			size = p.reportedSize
		}
		out = map[string]any{"encoding": "base64", "size": size, "content": base64.StdEncoding.EncodeToString([]byte(p.before))}
	case endpoint == "/git/commits/"+p.base:
		out = map[string]any{"tree": map[string]string{"sha": p.baseTree}}
	case endpoint == "/git/trees" && r.Method == http.MethodPost:
		var body struct{ Tree []map[string]string }
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		p.treeFiles = body.Tree
		out = map[string]string{"sha": p.newTree}
	case endpoint == "/git/commits" && r.Method == http.MethodPost:
		body, err := io.ReadAll(r.Body)
		require.NoError(p.t, err)
		p.commitBodies = append(p.commitBodies, string(body))
		var parsed struct{ Message string }
		require.NoError(p.t, json.Unmarshal(body, &parsed))
		p.message = parsed.Message
		out = map[string]string{"sha": p.commit}
	case endpoint == "/git/commits/"+p.commit:
		out = map[string]any{"tree": map[string]string{"sha": p.newTree}, "parents": []any{map[string]string{"sha": p.base}}, "message": p.message}
	case endpoint == "/git/refs" && r.Method == http.MethodPost:
		var body map[string]string
		require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
		p.branchName = body["ref"]
		require.Equal(p.t, "refs/heads/memql/change-"+p.grant.EffectID, p.branchName)
		p.refWrites++
		p.branch = p.commit
		code = 502
		out = map[string]any{} // lost write reply must be read back
	case endpoint == "/pulls":
		if r.Method == http.MethodGet {
			if p.revokeAt == "PR lookup" {
				p.revoked = true
			}
			require.Equal(p.t, "acme:memql/change-"+p.grant.EffectID, r.URL.Query().Get("head"))
			require.Equal(p.t, "main", r.URL.Query().Get("base"))
			out = []any{}
			if p.prExists {
				state := "open"
				if p.prClosed {
					state = "closed"
				}
				out = []any{map[string]string{"state": state, "html_url": "https://github.com/acme/repo/pull/9"}}
			}
		} else {
			var body map[string]any
			require.NoError(p.t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(p.t, true, body["draft"])
			require.Equal(p.t, "main", body["base"])
			p.prWrites++
			p.prExists = true
			code = 502
			out = map[string]any{}
		}
	default:
		p.t.Fatalf("unexpected provider operation %s %s", r.Method, r.URL)
	}
	body, err := json.Marshal(out)
	require.NoError(p.t, err)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

func TestRepositoryEffectsRejectProviderRepositoryReplacement(t *testing.T) {
	for _, operation := range []string{"read", "propose"} {
		t.Run(operation, func(t *testing.T) {
			p := newAuthorityProtocol(t)
			p.repositoryID = 84
			var err error
			if operation == "read" {
				_, err = readRepositoryChange(t.Context(), p, p.grant, []string{"src/example.go"})
			} else {
				_, err = proposeRepositoryChange(t.Context(), p, p, p.grant, p.proposal())
			}
			require.Error(t, err, "same name with a different stable provider ID is another repository")
			require.Empty(t, p.posts)
			require.Equal(t, []string{""}, p.reads, "refuse before reading source bytes")
		})
	}
}
func TestRepositoryEffectsReadExactApprovedImmutableFiles(t *testing.T) {
	p := newAuthorityProtocol(t)
	got, err := readRepositoryChange(t.Context(), p, p.grant, []string{"src/", "new.go"})
	require.NoError(t, err)
	require.Equal(t, p.base, got.ExpectedHead)
	require.Equal(t, "main", got.BaseBranch)
	require.Len(t, got.Files, 2)
	require.Equal(t, repositoryauthority.Replacement{Path: "new.go"}, got.Files[0])
	require.Equal(t, p.before, got.Files[1].Content)
	require.Len(t, got.Files[1].BeforeSHA256, 64)
	require.Empty(t, p.posts)
	for _, read := range p.reads {
		require.NotContains(t, read, strings.Repeat("1", 40), "never fetch unapproved bytes")
	}
}
func TestRepositoryEffectsBoundReadAndReplacementBytesBeforeWriting(t *testing.T) {
	for _, scenario := range []string{"read file bytes", "read total bytes", "read file count", "replacement bytes", "replacement total bytes", "replacement count", "traversal", "source hash"} {
		t.Run(scenario, func(t *testing.T) {
			p := newAuthorityProtocol(t)
			proposal := p.proposal()
			var err error
			switch scenario {
			case "read file bytes":
				p.reportedSize = (1 << 20) + 1
				_, err = readRepositoryChange(t.Context(), p, p.grant, []string{"src/example.go"})
			case "read total bytes":
				p.before = strings.Repeat("x", 512<<10)
				p.extraFiles = 8
				_, err = readRepositoryChange(t.Context(), p, p.grant, []string{"src/"})
			case "read file count":
				p.extraFiles = 256
				_, err = readRepositoryChange(t.Context(), p, p.grant, []string{"src/"})
			default:
				switch scenario {
				case "replacement bytes":
					proposal.Files[0].Content = strings.Repeat("x", (1<<20)+1)
				case "replacement total bytes":
					proposal.Files = nil
					for n := 0; n < 5; n++ {
						proposal.Files = append(proposal.Files, repositoryauthority.Replacement{Path: fmt.Sprintf("new%d.go", n), Content: strings.Repeat("x", 900<<10)})
					}
				case "replacement count":
					proposal.Files = make([]repositoryauthority.Replacement, 257)
				case "traversal":
					proposal.Files[0].Path = "../outside"
				case "source hash":
					proposal.Files[0].BeforeSHA256 = strings.Repeat("0", 64)
				}
				_, err = proposeRepositoryChange(t.Context(), p, p, p.grant, proposal)
			}
			require.Error(t, err)
			require.Empty(t, p.posts, "invalid input cannot upload Git objects or write branches")
		})
	}
}
func TestRepositoryEffectsRecheckGrantAfterProviderLookup(t *testing.T) {
	for _, phase := range []string{"branch lookup", "PR lookup"} {
		t.Run(phase, func(t *testing.T) {
			p := newAuthorityProtocol(t)
			p.revokeAt = phase
			_, err := proposeRepositoryChange(t.Context(), p, p, p.grant, p.proposal())
			require.Error(t, err)
			if phase == "branch lookup" {
				require.Zero(t, p.refWrites, "revocation during branch lookup must precede ref creation")
			}
			require.Zero(t, p.prWrites)
		})
	}
}
func TestRepositoryEffectsRetryReusesDurableProposalIdentity(t *testing.T) {
	p := newAuthorityProtocol(t)
	proposal := p.proposal()
	first, err := proposeRepositoryChange(t.Context(), p, p, p.grant, proposal)
	require.NoError(t, err)
	second, err := proposeRepositoryChange(t.Context(), p, p, p.grant, proposal)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, "complete", first.State)
	require.Equal(t, "memql/change-"+p.grant.EffectID, first.Branch)
	require.Equal(t, p.commit, first.Commit)
	require.Equal(t, 1, p.refWrites)
	require.Equal(t, 1, p.prWrites)
	require.Len(t, p.commitBodies, 2)
	require.Equal(t, p.commitBodies[0], p.commitBodies[1], "same durable timestamp creates same immutable commit")
	require.GreaterOrEqual(t, p.authorizations, 4)
	p.prClosed = true
	_, err = proposeRepositoryChange(t.Context(), p, p, p.grant, proposal)
	require.Error(t, err)
	require.Equal(t, 1, p.prWrites, "closed proposals do not reopen")
}
func TestRepositoryEffectsPreserveApprovedExecutableFileMode(t *testing.T) {
	p := newAuthorityProtocol(t)
	p.mode = "100755"
	_, err := proposeRepositoryChange(t.Context(), p, p, p.grant, p.proposal())
	require.NoError(t, err)
	require.Len(t, p.treeFiles, 1)
	require.Equal(t, "100755", p.treeFiles[0]["mode"], "replacement preserves observed executable bit")
}
