package azureblob

import (
	"context"
	"errors"
	"io/fs"
	"regexp"
	"strings"
)

// ClusterAssets accounts native shared DSL and hosted-site bytes to the
// cluster. It does not change the caller's actor, grants, cancellation or
// deadline. The native caller must authorize the package/site before writing.
// This adapter is not a DSL capability and accepts no arbitrary owner override.
// Existing personally owned ledger entries still refuse; ownership is never
// adopted or transferred. All retention, capacity and replica fences remain.
func (u *AzureBlobUploader) ClusterAssets() Uploader { return clusterAssets{u} }

type clusterAssets struct{ uploader *AzureBlobUploader }

func (a clusterAssets) Upload(ctx context.Context, container, object string, data []byte, contentType string) (string, error) {
	if !clusterAssetPath(object) {
		return "", errors.New("storage destination is not a native shared asset")
	}
	return a.uploader.upload(ctx, container, object, data, contentType, true)
}

var clusterAssetHash = regexp.MustCompile(`^[a-f0-9]{32}$`)
var clusterSiteVersion = regexp.MustCompile(`^v[a-f0-9]{12}$`)
var clusterSiteID = regexp.MustCompile(`^(?:v1:platform:site:)?[A-Za-z0-9_-]{1,160}$`)

func clusterAssetPath(object string) bool {
	if object == "packages/active.json" {
		return true
	}
	if object != strings.TrimSpace(object) || !fs.ValidPath(object) || strings.ContainsAny(object, "\\\x00\r\n") {
		return false
	}
	parts := strings.SplitN(object, "/", 4)
	if len(parts) != 4 || !fs.ValidPath(parts[3]) || parts[3] == "." {
		return false
	}
	switch parts[0] {
	case "packages":
		// Snapshots, build outputs and fetched external assets retain their own
		// existing accounting contract. Only a content-addressed DSL tree is shared.
		return parts[1] != "snapshots" && parts[1] != "builds" && parts[1] != "assets" && clusterAssetHash.MatchString(parts[2])
	case "sites":
		return clusterSiteID.MatchString(parts[1]) && clusterSiteVersion.MatchString(parts[2])
	}
	return false
}
