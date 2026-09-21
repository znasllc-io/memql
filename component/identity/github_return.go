package identity

import (
	"net/url"
	"strings"
)

// github_return.go -- where a GitHub round trip sends the browser back.
//
// Two flows end this way and they end on two different surfaces of this
// service: GitHub Connect's callback (component/identity/http) and the page
// that starts a GitHub App registration (component/identity/web), which sends
// somebody back when the state they arrived with is not good. Both land in
// MemQL OS carrying ONE marker the OS reads in ONE place
// (clients/os/src/apps/deployables/sources/connectReturn.ts), so the URL is
// composed once, here, rather than once per package that needs it.

// GithubResultParam is the query key MemQL OS reads a GitHub round trip's
// outcome from. Wire contract with connectReturn.ts's CONNECT_RESULT_PARAM.
const GithubResultParam = "github"

// GithubReturnURL composes the OS URL for one outcome.
//
// `osOrigin` is the shell's origin (ShellHomeURL, or "" when it cannot be
// named). `returnPath` is CLIENT-SUPPLIED and is re-validated here however
// many times it was validated before: a value that was safe when it was stored
// is not evidence it is safe when it is used, which is TakePostLoginRedirect's
// argument and the reason this does not trust its caller to have done it.
//
// With no nameable origin the result is a same-origin path. This service does
// not serve the OS, so the person lands on a 404 -- but a redirect to an origin
// this cluster cannot name would be a redirect to whatever the empty string
// composes into.
func GithubReturnURL(osOrigin, returnPath, result string) string {
	path := SafeRelativeRedirect(returnPath)
	if path == "" {
		path = "/"
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	marked := path + sep + GithubResultParam + "=" + url.QueryEscape(result)
	if base := strings.TrimRight(strings.TrimSpace(osOrigin), "/"); base != "" {
		return base + marked
	}
	return marked
}
