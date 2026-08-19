package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestHistoricalDocsCarryBanner pins DOCS_STANDARD.md section 4: a design
// doc that ships flips to `status: historical` AND gets a one-line banner
// (`> Historical: shipped in X.Y.Z; kept for rationale.`).
//
// The status and the banner are two halves of one convention and they had
// drifted apart -- 41 docs under docs/internal/ carried the front-matter
// status with no banner anywhere in the body (memql#4125). The status is
// what makes a doc exempt from the retired-vocabulary sweep, so a doc can
// acquire that exemption while still READING as current guidance to a
// human. The banner is the half a human sees.
func TestHistoricalDocsCarryBanner(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--", "docs/**.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	statusRe := regexp.MustCompile(`(?m)^status:\s*(\S+)\s*$`)
	bannerRe := regexp.MustCompile(`(?m)^> Historical:`)

	var historical, missing int
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		content := string(data)
		if !strings.HasPrefix(content, "---\n") {
			continue
		}
		end := strings.Index(content[4:], "\n---\n")
		if end == -1 {
			continue
		}
		frontMatter := content[4 : 4+end]
		m := statusRe.FindStringSubmatch(frontMatter)
		if m == nil || m[1] != "historical" {
			continue
		}
		historical++
		if !bannerRe.MatchString(content[4+end:]) {
			missing++
			t.Errorf("%s: front matter says `status: historical` but the body carries no "+
				"`> Historical: ...` banner. DOCS_STANDARD.md section 4 pairs the two; add\n"+
				"  > Historical: shipped in X.Y.Z; kept for rationale.\n"+
				"immediately after the H1.", f)
		}
	}

	if historical == 0 {
		t.Fatal("found 0 docs with `status: historical` -- this gate examined nothing. " +
			"Either the front-matter key was renamed or the ls-files pattern stopped matching.")
	}
	t.Logf("checked %d historical doc(s), %d missing a banner", historical, missing)
}
