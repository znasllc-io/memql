package portal

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// DefaultDistDir is where the image build puts the compiled bundle (see the
// Dockerfile's portal stage). Overridable with MEMQL_PORTAL_DIST, which is
// how a developer points a running binary at clients/portal/dist without
// rebuilding an image.
const DefaultDistDir = "/app/portal"

// DistDirEnvVar names the override. Registered in scripts/secrets/manifest.yaml
// -- cmd/envscan fails the build on an env var read by code and absent there.
const DistDirEnvVar = "MEMQL_PORTAL_DIST"

// indexFile is the SPA entry document, and the fallback for every path the
// bundle routes client-side.
const indexFile = "index.html"

// hashedAssetPrefix is the directory Vite emits content-hashed files into
// (vite.config.ts pins `assetsDir: "assets"`). Everything beneath it is
// immutable by construction: a changed file gets a changed hash and therefore
// a changed URL, so the browser can never be handed stale bytes under a URL
// it already has. Everything OUTSIDE it -- index.html above all -- is served
// no-cache, because its URL is stable while its content changes on every
// deploy, and it is what carries the new hashed URLs.
const hashedAssetPrefix = "assets/"

// unavailableMessage is what a request gets when there is no bundle on disk.
// It names the env var and the make target rather than saying "not found",
// because the realistic reader is an operator who just deployed and is
// looking at a 404 with no idea whether the cause is routing or packaging.
const unavailableMessage = "memQL portal assets are not installed on this node. " +
	"Build them with `make portal-build` and point " + DistDirEnvVar +
	" at clients/portal/dist, or deploy an image whose portal stage was built."

// DistDirFromEnv resolves the directory to serve from.
func DistDirFromEnv() string {
	if dir := strings.TrimSpace(os.Getenv(DistDirEnvVar)); dir != "" {
		return dir
	}
	return DefaultDistDir
}

// Options configures a Handler.
type Options struct {
	// Dir is the directory holding the built bundle. Empty means
	// DistDirFromEnv().
	Dir string
	// FS overrides Dir entirely. Used by tests; production always goes
	// through Dir.
	FS fs.FS
	// Logger receives the one boot-time warning when no bundle is present.
	// Nil is tolerated (slog.Default is used) so a caller cannot make the
	// handler panic by forgetting it.
	Logger *slog.Logger
}

// Handler serves the portal bundle. The mount prefix is NOT its concern: the
// caller wraps it in http.StripPrefix (see app/transport_portal.go), so every
// path reaching ServeHTTP is already relative to the mount. That keeps one
// piece of knowledge -- where the portal lives -- in one place, and it is
// what lets the same handler serve a deployment that carries a
// SERVER_PUBLIC_PATH prefix without knowing the prefix exists.
type Handler struct {
	fsys   fs.FS
	logger *slog.Logger
	// available is decided once, at construction. A node with no bundle
	// answers 404 with unavailableMessage instead of failing to boot: the
	// portal is one surface on a node that serves many, and refusing to start
	// the bff because a UI is missing would turn a cosmetic packaging mistake
	// into an outage.
	available bool
}

var _ http.Handler = (*Handler)(nil)

// New builds a Handler and reports, once, whether it found a bundle.
func New(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	fsys := opts.FS
	dir := opts.Dir
	if fsys == nil {
		if dir == "" {
			dir = DistDirFromEnv()
		}
		// os.DirFS is rooted: a path that would escape the directory is
		// refused by the filesystem itself, which is the backstop behind the
		// fs.ValidPath check in assetName.
		fsys = os.DirFS(dir)
	}

	h := &Handler{fsys: fsys, logger: logger, available: indexPresent(fsys)}
	if !h.available {
		// LOUD, and exactly once. A per-request log would bury the cause in
		// noise the moment a browser retries; a silent 404 leaves an operator
		// guessing between ingress, routing and packaging.
		logger.Warn("memQL portal assets not found -- /portal will answer 404",
			"dir", dir, "env", DistDirEnvVar, "expected", indexFile)
	} else {
		logger.Info("memQL portal assets mounted", "dir", dir)
	}
	return h
}

