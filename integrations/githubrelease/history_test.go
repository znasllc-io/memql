package githubrelease

import (
	"net/http"
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
