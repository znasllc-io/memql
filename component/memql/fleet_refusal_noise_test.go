package memql

import (
	"strings"
	"testing"
)

func TestFleetUnavailableOmitsRevokedAndForeignShareNoise(t *testing.T) {
	err := &FleetUnavailable{
		ModelId: "qwen3-embedding:0.6b",
		Total:   5,
		Considered: map[string]string{
			"v1:worker:registration:active": "does not offer model qwen3-embedding:0.6b",
			"v1:worker:registration:0ba":    "revoked",
			"v1:worker:registration:6ec":    "revoked",
			"v1:worker:registration:24ad":   "Neither consent is present. The owner has not shared it and the machine's policy.yaml does not set inference.serve: cluster. Both are needed.",
		},
		LastError: "worker_disconnected",
	}
	msg := err.Error()
	if strings.Contains(msg, "revoked") && !strings.Contains(msg, "revoked registration(s) omitted") {
		t.Fatalf("raw revoked lines must be omitted, got %q", msg)
	}
	if strings.Contains(msg, "24ad") || strings.Contains(msg, "Both are needed") {
		t.Fatalf("foreign share refusal must be summarised away when actionable reasons remain, got %q", msg)
	}
	if !strings.Contains(msg, "does not offer model") {
		t.Fatalf("actionable reason must remain, got %q", msg)
	}
	if !strings.Contains(msg, "other private machine(s) omitted") {
		t.Fatalf("want foreign-private summary, got %q", msg)
	}
}

func TestFleetUnavailableKeepsShareRefusalWhenOnlySignal(t *testing.T) {
	err := &FleetUnavailable{
		ModelId: "qwen3-embedding:0.6b",
		Total:   1,
		Considered: map[string]string{
			"v1:worker:registration:24ad": "Neither consent is present. Both are needed.",
		},
	}
	msg := err.Error()
	if !strings.Contains(msg, "Both are needed") {
		t.Fatalf("sole share refusal must stay visible, got %q", msg)
	}
}

func TestFleetUnavailableSanitizesWrongPodStreamLastError(t *testing.T) {
	err := &FleetUnavailable{
		ModelId:   "black",
		Total:     1,
		LastError: `this replica no longer holds a stream for black`,
		Considered: map[string]string{
			"v1:worker:registration:da0617": "offline",
		},
	}
	msg := err.Error()
	if strings.Contains(msg, "no longer holds a stream") {
		t.Fatalf("wrong-pod stream miss must not surface raw, got %q", msg)
	}
	if !strings.Contains(msg, "holding replica missed") {
		t.Fatalf("want sanitized affinity hint, got %q", msg)
	}
}
