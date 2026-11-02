package githubrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// isByteRead tells a read of an asset's bytes from a read of its metadata.
func isByteRead(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Accept") == "application/octet-stream"
}

// watchPatch runs before for every request until the PATCH arrives, and the fixture
// answers whatever before leaves alone. before returning true means it answered.
func (f *lifecycleFixture) watchPatch(before func(http.ResponseWriter, *http.Request) bool) {
	answer := f.base.hook
	patched := false
	f.base.hook = func(w http.ResponseWriter, r *http.Request) bool {
		f.base.mu.Lock()
		patched = patched || r.Method == http.MethodPatch
		pending := !patched
		f.base.mu.Unlock()
		if pending && before(w, r) {
			return true
		}
		return answer(w, r)
	}
}

// The regression. A claimed promotion is never repeated, so a read that fails before
// its PATCH strands the release until the owner publishes on GitHub. That read used
// to be of every byte, moments after the same bytes were read before the claim.
func TestPromoteIsNotStoppedByAByteReadFailingAfterObservation(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	ctx := context.Background()
	observed, err := lifecycle(t, f).Observe(ctx, 73, f.metadata, f.marker, f.inventory())
	if err != nil {
		t.Fatal(err)
	}
	f.watchPatch(func(w http.ResponseWriter, r *http.Request) bool {
		if isByteRead(r) {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	})
	state, err := lifecycle(t, f).Promote(ctx, 73, f.metadata, f.marker, f.inventory(), observed)
	if err != nil || state.Draft {
		t.Fatal("a failed byte read stopped the promotion before its PATCH", state, err)
	}
	if f.promotes != 1 {
		t.Fatal("promotion did not send exactly one PATCH", f.promotes)
	}
}

// What can strand a release no longer grows with it. One inventory page, the release
// and a lightweight tag: three reads for one file or for the 64 a release may hold.
func TestPromoteReadsTheSameBeforeItsPatchHoweverLargeTheRelease(t *testing.T) {
	for _, files := range []int{1, 64} {
		t.Run(fmt.Sprintf("%d files", files), func(t *testing.T) {
			f := lifecycleFixtureFor(t)
			f.exists, f.tagExists = true, true
			want := []AssetExpectation{}
			for index := range files {
				name, body := fmt.Sprintf("file-%02d.tar.gz", index), []byte(fmt.Sprintf("bytes of file %d", index))
				f.base.addAsset(name, body)
				e := expected(body)
				want = append(want, AssetExpectation{Name: name, SHA256: e.SHA256, Size: e.Size})
			}
			ctx := context.Background()
			observed, err := lifecycle(t, f).Observe(ctx, 73, f.metadata, f.marker, want)
			if err != nil {
				t.Fatal(err)
			}
			reads, byteReads := 0, 0
			f.watchPatch(func(_ http.ResponseWriter, r *http.Request) bool {
				reads++
				if isByteRead(r) {
					byteReads++
				}
				return false
			})
			if _, err := lifecycle(t, f).Promote(ctx, 73, f.metadata, f.marker, want, observed); err != nil {
				t.Fatal(err)
			}
			if reads != 3 || byteReads != 0 {
				t.Fatal("reads before the PATCH grew with the release", reads, byteReads)
			}
		})
	}
}

// A proof is worth only the read that made it. A receipt reloaded from the journal,
// a draft lookup or another release's state has read none of these bytes in this call.
func TestPromoteRequiresACompleteByteObservation(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	ctx := context.Background()
	genuine := f.observed(t)
	var reloaded ReleaseState
	data, err := json.Marshal(genuine)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	found, _, err := lifecycle(t, f).FindDraft(ctx, f.metadata, f.marker)
	if err != nil {
		t.Fatal(err)
	}
	other, published := genuine, genuine
	other.ReleaseID, published.Draft = 74, false
	for name, state := range map[string]ReleaseState{"no observation": {}, "reloaded receipt": reloaded, "draft lookup": found, "another release": other, "a published release": published} {
		t.Run(name, func(t *testing.T) {
			if _, err := lifecycle(t, f).Promote(ctx, 73, f.metadata, f.marker, f.inventory(), state); err == nil || errors.Is(err, ErrUncertain) {
				t.Fatal("promotion accepted a state no byte read produced", err)
			}
			if f.promotes != 0 {
				t.Fatal("a refused proof sent the PATCH", f.promotes)
			}
		})
	}
}

// cutOff fails a read for real: the transport retries a GET dropped before any bytes.
func cutOff(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	_, _ = w.Write([]byte("{"))
	w.(http.Flusher).Flush()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func noResponse(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// Only the owner can recover a read that fails here, so a read with no answer must not
// be the end of it. Any of the three alone would strand the release, and none of these
// failures says anything about the release.
func TestPromoteRepeatsAReadThatGetsNoAnswerBeforeItsPatch(t *testing.T) {
	const repo = "/repos/owner/project"
	status := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { w.WriteHeader(code) }
	}
	for read, path := range map[string]string{"inventory": repo + "/releases/73/assets", "release": repo + "/releases/73", "tag": repo + "/git/ref/tags/v1.2.3"} {
		run := func(t *testing.T, failures int, answer func(http.ResponseWriter)) (*lifecycleFixture, ReleaseState, int, error) {
			f := lifecycleFixtureFor(t)
			f.exists, f.tagExists = true, true
			f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
			observed := f.observed(t)
			failed := 0
			f.watchPatch(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == path && failed < failures {
					failed++
					answer(w)
					return true
				}
				return false
			})
			state, err := lifecycle(t, f).Promote(context.Background(), 73, f.metadata, f.marker, f.inventory(), observed)
			return f, state, failed, err
		}
		for name, answer := range map[string]func(http.ResponseWriter){"502": status(502), "429": status(429), "403": status(403), "401": status(401), "cut off": cutOff, "no response": noResponse} {
			t.Run(read+"/"+name+" recovers in time", func(t *testing.T) {
				f, state, failed, err := run(t, len(settleDelays), answer)
				if err != nil || state.Draft || f.promotes != 1 || failed != len(settleDelays) {
					t.Fatal("a read that recovered did not lead to one PATCH", state, err, f.promotes, failed)
				}
			})
		}
		// The remaining window: no PATCH went, so this is not uncertain.
		t.Run(read+"/never recovers", func(t *testing.T) {
			f, _, failed, err := run(t, 1000, status(502))
			if err == nil || errors.Is(err, ErrUncertain) || f.promotes != 0 {
				t.Fatal("a failed read before the PATCH was misreported or the PATCH went", err, f.promotes)
			}
			if failed != 1+len(settleDelays) {
				t.Fatal("repeats are not bounded by the settle delays", failed)
			}
		})
	}
}

// A read that shows a change is an answer, and reading again could only argue with it:
// a lagging replica would show the old state, and the release would go public on it.
func TestPromoteDoesNotReadPastDriftItHasSeen(t *testing.T) {
	const repo = "/repos/owner/project"
	gone := func(w http.ResponseWriter, _ *lifecycleFixture) { w.WriteHeader(http.StatusNotFound) }
	for _, tc := range []struct {
		name, path string
		answer     func(http.ResponseWriter, *lifecycleFixture)
	}{
		{"a replaced asset", repo + "/releases/73/assets", func(w http.ResponseWriter, f *lifecycleFixture) {
			shown := f.base.assets[0].asset
			shown.ID++
			_ = json.NewEncoder(w).Encode([]asset{shown})
		}},
		{"changed notes", repo + "/releases/73", func(w http.ResponseWriter, _ *lifecycleFixture) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 73, "draft": true, "tag_name": "v1.2.3", "name": "Other", "prerelease": false})
		}},
		{"a release that is gone", repo + "/releases/73", gone},
		{"a moved tag", repo + "/git/ref/tags/v1.2.3", func(w http.ResponseWriter, _ *lifecycleFixture) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/v1.2.3", "object": map[string]any{"type": "commit", "sha": strings.Repeat("b", 40)}})
		}},
		{"a tag that is gone", repo + "/git/ref/tags/v1.2.3", gone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := lifecycleFixtureFor(t)
			f.exists, f.tagExists = true, true
			f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
			observed := f.observed(t)
			answered := false
			f.watchPatch(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == tc.path && !answered {
					answered = true
					tc.answer(w, f)
					return true
				}
				return false
			})
			if _, err := lifecycle(t, f).Promote(context.Background(), 73, f.metadata, f.marker, f.inventory(), observed); err == nil || errors.Is(err, ErrUncertain) {
				t.Fatal("drift the read showed did not refuse the promotion", err)
			}
			if f.promotes != 0 {
				t.Fatal("a repeated read got past drift it had already seen", f.promotes)
			}
		})
	}
}

