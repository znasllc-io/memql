package imageverify

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

const protocolVersion = 1
const maxFrameBytes = 768 << 10
const maxPrepareBytes = 128 << 10

// A connection carries exactly one operation. HELLO binds both a fresh
// challenge and the canonical request hash before the outside worker persists
// the service identity; PREPARE is data only, and START is a separate message
// sent only after the actual bounded child cgroup has been verified.
type serviceRequest struct {
	Version   int     `json:"version"`
	Operation string  `json:"operation"`
	ID        string  `json:"id"`
	Workspace string  `json:"workspace,omitempty"`
	SourceSHA string  `json:"sourceSHA,omitempty"`
	Build     Request `json:"build"`
	Limits    Limits  `json:"limits"`
	Previous  *Record `json:"previous,omitempty"`
}
type wireMessage struct {
	Type          string          `json:"type"`
	Challenge     string          `json:"challenge"`
	RequestSHA256 string          `json:"requestSHA256"`
	Hello         *HelloIdentity  `json:"hello,omitempty"`
	Prepare       *serviceRequest `json:"prepare,omitempty"`
	ChildGroup    string          `json:"childGroup,omitempty"`
	Receipt       json.RawMessage `json:"receipt,omitempty"`
	Log           []byte          `json:"log,omitempty"`
	Stderr        bool            `json:"stderr,omitempty"`
	Error         string          `json:"error,omitempty"`
	Cleaned       bool            `json:"cleaned,omitempty"`
}
type framedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *framedWriter) send(message wireMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > maxFrameBytes {
		return errors.New("image verification frame exceeds its bound")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))
	if err = writeAll(w.out, prefix[:]); err != nil {
		return err
	}
	return writeAll(w.out, data)
}
func writeAll(out io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := out.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func readMessage(in io.Reader) (wireMessage, error) {
	var message wireMessage
	var prefix [4]byte
	if _, err := io.ReadFull(in, prefix[:]); err != nil {
		return message, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > maxFrameBytes {
		return message, errors.New("image verification frame exceeds its bound")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(in, data); err != nil {
		return message, err
	}
	if err := decodeClosedServiceObject(data, &message); err != nil {
		return message, err
	}
	if len(message.Log) > 16384 || len(message.Error) > 4096 || len(message.Receipt) > MaxReceiptBytes || !serviceNonce.MatchString(message.Challenge) || !serviceHash.MatchString(message.RequestSHA256) {
		return message, errors.New("image verification field exceeds its contract")
	}
	if err := validateMessageShape(message); err != nil {
		return message, err
	}

	return message, nil
}
func requestDigest(request serviceRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	if len(data) > maxPrepareBytes {
		return "", errors.New("image verification request exceeds its bound")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func randomHex(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (m wireMessage) bound(challenge, hash string) bool {
	return m.Challenge == challenge && m.RequestSHA256 == hash
}

// The closed union avoids accidentally interpreting a control frame with an
// additional action payload. Connection identities are checked by the caller.
func validateMessageShape(m wireMessage) error {
	hasHello, hasPrepare, hasChild := m.Hello != nil, m.Prepare != nil, m.ChildGroup != ""
	hasResult := len(m.Receipt) > 0 || m.Error != "" || m.Cleaned
	hasLog := len(m.Log) > 0 || m.Stderr
	valid := false
	switch m.Type {
	case "hello":
		valid = !hasPrepare && !hasChild && !hasResult && !hasLog
	case "prepare":
		valid = hasPrepare && !hasHello && !hasChild && !hasResult && !hasLog
	case "ready":
		valid = hasChild && !hasHello && !hasPrepare && !hasResult && !hasLog
	case "start", "cancel":
		valid = !hasHello && !hasPrepare && !hasChild && !hasResult && !hasLog
	case "log":
		valid = len(m.Log) > 0 && !hasHello && !hasPrepare && !hasChild && !hasResult
	case "result":
		valid = !hasHello && !hasPrepare && !hasChild && !hasLog
	}
	if !valid {
		return errors.New("image verification message has an invalid payload")
	}
	return nil
}
