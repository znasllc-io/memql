package release

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/installation"
)

// These are codecs, not arbitrary edits. Each file selects exactly one kind of
// signed-publication substitution. The operator binds every slot in advance.
type overlayFile struct {
	Path          string                `json:"path"`
	Codec         string                `json:"codec"`
	Images        []overlayImage        `json:"images,omitempty"`
	GitReferences []overlayGitReference `json:"gitReferences,omitempty"`
	Env           *overlayEnvValue      `json:"env,omitempty"`
}
type overlayGitReference struct {
	Field      string `json:"field"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Component  string `json:"component"`
}
type overlayEnvValue struct {
	Key       string `json:"key"`
	Component string `json:"component"`
	Value     string `json:"value"`
}

// Keep the v1 fields and their JSON representation unchanged. In-flight v1
// records retain their original configuration hash, snapshot and Git identity.
func (c overlayConfiguration) files() []overlayFile {
	if c.FormatVersion == 1 {
		return []overlayFile{{Path: c.Path, Codec: "kustomizeImages", Images: c.Images}}
	}
	return c.Files
}
func (r overlayRecord) files() []githubapp.BranchReplacement {
	if r.Configuration.FormatVersion == 1 {
		return []githubapp.BranchReplacement{r.Replacement}
	}
	return r.Replacements
}
func overlayFilePath(p string) bool {
	return p != "" && len(p) <= 512 && path.Clean(p) == p && !path.IsAbs(p) && p != "." && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/") && !strings.ContainsAny(p, "\\\x00\r\n") && utf8.ValidString(p)
}
func overlayKustomizationPath(p string) bool {
	return overlayFilePath(p) && path.Base(p) == "kustomization.yaml"
}
func validateOverlayImages(images []overlayImage) error {
	if len(images) == 0 || len(images) > 64 {
		return errors.New("installation overlay needs bounded image bindings")
	}
	seen := map[string]bool{}
	for _, im := range images {
		if im.Name == "" || len(im.Name) > 200 || im.Component == "" || len(im.Component) > 200 || im.Artifact == "" || len(im.Artifact) > 200 || im.Repository == "" || len(im.Repository) > 512 || seen[im.Name] || strings.ContainsAny(im.Repository, "@ \r\n") {
			return errors.New("installation overlay image bindings are incomplete or duplicated")
		}
		seen[im.Name] = true
	}
	return nil
}
func validateOverlayConfiguration(c overlayConfiguration) error {
	if !installationIdentifier.MatchString(c.InstallationID) || !overlayRepository.MatchString(c.Repository) || c.CredentialID == "" || c.OwnerUserID == "" || len(c.BaseBranch) > 128 || c.BaseBranch == "" || c.ProposalWorkflow == "" || c.ObservationWorkflow == "" {
		return errors.New("installation overlay requires an exact owner-bound configuration")
	}
	switch c.FormatVersion {
	case 1:
		if len(c.Files) != 0 || !overlayKustomizationPath(c.Path) {
			return errors.New("legacy installation overlay requires one image file")
		}
		return validateOverlayImages(c.Images)
	case 2:
		if c.Path != "" || len(c.Images) != 0 || len(c.Files) == 0 || len(c.Files) > 32 {
			return errors.New("installation overlay v2 requires only a bounded explicit file set")
		}
	default:
		return errors.New("installation overlay format is unsupported")
	}
	previous := ""
	slots := 0
	for _, file := range c.Files {
		if !overlayFilePath(file.Path) || file.Path <= previous {
			return errors.New("installation overlay files must be unique and sorted by path")
		}
		previous = file.Path
		switch file.Codec {
		case "kustomizeImages":
			if !overlayKustomizationPath(file.Path) || len(file.GitReferences) != 0 || file.Env != nil {
				return errors.New("installation image file has conflicting codecs")
			}
			if err := validateOverlayImages(file.Images); err != nil {
				return err
			}
			slots += len(file.Images)
		case "kustomizeGitReferences":
			if !overlayKustomizationPath(file.Path) || len(file.Images) != 0 || file.Env != nil || len(file.GitReferences) == 0 || len(file.GitReferences) > 16 {
				return errors.New("installation Git-reference file has conflicting or excessive codecs")
			}
			seen := map[string]bool{}
			for _, ref := range file.GitReferences {
				key := ref.Field + "\x00" + ref.Repository + "\x00" + ref.Path
				if (ref.Field != "resources" && ref.Field != "components") || ref.Component == "" || len(ref.Component) > 200 || seen[key] || argocd.ValidateSourceRepositories([]argocd.SourceRepository{{Repository: ref.Repository, Paths: []string{ref.Path}}}) != nil {
					return errors.New("installation Git-reference binding is incomplete, duplicate or unqualified")
				}
				seen[key] = true
			}
			slots += len(file.GitReferences)
		case "envValue":
			if file.Path != "product.env" || len(file.Images) != 0 || len(file.GitReferences) != 0 || file.Env == nil || file.Env.Key != "ENGINE_REF" || file.Env.Component == "" || len(file.Env.Component) > 200 || (file.Env.Value != "componentCommit" && file.Env.Value != "componentVersion") {
				return errors.New("installation environment codec permits only an explicit product.env ENGINE_REF binding")
			}
			slots++
		default:
			return errors.New("installation overlay file codec is unsupported")
		}
	}
	if slots > 128 {
		return errors.New("installation overlay binding set exceeds its bound")
	}
	return nil
}
func validateOverlaySource(c overlayConfiguration, source installation.PublicationSource) error {
	if err := validateOverlayConfiguration(c); err != nil {
		return err
	}
	if source.InstallationID != c.InstallationID || strings.TrimSuffix(source.RepositoryURL, ".git") != "https://github.com/"+c.Repository {
		return errors.New("installation overlay is outside the receiving source")
	}
	roots := []string{}
	for _, app := range source.Applications {
		roots = append(roots, app.Path)
	}
	if len(roots) == 0 && source.Path != "" {
		roots = append(roots, source.Path)
	}
	if len(roots) == 0 || len(roots) > 8 {
		return errors.New("installation overlay requires bounded receiving source paths")
	}
	for _, file := range c.files() {
		if file.Codec == "envValue" {
			continue
		} // Only product.env/ENGINE_REF passes configuration validation.
		admitted := false
		for _, root := range roots {
			if c.FormatVersion == 1 {
				admitted = admitted || file.Path == path.Join(root, "kustomization.yaml")
				continue
			}
			if root == "." || (overlayFilePath(root) && strings.HasPrefix(file.Path, root+"/")) {
				admitted = true
			}
		}
		if !admitted {
			return errors.New("installation overlay file is outside the receiving source paths")
		}
	}
	return nil
}

var overlayBeforeDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateOverlayRecordFiles(r overlayRecord) error {
	if err := validateOverlayConfiguration(r.Configuration); err != nil {
		return err
	}
	if r.Owner != r.Configuration.OwnerUserID || r.Request.InstallationID != r.Configuration.InstallationID || !installationIdentifier.MatchString(r.Request.RequestID) || !stringsHasDigest(r.Request.CandidateID) || !stringsHasDigest(r.Request.CatalogDigest) || !installationCommit.MatchString(r.Request.ExpectedBaseRevision) {
		return errors.New("stored installation overlay identity is invalid")
	}
	if (r.Configuration.FormatVersion == 1 && len(r.Replacements) != 0) || (r.Configuration.FormatVersion == 2 && r.Replacement != (githubapp.BranchReplacement{})) {
		return errors.New("stored installation overlay mixes file formats")
	}
	files := r.files()
	bindings := r.Configuration.files()
	if len(files) != len(bindings) {
		return errors.New("stored installation overlay file set is incomplete")
	}
	total := 0
	for index, file := range files {
		total += len(file.Content)
		if file.Path != bindings[index].Path || !overlayBeforeDigest.MatchString(file.BeforeSHA256) || len(file.Content) == 0 || len(file.Content) > 1<<20 || total > 1<<20 || !utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, 0) {
			return errors.New("stored installation overlay file intent is invalid")
		}
	}
	return nil
}
func buildOverlayFile(before []byte, file overlayFile, publication pipelines.VerifiedPublishedRelease) ([]byte, error) {
	switch file.Codec {
	case "kustomizeImages":
		return pinInstallationImages(before, file.Images, publication)
	case "kustomizeGitReferences":
		return pinInstallationGitReferences(before, file.GitReferences, publication)
	case "envValue":
		if file.Env == nil {
			return nil, errors.New("installation environment binding is missing")
		}
		return pinInstallationEnvValue(before, *file.Env, publication)
	default:
		return nil, errors.New("installation overlay codec is unavailable")
	}
}
