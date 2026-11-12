package shopify

import (
	"context"
	"fmt"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// heal_fence_test.go -- a sweep's own write is fenced on the row it judged.
//
// heal reads the stored row, asks the guard, and writes: two statements, with
// a delivery free to commit in between. The write carries the judged row's
// version; a write the row moved under is refused, and the row is read and
// judged again, once.

// versionedMirror is the rig's mirror with a version on every row, as the
// engine keeps one: a read answers it as the node's creation time, and a
// write moves it. `overtake` runs before the first write of each row it
// names and that write is then refused, as the engine refuses a fenced write
// the row moved under.
type versionedMirror struct {
	*mirrorEngine
	mu       stdsync.Mutex
	versions map[string]time.Time
	overtake map[string]func()
	fenced   map[string][]bool
	reads    map[string]int
}

func (r *mirrorRig) versioned() *versionedMirror {
	v := &versionedMirror{mirrorEngine: r.mirror, versions: map[string]time.Time{}, overtake: map[string]func(){}, fenced: map[string][]bool{}, reads: map[string]int{}}
	r.conn.engine = v
	return v
}

func (v *versionedMirror) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	q = strings.TrimSpace(q)
	if g := mirrorInsertRE.FindStringSubmatch(q); g != nil {
		concept, id := unquoted(g[1]), unquoted(g[2])
		key := concept + "|" + id
		v.mu.Lock()
		v.fenced[key] = append(v.fenced[key], memql.HasRowVersionFence(ctx, concept, id))
		meanwhile := v.overtake[key]
		delete(v.overtake, key)
		v.mu.Unlock()
		if meanwhile != nil {
			meanwhile()
			return nil, fmt.Errorf("execute: %w", memql.ErrRowVersionChanged)
		}
		res, err := v.mirrorEngine.Execute(ctx, q)
		v.mu.Lock()
		v.versions[key] = v.versions[key].Add(time.Second)
		if v.versions[key].Before(time.Unix(1, 0)) {
			v.versions[key] = time.Date(2026, 8, 23, 0, 0, 1, 0, time.UTC)
		}
		v.mu.Unlock()
		return res, err
	}
	res, err := v.mirrorEngine.Execute(ctx, q)
	if g := mirrorReadRE.FindStringSubmatch(q); g != nil && err == nil && res != nil && res.Bundle != nil {
		key := g[1] + "|" + unquoted(g[2])
		v.mu.Lock()
		v.reads[key]++
		at := v.versions[key]
		v.mu.Unlock()
		for _, node := range res.Bundle.GetNodes() {
			if !at.IsZero() {
				node.CreatedAt = timestamppb.New(at)
			}
		}
	}
	return res, err
}

func unquoted(s string) string {
	var out string
	_, _ = fmt.Sscanf(s, "%q", &out)
	return out
}

// seed writes a row into the versioned mirror as an earlier write left it.
func (v *versionedMirror) seed(r *mirrorRig, concept, gid, updatedAt string, read time.Time) string {
	r.held(concept, gid, updatedAt, read)
	key := generated.ConceptID(concept) + "|" + MirrorRowID(testStoreID, gid)
	v.mu.Lock()
	v.versions[key] = time.Date(2026, 8, 23, 0, 0, 1, 0, time.UTC)
	v.mu.Unlock()
	return key
}

func TestAHealTheRowMovedUnderIsJudgedAgainOnce(t *testing.T) {
	const gid = "gid://shopify/Product/1"
	spec := generated.Types["product"]
	page := func(updatedAt string, read time.Time) []memqlsync.MirrorWrite {
		return mapObject(spec, testStoreID, map[string]any{"id": gid, "title": "the sweep's page", "updatedAt": updatedAt}, "", read)[:1]
	}
	for _, tc := range []struct {
		name      string
		held      bool
		meanwhile string // the state a delivery writes between the read and the write
		twice     bool   // the row moves under the second write too
		healed    int
		failed    int
		title     string
		writes    int
	}{
		{name: "nobody else wrote", held: true, healed: 1, title: "the sweep's page", writes: 1},
		{name: "a row the mirror does not hold", healed: 1, title: "the sweep's page", writes: 1},
		{name: "a delivery wrote a newer state meanwhile", held: true, meanwhile: "2026-08-23T13:00:00Z", title: "a delivery", writes: 1},
		{name: "a delivery wrote an older state meanwhile", held: true, meanwhile: "2026-08-23T11:00:00Z", healed: 1, title: "the sweep's page", writes: 2},
		{name: "a delivery created the row meanwhile, newer", meanwhile: "2026-08-23T13:00:00Z", title: "a delivery", writes: 1},
		{name: "the row moves under the second write too", held: true, meanwhile: "2026-08-23T11:00:00Z", twice: true, failed: 1, title: "a delivery", writes: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMirrorRig(t)
			v := r.versioned()
			r.now = at("12:30:00")
			key := generated.ConceptID("product") + "|" + MirrorRowID(testStoreID, gid)
			if tc.held {
				v.seed(r, "product", gid, "2026-08-23T10:00:00Z", at("10:30:00"))
			}
			delivery := func() {
				r.put("product", gid, map[string]any{"title": "a delivery", "updatedAt": tc.meanwhile, "versionSource": "origin",
					"syncedAt": memqlsync.ReadTime(at("12:20:00")), "versionReadAt": memqlsync.ReadTime(at("12:20:00")), "deleted": false})
				v.mu.Lock()
				v.versions[key] = v.versions[key].Add(time.Minute)
				if v.versions[key].Before(time.Unix(1, 0)) {
					v.versions[key] = time.Date(2026, 8, 23, 0, 1, 0, 0, time.UTC)
				}
				v.mu.Unlock()
			}
			if tc.meanwhile != "" {
				v.overtake[key] = func() {
					delivery()
					if tc.twice {
						v.mu.Lock()
						v.overtake[key] = delivery
						v.mu.Unlock()
					}
				}
			}
			healed, failed := r.conn.heal(context.Background(), page("2026-08-23T12:00:00Z", at("12:25:00")))
			if healed != tc.healed || failed != tc.failed {
				t.Fatalf("healed %d, failed %d; want %d and %d", healed, failed, tc.healed, tc.failed)
			}
			if got := r.row("product", gid)["title"]; got != tc.title {
				t.Fatalf("the mirror holds %q, want %q", got, tc.title)
			}
			if got := v.fenced[key]; len(got) != tc.writes {
				t.Fatalf("%d writes reached the mirror, want %d", len(got), tc.writes)
			}
			for i, fenced := range v.fenced[key] {
				if !fenced {
					t.Errorf("write %d carried no row-version fence", i+1)
				}
			}
		})
	}
}
