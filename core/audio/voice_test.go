package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeSpeechWAVWithMetadataAndRejectBadMedia(t *testing.T) {
	pcm := make([]byte, 960)
	wav := CreateWAVChunk(pcm, 24000, 1, 16)
	metadata := []byte{'J', 'U', 'N', 'K', 3, 0, 0, 0, 1, 2, 3, 0}
	extended := append(append(append([]byte{}, wav[:12]...), metadata...), wav[12:]...)
	binary.LittleEndian.PutUint32(extended[4:8], uint32(len(extended)-8))
	decoded, rate, err := DecodeWAV(extended)
	if err != nil || rate != 24000 || !bytes.Equal(decoded, pcm) {
		t.Fatalf("metadata WAV: rate=%d err=%v", rate, err)
	}
	for _, bad := range [][]byte{wav[:20], []byte("not audio"), CreateWAVChunk(pcm, 24000, 2, 16)} {
		if _, _, err := DecodeWAV(bad); err == nil {
			t.Fatal("accepted unsupported/truncated speech")
		}
	}
}

func TestDecodeCompletedStreamingWAV(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6}
	wav := CreateWAVChunk(pcm, 24000, 1, 16)
	binary.LittleEndian.PutUint32(wav[4:8], 0xffffffff)
	binary.LittleEndian.PutUint32(wav[40:44], 0xffffffff)
	decoded, rate, err := DecodeWAV(wav)
	if err != nil || rate != 24000 || !bytes.Equal(decoded, pcm) {
		t.Fatalf("streaming WAV: rate=%d err=%v", rate, err)
	}
	for _, bad := range [][]byte{wav[:len(wav)-1], append([]byte{}, wav...)} {
		if len(bad) == len(wav) {
			binary.LittleEndian.PutUint32(bad[40:44], uint32(len(pcm)+2))
		}
		if _, _, err := DecodeWAV(bad); err == nil {
			t.Fatal("accepted incomplete PCM")
		}
	}
}
