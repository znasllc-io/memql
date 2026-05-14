// Package audio provides audio format utilities for TTS streaming.
package audio

import (
	"encoding/binary"
)

// WAV format constants
const (
	// WAV header size is always 44 bytes for PCM
	WAVHeaderSize = 44

	// Audio format code for PCM
	wavFormatPCM = 1
)

// WAVHeader creates a valid WAV file header for PCM audio data.
// This creates a complete, independently decodable WAV file when combined with PCM data.
//
// Parameters:
//   - pcmDataSize: size of the PCM audio data in bytes
//   - sampleRate: sample rate in Hz (e.g., 24000)
//   - numChannels: number of audio channels (1 = mono, 2 = stereo)
//   - bitsPerSample: bits per sample (typically 16 for PCM16)
//
// Returns a 44-byte WAV header that can be prepended to raw PCM data.
func WAVHeader(pcmDataSize int, sampleRate, numChannels, bitsPerSample int) []byte {
	header := make([]byte, WAVHeaderSize)

	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	fileSize := pcmDataSize + WAVHeaderSize - 8 // Total file size minus 8 bytes for RIFF header

	// RIFF header
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(fileSize))
	copy(header[8:12], "WAVE")

	// fmt subchunk
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)                      // Subchunk1Size (16 for PCM)
	binary.LittleEndian.PutUint16(header[20:22], wavFormatPCM)            // AudioFormat (1 = PCM)
	binary.LittleEndian.PutUint16(header[22:24], uint16(numChannels))     // NumChannels
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))      // SampleRate
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))        // ByteRate
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))      // BlockAlign
	binary.LittleEndian.PutUint16(header[34:36], uint16(bitsPerSample))   // BitsPerSample

	// data subchunk
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(pcmDataSize)) // Subchunk2Size

	return header
}

// CreateWAVChunk creates a complete WAV file from PCM data.
// Each chunk is independently decodable by browsers using decodeAudioData().
func CreateWAVChunk(pcmData []byte, sampleRate, numChannels, bitsPerSample int) []byte {
	header := WAVHeader(len(pcmData), sampleRate, numChannels, bitsPerSample)
	wav := make([]byte, len(header)+len(pcmData))
	copy(wav[:len(header)], header)
	copy(wav[len(header):], pcmData)
	return wav
}

// WAVChunk represents a chunk of audio as a complete WAV file.
type WAVChunk struct {
	Data       []byte  // Complete WAV file (header + PCM data)
	SampleRate int     // Sample rate in Hz
	Duration   float64 // Duration in seconds
}

// ChunkPCMToWAV splits PCM audio data into independently decodable WAV chunks.
// Each chunk is a complete WAV file that browsers can decode with decodeAudioData().
//
// Parameters:
//   - pcmData: raw PCM audio data
//   - sampleRate: sample rate in Hz (e.g., 24000)
//   - numChannels: number of channels (1 = mono)
//   - bitsPerSample: bits per sample (16 for PCM16)
//   - targetDurationMS: target duration per chunk in milliseconds (recommended: 200-300)
//
// Returns a slice of WAV chunks, each independently decodable.
func ChunkPCMToWAV(pcmData []byte, sampleRate, numChannels, bitsPerSample, targetDurationMS int) []WAVChunk {
	if len(pcmData) == 0 {
		return nil
	}

	// Calculate bytes per sample and target chunk size
	bytesPerSample := bitsPerSample / 8
	bytesPerSecond := sampleRate * numChannels * bytesPerSample
	targetChunkBytes := (bytesPerSecond * targetDurationMS) / 1000

	// Ensure chunk size is aligned to sample boundary
	sampleSize := numChannels * bytesPerSample
	targetChunkBytes = (targetChunkBytes / sampleSize) * sampleSize

	if targetChunkBytes == 0 {
		targetChunkBytes = sampleSize
	}

	var chunks []WAVChunk

	for offset := 0; offset < len(pcmData); offset += targetChunkBytes {
		end := offset + targetChunkBytes
		if end > len(pcmData) {
			end = len(pcmData)
		}

		chunkPCM := pcmData[offset:end]
		wavData := CreateWAVChunk(chunkPCM, sampleRate, numChannels, bitsPerSample)

		duration := float64(len(chunkPCM)) / float64(bytesPerSecond)

		chunks = append(chunks, WAVChunk{
			Data:       wavData,
			SampleRate: sampleRate,
			Duration:   duration,
		})
	}

	return chunks
}

