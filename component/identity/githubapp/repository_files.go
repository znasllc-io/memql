package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// RepositoryFiles reads only the approved files or directory prefixes at one
// immutable commit. A large or truncated selection is refused, not silently
// shortened into evidence that could mislead an author or reviewer. Missing
// exact paths are represented as new regular files; symlinks/submodules never
// become authorable content.
func (c *Client) RepositoryFiles(ctx context.Context, bearer, owner, repo, commit string, paths []string) ([]BranchReplacement, error) {
	if !branchChangeSHA.MatchString(commit) || len(paths) == 0 || len(paths) > 256 {
		return nil, errors.New("repository read requires an exact commit and bounded paths")
	}
	wanted := map[string]bool{}
	for _, p := range paths {
		clean := strings.TrimSuffix(p, "/")
		if clean == "" || clean == "." || clean == ".." || path.IsAbs(clean) || path.Clean(clean) != clean || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") || strings.ContainsAny(p, "\\\x00\r\n") || wanted[p] {
			return nil, errors.New("repository read contains an invalid path")
		}
		wanted[p] = true
	}
	var tree struct {
		Truncated bool
		Tree      []struct{ Path, Mode, Type, SHA string }
	}
	if _, err := c.do(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/trees/"+commit+"?recursive=1", bearer, nil, &tree, largeBody); err != nil {
		return nil, err
	}
	if tree.Truncated {
		return nil, errors.New("repository source tree is incomplete")
	}
	selected := map[string]string{}
	matched := map[string]bool{}
	for _, entry := range tree.Tree {
		for p := range wanted {
			prefix := strings.HasSuffix(p, "/")
			if entry.Path != p && !(prefix && strings.HasPrefix(entry.Path, p)) {
				continue
			}
			if entry.Type == "tree" && entry.Mode == "040000" {
				continue
			}
			if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || !branchChangeSHA.MatchString(entry.SHA) {
				return nil, errors.New("repository selection contains a link or unsupported entry")
			}
			if prior, ok := selected[entry.Path]; ok && prior != entry.SHA {
				return nil, errors.New("repository tree contains conflicting paths")
			}
			selected[entry.Path] = entry.SHA
			matched[p] = true
		}
	}
	for p := range wanted {
		if !matched[p] {
			if strings.HasSuffix(p, "/") {
				return nil, errors.New("approved repository directory contains no readable files")
			}
			for _, entry := range tree.Tree {
				if entry.Path == p {
					return nil, errors.New("approved file path names a directory")
				}
			}
			selected[p] = ""
		}
	}
	if len(selected) > 256 {
		return nil, errors.New("repository selection exceeds 256 files; narrow the request")
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]BranchReplacement, 0, len(names))
	total := 0
	for _, name := range names {
		file := BranchReplacement{Path: name}
		if sha := selected[name]; sha != "" {
			var blob struct {
				Encoding, Content string
				Size              int
			}
			if _, err := c.call(ctx, http.MethodGet, repoEndpoint(owner, repo)+"/git/blobs/"+sha, bearer, &blob); err != nil {
				return nil, err
			}
			if blob.Size < 0 || blob.Size > 1<<20 {
				return nil, errors.New("repository file exceeds the read limit")
			}
			content, err := decodeContent(blob.Encoding, blob.Content)
			if err != nil || len(content) > 1<<20 || len(content) != blob.Size || !utf8.Valid(content) || strings.ContainsRune(string(content), 0) {
				return nil, errors.New("repository file is not bounded UTF-8 text")
			}
			total += len(content)
			if total > 4<<20 {
				return nil, errors.New("repository selection exceeds the content limit")
			}
			sum := sha256.Sum256(content)
			file.BeforeSHA256 = hex.EncodeToString(sum[:])
			file.Content = string(content)
		}
		out = append(out, file)
	}
	return out, nil
}
