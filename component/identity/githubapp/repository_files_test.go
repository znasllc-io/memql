package githubapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRepositoryFilesReadsOnlyApprovedImmutableText(t *testing.T) {
	now := time.Now()
	blob := strings.Repeat("b", 40)
	h := newHub().json("/repos/acme/widget/git/trees/"+headSHA, 200, fmt.Sprintf(`{"tree":[{"path":"src/a.go","mode":"100644","type":"blob","sha":%q},{"path":"secret.txt","mode":"100644","type":"blob","sha":%q}]}`, blob, strings.Repeat("c", 40))).json("/repos/acme/widget/git/blobs/"+blob, 200, `{"encoding":"base64","size":4,"content":"`+base64.StdEncoding.EncodeToString([]byte("code"))+`"}`)
	files, err := testClient(t, h, &now).RepositoryFiles(context.Background(), installToken, "acme", "widget", headSHA, []string{"src/", "new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "new.txt" || files[0].BeforeSHA256 != "" || files[1].Path != "src/a.go" || files[1].Content != "code" || len(files[1].BeforeSHA256) != 64 {
		t.Fatalf("unexpected files: %+v", files)
	}
	if len(h.seen()) != 2 {
		t.Fatal("read outside the requested paths")
	}
}

func TestRepositoryFilesRefusesIncompleteOrUnsafeSelections(t *testing.T) {
	for _, tree := range []string{`{"truncated":true}`, `{"tree":[{"path":"src/a","type":"blob","mode":"120000","sha":"` + headSHA + `"}]}`, `{"tree":[{"path":"src/module","type":"commit","mode":"160000","sha":"` + headSHA + `"}]}`, `{"tree":[]}`} {
		now := time.Now()
		h := newHub().json("/repos/acme/widget/git/trees/"+headSHA, 200, tree)
		if _, err := testClient(t, h, &now).RepositoryFiles(context.Background(), installToken, "acme", "widget", headSHA, []string{"src/"}); err == nil {
			t.Fatal("unsafe source selection accepted", tree)
		}
	}
	for _, p := range []string{"../x", "/etc/passwd", ".git/config", "src/../x", "src\\x", ""} {
		now := time.Now()
		h := newHub()
		if _, err := testClient(t, h, &now).RepositoryFiles(context.Background(), installToken, "acme", "widget", headSHA, []string{p}); err == nil || len(h.seen()) != 0 {
			t.Fatal("invalid selection reached provider", p)
		}
	}
}
