package githubrelease

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a re-read looks at is the contract here, not how long it waits, so these
// tests shorten the waits to nothing. The count of delays stays the production
// count: a test that reads "every delay spent" still measures the real bound.
func init() {
	for index := range settleDelays {
		settleDelays[index] = time.Millisecond
	}
}

// leaveIncomplete puts the asset an interrupted upload leaves behind on the
// fixture: GitHub's listing names it, with no bytes to serve.
func (f *releaseFixture) leaveIncomplete(state string) int64 {
	f.addAsset(f.target.AssetName, nil)
	last := &f.assets[len(f.assets)-1]
	last.State, last.Digest = state, ""
	return last.ID
}

func TestPublishReplacesTheAssetAnInterruptedUploadLeftBehind(t *testing.T) {
	// Documented: "starter". The REST schema also lists "open".
	for _, state := range []string{"starter", "open"} {
		t.Run(state, func(t *testing.T) {
			f := fixture(t)
			stale := f.leaveIncomplete(state)
			data := []byte("approved bytes")
			receipt, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
			if err != nil {
				t.Fatal(err)
			}
			if len(f.deletes) != 1 || f.deletes[0] != stale || f.posts != 1 {
				t.Fatal("recovery did not remove exactly the incomplete asset and upload once", f.deletes, f.posts)
			}
			if receipt.AssetID == stale || receipt.ReplacedAssetID != stale {
				t.Fatal("receipt does not report the replacement", receipt, stale)
			}
			if len(f.assets) != 1 || f.assets[0].State != "uploaded" || string(f.assets[0].body) != string(data) {
				t.Fatal("release does not hold the approved bytes", f.assets)
			}
		})
	}
}

func TestPublishRecoversAnUploadCutOffPartWayOnAnotherHost(t *testing.T) {
	f := fixture(t)
	data := []byte("approved bytes")
	cut := true
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !cut {
			return false
		}
		cut = false
		// The connection dies part way through the body. GitHub keeps the asset
		// it opened, and the sender never hears an answer.
		_, _ = io.CopyN(io.Discard, r.Body, 4)
		f.mu.Lock()
		f.posts++
		f.leaveIncomplete("starter")
		f.mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return true
		}
		_ = conn.Close()
		return true
	}
	if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, data)); !errors.Is(err, ErrUncertain) {
		t.Fatal("a cut-off upload reported a known outcome", err)
	}
	if f.posts != 1 || len(f.deletes) != 0 {
		t.Fatal("one call repeated or removed its own upload", f.posts, f.deletes)
	}
	receipt, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if f.posts != 2 || len(f.deletes) != 1 || receipt.ReplacedAssetID != f.deletes[0] {
		t.Fatal("the retry did not recover through exactly one removal and one upload", f.posts, f.deletes, receipt)
	}
}

func TestPublishNeverRemovesAnAssetItCannotProveIncomplete(t *testing.T) {
	data := []byte("approved bytes")
	for _, mode := range []string{"uploaded wrong digest", "uploaded wrong size", "unknown state", "no state", "incomplete on a published release", "incomplete under a moved tag"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			switch mode {
			case "uploaded wrong digest":
				f.addAsset(f.target.AssetName, data)
				f.assets[0].Digest = "sha256:" + strings.Repeat("f", 64)
			case "uploaded wrong size":
				f.addAsset(f.target.AssetName, data)
				*f.assets[0].Size++
			case "unknown state":
				f.leaveIncomplete("quarantined")
			case "no state":
				f.leaveIncomplete("")
			case "incomplete on a published release":
				f.leaveIncomplete("starter")
				f.draft = false
			case "incomplete under a moved tag":
				f.leaveIncomplete("starter")
				f.commit = strings.Repeat("b", 40)
			}
			if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, data)); err == nil || errors.Is(err, ErrUncertain) {
				t.Fatal("this asset must refuse before any remote write", err)
			}
			if len(f.deletes) != 0 || f.posts != 0 {
				t.Fatal("a refusal changed the release", f.deletes, f.posts)
			}
		})
	}
}

func TestPublishLeavesOtherAssetsAloneDuringRecovery(t *testing.T) {
	f := fixture(t)
	f.addAsset("another-platform.tar.gz", []byte("another file"))
	f.assets[0].State = "starter"
	stale := f.leaveIncomplete("starter")
	if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, []byte("approved bytes"))); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 || f.deletes[0] != stale || len(f.assets) != 2 {
		t.Fatal("recovery reached beyond its own asset name", f.deletes, f.assets)
	}
}

func TestPublishDoesNotUploadOverAnAssetItCouldNotClear(t *testing.T) {
	for _, mode := range []string{"removal refused", "still listed after removal"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			f.leaveIncomplete("starter")
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodDelete {
					return false
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				f.deletes = append(f.deletes, f.assets[0].ID)
				if mode == "removal refused" {
					w.WriteHeader(500)
				} else {
					w.WriteHeader(204)
				}
				return true
			}
			if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, []byte("approved bytes"))); !errors.Is(err, ErrUncertain) {
				t.Fatal("an attempted removal must leave the outcome uncertain", err)
			}
			if f.posts != 0 || len(f.deletes) != 1 {
				t.Fatal("upload proceeded or removal repeated", f.posts, f.deletes)
			}
		})
	}
}

