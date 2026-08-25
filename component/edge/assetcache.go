// component/edge/assetcache.go
package edge

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ASSET VALIDATORS AND THE BUNDLE BYTE CACHE (memql#4545).
//
// The edge's header policy was already right -- index.html no-cache,
// hashed assets immutable -- and the machinery under it was missing
// entirely:
//
//   - Every blob:// asset was a FRESH download per request per file. A busy
//     site was a per-visitor blob storm for bytes that are immutable by
//     construction.
//   - Nothing set an ETag, and the blob path's ModTime was the zero time, so
//     http.ServeContent could not emit a validator and a 304 was
//     IMPOSSIBLE. The `no-cache` index.html re-transferred in full on every
//     load -- which is the request every returning visitor makes.
//
// # Validators come from the PATH, not from the bytes
//
// A published bundle lands under a CONTENT-ADDRESSED version prefix: the
// publisher derives it from a sha256 over the whole bundle's names and
// bytes (publish.go's version()), so `blob://sites/shop/vab12cd34ef56/` is
// a statement about content. (prefix, path) therefore identifies bytes
// uniquely, and the validator is a hash of two short strings rather than of
// the file -- which is what makes an ETag affordable on a path that has not
// read the file yet, and is what lets a 304 answer with ZERO downloads.
//
// The file:// path has no content hash: it is the tree the image shipped,
// or a working directory during the dev inner loop. There the validator
// includes the file's size and modification time, which the filesystem
// already knows and a Stat already returns. That is a strong validator for
// this purpose -- a rebuild moves mtime -- and it costs no read.
//
// # No invalidation, by construction
//
// The byte cache is keyed by (version prefix, path) and a republish changes
// the prefix (the bundleRef flip is atomic, memql#3713). So a new version
// is a new key, old entries age out by LRU, and there is no purge path to
// get wrong. This is the same property that makes a CDN safe in front of
// the edge; see the CDN posture section in site-hosting.md.

// bundleCacheMBEnv sizes the in-memory byte cache.
const bundleCacheMBEnv = "MEMQL_EDGE_BUNDLE_CACHE_MB"

// defaultBundleCacheMB is deliberately modest. The edge runs beside every
// other node type's memory budget, the cache buys latency rather than
// correctness, and a cache that pushes a replica into an OOM kill is worse
// than no cache at all. An operator serving one large hot bundle raises it.
const defaultBundleCacheMB = 64

// bundleCacheBytes resolves the configured cap.
//
// A zero or negative value DISABLES the cache rather than meaning
// "unlimited", which is the safer reading of an operator typing a number
// they did not think hard about: the worst outcome of disabling it is the
// pre-memql#4545 behaviour, and the worst outcome of unlimited is a replica
// that dies.
func bundleCacheBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(bundleCacheMBEnv))
	if raw == "" {
		return defaultBundleCacheMB << 20
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb <= 0 {
		return 0
	}
	return mb << 20
}

// assetValidator is implemented by a bundle filesystem that can name a
// file's validator WITHOUT reading it.
//
// The optional-interface shape is what keeps the 304 free of I/O on the
// path where I/O is expensive. blobFS implements it from its
// content-addressed prefix; os.DirFS (the file:// path) does not, and the
// handler falls back to a Stat there -- which is the right trade, because a
// Stat on local disk is not what anyone is trying to avoid.
type assetValidator interface {
	// ETagFor returns a strong ETag for name, including the surrounding
	// quotes, and whether one could be derived at all.
	ETagFor(name string) (string, bool)
}

// strongETag renders the quoted, strong (never W/) entity tag for a set of
// identity components.
//
// Strong rather than weak because the ETag identifies EXACT bytes: a weak
// validator would forbid range requests to be satisfied from it, and range
// requests are exactly what http.ServeContent exists to serve.
func strongETag(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		// The NUL separator matters: without it ("ab","c") and ("a","bc")
		// hash identically, which for (prefix, path) means two different
		// files in two different bundles could share a validator and a
		// visitor could be served the wrong cached bytes.
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return `"` + hex.EncodeToString(h.Sum(nil))[:32] + `"`
}