// Available reports whether a bundle was found at construction. Exposed for
// the boot log and for tests; the handler answers correctly either way.
func (h *Handler) Available() bool { return h != nil && h.available }

func indexPresent(fsys fs.FS) bool {
	info, err := fs.Stat(fsys, indexFile)
	return err == nil && !info.IsDir()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w, r)
	w.Header().Set("Content-Security-Policy", policyFor(r))

	if !h.available {
		noCache(w)
		http.Error(w, unavailableMessage, http.StatusNotFound)
		return
	}

	name, ok := assetName(r.URL.Path)
	if !ok {
		noCache(w)
		http.Error(w, "invalid portal asset path", http.StatusBadRequest)
		return
	}

	if name != "" && h.serveFile(w, r, name) {
		return
	}

	// A MISSING ASSET IS A 404, NOT THE FALLBACK. Everything under assets/ is
	// a build artefact the bundle asks for by exact hashed name; if it is not
	// there, handing back index.html means the browser receives HTML with a
	// script or stylesheet content type and reports "expected a JavaScript
	// module script but the server responded with text/html" -- a message
	// about MIME types, for a problem that is a missing file. The realistic
	// cause is a cached index.html from a previous deploy pointing at assets
	// this one does not have, and 404 is what tells the browser (and the
	// operator reading the network tab) exactly that.
	if strings.HasPrefix(name, hashedAssetPrefix) {
		noCache(w)
		http.Error(w, "portal asset not found", http.StatusNotFound)
		return
	}

	// SPA FALLBACK. Any other path that is not a file on disk gets index.html, so a
	// hard refresh (or a pasted link, or a bookmark) on a client-side route
	// like /portal/concepts/v1:cluster:node loads the application instead of
	// 404ing. The router then resolves the path and, if it recognises
	// nothing, renders its own not-found page -- the server cannot make that
	// distinction, because only the bundle knows its route table.
	h.serveIndex(w, r)
}

// serveFile serves name if it exists as a regular file, and reports whether
// it did. A directory is treated as absent: http.ServeFileFS would otherwise
// emit a directory listing of the deployed bundle.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	info, err := fs.Stat(h.fsys, name)
	if err != nil || info.IsDir() {
		return false
	}
	if strings.HasPrefix(name, hashedAssetPrefix) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		noCache(w)
	}
	http.ServeFileFS(w, r, h.fsys, name)
	return true
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	noCache(w)
	http.ServeFileFS(w, r, h.fsys, indexFile)
}

// noCache marks a response as revalidate-always. Deliberately "no-cache"
// rather than "no-store": the document must be re-validated on every
// navigation (it carries the hashed asset URLs, so a stale copy pins the
// browser to a previous deploy's bundle), but a 304 is still a perfectly good
// answer and costs no bytes.
func noCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
}

// assetName turns a mount-relative request path into an fs.FS name.
//
// The second return is false for a path that must not be served at all. The
// check is fs.ValidPath, which rejects any "." or ".." element, a leading or
// trailing slash, and an empty element -- so "../../etc/passwd" and
// "assets/../../secret" are refused HERE, before touching the filesystem,
// rather than relying on os.DirFS's rooting alone. Two independent refusals
// for the same escape is deliberate: this one also covers an fs.FS supplied
// by a caller that is not rooted at all.
//
// An empty name (the mount root, "/portal/") is VALID and means "no specific
// file" -- the caller falls through to index.html.
func assetName(urlPath string) (string, bool) {
	trimmed := strings.TrimPrefix(urlPath, "/")
	if trimmed == "" {
		return "", true
	}
	if !fs.ValidPath(trimmed) {
		return "", false
	}
	return trimmed, true
}
