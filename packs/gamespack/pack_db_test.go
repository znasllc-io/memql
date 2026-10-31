package gamespack

import "testing"

// A disabled pack is mounted-inert: its concepts load, its behaviour does not.
func TestLiveDisabledPackIsInert(t *testing.T) {
	lg := newLiveGames(t, Domain)
	if _, err := lg.call(t, asOwner("user-games-inert-"+runID()), "query", "gamesPlayers", nil); err == nil {
		t.Fatal("a query of a DISABLED pack resolved")
	}
}
