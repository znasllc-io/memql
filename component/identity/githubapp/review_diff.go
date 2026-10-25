package githubapp

import (
	"context"
	"crypto/sha1" // Git's content-addressed blob identity, not an authorization digest.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReviewFileVersion distinguishes absence from an existing empty file. BlobSHA
// is verified against the returned bytes; Mode is part of the reviewed change.
type ReviewFileVersion struct {
	Present bool   `json:"present"`
	Mode    string `json:"mode"`
	BlobSHA string `json:"blobSha"`
	Content string `json:"content"`
}
type ReviewFileChange struct {
	Path   string            `json:"path"`
	Before ReviewFileVersion `json:"before"`
	After  ReviewFileVersion `json:"after"`
}

// ReviewDiff contains every changed leaf in two complete immutable trees.
// Renames are losslessly represented as a deletion and addition; provider
// rename heuristics or truncated patch strings never supply the evidence.
type ReviewDiff struct {
	BeforeSHA  string             `json:"beforeSha"`
	AfterSHA   string             `json:"afterSha"`
	BeforeTree string             `json:"beforeTree"`
	AfterTree  string             `json:"afterTree"`
	Files      []ReviewFileChange `json:"files"`
}
type reviewTreeEntry struct{ Path, Mode, Type, SHA string }

func reviewPath(p string) bool {
	return p != "" && p != "." && p != ".." && !path.IsAbs(p) && path.Clean(p) == p && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/") && !strings.Contains(p, "\\") && utf8.ValidString(p) && strings.IndexFunc(p, unicode.IsControl) < 0
}
func (c *Client) reviewTree(ctx context.Context, bearer, owner, repo, commit string) (string, map[string]reviewTreeEntry, error) {
	var value struct {
		SHA  string
		Tree *struct{ SHA string }
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/commits/"+commit, bearer, &value); err != nil {
		return "", nil, err
	}
	if value.SHA != commit || value.Tree == nil || !branchChangeSHA.MatchString(value.Tree.SHA) {
		return "", nil, errors.New("review commit has no exact tree identity")
	}
	var tree struct {
		SHA       string
		Truncated *bool
		Tree      []reviewTreeEntry
	}
	if _, err := c.do(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/trees/"+value.Tree.SHA+"?recursive=1", bearer, nil, &tree, largeBody); err != nil {
		return "", nil, err
	}
	if tree.SHA != value.Tree.SHA || tree.Truncated == nil || *tree.Truncated || tree.Tree == nil || len(tree.Tree) > 100000 {
		return "", nil, errors.New("review source tree is incomplete or exceeds its bound")
	}
	entries := make(map[string]reviewTreeEntry, len(tree.Tree))
	seen := map[string]bool{}
	for _, e := range tree.Tree {
		if !reviewPath(e.Path) || seen[e.Path] || !branchChangeSHA.MatchString(e.SHA) {
			return "", nil, errors.New("review tree contains an invalid or duplicate entry")
		}
		seen[e.Path] = true
		if e.Type == "tree" && e.Mode == "040000" {
			continue
		}
		if !(e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755" || e.Mode == "120000")) && !(e.Type == "commit" && e.Mode == "160000") {
			return "", nil, errors.New("review tree contains an unknown entry type")
		}
		entries[e.Path] = e
	}
	return tree.SHA, entries, nil
}
func (c *Client) reviewFile(ctx context.Context, bearer, owner, repo string, e reviewTreeEntry, total *int) (ReviewFileVersion, error) {
	if e.SHA == "" {
		return ReviewFileVersion{}, nil
	}
	if e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755") {
		return ReviewFileVersion{}, errors.New("review changes include a link or submodule")
	}
	var blob struct {
		SHA, Encoding, Content string
		Size                   *int
	}
	if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/blobs/"+e.SHA, bearer, &blob); err != nil {
		return ReviewFileVersion{}, err
	}
	if blob.SHA != e.SHA || blob.Size == nil || *blob.Size < 0 || *blob.Size > 1<<20 {
		return ReviewFileVersion{}, errors.New("review blob identity or size is invalid")
	}
	content, err := decodeContent(blob.Encoding, blob.Content)
	if err != nil || len(content) != *blob.Size || !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return ReviewFileVersion{}, errors.New("review requires complete bounded UTF-8 file contents")
	}
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d%c", len(content), 0)
	hash.Write(content)
	if hex.EncodeToString(hash.Sum(nil)) != e.SHA {
		return ReviewFileVersion{}, errors.New("review blob bytes do not match their Git identity")
	}
	*total += len(content)
	if *total > 4<<20 {
		return ReviewFileVersion{}, errors.New("review content exceeds its complete-evidence limit")
	}
	return ReviewFileVersion{Present: true, Mode: e.Mode, BlobSHA: e.SHA, Content: string(content)}, nil
}

