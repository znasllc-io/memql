package githubrelease

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestObservePublishedHistoryDoesNotRequireLatestOrMutate(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists, f.metadata.Latest = true, true, true
	f.base.draft = false
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	original := f.base.hook
	latestReads := 0
	f.base.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/owner/project/releases/latest" {
			latestReads++
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return original(w, r)
	}
	l := lifecycle(t, f)
	if _, err := l.Observe(t.Context(), 73, f.metadata, f.marker, f.inventory()); err == nil || latestReads != 1 {
		t.Fatal("normal publication observation lost its latest-selection gate", err, latestReads)
	}
	state, err := lifecycle(t, f).ObservePublished(t.Context(), 73, f.metadata, f.marker, f.inventory())
	if err != nil || state.Draft || state.LatestVerified || latestReads != 1 || len(state.Assets) != 1 {
		t.Fatal(state, err, latestReads)
	}
	if f.creates != 0 || f.tags != 0 || f.promotes != 0 || f.base.posts != 0 {
		t.Fatal("history observation changed the release")
	}
	f.base.corrupt = true
	if _, err := lifecycle(t, f).ObservePublished(t.Context(), 73, f.metadata, f.marker, f.inventory()); err == nil {
		t.Fatal("replacement observer accepted changed canonical bytes")
	}
}

func TestObservePublishedHistoryRefusesDraftsAndInvalidMetadata(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	if _, err := lifecycle(t, f).ObservePublished(t.Context(), 73, f.metadata, f.marker, f.inventory()); err == nil {
		t.Fatal("draft was counted as published availability")
	}
	f.base.draft = false
	invalid := f.metadata
	invalid.Latest, invalid.Prerelease = true, true
	if _, err := lifecycle(t, f).ObservePublished(t.Context(), 73, invalid, f.marker, f.inventory()); err == nil {
		t.Fatal("historical observation sanitized invalid approved metadata")
	}
}

// A person may annotate a release once it is public. v0.30.8 has carried a deployment
// hold in its notes since 48 minutes after it went out, and that refused its archival
// from the first pass, 48 hours after sealing, for as long as the notes stood. The
// bytes and the identity are what retained history rests on, so those still refuse, and
// a draft or a promotion, which go on to publish, keep comparing what was reviewed.
func TestObservePublishedHistoryAcceptsEditedNotesButNotAnotherRelease(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.draft = false
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	reviewed := f.metadata
	f.metadata.Name, f.metadata.Prerelease = "Release 1.2.3 (held)", true
	f.metadata.Body = "> Deployment hold: do not deploy.\n\n" + reviewed.Body
	if _, err := lifecycle(t, f).Observe(t.Context(), 73, reviewed, f.marker, f.inventory()); err == nil || !strings.Contains(err.Error(), "reviewed metadata") {
		t.Fatal("a publication's observation accepted metadata nobody reviewed", err)
	}
	if err := lifecycle(t, f).CheckDraft(t.Context(), 73, reviewed, f.marker); err == nil {
		t.Fatal("a draft's check accepted metadata nobody reviewed")
	}
	state, err := lifecycle(t, f).ObservePublished(t.Context(), 73, reviewed, f.marker, f.inventory())
	if err != nil || state.Draft || len(state.Assets) != 1 {
		t.Fatal("annotating a release stopped it counting as published", state, err)
	}
	for name, change := range map[string]map[string]any{
		"another tag":    {"tag_name": "v9.9.9"},
		"another upload": {"upload_url": "https://uploads.example.com/repos/owner/project/releases/73/assets{?name,label}"},
		"another id":     {"id": 74},
		"a draft again":  {"draft": true},
	} {
		t.Run(name, func(t *testing.T) {
			original := f.base.hook
			t.Cleanup(func() { f.base.hook = original })
			f.base.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/repos/owner/project/releases/73" {
					return original(w, r)
				}
				served := map[string]any{"id": 73, "draft": false, "tag_name": f.base.target.Tag, "name": f.metadata.Name, "body": f.metadata.Body, "prerelease": f.metadata.Prerelease,
					"upload_url": f.base.target.UploadOrigin + "/repos/owner/project/releases/73/assets{?name,label}"}
				for key, value := range change {
					served[key] = value
				}
				_ = json.NewEncoder(w).Encode(served)
				return true
			}
			if _, err := lifecycle(t, f).ObservePublished(t.Context(), 73, reviewed, f.marker, f.inventory()); err == nil {
				t.Fatal("history accepted another release", name)
			}
		})
	}
	f.base.corrupt = true
	if _, err := lifecycle(t, f).ObservePublished(t.Context(), 73, reviewed, f.marker, f.inventory()); err == nil {
		t.Fatal("annotating a release excused its changed bytes")
	}
}
