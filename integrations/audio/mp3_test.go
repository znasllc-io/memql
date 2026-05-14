package audio

import (
	"testing"
)

// Sample MP3 frame header bytes for testing
// This is a valid MPEG1 Layer3 frame header: 0xFFFA (sync+version+layer) + bitrate/samplerate/etc
var validMP3Header = []byte{
	0xFF, 0xFB, 0x90, 0x00, // Valid MPEG1 Layer3 128kbps 44100Hz stereo
}

func TestFindMP3SyncWord(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		startPos int
		want     int
	}{
		{
			name:     "sync at start",
			data:     append(validMP3Header, make([]byte, 100)...),
			startPos: 0,
			want:     0,
		},
		{
			name:     "sync after garbage",
			data:     append([]byte{0x00, 0x00, 0x00}, append(validMP3Header, make([]byte, 100)...)...),
			startPos: 0,
			want:     3,
		},
		{
			name:     "no sync found",
			data:     []byte{0x00, 0x00, 0x00, 0x00},
			startPos: 0,
			want:     -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findMP3SyncWord(tt.data, tt.startPos)
			if got != tt.want {
				t.Errorf("findMP3SyncWord() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMP3FrameHeader(t *testing.T) {
	// Valid MPEG1 Layer3 128kbps 44100Hz stereo frame header
	// Frame size = (1152 * 128000 / 8) / 44100 = 417 bytes (no padding)
	header := []byte{0xFF, 0xFB, 0x90, 0x00}

	frame, size, err := parseMP3FrameHeader(header)
	if err != nil {
		t.Fatalf("parseMP3FrameHeader() error = %v", err)
	}

	if frame.Bitrate != 128 {
		t.Errorf("Bitrate = %v, want 128", frame.Bitrate)
	}

	if frame.SampleRate != 44100 {
		t.Errorf("SampleRate = %v, want 44100", frame.SampleRate)
	}

	expectedSize := 417 // (1152 * 128000 / 8) / 44100
	if size != expectedSize {
		t.Errorf("Frame size = %v, want %v", size, expectedSize)
	}

	// Duration should be samples/samplerate = 1152/44100 ≈ 0.0261s
	expectedDuration := float64(1152) / float64(44100)
	if frame.Duration < expectedDuration-0.001 || frame.Duration > expectedDuration+0.001 {
		t.Errorf("Duration = %v, want ~%v", frame.Duration, expectedDuration)
	}
}

func TestValidateMP3(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "valid MP3 header",
			data: append(validMP3Header, make([]byte, 100)...),
			want: true,
		},
		{
			name: "too short",
			data: []byte{0xFF, 0xFB},
			want: false,
		},
		{
			name: "no sync word",
			data: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateMP3(tt.data); got != tt.want {
				t.Errorf("ValidateMP3() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConcatenateFrames(t *testing.T) {
	frames := [][]byte{
		{0x01, 0x02},
		{0x03, 0x04},
		{0x05, 0x06},
	}

	result := concatenateFrames(frames)

	expected := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if len(result) != len(expected) {
		t.Errorf("concatenateFrames() length = %v, want %v", len(result), len(expected))
	}

	for i, b := range result {
		if b != expected[i] {
			t.Errorf("concatenateFrames()[%d] = %v, want %v", i, b, expected[i])
		}
	}
}

