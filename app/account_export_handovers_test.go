package app

import (
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/accountexport"
)

// A component that keeps account data in tables of its own hands it over to an
// account export by registering from an init() (component/accountexport). An
// export runs on an agent node, and a hand-over that is not linked into that
// binary is a file with its tables missing and no error. This test runs under
// every node type's build tag, so it holds for each binary.
func TestEveryBinaryLinksTheAccountExportHandOvers(t *testing.T) {
	got := accountexport.HandOvers()
	for _, want := range []string{"email-blocks", "site-statistics"} {
		if !slices.Contains(got, want) {
			t.Errorf("this binary does not link the hand-over %q (registered: %v), so an export run here would leave its tables out of the file", want, got)
		}
	}
}