// statETag builds a validator from what a Stat already returned. Used for
// the file:// path, which has no content-addressed prefix.
func statETag(bundleRef, name string, info fs.FileInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	mod := info.ModTime()
	if mod.IsZero() {
		// A zero ModTime carries no information, so the validator would say
		// "these bytes are the same" for two different builds at the same
		// path and size. Refusing to emit one is the honest answer -- the
		// response is then simply un-validated, exactly as before.
		return "", false
	}
	return strongETag(bundleRef, name, strconv.FormatInt(info.Size(), 10),
		strconv.FormatInt(mod.UTC().UnixNano(), 10)), true
}

// etagMatches reports whether an If-None-Match header selects the given
// entity tag.
//
// `*` matches anything that exists (RFC 9110). Otherwise the header is a
// comma-separated list of tags, and a leading `W/` is stripped before
// comparison because a client MAY weaken a tag it received -- refusing the
// weakened form would simply never 304 for those clients.
func etagMatches(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "" || tag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == tag {
			return true
		}
	}
	return false
}

// ---- the byte cache ----------------------------------------------------

// bundleCache is a size-bounded LRU over immutable bundle bytes.
//
// Small and hand-written rather than a dependency: the whole contract is
// get / put / evict-by-total-bytes, the entries are immutable so there is
// no update path, and there is no invalidation because a republish changes
// the key. A general-purpose cache library would bring TTLs, refresh
// policies and invalidation hooks that this cache must NOT have -- every
// one of them would be a way to serve the wrong version.
type bundleCache struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	items    map[string]*list.Element
	order    *list.List // front = most recently used
}

type cacheEntry struct {
	key  string
	data []byte
}

func newBundleCache(maxBytes int64) *bundleCache {
	return &bundleCache{
		maxBytes: maxBytes,
		items:    map[string]*list.Element{},
		order:    list.New(),
	}
}

// Get returns the cached bytes for key, if present.
//
// The returned slice is the CACHED one, not a copy. Every caller reads it
// and hands it to a response writer; nothing in this package mutates a
// bundle's bytes, and copying every hit would give back most of what the
// cache buys.
func (c *bundleCache) Get(key string) ([]byte, bool) {
	if c == nil || c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

// Put stores bytes under key, evicting least-recently-used entries until
// the total fits.
//
// An entry larger than the whole cache is NOT stored and does not evict
// anything: admitting it would flush every other entry to hold one item
// that the next request for anything else immediately evicts.
func (c *bundleCache) Put(key string, data []byte) {
	if c == nil || c.maxBytes <= 0 || int64(len(data)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.used -= int64(len(el.Value.(*cacheEntry).data))
		c.order.Remove(el)
		delete(c.items, key)
	}
	el := c.order.PushFront(&cacheEntry{key: key, data: data})
	c.items[key] = el
	c.used += int64(len(data))
	for c.used > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		c.order.Remove(oldest)
		delete(c.items, entry.key)
		c.used -= int64(len(entry.data))
	}
}

// stats reports the cache's occupancy. For tests and for a future metric;
// deliberately not exported.
func (c *bundleCache) stats() (entries int, used int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}

// assetETagFor resolves a file's validator through whichever mechanism the
// bundle filesystem supports.
//
// The two-rung ladder is what keeps this honest across both bundle
// schemes. A blob bundle answers from its content-addressed prefix with no
// I/O at all; a file bundle has no such prefix, so it pays a Stat and folds
// in size and mtime. Anything that can do neither serves UNVALIDATED --
// which is exactly the behaviour every path had before memql#4545, so an
// unrecognised filesystem degrades to the status quo rather than to a wrong
// answer.
func assetETagFor(fsys fs.FS, name, bundleRef string) (string, bool) {
	if v, ok := fsys.(assetValidator); ok {
		return v.ETagFor(name)
	}
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return "", false
	}
	return statETag(bundleRef, name, info)
}
