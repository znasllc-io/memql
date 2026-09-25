package audio

import (
	"context"
	"encoding/binary"
	"fmt"
)

// RoomTransport carries media only. Prompts, routing and workspace operations
// stay in the engine, under the authenticated person's context.
type RoomTransport interface {
	Open(context.Context, RoomOptions, func([]byte), func()) (Room, error)
}
type RoomOptions struct{ Name, Identity string }
type RoomCredentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Room  string `json:"room"`
}
type Room interface {
	Credentials() RoomCredentials
	Publish(context.Context, []byte, int) error // mono signed PCM16 and sample rate
	Event(any) error
	Close()
}

// DecodeWAV validates chunk boundaries instead of assuming a 44-byte header:
// TTS runtimes can include metadata and extended fmt chunks before the PCM.
func DecodeWAV(data []byte) ([]byte, int, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("speech runtime must return a PCM WAV file")
	}
	var rate int
	var pcm []byte
	valid := false
	for offset := 12; offset+8 <= len(data); {
		size := int64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size > int64(len(data)-offset) {
			return nil, 0, fmt.Errorf("truncated WAV chunk")
		}
		chunk := data[offset : offset+int(size)]
		switch string(data[offset-8 : offset-4]) {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, 0, fmt.Errorf("invalid WAV format")
			}
			rate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			valid = binary.LittleEndian.Uint16(chunk[:2]) == 1 && binary.LittleEndian.Uint16(chunk[2:4]) == 1 && binary.LittleEndian.Uint16(chunk[14:16]) == 16 && rate >= 8000 && rate <= 96000
		case "data":
			pcm = chunk
		}
		offset += int(size) + int(size%2)
	}
	if !valid || len(pcm) == 0 || len(pcm)%2 != 0 {
		return nil, 0, fmt.Errorf("speech requires mono 16-bit PCM WAV at 8–96 kHz")
	}
	return pcm, rate, nil
}
