package argocd

import (
	"bytes"
	"context"
	"crypto/sha1" // Git object IDs, not signatures or a new approval hash.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/core/id"
)

const (
	maxSourceObject = 4 << 20
	maxSourceBytes  = 32 << 20
	maxSourceCount  = 4096
)

// GitObjects supplies raw, decompressed Git object bodies. The native provider
// must honor cancellation and constrain its transport/credentials. Every body
// is bounded and checked against the exact requested Git ID before use, so a
// workspace path, archive label or supplied "commit" field cannot substitute
// different bytes. This is not a public callback or a shell execution surface.
type GitObjects interface {
	OpenGitObject(context.Context, string, string) (io.ReadCloser, error)
}

type SourceFile struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	Blob          string `json:"blob"`
	ContentDigest string `json:"contentDigest"`
}

// ClosedSource proves the supported Kustomize file closure against one exact
// Git commit. It does not authenticate the publisher, renderer or installation
// configuration, nor approve the resource diff. Those remain separate gates.
// No file contents, commit message or credentials escape in its projection.
type ClosedSource struct {
	spec   []byte
	files  []SourceFile
	digest string
}

func (s ClosedSource) Digest() string      { return s.digest }
func (s ClosedSource) Files() []SourceFile { return append([]SourceFile(nil), s.files...) }
func (s ClosedSource) Spec() RenderSpec {
	var spec RenderSpec
	_ = json.Unmarshal(s.spec, &spec)
	return spec
}
func (s ClosedSource) String() string {
	return fmt.Sprintf("ArgoCD source files=%d digest=%s", len(s.files), s.digest)
}
func (s ClosedSource) GoString() string { return s.String() }

type gitEntry struct{ mode, name, oid string }
type sourceVerifier struct {
	ctx        context.Context
	objects    GitObjects
	root       string
	bytes      int
	bodies     map[string][]byte
	trees      map[string]map[string]gitEntry
	files      map[string]SourceFile
	visiting   map[string]bool
	visited    map[string]bool
	settings   int
	attributes map[string]bool
}

// VerifySourceClosure follows file reads, not workflow policy. It verifies the
// commit -> tree -> blob object chain and admits only a supported, closed set
// of Kustomize inputs. It never follows a symlink, submodule, remote base or
// executable generator. No checkout, Git filter or working-tree file is read.
func VerifySourceClosure(ctx context.Context, objects GitObjects, spec RenderSpec) (ClosedSource, error) {
	if objects == nil {
		return ClosedSource{}, errors.New("Git source objects are unavailable")
	}
	request, specBody, err := renderRequest(spec, RepositoryCredentials{})
	if err != nil {
		return ClosedSource{}, err
	}
	if _, err := sourcePath(".", request.ApplicationSource.Path); err != nil {
		return ClosedSource{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	v := &sourceVerifier{ctx: ctx, objects: objects, bodies: map[string][]byte{}, trees: map[string]map[string]gitEntry{}, files: map[string]SourceFile{}, visiting: map[string]bool{}, visited: map[string]bool{}, attributes: map[string]bool{}}
	commit, err := v.read("commit", request.Revision)
	if err != nil {
		return ClosedSource{}, err
	}
	line, _, ok := bytes.Cut(commit, []byte{'\n'})
	if !ok || len(line) != 45 || !bytes.HasPrefix(line, []byte("tree ")) || !commitSHA.Match(line[5:]) {
		return ClosedSource{}, errors.New("Git commit does not name one canonical tree")
	}
	v.root = string(line[5:])
	if _, found, err := v.lookup(".gitmodules"); err != nil {
		return ClosedSource{}, err
	} else if found {
		return ClosedSource{}, errors.New("Git submodule configuration is not a closed installation source")
	}
	// Argo merges these files before selecting a renderer. Until that separate
	// source-override codec is qualified, refuse rather than overlooking it.
	for _, name := range []string{".argocd-source.yaml", ".argocd-source-" + request.AppName + ".yaml"} {
		p, err := sourcePath(request.ApplicationSource.Path, name)
		if err != nil {
			return ClosedSource{}, err
		}
		_, found, err := v.lookup(p)
		if err != nil {
			return ClosedSource{}, err
		}
		if found {
			return ClosedSource{}, errors.New("ArgoCD source parameter files must be moved into the reviewed overlay")
		}
	}
	if err := v.kustomization(request.ApplicationSource.Path, false); err != nil {
		return ClosedSource{}, err
	}
	files := make([]SourceFile, 0, len(v.files))
	for _, file := range v.files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	body, err := json.Marshal(struct {
		Spec  json.RawMessage `json:"spec"`
		Files []SourceFile    `json:"files"`
	}{specBody, files})
	if err != nil {
		return ClosedSource{}, errors.New("Git source evidence could not be encoded")
	}
	digest := "memql-id:" + string(id.NewUntracked().FromString("memql-argocd-closed-source-v1\n"+string(body)))
	return ClosedSource{spec: specBody, files: files, digest: digest}, nil
}

func (v *sourceVerifier) read(kind, oid string) ([]byte, error) {
	if err := v.ctx.Err(); err != nil {
		return nil, err
	}
	if !commitSHA.MatchString(oid) {
		return nil, errors.New("Git source contains an invalid object identity")
	}
	key := kind + "/" + oid
	if body, found := v.bodies[key]; found {
		return body, nil
	}
	if len(v.bodies) >= maxSourceCount {
		return nil, errors.New("Git source exceeds its object bound")
	}
	reader, err := v.objects.OpenGitObject(v.ctx, kind, oid)
	if err != nil {
		return nil, errors.New("Git source object could not be read")
	}
	if reader == nil {
		return nil, errors.New("Git source object reader is unavailable")
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, maxSourceObject+1))
	closeErr := reader.Close()
	if err := v.ctx.Err(); err != nil {
		return nil, err
	}
	if readErr != nil || closeErr != nil {
		return nil, errors.New("Git source object read did not complete")
	}
	if len(body) > maxSourceObject || len(body) > maxSourceBytes-v.bytes {
		return nil, errors.New("Git source exceeds its byte bound")
	}
	// SHA-1 is Git's existing object identifier. The externally approved commit
	// is fixed before this read; new native evidence uses core/id separately.
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "%s %d\x00", kind, len(body))
	_, _ = hash.Write(body)
	if hex.EncodeToString(hash.Sum(nil)) != oid {
		return nil, errors.New("Git source object does not match its requested identity")
	}
	v.bytes += len(body)
	v.bodies[key] = body
	return body, nil
}

