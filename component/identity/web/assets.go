package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// Asset versioning + cache-control strategy.
//
// Three pieces work together so a deployed change to a stylesheet,
// script, or icon reaches every user without them having to clear
// their browser cache:
//
//  1. Each embedded static asset gets an 8-char content hash computed
//     once at boot. The map { "/static/identity.css" -> "a1b2c3d4" }
//     lives on Server.assetVersions.
//
//  2. Templates reference assets via {{asset "/static/identity.css"}}
//     which expands to "/static/identity.css?v=a1b2c3d4". A new
//     deploy with a changed stylesheet produces a new hash, a new
//     URL, and the browser is FORCED to fetch the new bytes —
//     there's no way around it because the old cached entry is at
//     a different URL.
//
//  3. Static responses set
//     `Cache-Control: public, max-age=31536000, immutable`
//     so browsers cache the assets aggressively (a year), but the
//     hash-bearing URL means a content change is a different URL
//     entirely. Best of both worlds: no perf hit, no stale code.
//
// HTML responses get
//     `Cache-Control: no-store, must-revalidate`
//     so the browser ALWAYS re-fetches the document and sees the new
//     versioned asset URLs from #2. HTML is small; the round-trip
//     cost is negligible.
//
// Together: a deploy that changes ANY static file is reflected on
// every user's next page load, no matter how aggressively their
// browser caches.

// computeAssetVersions walks the embed FS once at boot and produces
// a path -> short-hash map for every file under the static/ subtree.
// Keys match the URL path the templates use (`/static/<basename>`).
//
// Hash collisions at 8 hex chars are statistically irrelevant for
// the dozen-or-so static files this app ships. If we ever ship
// hundreds, widen to 12.
func computeAssetVersions(fsys fs.FS, dir string) (map[string]string, error) {
	out := make(map[string]string)
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("hash %q: %w", p, err)
		}
		sum := sha256.Sum256(body)
		urlPath := "/" + p // "static/identity.css" -> "/static/identity.css"
		out[urlPath] = hex.EncodeToString(sum[:4])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("identity/web: scan static assets: %w", err)
	}
	return out, nil
}

// assetURL returns the versioned URL for a static path. Falls back
// to the bare path when no hash is known (template author typo or
// asset added after boot — neither should happen at runtime, but
// returning the bare path is harmless).
func (s *Server) assetURL(path string) string {
	if s == nil {
		return path
	}
	return appendVersion(path, s.assetVersions[path])
}

// AssetURL is the package-level equivalent of Server.assetURL,
// suitable for sibling packages (admin/, etc.) that render templates
// against the same embed.FS but don't hold a *web.Server instance.
// The version map is computed lazily on first use and cached.
//
// Returns the bare path when no hash is known.
func AssetURL(path string) string {
	v := defaultAssetVersion(path)
	return appendVersion(path, v)
}

func appendVersion(path, v string) string {
	if v == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&v=" + v
	}
	return path + "?v=" + v
}

var (
	defaultAssetVersionsOnce sync.Once
	defaultAssetVersionsMap  map[string]string
)

func defaultAssetVersion(path string) string {
	defaultAssetVersionsOnce.Do(func() {
		v, err := computeAssetVersions(FS, "static")
		if err == nil {
			defaultAssetVersionsMap = v
		}
	})
	if defaultAssetVersionsMap == nil {
		return ""
	}
	return defaultAssetVersionsMap[path]
}

// staticCacheHeaders wraps a static-file handler so every response
// carries the long-cache + immutable directive. Safe because the
// hash in the URL means a content change is a different URL —
// browsers cache the bytes for the full year and never serve stale
// content.
func staticCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// noStoreHTMLHeaders sets the headers that prevent any caching of
// rendered HTML pages. Browsers must re-fetch on every navigation,
// which is the lever that makes versioned asset URLs work — the
// HTML always carries the latest hashes.
func noStoreHTMLHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
}
