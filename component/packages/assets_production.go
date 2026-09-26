package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type assetCache interface {
	Read(context.Context, string, int64) ([]byte, bool, error)
	Write(context.Context, string, []byte) error
}

type releaseAssetImporter struct {
	http        *http.Client
	credentials CredentialResolver
	github      *githubapp.Client
	cache       assetCache
}

func newProductionAssetImporter(s *store, gh *githubapp.Client) AssetImporter {
	client := sourceHTTPClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many asset redirects")
		}
		host := req.URL.Hostname()
		if req.URL.Scheme != "https" || (host != "api.github.com" && !strings.HasSuffix(host, ".githubusercontent.com")) {
			return fmt.Errorf("asset redirect is not a GitHub download host")
		}
		if host != "api.github.com" {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &releaseAssetImporter{http: client, credentials: s.resolveCredential, github: gh, cache: azureAssetCache{}}
}

func (f *releaseAssetImporter) Import(ctx context.Context, packageID string, src RepoSource, a ManifestAsset, limits Limits) ([]byte, error) {
	if err := validateAssets([]ManifestAsset{a}); err != nil {
		return nil, err
	}
	if a.Size > limits.normalized().MaxFileBytes || a.Size > limits.normalized().MaxSourceBytes {
		return nil, refuse(CodeSourceTooLarge, "asset %q exceeds this cluster's size limit", a.Path)
	}
	remote, _ := parseReleaseAssetSource(a.Source)
	owner, repo, err := parseGitHubRepo(src.RepoUrl)
	if err != nil || !strings.EqualFold(owner, remote.owner) || !strings.EqualFold(repo, remote.repo) {
		return nil, fmt.Errorf("asset %q must come from a release of this package's GitHub repository", a.Path)
	}
	if packageID == "" || f.cache == nil {
		return nil, fmt.Errorf("asset import requires a package and configured blob storage")
	}
	ctx, finish := context.WithTimeout(ctx, sourceDownloadTimeout)
	defer finish()
	// Recheck the package owner's credential even for cache hits: a revoked
	// grant must not gain a new path through already imported private bytes.
	bearer := ""
	if src.CredentialId != "" {
		if f.credentials == nil {
			return nil, fmt.Errorf("this node cannot resolve the asset source credential")
		}
		credential, err := f.credentials(ctx, src.CredentialId, src.OwnerUserId)
		if err != nil {
			return nil, err
		}
		bearer, err = installationBearer(ctx, f.github, credential, owner, repo)
		if err != nil {
			return nil, err
		}
	}
	// The package scopes access; the digest deduplicates repeated paths within
	// that package. No manifest supplies a cluster storage key.
	scope := sha256.Sum256([]byte(packageID))
	key := "packages/assets/" + hex.EncodeToString(scope[:]) + "/" + a.SHA256
	if raw, found, err := f.cache.Read(ctx, key, a.Size); err != nil {
		return nil, fmt.Errorf("reading cached asset: %w", err)
	} else if found {
		if err := verifyAsset(a, raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	base := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	metadata, err := f.read(ctx, base+"/releases/tags/"+url.PathEscape(remote.tag), bearer, "application/vnd.github+json", 2*1024*1024)
	if err != nil {
		return nil, err
	}
	var release struct {
		Assets []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			State string `json:"state"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(metadata, &release); err != nil {
		return nil, fmt.Errorf("GitHub returned invalid release metadata")
	}
	var assetID int64
	for _, entry := range release.Assets {
		if entry.Name == remote.name {
			if entry.ID <= 0 || entry.Size != a.Size || entry.State != "uploaded" || assetID != 0 {
				return nil, fmt.Errorf("release asset metadata does not match %q", a.Path)
			}
			assetID = entry.ID
		}
	}
	if assetID == 0 {
		return nil, fmt.Errorf("release %q has no uploaded asset named %q", remote.tag, remote.name)
	}
	raw, err := f.read(ctx, fmt.Sprintf("%s/releases/assets/%d", base, assetID), bearer, "application/octet-stream", a.Size)
	if err != nil {
		return nil, err
	}
	if err := verifyAsset(a, raw); err != nil {
		return nil, err
	}
	if err := f.cache.Write(ctx, key, raw); err != nil {
		return nil, fmt.Errorf("storing verified asset: %w", err)
	}
	return raw, nil
}

func (f *releaseAssetImporter) read(ctx context.Context, target, bearer, accept string, max int64) ([]byte, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "memql-packages")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		// Redirect URLs can carry signed query strings; do not surface them.
		return nil, sourceDownloadError(ctx, fmt.Errorf("GitHub asset download could not complete"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub refused the asset download (HTTP %d); check the release and package source connection", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, fmt.Errorf("GitHub asset response exceeds the declared size")
	}
	raw, err := io.ReadAll(io.LimitReader(&sourceBody{ReadCloser: resp.Body, cancel: cancel, idle: sourceReadIdleTimeout}, max+1))
	if err != nil {
		return nil, sourceDownloadError(ctx, fmt.Errorf("GitHub asset download was interrupted"))
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("GitHub asset response exceeds the declared size")
	}
	return raw, nil
}

// Clients are resolved per operation, avoiding mutable initialization shared
// by concurrent deployment runs. The configured container is private; cached
// assets become web content only through the existing site publisher.
type azureAssetCache struct{}

func assetBlobClient(ctx context.Context) (*azureblob.AzureBlobUploader, string, error) {
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return nil, "", fmt.Errorf("this cluster has no blob storage configured for package assets")
	}
	uploader, err := azureblob.New(ctx)
	return uploader, container, err
}

func (azureAssetCache) Read(ctx context.Context, key string, max int64) ([]byte, bool, error) {
	uploader, container, err := assetBlobClient(ctx)
	if err != nil {
		return nil, false, err
	}
	raw, err := uploader.DownloadWithLimit(ctx, container, key, max)
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

func (azureAssetCache) Write(ctx context.Context, key string, raw []byte) error {
	uploader, container, err := assetBlobClient(ctx)
	if err != nil {
		return err
	}
	_, err = uploader.Upload(ctx, container, key, raw, "application/octet-stream")
	return err
}
