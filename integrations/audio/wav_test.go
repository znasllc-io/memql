package audio

import (
	"testing"
)

func TestWAVHeader(t *testing.T) {
	// Test creating a WAV header for 1 second of 24kHz mono 16-bit audio
	// 1 second = 24000 samples * 2 bytes = 48000 bytes
	pcmSize := 48000
	sampleRate := 24000
	numChannels := 1
	bitsPerSample := 16

	header := WAVHeader(pcmSize, sampleRate, numChannels, bitsPerSample)

	if len(header) != WAVHeaderSize {
		t.Errorf("Header size = %d, want %d", len(header), WAVHeaderSize)
	}

	// Check RIFF header
	if string(header[0:4]) != "RIFF" {
		t.Error("Missing RIFF marker")
	}

	// Check WAVE marker
	if string(header[8:12]) != "WAVE" {
		t.Error("Missing WAVE marker")
	}

	// Check fmt marker
	if string(header[12:16]) != "fmt " {
		t.Error("Missing fmt marker")
	}

	// Check data marker
	if string(header[36:40]) != "data" {
		t.Error("Missing data marker")
	}
}

func TestCreateWAVChunk(t *testing.T) {
	// Create a small PCM chunk
	pcmData := make([]byte, 4800) // 100ms at 24kHz mono 16-bit
	for i := range pcmData {
		pcmData[i] = byte(i % 256)
	}

	wav := CreateWAVChunk(pcmData, 24000, 1, 16)

	expectedSize := WAVHeaderSize + len(pcmData)
	if len(wav) != expectedSize {
		t.Errorf("WAV size = %d, want %d", len(wav), expectedSize)
	}

	// Verify header starts correctly
	if string(wav[0:4]) != "RIFF" {
		t.Error("WAV doesn't start with RIFF")
	}

	// Verify PCM data is included
	for i, b := range pcmData {
		if wav[WAVHeaderSize+i] != b {
			t.Errorf("PCM data mismatch at position %d", i)
			break
		}
	}
}

func TestChunkPCMToWAV(t *testing.T) {
	// Create 1 second of PCM data (24000 samples * 2 bytes = 48000 bytes)
	pcmData := make([]byte, 48000)
	for i := range pcmData {
		pcmData[i] = byte(i % 256)
	}

	// Chunk into ~200ms pieces
	chunks := ChunkPCMToWAV(pcmData, 24000, 1, 16, 200)

	// Should get approximately 5 chunks (1000ms / 200ms)
	if len(chunks) < 4 || len(chunks) > 6 {
		t.Errorf("Expected ~5 chunks, got %d", len(chunks))
	}

	// Each chunk should be a valid WAV file
	for i, chunk := range chunks {
		if len(chunk.Data) < WAVHeaderSize {
			t.Errorf("Chunk %d too small: %d bytes", i, len(chunk.Data))
			continue
		}

		if string(chunk.Data[0:4]) != "RIFF" {
			t.Errorf("Chunk %d doesn't start with RIFF", i)
		}

		if chunk.SampleRate != 24000 {
			t.Errorf("Chunk %d SampleRate = %d, want 24000", i, chunk.SampleRate)
		}

		// Duration should be approximately 200ms (except possibly the last chunk)
		if i < len(chunks)-1 && (chunk.Duration < 0.15 || chunk.Duration > 0.25) {
			t.Errorf("Chunk %d Duration = %f, expected ~0.2", i, chunk.Duration)
		}
	}

	// Total duration should be approximately 1 second
	var totalDuration float64
	for _, chunk := range chunks {
		totalDuration += chunk.Duration
	}
	if totalDuration < 0.9 || totalDuration > 1.1 {
		t.Errorf("Total duration = %f, expected ~1.0", totalDuration)
	}
}

func TestChunkPCMToWAV_Empty(t *testing.T) {
	chunks := ChunkPCMToWAV(nil, 24000, 1, 16, 200)
	if chunks != nil {
		t.Error("Expected nil for empty input")
	}

	chunks = ChunkPCMToWAV([]byte{}, 24000, 1, 16, 200)
	if chunks != nil {
		t.Error("Expected nil for empty slice")
	}
}

func TestChunkPCMToWAV_SmallInput(t *testing.T) {
	// Very small input (less than one chunk)
	pcmData := make([]byte, 100)
	chunks := ChunkPCMToWAV(pcmData, 24000, 1, 16, 200)

	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for small input, got %d", len(chunks))
	}

	if len(chunks) > 0 && string(chunks[0].Data[0:4]) != "RIFF" {
		t.Error("Small chunk doesn't start with RIFF")
	}
}
