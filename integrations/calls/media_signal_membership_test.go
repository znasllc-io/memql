package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMediaSignalMembershipReadsJoinBeforePanicLeavesGate(t *testing.T) {
	for _, panickingFirst := range []bool{true, false} {
		name := "child"
		if panickingFirst {
			name = "caller"
		}
		t.Run(name, func(t *testing.T) {
			entered, finish := make(chan struct{}), make(chan struct{})
			released := make(chan any, 1)
			go func() {
				// Models the gated callback's deferred release/recovery boundary.
				defer func() { released <- recover() }()
				blocked := func() { close(entered); <-finish }
				fails := func() { <-entered; panic("synthetic read panic") }
				if panickingFirst {
					mediaSignalReadPair(fails, blocked)
				} else {
					mediaSignalReadPair(blocked, fails)
				}
			}()
			<-entered
			select {
			case <-released:
				t.Fatal("caller released while sibling read was still running")
			case <-time.After(20 * time.Millisecond):
			}
			close(finish)
			select {
			case recovered := <-released:
				require.Equal(t, "synthetic read panic", recovered)
			case <-time.After(time.Second):
				t.Fatal("read panic did not return to caller recovery")
			}
		})
	}
}