// The price of this order, as a test. The same ID, the same reported metadata and
// different bytes is the one drift it cannot see until the release is public. GitHub
// has no call that does this, and the old second read could miss it too, in the
// moments between that read and the PATCH.
func TestPromoteFindsBytesChangedUnderAnUnchangedIDOnlyAfterItsPatch(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	observed := f.observed(t)
	f.base.mu.Lock()
	f.base.corrupt = true
	f.base.mu.Unlock()
	ctx := context.Background()
	if _, err := lifecycle(t, f).Promote(ctx, 73, f.metadata, f.marker, f.inventory(), observed); !errors.Is(err, ErrUncertain) {
		t.Fatal("changed bytes after the PATCH did not leave the outcome uncertain", err)
	}
	if f.promotes != 1 {
		t.Fatal("promotion did not send exactly one PATCH", f.promotes)
	}
	var conflict *ConflictError
	if _, err := lifecycle(t, f).Observe(ctx, 73, f.metadata, f.marker, f.inventory()); !errors.As(err, &conflict) {
		t.Fatal("the next look did not name the file", err)
	}
}

// Someone published it between the observation and the PATCH. There is nothing to
// send, and what the caller records still has to come from reading every byte.
func TestPromoteSendsNoPatchToAReleasePublishedSinceItsObservation(t *testing.T) {
	f := lifecycleFixtureFor(t)
	f.exists, f.tagExists = true, true
	f.metadata.Latest = true
	f.base.addAsset(f.base.target.AssetName, []byte("release bytes"))
	observed := f.observed(t)
	f.base.mu.Lock()
	f.base.draft = false
	f.base.mu.Unlock()
	byteReads := 0
	f.watchPatch(func(_ http.ResponseWriter, r *http.Request) bool {
		if isByteRead(r) {
			byteReads++
		}
		return false
	})
	state, err := lifecycle(t, f).Promote(context.Background(), 73, f.metadata, f.marker, f.inventory(), observed)
	if err != nil || state.Draft || !state.LatestVerified {
		t.Fatal("the published release was not proved for its record", state, err)
	}
	if f.promotes != 0 || byteReads != 1 {
		t.Fatal("a published release was patched or not read", f.promotes, byteReads)
	}
}
