//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/memql"
	"testing"
)

func TestAppDispatchPlacementUsesLocalStreamAndKeepsNoModelOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, selector      string
		local, peer, online bool
		want                bool
	}{
		{"local stream", "*", true, false, true, true},
		{"only remote stream", "*", false, true, true, false},
		{"both local and remote", "*", true, true, true, true},
		{"nobody eligible", "*", false, false, true, true},
		{"offline peer", "*", false, true, false, true},
		{"other app is not selected", "claude-code", false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doors := []memql.AppDoor{{AppId: "codex", Machines: []memql.AppMachine{{Online: tc.online, LocalStream: tc.local, Forwardable: tc.peer}}}}
			if got := appDispatchHere(tc.selector, doors); got != tc.want {
				t.Fatalf("placement=%t want=%t", got, tc.want)
			}
		})
	}
}
