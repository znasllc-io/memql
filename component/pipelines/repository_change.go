package pipelines

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// RepositoryChange selects a retained proposal and an operator-owned grant.
// A repository manifest cannot choose a token, destination repository, writable
// paths, target branch or author policy. Those come from the named grant.
type RepositoryChange struct {
	Rule string `yaml:"rule" json:"rule"`
	Step string `yaml:"step" json:"step"`
	Path string `yaml:"path" json:"path"`
}

// RepositoryChangeRule is installation configuration, never source policy.
// New pull requests branch from the run's exact default-branch commit. Updates
// require the signed delivery actor, current PR author and branch prefix.
type RepositoryChangeRule struct {
	Name         string   `json:"name"`
	Repository   string   `json:"repository"`
	Mode         string   `json:"mode"` // propose or update
	BranchPrefix string   `json:"branchPrefix"`
	Paths        []string `json:"paths"` // exact repository-relative file names
	Author       string   `json:"author,omitempty"`
	Actor        string   `json:"actor,omitempty"`
}

// RepositoryProposal contains bounded UTF-8 replacements for regular files.
// No deletion, mode change, executable, symlink or arbitrary Git operation is
// representable. Base SHA-256 values bind every edit to its source contents;
// an empty base digest requires that exact path to be absent at the base.
type RepositoryProposal struct {
	Schema int                     `json:"schema"`
	Commit string                  `json:"commit"`
	Key    string                  `json:"key"`
	Title  string                  `json:"title"`
	Body   string                  `json:"body"`
	Files  []RepositoryReplacement `json:"files"`
}

type RepositoryReplacement struct {
	Path       string `json:"path"`
	BaseSHA256 string `json:"baseSha256"`
	Content    string `json:"content"`
}

const RepositoryProposalMaxBytes = 4 << 20

var repositoryChangeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var repositoryProposalKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)
var repositoryChangeDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var repositoryChangeCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var repositoryChangeActor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\[\]-]{0,99}$`)

func repositoryChangePath(p string) bool {
	return p != "" && len(p) <= 512 && utf8.ValidString(p) && path.Clean(p) == p &&
		!path.IsAbs(p) && p != "." && p != ".." && !strings.HasPrefix(p, "../") &&
		!strings.ContainsAny(p, "\\\x00\r\n*?[]") && !strings.HasPrefix(p, ".git/") && p != ".git"
}

func (r RepositoryChangeRule) Validate() error {
	if !repositoryChangeName.MatchString(r.Name) || len(r.Repository) > 257 || !releaseSource.MatchString(r.Repository) ||
		(r.Mode != "propose" && r.Mode != "update") || len(r.Paths) == 0 || len(r.Paths) > 256 ||
		!strings.HasSuffix(r.BranchPrefix, "/") || !ValidBranchName(r.BranchPrefix+"proposal") ||
		len(r.BranchPrefix) > 100 || strings.HasPrefix(r.BranchPrefix, "refs/") {
		return errors.New("repository change grant requires a named repository, mode, branch prefix and exact file allowlist")
	}
	if r.Mode == "update" && (!repositoryChangeActor.MatchString(r.Author) || !repositoryChangeActor.MatchString(r.Actor)) {
		return errors.New("updating a pull request requires both an author and a signed delivery actor")
	}
	if r.Mode == "propose" && (r.Author != "" || r.Actor != "") {
		return errors.New("new proposals run only from the repository default branch")
	}
	seen := map[string]bool{}
	for _, p := range r.Paths {
		if !repositoryChangePath(p) || seen[p] || strings.HasPrefix(p, ".github/") {
			return errors.New("repository change grant contains an invalid, repeated or workflow path")
		}
		seen[p] = true
	}
	return nil
}

func ReadRepositoryProposal(raw []byte, rule RepositoryChangeRule, commit string) (RepositoryProposal, error) {
	var result RepositoryProposal
	if err := rule.Validate(); err != nil {
		return result, err
	}
	if len(raw) == 0 || len(raw) > RepositoryProposalMaxBytes || !utf8.Valid(raw) || !repositoryChangeCommit.MatchString(commit) {
		return result, errors.New("repository proposal requires bounded UTF-8 bytes and an exact source commit")
	}
	// Reject repeated object properties before typed decoding; a second parser
	// must not be able to approve a different proposal from the same bytes.
	if err := uniqueRepositoryJSON(raw); err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, errors.New("repository proposal has an invalid shape")
	}
	if result.Schema != 1 || result.Commit != commit || !repositoryProposalKey.MatchString(result.Key) ||
		strings.Contains(result.Key, "..") || len(result.Title) == 0 || len(result.Title) > 200 ||
		strings.ContainsAny(result.Title, "\r\n\x00") || len(result.Body) > 16000 || strings.ContainsRune(result.Body, 0) ||
		len(result.Files) > 256 {
		return result, errors.New("repository proposal identity, description or file count is invalid")
	}
	allowed := map[string]bool{}
	for _, p := range rule.Paths {
		allowed[p] = true
	}
	seen := map[string]bool{}
	for _, file := range result.Files {
		if !allowed[file.Path] || seen[file.Path] || (file.BaseSHA256 != "" && !repositoryChangeDigest.MatchString(file.BaseSHA256)) ||
			len(file.Content) > 1<<20 || strings.ContainsRune(file.Content, 0) {
			return result, errors.New("repository proposal file is outside its grant, duplicated or unbounded")
		}
		seen[file.Path] = true
	}
	return result, nil
}

func uniqueRepositoryJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var read func(int) error
	read = func(depth int) error {
		if depth > 8 {
			return errors.New("repository proposal JSON is too deep")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				s, ok := key.(string)
				if err != nil || !ok || seen[strings.ToLower(s)] {
					return errors.New("repository proposal JSON has a repeated property")
				}
				seen[strings.ToLower(s)] = true
				if err := read(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
		case json.Delim('['):
			for d.More() {
				if err := read(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if err := read(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("repository proposal JSON has trailing data")
	}
	return nil
}

func validateRepositoryChangeStep(spec *Spec, stage string, step StepSpec) *Refusal {
	scope := stage + "/" + step.Name
	r := step.RepositoryChange
	if r == nil || !repositoryChangeName.MatchString(r.Rule) || !repositoryChangePath(r.Path) || !strings.HasSuffix(r.Path, ".json") {
		return Refuse(CodeStepInvalid, scope, "repositoryChange requires a named operator rule and an exact retained .json proposal path.")
	}
	if step.SARIF != nil || step.GitHubReadToken || step.CheckoutHistory != "" || step.Run != "" || step.ImageBuild != nil || step.Image != "" || step.Execution != "" || step.Placement != "" || step.Platform != "" || step.CPUMilli != 0 || step.MemoryMiB != 0 || step.Packages != "" || step.Only != "" || step.Shards != 0 || step.When != nil || len(step.Needs) > 0 || len(step.Services) > 0 || step.Caches != nil || len(step.Secrets) > 0 || step.ImagePullSecret != "" || len(step.Artifacts) > 0 {
		return Refuse(CodeStepInvalid, scope, "repositoryChange accepts only name, rule, producer step, artifact path and timeout; no command receives a writer credential.")
	}
	if step.Timeout != "" {
		d, err := time.ParseDuration(step.Timeout)
		if err != nil || d < time.Minute || d > 10*time.Minute {
			return Refuse(CodeStepInvalid, scope, "Repository change timeout must be between 1m and 10m.")
		}
	}
	for _, prior := range spec.Stages {
		if prior.Name == stage {
			break
		}
		for _, producer := range prior.Steps {
			if StepKey(prior.Name, producer.Name) == r.Step && producer.SARIF == nil && producer.RepositoryChange == nil && producer.ImageBuild == nil && producer.Shards <= 1 && len(producer.Artifacts) > 0 {
				return nil
			}
		}
	}
	return Refuse(CodeStepInvalid, scope, "repositoryChange.step must name an unsharded producer in an earlier stage that retains artifacts.")
}

// ReadRepositoryChangeRules accepts only a bounded, unambiguous operator document.
func ReadRepositoryChangeRules(raw []byte) ([]RepositoryChangeRule, error) {
	var doc struct {
		Schema int                    `json:"schema"`
		Rules  []RepositoryChangeRule `json:"rules"`
	}
	if len(raw) == 0 || len(raw) > 1<<20 || !utf8.Valid(raw) {
		return nil, errors.New("repository change rules require bounded operator configuration")
	}
	if err := uniqueRepositoryJSON(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&doc); err != nil {
		return nil, errors.New("invalid repository change rules")
	}
	if doc.Schema != 1 || len(doc.Rules) > 64 {
		return nil, errors.New("invalid repository change rules schema or count")
	}
	seen := map[string]bool{}
	for _, r := range doc.Rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if seen[r.Name] {
			return nil, errors.New("duplicate repository change rule")
		}
		seen[r.Name] = true
	}
	return doc.Rules, nil
}
