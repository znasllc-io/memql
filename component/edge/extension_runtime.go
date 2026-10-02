package edge

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var extensionRoot = regexp.MustCompile(`^/[a-z0-9][a-z0-9/_-]*/$`)
var extensionResourceLabel = regexp.MustCompile(`^([a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?)--assets--[a-z0-9-]{20,55}$`)

func extensionRuntimePath(site *Site) string {
	if site == nil || !extensionRoot.MatchString(site.ExtensionRuntimePath) {
		return ""
	}
	return site.ExtensionRuntimePath
}

// One DNS label keeps the origin under the installation's existing wildcard
// certificate. Only a site's explicit runtime opt-in makes an alias resolve.
func extensionResourceParent(host string) string {
	label, domain, ok := strings.Cut(host, ".")
	if !ok || len(label) > 63 {
		return ""
	}
	match := extensionResourceLabel.FindStringSubmatch(label)
	if match == nil {
		return ""
	}
	return match[1] + "." + domain
}

func extensionRuntimePolicy(w http.ResponseWriter, r *http.Request, site *Site, base string) (string, bool) {
	root := extensionRuntimePath(site)
	if root == "" {
		return "", false
	}
	// Resolve to a clean path before classifying: encoded traversals must never
	// turn a renderer-origin response into a top-level app or credential route.
	requestPath := path.Clean(r.URL.Path)
	if requestPath != strings.TrimSuffix(root, "/") && !strings.HasPrefix(requestPath, root) {
		return "", false
	}
	if site.ResourceParentHost != "" {
		w.Header().Del("X-Frame-Options")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Del("Access-Control-Allow-Credentials")
		// Upstream webview documents supply their own script CSP. Their fake.html
		// frame is populated with an extension's nonce CSP after loading. Applying
		// the app's script hashes to it would block every custom editor. This origin
		// has no app storage or APIs and only serves this public asset subtree.
		return "frame-ancestors https://" + site.ResourceParentHost + " 'self'; base-uri 'self'; form-action 'none'", true
	}
	if strings.HasPrefix(requestPath, root+"assets/") || strings.HasPrefix(requestPath, root+"extensions/") {
		if origin, err := url.Parse(r.Header.Get("Origin")); err == nil && origin.Scheme == "https" && origin.User == nil && origin.Port() == "" && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" && extensionResourceParent(origin.Host) == site.Hostname {
			w.Header().Set("Access-Control-Allow-Origin", origin.String())
			w.Header().Add("Vary", "Origin")
		}
	}
	_, domain, ok := strings.Cut(site.Hostname, ".")
	if !ok || !validHost(domain) {
		return "", false
	}
	// The application can embed only its own worker and cluster-local isolated
	// renderers. Other hosted sites retain their frame-ancestors refusal.
	policy := strings.Replace(base, "script-src 'self'", "script-src 'self' 'wasm-unsafe-eval'", 1)
	policy += "; worker-src 'self' blob:; frame-src 'self' https://*." + domain
	policy = strings.Replace(policy, "connect-src 'self'", "connect-src 'self' https://api."+domain+" wss://api."+domain, 1)
	// Stable build output for the extension host only. CommonJS browser VSIX
	// bundles need evaluation inside its worker; the application does not.
	if requestPath == root+"assets/extension-host.html" {
		policy = strings.Replace(policy, "script-src 'self'", "script-src 'self' 'unsafe-eval' blob:", 1)
		policy = strings.Replace(policy, "frame-ancestors 'none'", "frame-ancestors 'self'", 1)
		w.Header().Del("X-Frame-Options")
	}
	return policy, true
}
