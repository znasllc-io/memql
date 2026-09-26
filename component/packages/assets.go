package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/edge"
)

// ManifestAsset is a file supplied separately from source and imported only
// after confirmation. Path is relative to the published site, not the build
// workspace. The digest pins the bytes even if a release asset is replaced.
type ManifestAsset struct {
	Path   string `yaml:"path" json:"path"`
	Source string `yaml:"source" json:"source"`
	SHA256 string `yaml:"sha256" json:"sha256"`
	Size   int64  `yaml:"size" json:"size"`
}

type releaseAssetSource struct{ owner, repo, tag, name string }

func parseReleaseAssetSource(raw string) (releaseAssetSource, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return releaseAssetSource{}, fmt.Errorf("asset source must be an HTTPS GitHub release download URL without credentials or query parameters")
	}
	p := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(p) != 6 || p[2] != "releases" || p[3] != "download" {
		return releaseAssetSource{}, fmt.Errorf("asset source must name a GitHub release tag and file")
	}
	for i := range p {
		p[i], err = url.PathUnescape(p[i])
		if err != nil || p[i] == "" || strings.ContainsAny(p[i], "\x00\\") || p[i] == "." || p[i] == ".." {
			return releaseAssetSource{}, fmt.Errorf("asset source contains an invalid path segment")
		}
	}
	if strings.ContainsAny(p[0]+p[1]+p[5], "/") {
		return releaseAssetSource{}, fmt.Errorf("asset repository and filename must be single path segments")
	}
	return releaseAssetSource{p[0], p[1], p[4], p[5]}, nil
}

func validateAssets(assets []ManifestAsset) error {
	seen := map[string]bool{}
	for _, a := range assets {
		clean, err := safeArchivePath(a.Path)
		if err != nil || clean != a.Path || a.Path == "." || !fs.ValidPath(a.Path) {
			return fmt.Errorf("asset %q must have a clean, relative site path", a.Path)
		}
		if seen[a.Path] {
			return fmt.Errorf("asset path %q is declared twice", a.Path)
		}
		seen[a.Path] = true
		digest, err := hex.DecodeString(a.SHA256)
		if err != nil || len(digest) != sha256.Size || a.SHA256 != strings.ToLower(a.SHA256) || a.Size <= 0 || a.Size == math.MaxInt64 {
			return fmt.Errorf("asset %q needs a lowercase SHA-256 digest and positive size", a.Path)
		}
		if _, err := parseReleaseAssetSource(a.Source); err != nil {
			return fmt.Errorf("asset %q: %w", a.Path, err)
		}
	}
	for p := range seen {
		for parent := p; strings.Contains(parent, "/"); {
			parent = parent[:strings.LastIndexByte(parent, '/')]
			if seen[parent] {
				return fmt.Errorf("asset %q is also the parent directory of %q", parent, p)
			}
		}
	}
	return nil
}

func assetPlanFingerprint(assets []ManifestAsset) string {
	if len(assets) == 0 {
		return ""
	}
	copyAssets := append([]ManifestAsset(nil), assets...)
	sort.Slice(copyAssets, func(i, j int) bool { return copyAssets[i].Path < copyAssets[j].Path })
	raw, _ := json.Marshal(copyAssets)
	return string(raw)
}

func assetDeclaredLimits(assets []ManifestAsset, limits Limits) error {
	limits = limits.normalized()
	if len(assets) > limits.MaxFileCount {
		return fmt.Errorf("declared assets exceed the file count limit")
	}
	var total int64
	for _, a := range assets {
		if a.Size > limits.MaxFileBytes || a.Size > limits.MaxSourceBytes-total {
			return fmt.Errorf("asset %q exceeds the file or complete output size limit", a.Path)
		}
		total += a.Size
	}
	return nil
}

