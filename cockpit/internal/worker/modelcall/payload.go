package modelcall

import (
	"fmt"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Payload maps the engine's binary modality envelopes. Text remains in messages.
type Payload struct {
	Images         []ImagePart
	Audio          []byte
	AudioMediaType string
	Speech         SpeakRequest
	Image          ImageRequest
}

func payloadFor(start *memqlv1.ModelCallStart) (Payload, bool) { return readPayload(start) }

// Kept as an injectable seam for the serving-path tests.
var readPayload = func(start *memqlv1.ModelCallStart) (Payload, bool) {
	if start == nil {
		return Payload{}, false
	}
	var p Payload
	for _, message := range start.GetMessages() {
		for _, image := range message.GetImages() {
			p.Images = append(p.Images, ImagePart{Data: image.GetData(), MediaType: image.GetMediaType()})
		}
	}
	if a := start.GetAudio(); a != nil {
		p.Audio = a.GetData()
		p.AudioMediaType = a.GetMediaType()
	}
	if v := start.GetSpeech(); v != nil {
		p.Speech = SpeakRequest{Voice: v.GetVoice(), Format: v.GetFormat(), Speed: v.GetSpeed(), SpeedSet: v.GetSpeedSet()}
	}
	if v := start.GetImage(); v != nil {
		p.Image = ImageRequest{Width: int(v.GetWidth()), Height: int(v.GetHeight()), Count: int(v.GetCount()), Format: v.GetFormat()}
	}
	switch start.GetKind() {
	case KindVision:
		return p, len(p.Images) > 0
	case KindTranscribe:
		return p, len(p.Audio) > 0 && len(p.Audio) <= 16<<20
	case KindSpeak, KindImage:
		return p, true // Knobs are optional; the prompt is in messages.
	default:
		return p, false
	}
}

// audioFormatFor turns a media type into the container name the
// OpenAI-compatible input_audio field wants.
//
// A media type this side does not recognise yields an empty string,
// which Transcribe refuses by name. That is the fail-closed direction,
// and it is the whole reason this conversion is a table rather than a
// string split on "/": "audio/x-wav" and "audio/wave" are both wav, and
// "audio/webm" is not "webm" by coincidence but by the same list.
func audioFormatFor(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/ogg", "audio/opus":
		return "opus"
	case "audio/flac":
		return "flac"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "m4a"
	case "audio/webm":
		return "webm"
	default:
		return ""
	}
}

// modalityResult is a modality call's non-text OUTPUT, on its way to
// ModelCallEnd. The text half rides the deltas, exactly as a chat
// generation's does.
type modalityResult struct {
	Segments       []TranscriptSegment
	Audio          []byte
	AudioMediaType string
	Images         []ImagePart
}

func (r modalityResult) empty() bool {
	return len(r.Segments) == 0 && len(r.Audio) == 0 && len(r.Images) == 0
}

// Non-streaming media belongs on End; never encode binary data into content.
func attachModalityResult(end *memqlv1.ModelCallEnd, r modalityResult) {
	for _, segment := range r.Segments {
		end.Segments = append(end.Segments, &memqlv1.ModelCallTranscriptSegment{StartSeconds: segment.StartSeconds, EndSeconds: segment.EndSeconds, Text: segment.Text})
	}
	if len(r.Audio) > 0 {
		end.Audio = &memqlv1.ModelCallAudio{Data: r.Audio, MediaType: r.AudioMediaType}
	}
	for _, image := range r.Images {
		end.Images = append(end.Images, &memqlv1.ModelCallImage{Data: image.Data, MediaType: image.MediaType})
	}
}

// isModalityKind reports whether this kind is served by runModality.
func isModalityKind(kind string) bool {
	_, ok := modalityKinds[kind]
	return ok
}

// quoteAll renders a list of kinds for a refusal sentence. The refusal
// names every kind this worker serves, because a router that sent an
// unknown one is a version skew and the operator reading the report
// needs to see which side is behind.
func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}

// modalityUnavailableSentence is the refusal, in one place so the four
// kinds cannot drift into four different wordings.
func modalityUnavailableSentence(word string) string {
	return fmt.Sprintf(
		"this cockpit serves %s but the call carried no %s payload", word, word)
}
