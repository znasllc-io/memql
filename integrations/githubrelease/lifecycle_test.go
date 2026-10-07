package githubrelease

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The base fixture already verifies explicit credentials, immutable tag
// resolution, full asset bytes and redirect policy. This adds only the three
// lifecycle mutations and their independent inventory/readback responses.
type lifecycleFixture struct {
	base                                                 *releaseFixture
	metadata                                             Metadata
	marker                                               string
	exists                                               bool
	tagExists                                            bool
	creates, tags, promotes                              int
	loseCreate, losePromote, rejectCreate, rejectPromote bool
}

func lifecycleFixtureFor(t *testing.T) *lifecycleFixture {
	f := &lifecycleFixture{base: fixture(t), metadata: Metadata{Name: "Release 1.2.3", Body: "Reviewed changes."}, marker: "durable-intent"}
	f.base.hook = func(w http.ResponseWriter, r *http.Request) bool {
		f.base.mu.Lock()
		defer f.base.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
			t.Error("missing explicit credentials/version")
			w.WriteHeader(401)
			return true
		}
		path := "/repos/owner/project"
		var request map[string]any
		if r.Method == "POST" || r.Method == "PATCH" {
			if r.URL.Path == path+"/releases/73/assets" {
				return false
			}
			if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid lifecycle request")
				w.WriteHeader(400)
				return true
			}
		}
		response := func() map[string]any {
			body, _ := releaseBody(f.metadata, f.marker)
			return map[string]any{"id": 73, "draft": f.base.draft, "tag_name": f.base.target.Tag, "name": f.metadata.Name, "body": body, "prerelease": f.metadata.Prerelease, "upload_url": f.base.target.UploadOrigin + path + "/releases/73/assets{?name,label}"}
		}
		lose := func() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
			} else {
				_ = conn.Close()
			}
		}
		switch {
		case r.Method == "GET" && r.URL.Path == path+"/git/ref/tags/"+f.base.target.Tag:
			if !f.tagExists {
				w.WriteHeader(404)
				return true
			}
			return false
		case r.Method == "POST" && r.URL.Path == path+"/git/refs":
			f.tags++
			if request["ref"] != "refs/tags/"+f.base.target.Tag || request["sha"] != f.base.target.SourceCommit {
				t.Error("tag intent changed")
			}
			f.tagExists = true
			w.WriteHeader(201)
			return true
		case r.Method == "GET" && r.URL.Path == path+"/releases":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
				t.Error("unbounded releases query")
			}
			entries := []any{}
			if f.exists {
				entries = append(entries, response())
			}
			_ = json.NewEncoder(w).Encode(entries)
			return true
		case r.Method == "POST" && r.URL.Path == path+"/releases":
			f.creates++
			body, _ := releaseBody(f.metadata, f.marker)
			if request["draft"] != true || request["name"] != f.metadata.Name || request["body"] != body || request["tag_name"] != f.base.target.Tag || request["target_commitish"] != f.base.target.SourceCommit || request["make_latest"] != "false" || request["generate_release_notes"] != false {
				t.Error("draft intent changed", request)
			}
			if f.rejectCreate {
				w.WriteHeader(502)
				return true
			}
			f.exists = true
			if f.loseCreate {
				lose()
				return true
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(response())
			return true
		case r.Method == "GET" && r.URL.Path == path+"/releases/73":
			if !f.exists {
				w.WriteHeader(404)
			} else {
				_ = json.NewEncoder(w).Encode(response())
			}
			return true
		case r.Method == "GET" && r.URL.Path == path+"/releases/latest":
			if f.base.draft {
				w.WriteHeader(404)
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 73})
			}
			return true
		case r.Method == "PATCH" && r.URL.Path == path+"/releases/73":
			f.promotes++
			if len(request) != 2 || request["draft"] != false || request["make_latest"] != strconv.FormatBool(f.metadata.Latest) {
				t.Error("promotion changed undeclared metadata")
			}
			if f.rejectPromote {
				w.WriteHeader(502)
				return true
			}
			f.base.draft = false
			if f.losePromote {
				lose()
				return true
			}
			_ = json.NewEncoder(w).Encode(response())
			return true
		}
		return false
	}
	return f
}
func lifecycle(t *testing.T, f *lifecycleFixture) *Lifecycle {
	t.Helper()
	target := f.base.target
	target.ReleaseID = 0
	l, err := NewLifecycle(target)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func (f *lifecycleFixture) inventory() []AssetExpectation {
	return []AssetExpectation{{Name: f.base.target.AssetName, SHA256: expected([]byte("release bytes")).SHA256, Size: int64(len("release bytes"))}}
}
func TestLifecycleCreateRecoverAndPromote(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "lost replies"}[lost], func(t *testing.T) {
			f := lifecycleFixtureFor(t)
			f.loseCreate, f.losePromote = lost, lost
			l := lifecycle(t, f)
			ctx := context.Background()
			if err := l.EnsureTag(ctx); err != nil {
				t.Fatal(err)
			}
			if err := lifecycle(t, f).EnsureTag(ctx); err != nil {
				t.Fatal(err)
			}
			if f.tags != 1 {
				t.Fatal("tag was retried")
			}
			state, err := l.CreateDraft(ctx, f.metadata, f.marker)
			if err != nil || !state.Draft || state.ReleaseID != 73 {
				t.Fatal(state, err)
			}
			state, found, err := lifecycle(t, f).FindDraft(ctx, f.metadata, f.marker)
			if err != nil || !found || !state.Draft || f.creates != 1 {
				t.Fatal(state, found, err, f.creates)
			}
			f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
			state, err = lifecycle(t, f).Promote(ctx, 73, f.metadata, f.marker, f.inventory())
			if err != nil || state.Draft || len(state.Assets) != 1 {
				t.Fatal(state, err)
			}
			state, err = lifecycle(t, f).Observe(ctx, 73, f.metadata, f.marker, f.inventory())
			if err != nil || state.Draft || f.promotes != 1 {
				t.Fatal(state, err, f.promotes)
			}
		})
	}
}
func TestLifecycleUncertainRequiresReadOnlyRecovery(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.tagExists = true
	f.rejectCreate = true
	l := lifecycle(t, f)
	if _, err := l.CreateDraft(context.Background(), f.metadata, f.marker); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	if _, found, err := lifecycle(t, f).FindDraft(context.Background(), f.metadata, f.marker); err != nil || found || f.creates != 1 {
		t.Fatal(found, err, f.creates)
	}
	f.exists = true
	f.rejectPromote = true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	if _, err := l.Promote(context.Background(), 73, f.metadata, f.marker, f.inventory()); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	state, err := lifecycle(t, f).Observe(context.Background(), 73, f.metadata, f.marker, f.inventory())
	if err != nil || !state.Draft || f.promotes != 1 {
		t.Fatal(state, err, f.promotes)
	}
}
func TestLifecycleRefusesDriftBeforePromotion(t *testing.T) {
	for _, mode := range []string{"source", "name", "body", "prerelease", "unexpected asset", "missing asset", "corrupt bytes", "duplicate name", "wrong size", "wrong digest", "missing marker"} {
		t.Run(mode, func(t *testing.T) {
			f := lifecycleFixtureFor(t)
			f.exists, f.tagExists = true, true
			f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
			l := lifecycle(t, f)
			want := f.inventory()
			metadata := f.metadata
			marker := f.marker
			switch mode {
			case "source":
				f.base.commit = strings.Repeat("b", 40)
			case "name":
				metadata.Name = "Other"
			case "body":
				metadata.Body = "Different notes"
			case "prerelease":
				metadata.Prerelease = true
			case "unexpected asset":
				f.base.addAsset("unexpected.zip", nil)
			case "missing asset":
				f.base.assets = nil
			case "corrupt bytes":
				f.base.corrupt = true
			case "duplicate name":
				want = append(want, want[0])
			case "wrong size":
				want[0].Size++
			case "wrong digest":
				want[0].SHA256 = "sha256:" + strings.Repeat("b", 64)
			case "missing marker":
				marker = "different"
			}
			if _, err := l.Promote(context.Background(), 73, metadata, marker, want); err == nil {
				t.Fatal("drift accepted")
			}
			if f.promotes != 0 {
				t.Fatal("drift mutated release")
			}
		})
	}
}
func TestLifecycleMetadataAndDeferredTargetValidation(t *testing.T) {
	f := lifecycleFixtureFor(t)
	target := f.base.target
	target.ReleaseID = 0
	if _, err := NewPublisher(target); err == nil {
		t.Fatal("asset upload admitted an unbound draft")
	}
	if _, err := NewLifecycle(target); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Metadata{{}, {Name: "x\ny"}, {Name: "x", Body: strings.Repeat("x", (64<<10)+1)}, {Name: "x", Body: "<!-- memql-release-intent:forged -->"}} {
		if err := m.Validate(); err == nil {
			t.Fatal("bad metadata accepted")
		}
	}
}

func TestLifecyclePromotesExplicitLatestSelection(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.metadata.Latest = true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	state, err := lifecycle(t, f).Promote(context.Background(), 73, f.metadata, f.marker, f.inventory())
	if err != nil || !state.LatestVerified || state.Draft {
		t.Fatal(state, err)
	}
	if err := (Metadata{Name: "preview", Prerelease: true, Latest: true}).Validate(); err == nil {
		t.Fatal("prerelease selected as latest")
	}
}

func TestLifecycleDetectsReplacedAssetsAfterPromotion(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	original := f.base.hook
	f.base.hook = func(w http.ResponseWriter, r *http.Request) bool {
		handled := original(w, r)
		if r.Method == "PATCH" {
			f.base.mu.Lock()
			f.base.assets[0].ID++
			f.base.mu.Unlock()
		}
		return handled
	}
	if _, err := lifecycle(t, f).Promote(context.Background(), 73, f.metadata, f.marker, f.inventory()); !errors.Is(err, ErrUncertain) {
		t.Fatal("asset replacement was not uncertain", err)
	}
	if f.promotes != 1 {
		t.Fatal("promotion repeated")
	}
}
