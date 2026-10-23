package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// BranchReplacement is a UTF-8 regular-file edit. Empty BeforeSHA256 means
// that this path must not exist in the immutable base tree.
type BranchReplacement struct{ Path, BeforeSHA256, Content string }

// BranchChange carries an already-authorized, exact branch effect. Timestamp
// is the durable run timestamp, never time.Now: retries construct the SAME Git
// tree and commit, including after a controller loses the HTTP response.
type BranchChange struct {
	Branch, ExpectedHead, OperationID, Title string
	Timestamp                                time.Time
	Create                                   bool
	Files                                    []BranchReplacement
	// Authorize refreshes the grant immediately before the mutable effect.
	// The prepared SHA permits read-only recovery of this exact prior effect.
	Authorize func(context.Context, string) (string, error)
}

var branchChangeSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var branchChangeDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func branchChangeRef(ref string) bool {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "-") || len(ref) > 200 || !utf8.ValidString(ref) || strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\r\n ~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") || strings.HasSuffix(ref, ".") {
		return false
	}
	for _, ch := range ref {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// ApplyBranchChange preserves every unmentioned entry and refuses mode
// changes. The only mutable write is create-ref or a fast-forward-only update
// to a commit with exactly one parent, ExpectedHead. A moved branch therefore
// cannot be overwritten. An uncertain reply is reconciled against the exact
// deterministic commit; it is never reported as success from HTTP alone.
func (c *Client) ApplyBranchChange(ctx context.Context, bearer, owner, repo string, edit BranchChange) (string, error) {
	if !branchChangeSHA.MatchString(edit.ExpectedHead) || !branchChangeDigest.MatchString(edit.OperationID) ||
		edit.Timestamp.IsZero() || len(edit.Title) == 0 || len(edit.Title) > 200 || strings.ContainsAny(edit.Title, "\r\n\x00") ||
		!branchChangeRef(edit.Branch) || edit.Authorize == nil || len(edit.Files) == 0 || len(edit.Files) > 256 {
		return "", errors.New("branch change requires a bounded exact source and effect identity")
	}
	base := repoEndpoint(owner, repo)
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := c.call(ctx, http.MethodGet, base+"/git/commits/"+edit.ExpectedHead, bearer, &commit); err != nil {
		return "", err
	}
	if !branchChangeSHA.MatchString(commit.Tree.SHA) {
		return "", errors.New("GitHub did not identify the immutable source tree")
	}
	var tree struct {
		Truncated bool                                     `json:"truncated"`
		Tree      []struct{ Path, Mode, Type, SHA string } `json:"tree"`
	}
	if _, err := c.do(ctx, http.MethodGet, base+"/git/trees/"+commit.Tree.SHA+"?recursive=1", bearer, nil, &tree, largeBody); err != nil {
		return "", err
	}
	if tree.Truncated {
		return "", errors.New("source tree inventory is incomplete")
	}
	type entry struct{ Mode, Kind, SHA string }
	entries := map[string]entry{}
	for _, item := range tree.Tree {
		if _, found := entries[item.Path]; found {
			return "", errors.New("source tree has duplicate paths")
		}
		entries[item.Path] = entry{item.Mode, item.Type, item.SHA}
	}
	changes := []map[string]string{}
	seen := map[string]bool{}
	total := 0
	for _, file := range edit.Files {
		total += len(file.Content)
		if file.Path == "" || len(file.Path) > 512 || path.Clean(file.Path) != file.Path || path.IsAbs(file.Path) ||
			file.Path == "." || file.Path == ".." || strings.HasPrefix(file.Path, "../") || strings.HasPrefix(file.Path, ".git/") || file.Path == ".git" ||
			strings.ContainsAny(file.Path, "\x00\r\n\\") || !utf8.ValidString(file.Path) || !utf8.ValidString(file.Content) || seen[file.Path] || len(file.Content) > 1<<20 || total > 4<<20 || strings.ContainsRune(file.Content, 0) {
			return "", errors.New("branch replacement path or content is invalid")
		}
		seen[file.Path] = true
		// A directory ancestor cannot be a symlink, blob or submodule.
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if e, found := entries[parent]; found && (e.Kind != "tree" || e.Mode != "040000") {
				return "", errors.New("branch replacement crosses a non-directory entry")
			}
		}
		e, found := entries[file.Path]
		mode := "100644"
		if file.BeforeSHA256 == "" {
			if found {
				return "", errors.New("new branch replacement path already exists")
			}
		} else {
			if !branchChangeDigest.MatchString(file.BeforeSHA256) || !found || e.Kind != "blob" || (e.Mode != "100644" && e.Mode != "100755") || !branchChangeSHA.MatchString(e.SHA) {
				return "", errors.New("branch replacement base is not a regular file")
			}
			mode = e.Mode
			var blob struct {
				Content, Encoding string
				Size              int64
			}
			if _, err := c.call(ctx, http.MethodGet, base+"/git/blobs/"+e.SHA, bearer, &blob); err != nil {
				return "", err
			}
			if blob.Encoding != "base64" || blob.Size < 0 || blob.Size > 1<<20 {
				return "", errors.New("branch replacement base exceeds its byte bound")
			}
			raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
			hash := sha256.Sum256(raw)
			if err != nil || int64(len(raw)) != blob.Size || hex.EncodeToString(hash[:]) != file.BeforeSHA256 {
				return "", errors.New("branch replacement source bytes changed")
			}
		}
		changes = append(changes, map[string]string{"path": file.Path, "mode": mode, "type": "blob", "content": file.Content})
	}
	var newTree struct {
		SHA string `json:"sha"`
	}
	if _, err := c.callJSON(ctx, http.MethodPost, base+"/git/trees", bearer, map[string]any{"base_tree": commit.Tree.SHA, "tree": changes}, &newTree); err != nil {
		return "", err
	}
	if !branchChangeSHA.MatchString(newTree.SHA) {
		return "", errors.New("GitHub did not identify the proposed tree")
	}
	identity := map[string]string{"name": "MemQL", "email": "noreply@github.com", "date": edit.Timestamp.UTC().Format(time.RFC3339)}
	message := edit.Title + "\n\nMemQL-Operation: " + edit.OperationID
	var created struct {
		SHA string `json:"sha"`
	}
	if _, err := c.callJSON(ctx, http.MethodPost, base+"/git/commits", bearer, map[string]any{"message": message, "tree": newTree.SHA, "parents": []string{edit.ExpectedHead}, "author": identity, "committer": identity}, &created); err != nil {
		return "", err
	}
	if !branchChangeSHA.MatchString(created.SHA) {
		return "", errors.New("GitHub did not identify the proposed commit")
	}
	var prepared struct {
		Tree    struct{ SHA string }
		Parents []struct{ SHA string }
		Message string
	}
	if _, err := c.call(ctx, http.MethodGet, base+"/git/commits/"+created.SHA, bearer, &prepared); err != nil {
		return "", err
	}
	if prepared.Tree.SHA != newTree.SHA || len(prepared.Parents) != 1 || prepared.Parents[0].SHA != edit.ExpectedHead || prepared.Message != message {
		return "", errors.New("prepared commit differs from the exact proposed tree and parent")
	}
	var err error
	bearer, err = edit.Authorize(ctx, created.SHA)
	if err != nil {
		return "", err
	}
	if bearer == "" {
		return "", errors.New("repository write has no current grant")
	}
	current, _, readErr := c.BranchHead(ctx, bearer, owner, repo, edit.Branch)
	if readErr == nil && current == created.SHA {
		return created.SHA, nil
	}
	if edit.Create {
		if readErr == nil || StatusOf(readErr) != http.StatusNotFound {
			return "", errors.New("proposal branch already exists or could not be read")
		}
	} else if readErr != nil || current != edit.ExpectedHead {
		return "", errors.New("pull-request branch moved before the repository write")
	}
	// The lookup above may overlap a revocation on another replica. Recheck
	// after reconciliation so its old bearer cannot authorize a new ref write.
	bearer, err = edit.Authorize(ctx, created.SHA)
	if err != nil {
		return "", err
	}
	if bearer == "" {
		return "", errors.New("repository write has no current grant")
	}
	var ref any
	var writeErr error
	if edit.Create {
		_, writeErr = c.callJSON(ctx, http.MethodPost, base+"/git/refs", bearer, map[string]any{"ref": "refs/heads/" + edit.Branch, "sha": created.SHA}, &ref)
	} else {
		_, writeErr = c.callJSON(ctx, http.MethodPatch, base+"/git/refs/heads/"+escapePath(edit.Branch), bearer, map[string]any{"sha": created.SHA, "force": false}, &ref)
	}
	// Read-back is allowed after cancellation; no new write is. The immutable
	// prepared commit makes a lost response recoverable on any replica.
	observe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	current, _, readErr = c.BranchHead(observe, bearer, owner, repo, edit.Branch)
	if readErr == nil && current == created.SHA {
		return created.SHA, nil
	}
	if writeErr != nil {
		return "", errors.New("repository write was not confirmed; reconcile the exact prepared commit before retrying")
	}
	return "", errors.New("repository branch read-back differs from the prepared commit")
}

