package memql

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/audio"
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

func TestEchoReferenceRejectsOnlyRecentRepeatedSpeech(t *testing.T) {
	p := &voicePlayback{}
	p.speaking("Files and documents, compose reports and lists. What would you like?")
	for _, text := range []string{"Files and documents compose reports", "COMPOSE reports and lists."} {
		if !p.echo(text) {
			t.Fatalf("echo accepted: %q", text)
		}
	}
	for _, text := range []string{"stop", "wait", "What do you mean by files and documents?", "Create a report about birds", "yes"} {
		if p.echo(text) {
			t.Fatalf("user interruption rejected: %q", text)
		}
	}
	p.finished()
	p.through = time.Now().Add(-time.Second)
	if p.echo("Files and documents compose reports") {
		t.Fatal("old speech suppressed a new turn")
	}
}

type voiceTestRoom struct{}

func (voiceTestRoom) Credentials() audio.RoomCredentials         { return audio.RoomCredentials{} }
func (voiceTestRoom) Publish(context.Context, []byte, int) error { return nil }
func (voiceTestRoom) Event(any) error                            { return nil }
func (voiceTestRoom) Close()                                     {}

func TestVoiceFalseInterruptionPreservesReplyAndRealSpeechCancelsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []byte, 100)
	inputs := make(chan voiceInput, 3)
	inputs <- voiceInput{text: "hello"}
	inputs <- voiceInput{text: "files and documents compose reports"}
	inputs <- voiceInput{text: "stop and show me users"}
	started := make(chan string, 3)
	stopped := make(chan string, 3)
	checked := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		driveVoice(ctx, voiceTestRoom{}, input, func(context.Context, []byte) voiceInput { v := <-inputs; checked <- struct{}{}; return v }, func(ctx context.Context, v voiceInput, p *voicePlayback) {
			p.speaking("Files and documents compose reports and lists")
			started <- v.text
			<-ctx.Done()
			stopped <- v.text
		})
	}()
	utterance := func() {
		for i := 0; i < 65; i++ {
			frame := make([]byte, 960)
			if i < 30 {
				for j := 0; j < len(frame); j += 2 {
					binary.LittleEndian.PutUint16(frame[j:], 5000)
				}
			}
			input <- frame
		}
	}
	wait := func(ch <-chan string, want string) {
		t.Helper()
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("voice driver stalled")
		}
	}
	utterance()
	<-checked
	wait(started, "hello")
	utterance()
	<-checked
	select {
	case <-stopped:
		t.Fatal("echo discarded the reply")
	case <-time.After(20 * time.Millisecond):
	}
	utterance()
	<-checked
	wait(stopped, "hello")
	wait(started, "stop and show me users")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("voice driver leaked")
	}
}

func TestEchoUsesCaptureTimeEvenWhenTranscriptionIsSlow(t *testing.T) {
	p := &voicePlayback{}
	p.speaking("Files and documents compose reports")
	p.finished()
	capturedAt := time.Now().Add(-10 * time.Second)
	p.since = capturedAt.Add(-time.Second)
	p.through = time.Now().Add(-time.Second)
	if !p.echoAt("Files and documents compose reports", capturedAt) {
		t.Fatal("slow ASR turned playback into an interruption")
	}
	if p.echo("Files and documents compose reports") {
		t.Fatal("stale reference suppressed new speech")
	}
}

func TestEchoReferenceSurvivesPlaybackPauses(t *testing.T) {
	p := &voicePlayback{}
	p.speaking("Files and documents compose reports")
	p.since = time.Now().Add(-time.Minute)
	p.through = time.Now().Add(-time.Second)
	if !p.echo("Files and documents compose reports") {
		t.Fatal("delayed playback lost its echo reference before delivery finished")
	}
	p.finished()
	if !p.echo("Files and documents compose reports") {
		t.Fatal("playback completion lost the acoustic echo tail")
	}
	p.through = time.Now().Add(-time.Second)
	if p.echo("Files and documents compose reports") {
		t.Fatal("completed playback suppressed a later user turn")
	}
}

func TestVoiceKeepsSpeechThatArrivesDuringSlowTranscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan []byte)
	recognizing := make(chan struct{})
	release := make(chan struct{})
	replies := make(chan string, 2)
	done := make(chan struct{})
	calls := 0
	go func() {
		defer close(done)
		driveVoice(ctx, voiceTestRoom{}, input, func(context.Context, []byte) voiceInput {
			calls++
			if calls == 1 {
				close(recognizing)
				<-release
				return voiceInput{text: "first"}
			}
			return voiceInput{text: "second"}
		}, func(_ context.Context, v voiceInput, _ *voicePlayback) { replies <- v.text })
	}()
	send := func() {
		for i := 0; i < 65; i++ {
			frame := make([]byte, 960)
			if i < 30 {
				for j := 0; j < len(frame); j += 2 {
					binary.LittleEndian.PutUint16(frame[j:], 5000)
				}
			}
			select {
			case input <- frame:
			case <-time.After(time.Second):
				t.Fatal("microphone stopped draining")
			}
		}
	}
	send()
	<-recognizing
	send()
	close(release)
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-replies:
			if got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("speech was lost during transcription")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("voice driver leaked")
	}
}
