package avatarvendor

// forward.go holds the CGO-free pieces of the assistant-PCM -> avatar
// forwarding leg (#1428): the byte-stream header attributes and the
// forwarded-frames meter. The media-plane sink that uses them
// (avatarForwardingSink in the voice-tagged room glue, and the direct-path
// browser republish) stays vendor-neutral; everything testable about WHAT it
// stamps and HOW MUCH it forwarded lives here so the default CI lane proves
// it.

import (
	"strconv"
	"time"
)

// PCMStreamAttributes builds the byte-stream header attributes for one audio
// segment: the PRODUCER's actual PCM16 sample rate plus mono channel count.
//
// The rate stamped here must describe the bytes actually written -- the vendor
// engine resamples FROM this header (LiveKit Agents' DataStreamAudioReceiver
// reads it; the browser relay already forwards at whatever rate its
// AudioContext runs and Simli tracks it). Stamping the vendor's preferred rate
// instead of the producer's was the #1428 format bug: the realtime executor
// emits 24 kHz PCM16 while the header said 16 kHz, so even a wired-up
// forwarding leg would have lip-synced at the wrong rate. A non-positive rate
// falls back to DefaultPCMSampleRate so a misconfigured caller still produces
// a well-formed header.
func PCMStreamAttributes(sampleRate int) map[string]string {
	if sampleRate <= 0 {
		sampleRate = DefaultPCMSampleRate
	}
	return map[string]string{
		"sample_rate":  strconv.Itoa(sampleRate),
		"num_channels": "1",
	}
}

// PCMDurationMs returns the playback duration in milliseconds of `byteLen`
// bytes of mono PCM16 at `sampleRate`. Used by the forwarding meter to report
// how much assistant SPEECH (not just bytes) reached the vendor. Returns 0 for
// non-positive inputs.
func PCMDurationMs(byteLen int64, sampleRate int) int64 {
	if byteLen <= 0 || sampleRate <= 0 {
		return 0
	}
	// 2 bytes per sample, mono.
	samples := byteLen / 2
	return samples * 1000 / int64(sampleRate)
}

// ForwardMeter counts the assistant-PCM frames forwarded to the avatar vendor
// over one audio segment (one utterance). The forwarding sink Adds on every
// frame it writes and Snapshots on segment close (Flush) to emit the
// frames-forwarded-to-vendor log line -- the observability #1428 adds so a
// dead forwarding leg shows up as a MISSING per-utterance log instead of
// silently-frozen lips.
//
// Not safe for concurrent use on its own: the forwarding sink already
// serializes frame writes under its own mutex, and the meter is documented to
// live under that same lock.
type ForwardMeter struct {
	frames  int64
	bytes   int64
	started time.Time
}

// Add records one forwarded frame of byteLen bytes. The first Add of a
// segment stamps the segment start time (used for the wall-clock duration in
// the snapshot).
func (m *ForwardMeter) Add(byteLen int) {
	if m.frames == 0 {
		m.started = time.Now()
	}
	m.frames++
	m.bytes += int64(byteLen)
}

// ForwardSnapshot is one closed segment's forwarding totals.
type ForwardSnapshot struct {
	// Frames / Bytes are the forwarded frame count and byte total.
	Frames int64
	Bytes  int64
	// Wall is the wall-clock span from the first to the last forwarded frame's
	// Add (zero when no frames were forwarded).
	Wall time.Duration
}

// SnapshotAndReset returns the segment totals and resets the meter for the
// next segment. A zero-frame snapshot (Frames == 0) means nothing was
// forwarded since the last reset; callers skip the log line for those (a
// barge-in can flush before any frame lands).
func (m *ForwardMeter) SnapshotAndReset() ForwardSnapshot {
	snap := ForwardSnapshot{Frames: m.frames, Bytes: m.bytes}
	if m.frames > 0 {
		snap.Wall = time.Since(m.started)
	}
	m.frames = 0
	m.bytes = 0
	m.started = time.Time{}
	return snap
}
