package azureblob

import (
	"context"
	"strings"
	"testing"
)

func TestClusterAssetAdapterRestrictsNativeDestinations(t *testing.T) {
	hash := strings.Repeat("a", 32)
	for _, object := range []string{"packages/active.json", "packages/example/" + hash + "/concepts.memql", "packages/example/" + hash + "/templates/main.tmpl", "sites/site-one/v123456789abc/index.html", "sites/v1:platform:site:site-one/v123456789abc/assets/main.js"} {
		if !clusterAssetPath(object) {
			t.Fatalf("native destination refused: %s", object)
		}
	}
	for _, object := range []string{"library/alice/file", "backups/alice/file", "objects/alice/file", "packages/snapshots/" + hash + "/source.tar.gz", "packages/builds/" + hash + "/output.tar.gz", "packages/assets/" + hash + "/file", "packages/example/latest/file", "packages/example/" + hash + "/../file", "/packages/active.json", " packages/active.json", "packages/active.json ", "packages/example/" + hash + "/file ", "sites/other/latest/index.html", "sites/v1:identity:user:other/v123456789abc/index.html", "sites/site/v123456789abc/../file", "sites/site/v123456789abc/file\\name", "packages/example/" + hash + "/file\x00"} {
		// Refusal is before client initialization or any provider/accounting call.
		var uploader *AzureBlobUploader
		if _, err := uploader.ClusterAssets().Upload(context.Background(), "container", object, nil, ""); err == nil || !strings.Contains(err.Error(), "not a native shared asset") {
			t.Fatalf("destination %q: %v", object, err)
		}
	}
}
