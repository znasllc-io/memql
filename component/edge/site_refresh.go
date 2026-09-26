package edge

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/buildinfo"
	"golang.org/x/net/html"
)

const siteRefreshPath = "/_memql/site-refresh.js"
const deploymentVersionHeader = "X-MemQL-Deployment"

//go:embed site_refresh.js
var siteRefreshScript []byte

// The public identifier contains no store credentials or internal bundle path.
// It changes for a publish/rollback, runtime settings or binding change, and
// engine upgrades (including baked file:// applications such as MemQL OS).
func deploymentVersion(site *Site) string {
	settings, _ := json.Marshal(site.Settings)
	store := ""
	if site.Store != nil {
		store = site.Store.ID + "|" + site.Store.Domain + "|" + site.Store.StorefrontTokenRef + "|" + site.Store.APIVersion
	}
	return strings.Trim(strongETag("site-refresh-v1", site.BundleRef, buildinfo.Version(), buildinfo.Commit(), store, string(settings)), `"`)
}

func serveSiteRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "site-refresh.js", time.Time{}, bytes.NewReader(siteRefreshScript))
}

// Insert without serializing the document: reserialization would change the
// bytes of inline scripts whose exact hashes are already in its CSP. An
// external same-origin script keeps the existing script-src policy intact.
func refreshingDocument(fsys fs.FS, name, version string) ([]byte, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	tag := []byte(`<script src="` + siteRefreshPath + `" data-memql-version="` + version + `" defer></script>`)
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	offset, insertion := 0, 0
	for {
		kind := tokenizer.Next()
		raw := tokenizer.Raw()
		offset += len(raw)
		if kind == html.ErrorToken {
			break
		}
		if kind == html.DoctypeToken {
			insertion = offset
		}
		if kind == html.StartTagToken {
			token, _ := tokenizer.TagName()
			if bytes.EqualFold(token, []byte("head")) {
				insertion = offset
				break
			}
			if bytes.EqualFold(token, []byte("body")) {
				insertion = offset - len(raw)
				break
			}
		}
	}
	result := make([]byte, 0, len(data)+len(tag))
	result = append(result, data[:insertion]...)
	result = append(result, tag...)
	return append(result, data[insertion:]...), nil
}
