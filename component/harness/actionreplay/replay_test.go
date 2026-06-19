package actionreplay

import (
	"context"
	"fmt"
	"testing"
)

// stubInvoker returns canned results per call index and records the calls.
type stubInvoker struct {
	results map[string]string // capability -> JSON result
	errOn   string            // capability that should error
	seen    []string
}

func (s *stubInvoker) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	s.seen = append(s.seen, name)
	if name == s.errOn {
		return "", fmt.Errorf("boom")
	}
	if r, ok := s.results[name]; ok {
		return r, nil
	}
	return "{}", nil
}

func TestFingerprint_StableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{"path": "/x", "content": "hi", "mode": 644}
	b := map[string]any{"mode": 644, "content": "hi", "path": "/x"}
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatalf("fingerprint must be independent of map key order")
	}
	if Fingerprint(a) == Fingerprint(map[string]any{"path": "/y"}) {
		t.Fatalf("different values must hash differently")
	}
}

func TestReplay_ExecutesInOrderAndFingerprints(t *testing.T) {
	inv := &stubInvoker{results: map[string]string{
		"fs.mkdir":     `{"ok":true}`,
		"fs.writeFile": `{"bytes":5}`,
	}}
	calls := []ReplayCall{
		{Index: 0, Capability: "fs.mkdir", Args: map[string]any{"path": "/p"}},
		{Index: 1, Capability: "fs.writeFile", Args: map[string]any{"path": "/p/f", "content": "hello"}},
	}
	results, fp, err := Replay(context.Background(), inv, calls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if got := []string{"fs.mkdir", "fs.writeFile"}; !equal(inv.seen, got) {
		t.Fatalf("calls out of order: %v", inv.seen)
	}
	// Deterministic: replaying the same calls yields the same fingerprint.
	inv2 := &stubInvoker{results: inv.results}
	_, fp2, _ := Replay(context.Background(), inv2, calls)
	if fp != fp2 {
		t.Fatalf("identical replay must yield identical fingerprint")
	}
}

func TestReplay_CallErrorAborts(t *testing.T) {
	inv := &stubInvoker{errOn: "fs.writeFile"}
	calls := []ReplayCall{
		{Index: 0, Capability: "fs.mkdir", Args: map[string]any{"path": "/p"}},
		{Index: 1, Capability: "fs.writeFile", Args: map[string]any{"path": "/p/f"}},
	}
	_, fp, err := Replay(context.Background(), inv, calls)
	if err == nil {
		t.Fatalf("expected error on failing call")
	}
	if fp != "" {
		t.Fatalf("aborted replay must not return a fingerprint")
	}
}

func TestReplay_NilInvoker(t *testing.T) {
	if _, _, err := Replay(context.Background(), nil, nil); err == nil {
		t.Fatalf("nil invoker must error")
	}
}

func TestVerify(t *testing.T) {
	if !Verify("abc", "abc") {
		t.Fatalf("matching fingerprints must verify")
	}
	if Verify("abc", "def") {
		t.Fatalf("mismatched fingerprints must not verify")
	}
	if Verify("", "") {
		t.Fatalf("empty recorded fingerprint must be unverifiable")
	}
}

func TestReinforce_RisesAndCaps(t *testing.T) {
	r, n := Reinforce(0.5, 0)
	if !(r > 0.5 && r < 1.0) {
		t.Fatalf("reliability should rise toward 1.0, got %v", r)
	}
	if n != 1 {
		t.Fatalf("reinforceCount should increment, got %d", n)
	}
	// Repeated reinforcement converges to <=1.0 and never exceeds it.
	rel := 0.5
	cnt := 0
	for i := 0; i < 50; i++ {
		rel, cnt = Reinforce(rel, cnt)
	}
	if rel > 1.0 {
		t.Fatalf("reliability must cap at 1.0, got %v", rel)
	}
	if cnt != 50 {
		t.Fatalf("reinforceCount should be 50, got %d", cnt)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
