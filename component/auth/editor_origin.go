package auth

import (
	"github.com/znasllc-io/memql/component/frontdoor"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// VS Code for the Web runs its extension worker on an isolated Microsoft CDN
// origin. This is permission to use bearer/device-grant routes, never permission
// to read cookie-authenticated identity pages. The browser and desktop clients
// still authenticate as the same registered MemQL editor client.
var editorWorkerHost = regexp.MustCompile(`^v--[a-z0-9]{20,80}\.vscode-cdn\.net$`)

func IsWebEditorOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain != "" && u.Host == frontdoor.VSCodeHost(domain) {
		return true
	}
	switch u.Host {
	case "vscode.dev", "insiders.vscode.dev", "github.dev":
		return true
	}
	return editorWorkerHost.MatchString(u.Host)
}

func IsEditorIdentityPath(path string) bool {
	return path == "/device/code" || path == "/oauth/token" || path == "/auth/logout"
}