// ImmutableReviewDiff reads both entire tree inventories, then complete before
// and after bytes for every changed file. Unsupported changes fail closed.
func (c *Client) ImmutableReviewDiff(ctx context.Context, bearer, owner, repo, before, after string) (ReviewDiff, error) {
	return c.immutableReviewDiff(ctx, bearer, owner, repo, before, after, false)
}
func (c *Client) immutableReviewDiff(ctx context.Context, bearer, owner, repo, before, after string, allowEmpty bool) (ReviewDiff, error) {
	empty := ReviewDiff{}
	if bearer == "" || owner == "" || repo == "" || !branchChangeSHA.MatchString(before) || !branchChangeSHA.MatchString(after) || before == after {
		return empty, errors.New("review requires two distinct immutable commits")
	}
	bt, b, err := c.reviewTree(ctx, bearer, owner, repo, before)
	if err != nil {
		return empty, err
	}
	at, a, err := c.reviewTree(ctx, bearer, owner, repo, after)
	if err != nil {
		return empty, err
	}
	names := map[string]bool{}
	for p, e := range b {
		if e != a[p] {
			names[p] = true
		}
	}
	for p, e := range a {
		if e != b[p] {
			names[p] = true
		}
	}
	if (!allowEmpty && len(names) == 0) || len(names) > 256 {
		return empty, errors.New("review requires between one and 256 complete file changes")
	}
	paths := make([]string, 0, len(names))
	for p := range names {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := ReviewDiff{BeforeSHA: before, AfterSHA: after, BeforeTree: bt, AfterTree: at, Files: make([]ReviewFileChange, 0, len(paths))}
	total := 0
	for _, p := range paths {
		old, err := c.reviewFile(ctx, bearer, owner, repo, b[p], &total)
		if err != nil {
			return empty, err
		}
		next, err := c.reviewFile(ctx, bearer, owner, repo, a[p], &total)
		if err != nil {
			return empty, err
		}
		out.Files = append(out.Files, ReviewFileChange{Path: p, Before: old, After: next})
	}
	return out, nil
}

// PullRequestReviewDiff uses the actual merge base, not a possibly advanced
// target branch tip, then rechecks the complete PR identity after all reads.
func (c *Client) PullRequestReviewDiff(ctx context.Context, bearer, owner, repo string, number int, head, branch string) (PullRequestObservation, ReviewDiff, error) {
	empty := PullRequestObservation{}
	none := ReviewDiff{}
	pr, err := c.PullRequestState(ctx, bearer, owner, repo, number)
	if err != nil {
		return empty, none, err
	}
	if pr.State != "open" || pr.Merged || pr.HeadSHA != head || pr.BaseRef != branch || !strings.EqualFold(pr.HeadRepository, owner+"/"+repo) {
		return empty, none, errors.New("review pull request is not the exact open same-repository source")
	}
	var compared struct {
		BaseCommit      *struct{ SHA string } `json:"base_commit"`
		MergeBaseCommit *struct{ SHA string } `json:"merge_base_commit"`
	}
	if _, err = c.do(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/compare/"+pr.BaseSHA+"..."+head, bearer, nil, &compared, largeBody); err != nil {
		return empty, none, err
	}
	if compared.BaseCommit == nil || compared.BaseCommit.SHA != pr.BaseSHA || compared.MergeBaseCommit == nil || !branchChangeSHA.MatchString(compared.MergeBaseCommit.SHA) {
		return empty, none, errors.New("review has no exact immutable merge-base proof")
	}
	diff, err := c.ImmutableReviewDiff(ctx, bearer, owner, repo, compared.MergeBaseCommit.SHA, head)
	if err != nil {
		return empty, none, err
	}
	current, err := c.PullRequestState(ctx, bearer, owner, repo, number)
	if err != nil {
		return empty, none, err
	}
	if current != pr {
		return empty, none, errors.New("pull request changed while complete review evidence was read")
	}
	return pr, diff, nil
}

// SameReviewChanges accepts an ordered synthetic merge only when every before
// and after file/mode is identical to the reviewed change. Earlier disjoint
// members may alter other files; overlapping resolution or extra edits refuse.
func SameReviewChanges(reviewed, merged ReviewDiff) bool {
	return len(reviewed.Files) > 0 && reflect.DeepEqual(reviewed.Files, merged.Files)
}

// VerifyReviewTargetAdvance permits an unrelated target-branch advance without
// changing saved review evidence. Both ancestry and every changed target path
// are proved from immutable provider objects. A rewritten base, an overlapping
// file/directory, or incomplete evidence must be reviewed again.
func (c *Client) VerifyReviewTargetAdvance(ctx context.Context, bearer, owner, repo, before, after string, reviewed ReviewDiff) error {
	if !branchChangeSHA.MatchString(before) || !branchChangeSHA.MatchString(after) || len(reviewed.Files) == 0 {
		return errors.New("review target advance lacks immutable evidence")
	}
	if before == after {
		return ctx.Err()
	}
	if err := c.releaseSourceIncluded(ctx, bearer, owner, repo, before, after); err != nil {
		return err
	}
	advanced, err := c.immutableReviewDiff(ctx, bearer, owner, repo, before, after, true)
	if err != nil {
		return err
	}
	for _, changed := range advanced.Files {
		for _, original := range reviewed.Files {
			if changed.Path == original.Path || strings.HasPrefix(changed.Path, original.Path+"/") || strings.HasPrefix(original.Path, changed.Path+"/") {
				return errors.New("target branch changed a reviewed path; a fresh independent review is required")
			}
		}
	}
	return ctx.Err()
}
func (d ReviewDiff) Digest() string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ObserveReviewCheck is reconciliation only. An absent or ambiguous result
// never authorizes a second create after a durable attempted report.
func (c *Client) ObserveReviewCheck(ctx context.Context, bearer, owner, repo, head, name, external string, app int64) (int64, error) {
	if bearer == "" || !validReviewIdentity(head, name, external, app) {
		return 0, errors.New("review observation requires an exact native intent")
	}
	return c.findReviewCheck(ctx, bearer, owner, repo, head, name, external, app)
}