func validateAssetRepositories(rep *Report, repository string) error {
	owner, repo, repoErr := parseGitHubRepo(repository)
	for i := range rep.Deployables {
		d := &rep.Deployables[i]
		for _, asset := range d.Assets {
			source, err := parseReleaseAssetSource(asset.Source)
			if repoErr != nil || err != nil || !strings.EqualFold(owner, source.owner) || !strings.EqualFold(repo, source.repo) {
				ref := refuseScoped(CodeManifestInvalid, d.Name, "asset %q must name a release in this package's GitHub repository; external assets require a repository source", asset.Path)
				d.Problem = ptr(problemFrom(ref, true))
				rep.add(*d.Problem)
				return ref
			}
		}
	}
	return nil
}

// AssetImporter verifies and durably caches bytes in the cluster's blob store.
// Its scope is a package whose authorization the pipeline has already checked.
type AssetImporter interface {
	Import(context.Context, string, RepoSource, ManifestAsset, Limits) ([]byte, error)
}

func (d *Deps) importAssets(ctx context.Context, req DeployRequest, pkg map[string]any, dep DeployableReport, bundle edge.Bundle) error {
	if len(dep.Assets) == 0 {
		return nil
	}
	if d.Assets == nil {
		return refuseScoped(CodeSourceUnreadable, dep.Name, "this cluster cannot import the assets declared by %q", dep.Name)
	}
	if bundle == nil {
		return refuseScoped(CodeDeployableBuildFailed, dep.Name, "the build returned no output to attach assets to")
	}
	if err := d.validatePackageSourceConnection(ctx, pkg); err != nil {
		return err
	}
	if err := validateAssets(dep.Assets); err != nil {
		return refuseScoped(CodeManifestInvalid, dep.Name, "%v", err)
	}
	limits := d.Limits.normalized()
	var total int64
	for _, raw := range bundle {
		if int64(len(raw)) > limits.MaxSourceBytes-total {
			return refuse(CodeSourceTooLarge, "built output exceeds the source size limit")
		}
		total += int64(len(raw))
	}
	if len(bundle) > limits.MaxFileCount || len(dep.Assets) > limits.MaxFileCount-len(bundle) {
		return refuse(CodeSourceTooLarge, "built output and declared assets exceed the file count limit")
	}
	for _, a := range dep.Assets {
		if a.Size > limits.MaxFileBytes || a.Size > limits.MaxSourceBytes-total {
			return refuse(CodeSourceTooLarge, "asset %q exceeds the file or complete output size limit", a.Path)
		}
		total += a.Size
		for existing := range bundle {
			if existing == a.Path || strings.HasPrefix(existing, a.Path+"/") || strings.HasPrefix(a.Path, existing+"/") {
				return refuseScoped(CodeManifestInvalid, dep.Name, "asset %q conflicts with built output %q", a.Path, existing)
			}
		}
	}
	// A single bounded budget for all imports, including cached blob reads.
	ctx, cancel := context.WithTimeout(ctx, sourceDownloadTimeout)
	defer cancel()
	src := RepoSource{RepoUrl: rowString(pkg, "repoUrl"), CredentialId: rowString(pkg, "credentialId"), OwnerUserId: rowString(pkg, "ownerUserId")}
	for _, a := range dep.Assets {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := d.Assets.Import(ctx, req.PackageId, src, a, limits)
		if err != nil {
			if RefusalCode(err) != "" {
				return err
			}
			return refuseScoped(CodeSourceUnreadable, dep.Name, "asset %q could not be imported: %v", a.Path, err)
		}
		if err := verifyAsset(a, raw); err != nil {
			return refuseScoped(CodeSourceUnreadable, dep.Name, "%v", err)
		}
		bundle[a.Path] = raw
	}
	return nil
}

func verifyAsset(a ManifestAsset, raw []byte) error {
	if int64(len(raw)) != a.Size {
		return fmt.Errorf("asset %q has %d bytes; expected %d", a.Path, len(raw), a.Size)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != a.SHA256 {
		return fmt.Errorf("asset %q does not match its declared SHA-256 digest", a.Path)
	}
	return nil
}
