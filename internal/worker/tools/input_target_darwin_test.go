//go:build computeruse && darwin

package tools

import "testing"

// Creates and inspects Quartz packets only: never calls targetSend, posts
// input, queries focus, or activates an app. PID/window are fixture identities.
func TestDarwinPointerPacketsCarryObservedWindow(t *testing.T) {
	for _, kind := range []string{"move", "mouse"} {
		t.Run(kind, func(t *testing.T) {
			packet, err := newDarwinTargetEvent(WindowInfo{ID: 42, PID: 123}, targetInputEvent{kind: kind, button: "left", x: 100, y: 200, dy: 1, down: true})
			if err != nil {
				t.Fatal(err)
			}
			defer packet.close()
			pid, window, handler := packet.routing()
			if pid != 123 || window != 42 || handler != 42 {
				t.Fatalf("pointer can lose its observed destination: pid=%d window=%d handlingWindow=%d", pid, window, handler)
			}
		})
	}
}

func TestDarwinPacketsRefuseUnaddressedTargets(t *testing.T) {
	for _, w := range []WindowInfo{{ID: 42}, {PID: 123}, {ID: 42, PID: -1}} {
		packet, err := newDarwinTargetEvent(w, targetInputEvent{kind: "key", key: "enter", down: true})
		if packet != nil {
			packet.close()
			t.Fatal("created unaddressed packet")
		}
		if err == nil {
			t.Fatal("missing identity accepted")
		}
	}
}

func TestDarwinScrollAndKeyboardPacketsRetainPID(t *testing.T) {
	for _, input := range []targetInputEvent{{kind: "scroll", x: 10, y: 20, dy: 1}, {kind: "key", key: "enter", down: true}, {kind: "text", text: "Ω", down: true}} {
		packet, err := newDarwinTargetEvent(WindowInfo{ID: 42, PID: 123}, input)
		if err != nil {
			t.Fatal(err)
		}
		pid, window, handling := packet.routing()
		packet.close()
		if pid != 123 {
			t.Fatal("packet lost PID")
		}
		if window != 0 || handling != 0 {
			t.Fatal("keyboard/scroll must not claim mouse-window annotations")
		}
	}
}
