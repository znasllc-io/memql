package deploycontrol

import (
	"encoding/json"
	"testing"
)

func TestParseCapabilityResult_Success(t *testing.T) {
	stdout := []byte(`{"ok":true,"capability":"k3d.down","changed":true,"result":{"cluster":"memql","deleted":true},"error":null}` + "\n")
	r, err := ParseCapabilityResult(stdout)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !r.OK || r.Capability != "k3d.down" || !r.Changed {
		t.Fatalf("unexpected envelope: %+v", r)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err() on a success envelope: %v", err)
	}
	var got struct {
		Cluster string `json:"cluster"`
		Deleted bool   `json:"deleted"`
	}
	if uerr := json.Unmarshal(r.Result, &got); uerr != nil {
		t.Fatal(uerr)
	}
	if got.Cluster != "memql" || !got.Deleted {
		t.Fatalf("unexpected result body: %+v", got)
	}
}

func TestParseCapabilityResult_Failure(t *testing.T) {
	stdout := []byte(`{"ok":false,"capability":"k3d.up","changed":false,"result":{},"error":{"code":4,"message":"docker is not running"}}`)
	r, err := ParseCapabilityResult(stdout)
	if err != nil {
		t.Fatalf("parse should not error on a well-formed failure envelope: %v", err)
	}
	if r.OK {
		t.Fatal("OK should be false")
	}
	e := r.Err()
	if e == nil {
		t.Fatal("Err() must be non-nil for a failure envelope")
	}
	if got := e.Error(); got == "" || r.Error == nil || r.Error.Code != 4 {
		t.Fatalf("unexpected error surfacing: %q / %+v", got, r.Error)
	}
}

// A capability writes logs to stderr, so stdout the parser sees is the envelope
// alone (possibly with a trailing newline). Confirm the trailing-newline case.
func TestParseCapabilityResult_TrailingNewline(t *testing.T) {
	stdout := []byte(`{"ok":true,"capability":"k3d.status","changed":false,"result":{"pods":3},"error":null}` + "\n\n")
	r, err := ParseCapabilityResult(stdout)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !r.OK || r.Capability != "k3d.status" {
		t.Fatalf("unexpected envelope: %+v", r)
	}
}

func TestParseCapabilityResult_NoEnvelope(t *testing.T) {
	if _, err := ParseCapabilityResult([]byte("INFO: doing a thing\nINFO: done\n")); err == nil {
		t.Fatal("expected an error when stdout carries no JSON envelope")
	}
	if _, err := ParseCapabilityResult(nil); err == nil {
		t.Fatal("expected an error on empty stdout")
	}
}

func TestParseCapabilityResult_Malformed(t *testing.T) {
	if _, err := ParseCapabilityResult([]byte(`{"ok":true,`)); err == nil {
		t.Fatal("expected an error on a malformed envelope")
	}
}
