package pipelines

import (
	"strings"
	"testing"
)

func TestValidBranchName(t *testing.T) {
	for _, name := range []string{"main", "trunk", "feature/login", "epic/pipelines-local-green", "fix_1.2", "release-0.25", "a/b/c", "UPPER", "dependabot/go_modules/x-1.2.3"} {
		if !ValidBranchName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", " main", "main ", "a b", "tab\there", "-main", "/main", "main/", "main.", "a..b", "a//b", "a@{1}", "@", "main~1", "main^", "a:b", "a?b", "a*b", "a[b", `a\b`, "refs/heads/main", ".hidden", "a/.b", "a.lock", "a/b.lock", strings.Repeat("a", 256)} {
		if ValidBranchName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
