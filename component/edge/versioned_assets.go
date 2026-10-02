package edge

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

const versionedAssetPath = "/_memql/assets/"

// Only the publisher's content-addressed blob layout has this guarantee.
// A file:// working tree or an arbitrary blob prefix can change in place.
var publishedBundleRef = regexp.MustCompile(`^blob://sites/[^/]+/v[0-9a-f]{12}/$`)

type filePolicy struct {
	assetPrefix     string
	immutable       bool
	suppressRefresh bool
}

func assetPrefixFor(site *Site) string {
	if site == nil || extensionRuntimePath(site) != "" || !publishedBundleRef.MatchString(site.BundleRef) {
		return ""
	}
	return versionedAssetPath + strings.Trim(strongETag("assets-v1", site.BundleRef), `"`) + "/"
}

// The version is a selector for the already-authorized bundle, never a blob
// address. Looking up an arbitrary old prefix would also expose unpublished
// candidates without their preview grant. A stale URL therefore returns 404
// rather than serving a different version or reaching historical storage.
func (h *Handler) serveVersionedAsset(w http.ResponseWriter, r *http.Request, site *Site) string {
	securityHeaders(w, r)
	h.setContentSecurityPolicy(w, r, site, nil, "", "", false)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		noCache(w)
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return pathClassUnserved
	}
	prefix := assetPrefixFor(site)
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if prefix == "" || !strings.HasPrefix(r.URL.Path, prefix) || !fs.ValidPath(name) || name == "." || !staticAssetName(name) {
		noCache(w)
		http.NotFound(w, r)
		return pathClassUnserved
	}
	fsys, err := h.opener.Open(site.BundleRef)
	if err != nil {
		noCache(w)
		http.Error(w, "this site is unavailable", http.StatusServiceUnavailable)
		return pathClassUnserved
	}
	info, err := fs.Stat(fsys, name)
	if err != nil || info.IsDir() {
		noCache(w)
		http.NotFound(w, r)
		return pathClassUnserved
	}
	etag, hasETag := assetETagFor(fsys, name, site.BundleRef)
	h.serveFile(w, r, fsys, name, etag, hasETag, filePolicy{immutable: true})
	return pathClassAsset
}

// Documents and extensionless routes never acquire an immutable URL. This
// list also distinguishes missing file requests from client-side routes;
// dotted route values such as /people/jane.doe can still reach the router.
func staticAssetName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs", ".css", ".map", ".json", ".webmanifest", ".xml", ".txt",
		".ico", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg",
		".woff", ".woff2", ".ttf", ".otf", ".eot", ".wasm", ".pdf",
		".mp3", ".mp4", ".webm", ".ogg", ".wav":
		return true
	}
	return false
}

// Astro's short bundler hashes are not verified SHA-256 filenames. Instead,
// qualify their external script/style URLs with the whole bundle's immutable
// version. Relative imports retain the prefix, without rewriting JS or CSS.
// Copy raw tokens except the changed opening tag: inline script bytes must
// remain identical to the CSP hashes computed from the original document.
func versionAssetLinks(data []byte, prefix string) []byte {
	if prefix == "" {
		return data
	}
	var out bytes.Buffer
	z := html.NewTokenizer(bytes.NewReader(data))
	for {
		kind := z.Next()
		raw := append([]byte(nil), z.Raw()...)
		if kind == html.ErrorToken {
			out.Write(raw)
			return out.Bytes()
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			out.Write(raw)
			continue
		}
		token := z.Token()
		attribute := ""
		switch token.Data {
		case "script":
			attribute = "src"
		case "link":
			attribute = "href"
		}
		changed := false
		for i := range token.Attr {
			attr := &token.Attr[i]
			if attribute == "" || attr.Key != attribute || attr.Namespace != "" {
				continue
			}
			u, err := url.Parse(attr.Val)
			if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/_astro/") || !staticAssetName(u.Path) || !fs.ValidPath(strings.TrimPrefix(u.Path, "/")) {
				continue
			}
			u.Path = prefix + strings.TrimPrefix(u.Path, "/")
			u.RawPath = ""
			attr.Val = u.String()
			changed = true
		}
		if changed {
			out.WriteString(token.String())
		} else {
			out.Write(raw)
		}
	}
}
