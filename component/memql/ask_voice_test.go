package memql

import (
	"encoding/binary"
	"testing"
)

func TestVoiceActivityIgnoresClicksAndCommitsAfterSpeech(t *testing.T) {
	frame := func(value int16) []byte {
		b := make([]byte, 960)
		for i := 0; i < len(b); i += 2 {
			binary.LittleEndian.PutUint16(b[i:], uint16(value))
		}
		return b
	}
	silence, speech := frame(0), frame(5000)
	detector := voiceActivity{}
	for i := 0; i < 100; i++ {
		input := silence
		if i == 10 {
			input = speech
		}
		started, utterance := detector.push(input)
		if started || len(utterance) > 0 {
			t.Fatal("click interrupted or became a turn")
		}
	}
	starts, turns := 0, 0
	for i := 0; i < 70; i++ {
		input := silence
		if i < 30 {
			input = speech
		}
		started, utterance := detector.push(input)
		if started {
			starts++
		}
		if len(utterance) > 0 {
			turns++
			if i < 62 {
				t.Fatal("committed before the end-of-speech pause")
			}
		}
	}
	if starts != 1 || turns != 1 {
		t.Fatalf("starts=%d turns=%d", starts, turns)
	}
}

func TestSpeechSegmentationWaitsForSentenceOrBound(t *testing.T) {
	first := "This is a complete sentence that is long enough to speak."
	sentence, rest := nextSpeechSegment(first+" Still arriving", false)
	if sentence != first || rest != " Still arriving" {
		t.Fatalf("sentence=%q rest=%q", sentence, rest)
	}
	if sentence, _ := nextSpeechSegment("An unfinished response", false); sentence != "" {
		t.Fatal("spoke a partial short sentence")
	}
}
