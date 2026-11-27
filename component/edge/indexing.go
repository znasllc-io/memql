package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const bundleIndexingFile = "memql-indexing.json"
const bundleIndexingMaxBytes = 4096
const bundleNoIndex = "noindex, nofollow, noarchive"

// A bundle may opt into a closed indexing policy. This is crawler advice,
// never access control, a redirect, or a configurable response-header API.
// Missing metadata preserves legacy sites; a declared but unreadable or invalid
// policy refuses indexing. Only resolved HTML uses it, including SPA fallbacks
// and conditional GET/HEAD responses, before serveFile can return a 304.
func (h *Handler) setBundleIndexing(w http.ResponseWriter, r *http.Request, site *Site, fsys fs.FS, name, etag string) string {
	if !isDocumentName(name) {
		return etag
	}
	data := h.bundleIndexingMetadata(site, fsys)
	if len(data) == 1 && data[0] == 0 {
		return etag
	} // positively absent
	// A policy-only edit must invalidate an old HTML response: a 304 which
	// omits X-Robots-Tag would otherwise retain its cached prior noindex.
	if etag != "" {
		etag = strongETag("indexing-v1", etag, string(data))
	}
	if len(data) > 1 && data[0] == 2 {
		if policy, ok := parseBundleIndexing(data[1:]); ok && policy.allows(r) {
			return etag
		}
	}
	// Adding our refusal preserves existing bot-specific and preview restrictions.
	// In particular, canonical permission never removes a previous noindex header.
	w.Header().Add("X-Robots-Tag", bundleNoIndex)
	return etag
}

type bundleIndexingPolicy struct {
	authority string
	paths     []string
}

func (p bundleIndexingPolicy) allows(r *http.Request) bool {
	if p.authority == "" || r == nil || r.URL == nil || strings.ToLower(r.Host) != p.authority ||
		r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		return false
	}
	for _, publicPath := range p.paths {
		if r.URL.Path == publicPath {
			return true
		}
	}
	return false
}

// Reuse the bounded byte LRU and singleflight with a separate key namespace.
// Only the publisher's content-addressed layout and a no-read validator may
// cache metadata (including true absence). A mutable file:// tree is reread,
// bounded, on every HTML request; host/path/query decisions are NEVER cached.
// Transient I/O failures refuse this response but are not cached as absence.
func (h *Handler) bundleIndexingMetadata(site *Site, fsys fs.FS) []byte {
	key := ""
	if site != nil && publishedBundleRef.MatchString(site.BundleRef) {
		if validator, ok := fsys.(assetValidator); ok {
			if etag, valid := validator.ETagFor(bundleIndexingFile); valid {
				key = "indexing-v1:" + site.BundleRef + ":" + etag
			}
		}
	}
	if key == "" {
		data, _ := readBundleIndexingMetadata(fsys)
		return data
	}
	if data, hit := h.hashCache.Get(key); hit {
		return data
	}
	value, _, _ := h.hashSF.Do(key, func() (any, error) {
		if data, hit := h.hashCache.Get(key); hit {
			return data, nil
		}
		data, cacheable := readBundleIndexingMetadata(fsys)
		if cacheable {
			h.hashCache.Put(key, data)
		}
		return data, nil
	})
	return value.([]byte)
}

func readBundleIndexingMetadata(fsys fs.FS) ([]byte, bool) {
	denied := []byte{1}
	if fsys == nil {
		return denied, false
	}
	var data []byte
	var err error
	if bounded, ok := fsys.(interface{ indexingMetadata() ([]byte, error) }); ok {
		data, err = bounded.indexingMetadata()
		if errors.Is(err, fs.ErrNotExist) {
			return []byte{0}, true
		}
		if err != nil {
			return denied, false
		}
	} else {
		f, openErr := fsys.Open(bundleIndexingFile)
		if errors.Is(openErr, fs.ErrNotExist) {
			return []byte{0}, true
		}
		if openErr != nil {
			return denied, false
		}
		data, err = io.ReadAll(io.LimitReader(f, bundleIndexingMaxBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return denied, false
		}
	}
	if len(data) > bundleIndexingMaxBytes {
		return denied, true
	}
	policy, ok := parseBundleIndexing(data)
	if !ok || policy.authority == "" {
		return denied, true
	}
	return append([]byte{2}, data...), true
}

// Decode each top-level field exactly once; DisallowUnknownFields alone would
// silently accept duplicate keys. The size bound applies before JSON parsing.
func parseBundleIndexing(data []byte) (bundleIndexingPolicy, bool) {
	var empty bundleIndexingPolicy
	if len(data) > bundleIndexingMaxBytes || !utf8.Valid(data) {
		return empty, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return empty, false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return empty, false
		}
		key, ok := token.(string)
		if !ok || fields[key] != nil {
			return empty, false
		}
		switch key {
		case "version", "mode", "canonicalOrigin", "publicPaths":
		default:
			return empty, false
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return empty, false
		}
		fields[key] = value
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return empty, false
	}
	if dec.Decode(new(any)) != io.EOF {
		return empty, false
	}
	var version int
	var mode string
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 || json.Unmarshal(fields["mode"], &mode) != nil {
		return empty, false
	}
	if mode == "disabled" {
		return empty, len(fields) == 2
	}
	if mode != "canonical" || len(fields) != 4 {
		return empty, false
	}
	var origin string
	var paths []string
	if json.Unmarshal(fields["canonicalOrigin"], &origin) != nil || json.Unmarshal(fields["publicPaths"], &paths) != nil || len(paths) == 0 || len(paths) > 32 {
		return empty, false
	}
	authority, ok := indexingAuthority(origin)
	if !ok {
		return empty, false
	}
	seen := map[string]bool{}
	for _, value := range paths {
		if !indexingPath(value) || seen[value] {
			return empty, false
		}
		seen[value] = true
	}
	return bundleIndexingPolicy{authority: authority, paths: paths}, true
}

func indexingAuthority(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || origin != "https://"+u.Host || u.Host != strings.ToLower(u.Host) || !validHost(u.Host) {
		return "", false
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(host, ".") {
		return "", false
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if strings.ContainsAny(host, ":[]") {
			return "", false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", false
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || n == 443 || strconv.Itoa(n) != port {
			return "", false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	return u.Host, true
}

func indexingPath(value string) bool {
	if len(value) == 0 || len(value) > 1024 || value[0] != '/' || path.Clean(value) != value || strings.ContainsAny(value, "\\%?#") {
		return false
	}
	u, err := url.ParseRequestURI(value)
	return err == nil && u.Path == value && u.EscapedPath() == value && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery
}