// EnsureChangePullRequest opens a draft for one already-created branch. A
// repeated or uncertain POST is reconciled by its unique head/base identity.
// A deliberately closed proposal stays closed; it is never silently reopened.
func (c *Client) EnsureChangePullRequest(ctx context.Context, bearer, owner, repo, head, base, title, body string) (string, error) {
	return c.ensureChangePullRequest(ctx, bearer, owner, repo, head, base, title, body, nil)
}

// EnsureAuthorizedChangePullRequest refreshes authorization after read-only
// reconciliation and immediately before creating the draft. A revoked grant
// cannot use the bearer that was valid before the lookup.
func (c *Client) EnsureAuthorizedChangePullRequest(ctx context.Context, bearer, owner, repo, head, base, title, body string, authorize func(context.Context) (string, error)) (string, error) {
	if authorize == nil {
		return "", errors.New("pull-request creation requires current authorization")
	}
	return c.ensureChangePullRequest(ctx, bearer, owner, repo, head, base, title, body, authorize)
}

func (c *Client) ensureChangePullRequest(ctx context.Context, bearer, owner, repo, head, base, title, body string, authorize func(context.Context) (string, error)) (string, error) {
	if !branchChangeRef(head) || !branchChangeRef(base) || head == base || len(title) == 0 || len(title) > 200 || strings.ContainsAny(title, "\x00\r\n") || len(body) > 16000 || !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", errors.New("pull-request proposal is invalid or unbounded")
	}
	endpoint := repoEndpoint(owner, repo) + "/pulls"
	lookup := func(ctx context.Context) (string, error) {
		// Explicit JSON names are needed for GitHub's snake-case URL field.
		var wire []struct {
			URL   string `json:"html_url"`
			State string `json:"state"`
		}
		_, err := c.call(ctx, http.MethodGet, endpoint+"?state=all&head="+url.QueryEscape(owner+":"+head)+"&base="+url.QueryEscape(base)+"&per_page=2", bearer, &wire)
		if err != nil {
			return "", err
		}
		if len(wire) == 0 {
			return "", nil
		}
		if len(wire) != 1 || wire[0].State != "open" || !strings.HasPrefix(wire[0].URL, "https://github.com/"+owner+"/"+repo+"/pull/") {
			return "", errors.New("repository proposal is closed or ambiguous")
		}
		return wire[0].URL, nil
	}
	if prior, err := lookup(ctx); err != nil || prior != "" {
		return prior, err
	}
	if authorize != nil {
		fresh, err := authorize(ctx)
		if err != nil {
			return "", err
		}
		if fresh == "" {
			return "", errors.New("pull-request authorization returned no credential")
		}
		bearer = fresh
	}
	var created struct {
		URL string `json:"html_url"`
	}
	_, writeErr := c.callJSON(ctx, http.MethodPost, endpoint, bearer, map[string]any{"head": head, "base": base, "title": title, "body": body, "draft": true}, &created)
	observe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	observed, err := lookup(observe)
	if err != nil || observed == "" {
		return "", errors.New("pull-request creation was not confirmed; reconcile the existing branch before retrying")
	}
	if writeErr == nil && created.URL != observed {
		return "", errors.New("pull-request read-back differs from the creation receipt")
	}
	return observed, nil
}
