package selfupdate

import "testing"

func TestPaginationRelationVariantsCannotTruncateCatalog(t *testing.T) {
	initial := "https://api.github.com/repos/znasllc-io/memql/releases"
	next := initial + "?page=2"
	for _, relation := range []string{`rel=next`, `rel = "next"`, `rel="prev next"`, `rel = next; type="application/json"`} {
		got, err := nextPage("<"+next+">; "+relation, initial, initial)
		if err != nil || got != next {
			t.Errorf("%q: got %q %v", relation, got, err)
		}
	}
	for _, relation := range []string{`rel`, `rel="next`, `rel="next"; rel="last"`, `title="next"`} {
		if _, err := nextPage("<"+next+">; "+relation, initial, initial); err == nil {
			t.Errorf("silently accepted %q", relation)
		}
	}
}
