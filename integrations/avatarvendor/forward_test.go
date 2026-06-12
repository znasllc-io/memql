package avatarvendor

import (
	"testing"
)

// TestPCMStreamAttributes_StampsProducerRate is the #1428 conversion contract:
// the byte-stream header must declare the PRODUCER's PCM rate (24 kHz for the
// realtime executor, 16 kHz for the cascade TTS), not the vendor's preferred
// rate -- the vendor engine resamples from the header.
func TestPCMStreamAttributes_StampsProducerRate(t *testing.T) {
	cases := []struct {
		name string
		rate int
		want string
	}{
		{"realtime executor 24kHz", 24000, "24000"},
		{"cascade TTS 16kHz", 16000, "16000"},
		{"browser-context 48kHz", 48000, "48000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs := PCMStreamAttributes(tc.rate)
			if got := attrs["sample_rate"]; got != tc.want {
				t.Fatalf("sample_rate = %q, want %q", got, tc.want)
			}
			if got := attrs["num_channels"]; got != "1" {
				t.Fatalf("num_channels = %q, want \"1\"", got)
			}
		})
	}
}

// TestPCMStreamAttributes_FallbackRate: a non-positive rate still produces a
// well-formed header at the package default rather than "0".
func TestPCMStreamAttributes_FallbackRate(t *testing.T) {
	for _, rate := range []int{0, -1} {
		attrs := PCMStreamAttributes(rate)
		if got := attrs["sample_rate"]; got != "16000" {
			t.Fatalf("PCMStreamAttributes(%d) sample_rate = %q, want fallback \"16000\"", rate, got)
		}
	}
}

func TestPCMDurationMs(t *testing.T) {
	cases := []struct {
		name    string
		byteLen int64
		rate    int
		want    int64
	}{
		// 24000 samples (48000 bytes) at 24 kHz = exactly 1s.
		{"one second at 24kHz", 48000, 24000, 1000},
		// 16000 samples (32000 bytes) at 16 kHz = exactly 1s.
		{"one second at 16kHz", 32000, 16000, 1000},
		// 480 samples (960 bytes) at 24 kHz = 20ms (one realtime frame).
		{"20ms frame at 24kHz", 960, 24000, 20},
		{"zero bytes", 0, 24000, 0},
		{"zero rate", 48000, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PCMDurationMs(tc.byteLen, tc.rate); got != tc.want {
				t.Fatalf("PCMDurationMs(%d, %d) = %d, want %d", tc.byteLen, tc.rate, got, tc.want)
			}
		})
	}
}

// TestForwardMeter_CountsAndResets: the meter accumulates frames+bytes across
// one segment, snapshots the totals, and starts the next segment from zero --
// the per-utterance "frames forwarded to vendor" observability contract.
func TestForwardMeter_CountsAndResets(t *testing.T) {
	var m ForwardMeter

	// Empty segment (barge-in before any frame): zero snapshot, no wall time.
	empty := m.SnapshotAndReset()
	if empty.Frames != 0 || empty.Bytes != 0 || empty.Wall != 0 {
		t.Fatalf("empty snapshot = %+v, want zero", empty)
	}

	m.Add(960)
	m.Add(960)
	m.Add(480)
	snap := m.SnapshotAndReset()
	if snap.Frames != 3 {
		t.Fatalf("Frames = %d, want 3", snap.Frames)
	}
	if snap.Bytes != 2400 {
		t.Fatalf("Bytes = %d, want 2400", snap.Bytes)
	}
	if snap.Wall < 0 {
		t.Fatalf("Wall = %v, want >= 0", snap.Wall)
	}

	// Next segment starts from zero.
	m.Add(100)
	next := m.SnapshotAndReset()
	if next.Frames != 1 || next.Bytes != 100 {
		t.Fatalf("post-reset snapshot = %+v, want 1 frame / 100 bytes", next)
	}
}
