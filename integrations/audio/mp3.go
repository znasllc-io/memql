// Package audio provides audio format utilities for TTS streaming.
package audio

import (
	"bytes"
	"errors"
	"fmt"
)

// MP3 frame header constants
const (
	mp3SyncWord      = 0xFFE0 // First 11 bits of sync word (0xFF + 0xE0 mask)
	mp3MinFrameSize  = 48     // Minimum MP3 frame size in bytes
	mp3HeaderSize    = 4      // MP3 frame header is 4 bytes
)

// MP3 bitrate tables (kbps) for MPEG Audio Layer III
// Index by [version][bitrate_index]
var mp3BitrateTable = map[int]map[int]int{
	// MPEG Version 1, Layer III
	3: {
		0: 0, 1: 32, 2: 40, 3: 48, 4: 56, 5: 64, 6: 80, 7: 96,
		8: 112, 9: 128, 10: 160, 11: 192, 12: 224, 13: 256, 14: 320, 15: 0,
	},
	// MPEG Version 2/2.5, Layer III
	2: {
		0: 0, 1: 8, 2: 16, 3: 24, 4: 32, 5: 40, 6: 48, 7: 56,
		8: 64, 9: 80, 10: 96, 11: 112, 12: 128, 13: 144, 14: 160, 15: 0,
	},
}

// MP3 sample rate tables (Hz)
// Index by [version][samplerate_index]
var mp3SampleRateTable = map[int]map[int]int{
	// MPEG Version 1
	3: {0: 44100, 1: 48000, 2: 32000, 3: 0},
	// MPEG Version 2
	2: {0: 22050, 1: 24000, 2: 16000, 3: 0},
	// MPEG Version 2.5
	0: {0: 11025, 1: 12000, 2: 8000, 3: 0},
}

// MP3 samples per frame
var mp3SamplesPerFrame = map[int]int{
	3: 1152, // MPEG Version 1
	2: 576,  // MPEG Version 2
	0: 576,  // MPEG Version 2.5
}

// MP3Frame represents a parsed MP3 frame.
type MP3Frame struct {
	Data       []byte
	SampleRate int
	Bitrate    int
	Duration   float64 // Duration in seconds
}

// MP3Chunk represents a chunk of MP3 frames that forms a complete, decodable unit.
type MP3Chunk struct {
	Data       []byte
	SampleRate int
	Duration   float64 // Total duration in seconds
	FrameCount int
}

// ParseMP3Frames parses MP3 data and returns individual frames.
func ParseMP3Frames(data []byte) ([]MP3Frame, error) {
	var frames []MP3Frame
	pos := 0

	for pos < len(data)-mp3HeaderSize {
		// Find sync word
		frameStart := findMP3SyncWord(data, pos)
		if frameStart < 0 {
			break // No more frames
		}

		// Parse frame header
		frame, frameSize, err := parseMP3FrameHeader(data[frameStart:])
		if err != nil {
			// Skip this byte and try next
			pos = frameStart + 1
			continue
		}

		// Ensure we have enough data for the full frame
		if frameStart+frameSize > len(data) {
			break // Incomplete frame at end
		}

		frame.Data = data[frameStart : frameStart+frameSize]
		frames = append(frames, frame)
		pos = frameStart + frameSize
	}

	if len(frames) == 0 {
		return nil, errors.New("no valid MP3 frames found")
	}

	return frames, nil
}

// ChunkMP3ForStreaming splits MP3 data into independently decodable chunks.
// Each chunk is a complete MP3 file that browsers can decode with decodeAudioData().
// targetDuration is the target duration per chunk in milliseconds (recommended: 200-300ms).
func ChunkMP3ForStreaming(data []byte, targetDurationMS int) ([]MP3Chunk, error) {
	frames, err := ParseMP3Frames(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse MP3 frames: %w", err)
	}

	if len(frames) == 0 {
		return nil, errors.New("no frames to chunk")
	}

	targetDuration := float64(targetDurationMS) / 1000.0
	var chunks []MP3Chunk
	var currentChunk MP3Chunk
	var currentFrames [][]byte

	for _, frame := range frames {
		currentFrames = append(currentFrames, frame.Data)
		currentChunk.Duration += frame.Duration
		currentChunk.FrameCount++
		currentChunk.SampleRate = frame.SampleRate

		// Check if we've reached target duration
		if currentChunk.Duration >= targetDuration {
			// Build chunk data
			currentChunk.Data = concatenateFrames(currentFrames)
			chunks = append(chunks, currentChunk)

			// Reset for next chunk
			currentChunk = MP3Chunk{}
			currentFrames = nil
		}
	}

	// Handle remaining frames
	if len(currentFrames) > 0 {
		currentChunk.Data = concatenateFrames(currentFrames)
		chunks = append(chunks, currentChunk)
	}

	return chunks, nil
}