func (v *sourceVerifier) tree(oid string) (map[string]gitEntry, error) {
	if tree, found := v.trees[oid]; found {
		return tree, nil
	}
	body, err := v.read("tree", oid)
	if err != nil {
		return nil, err
	}
	entries := map[string]gitEntry{}
	for len(body) > 0 {
		header, rest, ok := bytes.Cut(body, []byte{0})
		if !ok || len(rest) < 20 {
			return nil, errors.New("Git source tree is malformed")
		}
		mode, name, ok := strings.Cut(string(header), " ")
		if !ok || name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") || strings.ContainsFunc(name, unicode.IsControl) ||
			(mode != "40000" && mode != "100644" && mode != "100755" && mode != "120000" && mode != "160000") {
			return nil, errors.New("Git source tree contains an unsupported entry")
		}
		if _, exists := entries[name]; exists {
			return nil, errors.New("Git source tree has duplicate names")
		}
		entries[name] = gitEntry{mode: mode, name: name, oid: hex.EncodeToString(rest[:20])}
		body = rest[20:]
	}
	v.trees[oid] = entries
	return entries, nil
}

func (v *sourceVerifier) lookup(p string) (gitEntry, bool, error) {
	if p == "." {
		return gitEntry{mode: "40000", oid: v.root}, true, nil
	}
	parts := strings.Split(p, "/")
	if len(parts) > 128 {
		return gitEntry{}, false, errors.New("Git source path exceeds its depth bound")
	}
	current := gitEntry{mode: "40000", oid: v.root}
	for _, part := range parts {
		if current.mode != "40000" {
			return gitEntry{}, false, errors.New("Git source path traverses a non-directory, symlink or submodule")
		}
		tree, err := v.tree(current.oid)
		if err != nil {
			return gitEntry{}, false, err
		}
		next, found := tree[part]
		if !found {
			return gitEntry{}, false, nil
		}
		current = next
	}
	return current, true, nil
}

func (v *sourceVerifier) file(p string) ([]byte, error) {
	if err := v.checkAttributes(path.Dir(p)); err != nil {
		return nil, err
	}
	return v.plainFile(p)
}

func (v *sourceVerifier) plainFile(p string) ([]byte, error) {
	entry, found, err := v.lookup(p)
	if err != nil {
		return nil, err
	}
	if !found || (entry.mode != "100644" && entry.mode != "100755") {
		return nil, fmt.Errorf("Git source input %q is not a committed regular file", p)
	}
	body, err := v.read("blob", entry.oid)
	if err != nil {
		return nil, err
	}
	if _, known := v.files[p]; !known && len(v.files) >= maxSourceCount {
		return nil, errors.New("Git source exceeds its file bound")
	}
	digest := "memql-id:" + string(id.NewUntracked().FromString("memql-source-file-v1\n"+string(body)))
	v.files[p] = SourceFile{Path: p, Mode: entry.mode, Blob: entry.oid, ContentDigest: digest}
	return body, nil
}

func (v *sourceVerifier) checkAttributes(dir string) error {
	if v.attributes[dir] {
		return nil
	}
	if dir != "." {
		if err := v.checkAttributes(path.Dir(dir)); err != nil {
			return err
		}
	}
	p := path.Join(dir, ".gitattributes")
	if _, found, err := v.lookup(p); err != nil {
		return err
	} else if found {
		body, err := v.plainFile(p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || strings.HasPrefix(fields[0], "[attr]") || strings.ContainsAny(fields[0], "\"\\") {
				return errors.New("Git source has unqualified attribute syntax")
			}
			for _, value := range fields[1:] {
				switch value {
				case "text", "-text", "!text", "text=auto", "eol=lf", "eol=crlf", "diff", "-diff", "!diff", "linguist-generated=true", "linguist-generated=false", "linguist-vendored=true", "linguist-vendored=false":
				default:
					return errors.New("Git source has attributes requiring external or unqualified checkout behavior")
				}
			}
		}
	}
	v.attributes[dir] = true
	return nil
}

func sourcePath(base, reference string) (string, error) {
	if reference == "" || !utf8.ValidString(reference) || path.IsAbs(reference) || strings.ContainsAny(reference, ":?#\\") || strings.ContainsFunc(reference, unicode.IsControl) {
		return "", errors.New("Kustomize input must be a repository-relative file or directory")
	}
	joined := path.Join(base, reference)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", errors.New("Kustomize input leaves the verified Git tree")
	}
	if len(strings.Split(joined, "/")) > 128 {
		return "", errors.New("Git source path exceeds its depth bound")
	}
	return joined, nil
}
