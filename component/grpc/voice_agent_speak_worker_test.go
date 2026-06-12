package memql

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectForwarded waits until the recorder holds want replies (or times out)
// and returns the snapshot.
func collectForwarded(t *testing.T, mu *sync.Mutex, got *[]voiceAgentReply, want int) []voiceAgentReply {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*got)
		mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]voiceAgentReply(nil), (*got)...)
}

// TestRunVoiceSpeakWorker_PreservesOrder pins the #1429 ordering contract:
// replies enqueued by the event-bus callback are forwarded in FIFO order by
// the single per-session worker -- moving the engine lookup off the bus
// callback must not reorder a session's speak stream.
func TestRunVoiceSpeakWorker_PreservesOrder(t *testing.T) {
	queue := make(chan voiceAgentReply, voiceSpeakQueueDepth)
	done := make(chan struct{})
	defer close(done)

	var mu sync.Mutex
	var got []voiceAgentReply
	go runVoiceSpeakWorker(queue, done,
		func(voiceAgentReply) string { return "always_on" },
		func(r voiceAgentReply) {
			mu.Lock()
			got = append(got, r)
			mu.Unlock()
		})

	const n = 16
	for i := 0; i < n; i++ {
		queue <- voiceAgentReply{utteranceId: fmt.Sprintf("utt-%03d", i), text: fmt.Sprintf("reply %d", i)}
	}

	forwarded := collectForwarded(t, &mu, &got, n)
	require.Len(t, forwarded, n)
	for i, r := range forwarded {
		assert.Equal(t, fmt.Sprintf("utt-%03d", i), r.utteranceId, "FIFO order must be preserved")
	}
}

// TestRunVoiceSpeakWorker_GatesOnMode pins the mode gate: only always_on
// forwards; mirror_user / always_off replies are dropped without forwarding,
// and the mode is re-consulted per reply (a mid-session flip applies).
func TestRunVoiceSpeakWorker_GatesOnMode(t *testing.T) {
	queue := make(chan voiceAgentReply, voiceSpeakQueueDepth)
	done := make(chan struct{})
	defer close(done)

	modes := map[string]string{
		"utt-on":     "always_on",
		"utt-mirror": "mirror_user",
		"utt-off":    "always_off",
		"utt-on-2":   "always_on",
	}
	var mu sync.Mutex
	var got []voiceAgentReply
	go runVoiceSpeakWorker(queue, done,
		func(r voiceAgentReply) string { return modes[r.utteranceId] },
		func(r voiceAgentReply) {
			mu.Lock()
			got = append(got, r)
			mu.Unlock()
		})

	for _, id := range []string{"utt-on", "utt-mirror", "utt-off", "utt-on-2"} {
		queue <- voiceAgentReply{utteranceId: id}
	}

	forwarded := collectForwarded(t, &mu, &got, 2)
	require.Len(t, forwarded, 2)
	assert.Equal(t, "utt-on", forwarded[0].utteranceId)
	assert.Equal(t, "utt-on-2", forwarded[1].utteranceId)
}

// TestRunVoiceSpeakWorker_StopsOnDone pins the teardown contract: closing done
// terminates the worker; replies enqueued after termination are never
// forwarded (the session is over).
func TestRunVoiceSpeakWorker_StopsOnDone(t *testing.T) {
	queue := make(chan voiceAgentReply, voiceSpeakQueueDepth)
	done := make(chan struct{})

	var mu sync.Mutex
	var got []voiceAgentReply
	exited := make(chan struct{})
	go func() {
		runVoiceSpeakWorker(queue, done,
			func(voiceAgentReply) string { return "always_on" },
			func(r voiceAgentReply) {
				mu.Lock()
				got = append(got, r)
				mu.Unlock()
			})
		close(exited)
	}()

	close(done)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit after done closed")
	}

	queue <- voiceAgentReply{utteranceId: "utt-late"}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, got, "no reply may be forwarded after teardown")
}

// TestChannelModeCache pins the TTL cache the speak worker resolves through:
// a fresh entry answers WITHOUT calling resolve (zero engine queries inside
// the TTL), an expired or empty entry re-resolves exactly once and re-arms.
func TestChannelModeCache(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &channelModeCache{ttl: 3 * time.Second, now: func() time.Time { return now }}

	calls := 0
	resolve := func() string { calls++; return "always_on" }

	// Empty cache -> resolve.
	assert.Equal(t, "always_on", c.get(resolve))
	assert.Equal(t, 1, calls)

	// Fresh hit -> no resolve.
	now = now.Add(2 * time.Second)
	assert.Equal(t, "always_on", c.get(resolve))
	assert.Equal(t, 1, calls, "a fresh cache hit must not query")

	// Past the TTL -> re-resolve (a flipped toggle is picked up).
	now = now.Add(2 * time.Second)
	resolve = func() string { calls++; return "always_off" }
	assert.Equal(t, "always_off", c.get(resolve))
	assert.Equal(t, 2, calls)

	// And the new answer is cached again.
	now = now.Add(time.Second)
	assert.Equal(t, "always_off", c.get(resolve))
	assert.Equal(t, 2, calls)
}

// TestChannelModeCache_SeededSkipsFirstResolve pins the session-start seeding:
// a cache seeded with the SessionStart-resolved mode answers the first
// speak-eligible reply with zero engine queries.
func TestChannelModeCache_SeededSkipsFirstResolve(t *testing.T) {
	now := time.Unix(2000, 0)
	c := &channelModeCache{ttl: 3 * time.Second, now: func() time.Time { return now }}
	c.mode = "always_on"
	c.fetched = now

	called := false
	got := c.get(func() string { called = true; return "always_off" })
	assert.Equal(t, "always_on", got)
	assert.False(t, called, "the seeded entry must serve the first lookup")
}