func TestPublishWaitsForItsOwnUploadToSettle(t *testing.T) {
	data := []byte("approved bytes")
	for _, tc := range []struct {
		name string
		// reads answers one inventory read after the POST by index; the last
		// entry repeats.
		reads     []string
		uncertain bool
	}{
		{name: "listing lags", reads: []string{"absent", "absent", "listed"}},
		{name: "still finishing", reads: []string{"starter", "starter", "listed"}},
		{name: "never listed", reads: []string{"absent"}, uncertain: true},
		{name: "never finishes", reads: []string{"starter"}, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			read := -1
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/assets") {
					return false
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.posts == 0 {
					_ = json.NewEncoder(w).Encode([]asset{})
					return true
				}
				read++
				answer := tc.reads[min(read, len(tc.reads)-1)]
				switch answer {
				case "absent":
					_ = json.NewEncoder(w).Encode([]asset{})
				case "starter":
					shown := f.assets[0].asset
					shown.State = "starter"
					_ = json.NewEncoder(w).Encode([]asset{shown})
				default:
					_ = json.NewEncoder(w).Encode([]asset{f.assets[0].asset})
				}
				return true
			}
			_, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
			if tc.uncertain && !errors.Is(err, ErrUncertain) {
				t.Fatal("an upload that never settled reported a known outcome", err)
			}
			if !tc.uncertain && err != nil {
				t.Fatal(err)
			}
			if f.posts != 1 || len(f.deletes) != 0 {
				t.Fatal("waiting repeated or removed its own upload", f.posts, f.deletes)
			}
			if tc.uncertain && read != len(settleDelays) {
				t.Fatal("re-reads are not bounded by the settle delays", read)
			}
		})
	}
}

func TestCreateDraftRereadsBeforeReportingUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lag       int
		uncertain bool
	}{
		{name: "listing lags then shows the draft", lag: len(settleDelays)},
		{name: "listing never shows the draft", lag: 1000, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := lifecycleFixtureFor(t)
			f.tagExists = true
			f.listLag = tc.lag
			state, err := lifecycle(t, f).CreateDraft(context.Background(), f.metadata, f.marker)
			if tc.uncertain {
				if !errors.Is(err, ErrUncertain) {
					t.Fatal("an unseen draft reported a known outcome", err)
				}
			} else if err != nil || state.ReleaseID != 73 || !state.Draft {
				t.Fatal("a draft GitHub holds was not found", state, err)
			}
			if f.creates != 1 {
				t.Fatal("re-reading repeated the once-only POST", f.creates)
			}
			// One read before the POST, one after it, one per delay at most.
			if f.listReads > 2+len(settleDelays) {
				t.Fatal("re-reads are not bounded by the settle delays", f.listReads)
			}
		})
	}
}

// A read that fails says nothing about the bytes. Calling it a conflict tells the
// owner to remove a file that is correct.
func TestPublishCallsOnlyACompletedReadAConflict(t *testing.T) {
	data := []byte("approved bytes of some length")
	for _, tc := range []struct {
		name     string
		answer   func(w http.ResponseWriter)
		conflict bool
	}{
		{name: "connection dropped mid-read", answer: func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data[:4])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}},
		{name: "different bytes of the same length", conflict: true, answer: func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(strings.Repeat("x", len(data))))
		}},
		{name: "more bytes than approved", conflict: true, answer: func(w http.ResponseWriter) {
			_, _ = w.Write(append(append([]byte(nil), data...), 'x'))
		}},
		{name: "fewer bytes, read to its end", conflict: true, answer: func(w http.ResponseWriter) {
			_, _ = w.Write(data[:4])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			f.addAsset(f.target.AssetName, data)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/releases/assets/") {
					return false
				}
				tc.answer(w)
				return true
			}
			_, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
			if err == nil {
				t.Fatal("an unproven asset was accepted")
			}
			var conflict *ConflictError
			if got := errors.As(err, &conflict); got != tc.conflict || errors.Is(err, ErrConflict) != tc.conflict {
				t.Fatal("wrong classification of this read", err)
			}
			if tc.conflict && conflict.AssetName != f.target.AssetName {
				t.Fatal("the conflict does not name its file", conflict.AssetName)
			}
		})
	}
}

// GitHub has no conditional delete. The last moment to notice is one more read.
func TestPublishKeepsAnAssetThatFinishedWhileItWasChecking(t *testing.T) {
	f := fixture(t)
	data := []byte("approved bytes")
	f.addAsset(f.target.AssetName, data)
	reads := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/assets") {
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		reads++
		if len(f.assets) == 0 {
			_ = json.NewEncoder(w).Encode([]asset{})
			return true
		}
		shown := f.assets[0].asset
		if reads == 1 {
			shown.State = "starter"
		}
		_ = json.NewEncoder(w).Encode([]asset{shown})
		return true
	}
	receipt, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 0 || f.posts != 0 || receipt.ReplacedAssetID != 0 {
		t.Fatal("an asset that had finished was removed or sent again", f.deletes, f.posts)
	}
}

func TestPublishReportsARemovalEvenWhenItThenFails(t *testing.T) {
	f := fixture(t)
	stale := f.leaveIncomplete("starter")
	f.reject = true
	receipt, err := publisher(t, f.target).Publish(context.Background(), verified(t, []byte("approved bytes")))
	if !errors.Is(err, ErrUncertain) {
		t.Fatal("a rejected upload reported a known outcome", err)
	}
	if receipt.ReplacedAssetID != stale {
		t.Fatal("a remote delete left no trace because the upload after it failed", receipt.ReplacedAssetID, stale)
	}
}
