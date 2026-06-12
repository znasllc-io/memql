package agent

// transcript_confidence.go is the #1431 input-transcription confidence gate:
// the realtime session requests per-token logprobs on the user's native input
// transcription (session include: item.input_audio_transcription.logprobs) and
// this gate drops a FINAL whose mean token logprob falls below a floor --
// silence/echo that slipped past noise reduction (#1431) and the server_vad
// energy gate (#1203) tends to transcribe as a low-confidence final.
//
// It COMPOSES with the #1199 post-hoc denylist filter (both run on the final;
// either can drop it); it does not replace it. CRITICAL contract: logprobs are
// intermittently missing even when requested (community-reported), so a final
// WITHOUT logprobs must always pass -- absence of the signal is never evidence
// of low confidence.
//
// Pure Go, no CGO/build-tag dependency: unit-testable in the default CI lane.

// transcriptConfidenceGate holds the mean-logprob floor an input-transcription
// final must clear when logprobs are present. The zero value is NOT meaningful
// (a floor of 0 would drop almost everything); build via the executor's
// SetTranscriptConfidenceFloor so a nil gate means "disabled".
type transcriptConfidenceGate struct {
	// floor is the minimum mean per-token logprob (logprobs are <= 0; closer
	// to 0 = more confident). MEMQL_REALTIME_TRANSCRIPT_MIN_CONFIDENCE.
	floor float64
}

// pass reports whether a final with the given per-token logprobs clears the
// gate. Missing logprobs (nil/empty) always pass.
func (g *transcriptConfidenceGate) pass(logprobs []float64) bool {
	if g == nil || len(logprobs) == 0 {
		return true
	}
	return meanLogprob(logprobs) >= g.floor
}

// meanLogprob is the arithmetic mean of the per-token logprobs. Returns 0 (the
// "fully confident" value) for an empty slice; callers gate on len first.
func meanLogprob(logprobs []float64) float64 {
	if len(logprobs) == 0 {
		return 0
	}
	var sum float64
	for _, lp := range logprobs {
		sum += lp
	}
	return sum / float64(len(logprobs))
}

// SetTranscriptConfidenceFloor arms the #1431 confidence gate on the executor:
// an input-transcription FINAL that arrives with logprobs whose mean falls
// below floor is dropped before it reaches chat. Call before Start (the drain
// loop reads it). Finals without logprobs always pass regardless of floor.
func (e *RealtimeExecutor) SetTranscriptConfidenceFloor(floor float64) {
	e.confidenceGate = &transcriptConfidenceGate{floor: floor}
}