// findMP3SyncWord finds the next MP3 sync word starting from pos.
// Returns -1 if not found.
func findMP3SyncWord(data []byte, pos int) int {
	for i := pos; i < len(data)-1; i++ {
		// Check for sync word: 11 bits set (0xFF followed by 0xE0 or higher)
		if data[i] == 0xFF && (data[i+1]&0xE0) == 0xE0 {
			// Validate it's actually a valid frame header
			if i+mp3HeaderSize <= len(data) {
				if _, _, err := parseMP3FrameHeader(data[i:]); err == nil {
					return i
				}
			}
		}
	}
	return -1
}

// parseMP3FrameHeader parses an MP3 frame header and returns frame info and size.
func parseMP3FrameHeader(data []byte) (MP3Frame, int, error) {
	if len(data) < mp3HeaderSize {
		return MP3Frame{}, 0, errors.New("insufficient data for frame header")
	}

	// Byte 0-1: Sync word + version + layer
	// Byte 2: Bitrate + sample rate + padding + private
	// Byte 3: Channel mode + mode extension + copyright + original + emphasis

	header := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])

	// Check sync word (11 bits)
	if (header >> 21) != 0x7FF {
		return MP3Frame{}, 0, errors.New("invalid sync word")
	}

	// MPEG Audio version (2 bits)
	version := int((header >> 19) & 0x03)
	if version == 1 { // Reserved
		return MP3Frame{}, 0, errors.New("reserved MPEG version")
	}

	// Layer (2 bits) - we only support Layer III
	layer := int((header >> 17) & 0x03)
	if layer != 1 { // Layer III = 01
		return MP3Frame{}, 0, errors.New("not Layer III")
	}

	// Bitrate index (4 bits)
	bitrateIndex := int((header >> 12) & 0x0F)
	if bitrateIndex == 0 || bitrateIndex == 15 {
		return MP3Frame{}, 0, errors.New("invalid bitrate index")
	}

	// Sample rate index (2 bits)
	sampleRateIndex := int((header >> 10) & 0x03)
	if sampleRateIndex == 3 {
		return MP3Frame{}, 0, errors.New("invalid sample rate index")
	}

	// Padding (1 bit)
	padding := int((header >> 9) & 0x01)

	// Get bitrate version key (MPEG1 = 3, MPEG2/2.5 = 2)
	bitrateVersionKey := version
	if version == 0 { // MPEG 2.5
		bitrateVersionKey = 2
	}

	bitrate := mp3BitrateTable[bitrateVersionKey][bitrateIndex]
	sampleRate := mp3SampleRateTable[version][sampleRateIndex]
	samplesPerFrame := mp3SamplesPerFrame[version]

	if bitrate == 0 || sampleRate == 0 {
		return MP3Frame{}, 0, errors.New("invalid bitrate or sample rate")
	}

	// Calculate frame size
	// Frame size = (samples_per_frame / 8 * bitrate * 1000) / sample_rate + padding
	frameSize := (samplesPerFrame * bitrate * 1000 / 8) / sampleRate + padding

	// Calculate duration
	duration := float64(samplesPerFrame) / float64(sampleRate)

	return MP3Frame{
		SampleRate: sampleRate,
		Bitrate:    bitrate,
		Duration:   duration,
	}, frameSize, nil
}

// concatenateFrames combines frame data into a single byte slice.
func concatenateFrames(frames [][]byte) []byte {
	var buf bytes.Buffer
	for _, frame := range frames {
		buf.Write(frame)
	}
	return buf.Bytes()
}

// ValidateMP3 checks if data appears to be valid MP3.
func ValidateMP3(data []byte) bool {
	if len(data) < mp3MinFrameSize {
		return false
	}
	pos := findMP3SyncWord(data, 0)
	return pos >= 0
}

