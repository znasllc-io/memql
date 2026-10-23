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
			if window := packet.appKitWindow(); window != 42 {
				t.Fatalf("AppKit cannot route the pointer packet to its observed window: got %d, want 42", window)
			}
		})
	}
}

func TestDarwinAppKitMousePacketsPreserveRoutingAndCoordinates(t *testing.T) {
	for index, button := range []string{"left", "right", "middle"} {
		for _, down := range []bool{true, false} {
			packet, err := newDarwinTargetEvent(WindowInfo{ID: 42, PID: 123, X: -350, Y: 120, Width: 800, Height: 600}, targetInputEvent{kind: "mouse", button: button, x: -100, y: 231, down: down, modifiers: []string{"shift"}})
			if err != nil {
				t.Fatal(err)
			}
			kind, actualButton, x, y, flags := packet.pointerDescription()
			wantKind := []int{1, 3, 25}[index]
			if !down {
				wantKind++
			}
			pid, _, _ := packet.routing()
			// A newly allocated private source can have a positive unique state
			// ID; -1 requests one, it is not necessarily the allocated ID.
			if packet.appKitWindow() != 42 || pid != 123 || kind != wantKind || actualButton != int64(index) || x != -100 || y != 231 || flags != 1<<17 || packet.sourceState() != packet.originalSourceState() || packet.sourceState() == 0 || packet.sourceState() == 1 {
				t.Fatalf("packet changed: window=%d pid=%d kind=%d button=%d point=(%f,%f) flags=%d sourceState=%d", packet.appKitWindow(), pid, kind, actualButton, x, y, flags, packet.sourceState())
			}
			packet.close()
		}
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
