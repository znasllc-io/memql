package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A row the Library fence refused records the account the rules chose as its
// candidate; a refused write that chose no account (a conversion alone)
// records none, never an empty account.
func TestAccountAssignmentFenceRefusalCandidates(t *testing.T) {
	require.Equal(t, []string{"acct-1"}, fenceRefusalCandidates("acct-1"))
	require.Equal(t, []string{}, fenceRefusalCandidates(""))
}
