package packages

import (
	"strings"
	"testing"
)

// parseGitHubRepo is the one place a repository URL is read, and it is read
// by three callers -- the fetcher, the poll and the probe -- so the code it
// refuses a host with is the code all three answer. A non-GitHub host is a
// SOURCE-FORM refusal (source_host_unsupported: the repair is the other source
// form, a zip of the same tree) and not an unreadable source, which is what it
// was filed as before epic memql#4885 gave the condition a code of its own.
func TestParseGitHubRepoRefusesAnotherHostByName(t *testing.T) {
	for _, raw := range []string{
		"https://gitlab.com/acme/widget",
		"https://bitbucket.org/acme/widget",
		"https://github.com.evil.example/acme/widget",
	} {
		_, _, err := parseGitHubRepo(raw)
		if got := RefusalCode(err); got != CodeSourceHostUnsupported {
			t.Errorf("%q: want %s, got %s (%v)", raw, CodeSourceHostUnsupported, got, err)
			continue
		}
		// The sentence names the two ways forward, in the words the
		// credential host check already uses, so the Source stop renders one
		// repair for one condition however it was reached.
		if !strings.Contains(err.Error(), "only github.com today, or upload a zip") {
			t.Errorf("%q: the refusal must name the two ways forward, got: %v", raw, err)
		}
	}

	// The reachable positive, and the boundary of the code: the two
	// spellings GitHub answers on parse, and a URL that is not a URL at all
	// (or names no repository) is still source_unreadable -- there is no
	// host to be unsupported.
	for _, raw := range []string{"https://github.com/acme/widget", "https://www.github.com/acme/widget.git"} {
		owner, repo, err := parseGitHubRepo(raw)
		if err != nil || owner != "acme" || repo != "widget" {
			t.Errorf("%q: got %q/%q, %v", raw, owner, repo, err)
		}
	}
	for _, raw := range []string{"", "https://github.com/acme", "::not a url"} {
		if _, _, err := parseGitHubRepo(raw); RefusalCode(err) != CodeSourceUnreadable {
			t.Errorf("%q: want %s, got %v", raw, CodeSourceUnreadable, err)
		}
	}
}
