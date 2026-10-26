package imageverify

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return w.Buffer.Write(p)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
func TestProtocolHandlesShortWritesAndRejectsStall(t *testing.T) {
	out := &shortWriter{}
	message := wireMessage{Type: "hello", Challenge: strings.Repeat("a", 32), RequestSHA256: strings.Repeat("b", 64)}
	if err := (&framedWriter{out: out}).send(message); err != nil {
		t.Fatal(err)
	}
	got, err := readMessage(out)
	if err != nil || !reflect.DeepEqual(got, message) {
		t.Fatalf("message changed: %#v %v", got, err)
	}
	if err := (&framedWriter{out: zeroWriter{}}).send(message); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("stalled writer did not fail")
	}
}
func TestProtocolRejectsOversizeTruncationAndUnknownFields(t *testing.T) {
	for label, raw := range map[string][]byte{"oversize": {0, 16, 0, 0}, "truncated": {0, 0, 0, 8, '{'}} {
		t.Run(label, func(t *testing.T) {
			if _, err := readMessage(bytes.NewReader(raw)); err == nil {
				t.Fatal("accepted malformed frame")
			}
		})
	}
	for _, body := range []string{`{"type":"hello","executable":"/bin/sh"}`, `{"type":"hello"} {"type":"start"}`} {
		var frame bytes.Buffer
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
		frame.Write(prefix[:])
		frame.WriteString(body)
		if _, err := readMessage(&frame); err == nil {
			t.Fatal("accepted untyped or trailing input")
		}
	}
}
func TestCanonicalRequestHashBindsArgsAndIdentity(t *testing.T) {
	a := serviceRequest{Version: 1, Operation: "verify", ID: strings.Repeat("a", 32), Build: Request{Args: map[string]string{"B": "2", "A": "1"}}}
	b := a
	b.Build.Args = map[string]string{"A": "1", "B": "2"}
	ha, e := requestDigest(a)
	if e != nil {
		t.Fatal(e)
	}
	hb, _ := requestDigest(b)
	if ha != hb {
		t.Fatal("map order changed request identity")
	}
	b.Build.Args["A"] = "different"
	hc, _ := requestDigest(b)
	if ha == hc {
		t.Fatal("argument change did not alter request identity")
	}
	var wire bytes.Buffer
	msg := wireMessage{Type: "prepare", Challenge: strings.Repeat("a", 32), RequestSHA256: ha, Prepare: &a}
	if err := (&framedWriter{out: &wire}).send(msg); err != nil {
		t.Fatal(err)
	}
	got, err := readMessage(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.bound(strings.Repeat("b", 32), ha) || got.bound(strings.Repeat("a", 32), hc) {
		t.Fatal("stale connection identity accepted")
	}
	if _, err = json.Marshal(got.Prepare); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolRejectsDuplicateAndMixedControlPayloads(t *testing.T) {
	for _, body := range []string{`{"type":"start","type":"cancel"}`, `{"type":"start","challenge":"` + strings.Repeat("a", 32) + `","requestSHA256":"` + strings.Repeat("b", 64) + `","prepare":{"version":1}}`} {
		var frame bytes.Buffer
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
		frame.Write(prefix[:])
		frame.WriteString(body)
		if _, err := readMessage(&frame); err == nil {
			t.Fatal("ambiguous control input accepted")
		}
	}
}
